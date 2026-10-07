package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

var testStatuses = Statuses{
	{Label: "status:cancelled", Title: "Отменён", Final: true},
	{Label: "status:prod", Title: "В проде", Final: true, Notify: true},
	{Label: "status:in-progress", Title: "В работе", Notify: true},
	{Label: "status:dev", Title: "В dev"},
	{Label: "status:new", Title: "Заведён"},
}

// TestListSkipsPullRequests: REST GitHub считает issue каждый pull request, и
// без отсева метки чужого PR оказались бы статусом нашего тикета.
func TestListSkipsPullRequests(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	publishCase(t, cases, 7001, 12)

	server := githubStub(t, map[string]string{
		"GET /repos/galera-club/tg-intake/issues": `[
			{"number": 13, "pull_request": {"url": "x"}, "labels": [{"name": "status:prod"}]},
			{"number": 12, "labels": [{"name": "type:bug"}, {"name": "status:in-progress"}]}]`,
	})
	tickets := newTestTickets(t, cases, server.URL)

	list, _, err := tickets.List(context.Background(), testProject(t, pool), 7001, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("тикетов: %d, ожидался 1", len(list))
	}
	if list[0].Status.Label != "status:in-progress" {
		t.Errorf("статус: %q", list[0].Status.Label)
	}
}

// TestListBackfillsOldTicket: тикет, вытесненный из окна сканирования свежими
// issue и pull request'ами, добирается поштучно, а не остаётся «без статуса»
// при живом GitHub.
func TestListBackfillsOldTicket(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	publishCase(t, cases, 7009, 5)

	server := githubStub(t, map[string]string{
		"GET /repos/galera-club/tg-intake/issues":   `[{"number": 120, "labels": []}]`,
		"GET /repos/galera-club/tg-intake/issues/5": `{"number": 5, "labels": [{"name": "status:prod"}]}`,
	})
	tickets := newTestTickets(t, cases, server.URL)

	list, _, err := tickets.List(context.Background(), testProject(t, pool), 7009, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Status.Label != "status:prod" {
		t.Fatalf("статус не добрался поштучно: %+v", list)
	}
}

// TestListSurvivesGitHubError: просмотр не должен умирать вместе с чужим
// сервисом - номера и заголовки уже прочитаны из своей базы.
func TestListSurvivesGitHubError(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	publishCase(t, cases, 7002, 21)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	tickets := newTestTickets(t, cases, server.URL)

	list, _, err := tickets.List(context.Background(), testProject(t, pool), 7002, 0)
	if err != nil {
		t.Fatalf("отказ GitHub уронил список: %v", err)
	}
	if len(list) != 1 || list[0].Number != 21 {
		t.Fatalf("список: %+v", list)
	}
	if list[0].Status.Label != "" {
		t.Errorf("статус не мог прочитаться, а он есть: %q", list[0].Status.Label)
	}
}

// TestCancelRemovesAllStatusLabels: отмена ставит свою метку и снимает все
// прочие метки статуса точечно. PUT затёр бы и type, и author, проставленные
// не нами.
func TestCancelRemovesAllStatusLabels(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7003, 30)

	seen := &requestLog{}
	server := recordingStub(t, seen, map[string]string{
		"GET /repos/galera-club/tg-intake/issues/30": `{"number": 30, "labels": [
			{"name": "type:bug"}, {"name": "status:in-progress"}, {"name": "status:new"}]}`,
	})
	tickets := newTestTickets(t, cases, server.URL)

	job := Job{ID: 1, Kind: JobCancelIssue, Payload: cancelJSON(cs.ID, 7003)}
	if err := tickets.RunCancel(context.Background(), job); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	want := []string{
		"POST /repos/galera-club/tg-intake/issues/30/labels",
		"DELETE /repos/galera-club/tg-intake/issues/30/labels/status:in-progress",
		"DELETE /repos/galera-club/tg-intake/issues/30/labels/status:new",
		"PATCH /repos/galera-club/tg-intake/issues/30",
	}
	for _, w := range want {
		if !seen.has(w) {
			t.Errorf("не было запроса %s; было: %v", w, seen.list())
		}
	}
	for _, got := range seen.list() {
		if strings.HasPrefix(got, "PUT ") {
			t.Errorf("замена набора меток вместо точечной правки: %s", got)
		}
	}
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 1 {
		t.Errorf("уведомлений автору: %d, ожидалось 1", n)
	}
}

// TestCancelOutcomeMarked: тикет успели закрыть, пока отмена ждала очереди.
// Исход всё равно помечен исходом отмены - иначе он придёт новым сообщением, а
// экран «Отменяю тикет #N» останется висеть непереписанным.
func TestCancelOutcomeMarked(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7005, 50)

	server := githubStub(t, map[string]string{
		"GET /repos/galera-club/tg-intake/issues/50": `{"number": 50, "labels": [{"name": "status:prod"}]}`,
	})
	tickets := newTestTickets(t, cases, server.URL)

	job := Job{ID: 1, Kind: JobCancelIssue, Payload: cancelJSON(cs.ID, 7005)}
	if err := tickets.RunCancel(ctx, job); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	var buttons string
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(payload->>'buttons', '') FROM jobs
		WHERE kind = $1 AND payload->>'case_id' = $2`, JobNotify, cs.ID).Scan(&buttons)
	if err != nil {
		t.Fatalf("load notify: %v", err)
	}
	if buttons != keysCancel {
		t.Errorf("исход отмены помечен %q, ожидалось %q", buttons, keysCancel)
	}
}

// TestCancelDeniedForStranger: просмотр общий, отмена авторская. Кнопка живёт в
// чужом сообщении дольше, чем экран, поэтому право проверяется и в работе.
func TestCancelDeniedForStranger(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7004, 40)

	seen := &requestLog{}
	server := recordingStub(t, seen, nil)
	tickets := newTestTickets(t, cases, server.URL)

	job := Job{ID: 1, Kind: JobCancelIssue, Payload: cancelJSON(cs.ID, 9999)}
	if err := tickets.RunCancel(context.Background(), job); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := seen.list(); len(got) != 0 {
		t.Errorf("чужая отмена дошла до GitHub: %v", got)
	}

	if _, err := tickets.Cancel(context.Background(), testProject(t, pool), 40, 9999, 0); err != ErrNotAuthor {
		t.Errorf("постановка чужой отмены: %v", err)
	}
}

// TestCancelTwiceOneEvent: повтор работы после падения между закрытием issue и
// гашением работы не должен писать второе событие в журнал.
func TestCancelTwiceOneEvent(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7005, 50)

	server := githubStub(t, map[string]string{
		"GET /repos/galera-club/tg-intake/issues/50": `{"number": 50, "labels": [{"name": "status:new"}]}`,
	})
	tickets := newTestTickets(t, cases, server.URL)

	job := Job{ID: 1, Kind: JobCancelIssue, Payload: cancelJSON(cs.ID, 7005)}
	for range 2 {
		if err := tickets.RunCancel(context.Background(), job); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	}

	var events int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM case_events WHERE case_id = $1 AND kind = 'cancelled_by_author'`,
		cs.ID).Scan(&events)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("событий отмены: %d, ожидалось 1", events)
	}
}

func newTestTickets(t *testing.T, cases *Cases, api string) *Tickets {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewTickets(cases, NewGitHub("token", api, testStatuses, log), testStatuses, log, 0)
}

// publishCase доводит обращение до опубликованного тикета: просмотр работает
// только с ними.
func publishCase(t *testing.T, cases *Cases, userID int64, issue int) *Case {
	t.Helper()
	return publishCaseIn(t, cases, userID, issue, "tg-intake")
}

func publishCaseIn(t *testing.T, cases *Cases, userID int64, issue int, slug string) *Case {
	t.Helper()
	ctx := context.Background()

	cs, _, err := cases.StartCase(ctx, User{ID: userID, First: "Тест"}, slug, modeTicket)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	_, err = cases.pool.Exec(ctx, `
		UPDATE cases SET status = 'published', kind = 'bug', title = 'Форма не сохраняется',
		                 summary = '## Случай' || chr(10) || 'заказ 4821', issue_number = $2::int,
		                 issue_url = 'https://example.invalid/issues/' || $2::int
		WHERE id = $1`, cs.ID, issue)
	if err != nil {
		t.Fatalf("publish case: %v", err)
	}
	return reload(t, cases, cs.ID)
}

func testProject(t *testing.T, pool *pgxpool.Pool) Project {
	t.Helper()

	projects, err := ListProjects(context.Background(), pool)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	for _, p := range projects {
		if p.Slug == "tg-intake" {
			return p
		}
	}
	t.Fatal("в базе нет проекта tg-intake: миграция seed не применена")
	return Project{}
}

func cancelJSON(caseID string, userID int64) []byte {
	raw, _ := json.Marshal(cancelPayload{CaseID: caseID, UserID: userID})
	return raw
}

// requestLog копит «метод путь» запросов: тесты отмены проверяют не ответ
// GitHub, а то, какие запросы ушли и в каком виде.
type requestLog struct{ seen []string }

func (r *requestLog) add(method, path string) { r.seen = append(r.seen, method+" "+path) }
func (r *requestLog) list() []string          { return r.seen }
func (r *requestLog) has(want string) bool {
	for _, got := range r.seen {
		if got == want {
			return true
		}
	}
	return false
}

func githubStub(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	return recordingStub(t, &requestLog{}, routes)
}

// recordingStub отвечает по карте «метод путь» и записывает всё, что пришло.
// Неизвестный путь - пустой JSON-массив: отмене хватает, а список без своих
// номеров просто останется без статусов.
func recordingStub(t *testing.T, seen *requestLog, routes map[string]string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, _, _ := strings.Cut(r.URL.RequestURI(), "?")
		seen.add(r.Method, path)

		w.Header().Set("Content-Type", "application/json")
		if body, ok := routes[fmt.Sprintf("%s %s", r.Method, path)]; ok {
			fmt.Fprint(w, body)
			return
		}
		fmt.Fprint(w, "[]")
	}))
	t.Cleanup(server.Close)
	return server
}

// TestListByProject: список показывает тикеты своего проекта и только
// опубликованные. Чужой тикет в списке означал бы утечку между проектами, а
// черновик - тикет, которого в GitHub нет.
func TestListByProject(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	other := addProject(t, pool, "zz-other")

	publishCase(t, cases, 7006, 60)
	publishCaseIn(t, cases, 7007, 61, other.Slug)
	// Черновик без issue_number: собран, но до тикета не дошёл.
	if _, _, err := cases.StartCase(context.Background(), User{ID: 7008, First: "Тест"}, "tg-intake", modeTicket); err != nil {
		t.Fatalf("start draft: %v", err)
	}

	tickets := newTestTickets(t, cases, githubStub(t, nil).URL)
	list, _, err := tickets.List(context.Background(), testProject(t, pool), 7006, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Number != 60 {
		t.Fatalf("список чужого проекта или с черновиком: %+v", list)
	}
}

// TestCancelResumesAfterPartialFailure: работа упала между простановкой своей
// метки и закрытием issue. Повтор обязан довести отмену до конца, а не решить
// по собственной метке, что тикет уже закрыт.
func TestCancelResumesAfterPartialFailure(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7009, 70)

	seen := &requestLog{}
	labelled := false
	failDelete := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, _, _ := strings.Cut(r.URL.RequestURI(), "?")
		seen.add(r.Method, path)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet:
			// После успешного POST на issue висят обе метки: своя и прежняя.
			if labelled {
				fmt.Fprint(w, `{"number": 70, "labels": [
					{"name": "status:cancelled"}, {"name": "status:in-progress"}]}`)
				return
			}
			fmt.Fprint(w, `{"number": 70, "labels": [{"name": "status:in-progress"}]}`)
		case r.Method == http.MethodPost:
			labelled = true
			fmt.Fprint(w, "[]")
		case r.Method == http.MethodDelete && failDelete:
			// 4xx, а не 5xx: клиент не тратит на него три повтора с отсрочкой, и
			// тест не растягивается на семь секунд ради того же исхода.
			w.WriteHeader(http.StatusBadRequest)
		default:
			fmt.Fprint(w, "[]")
		}
	}))
	t.Cleanup(server.Close)

	tickets := newTestTickets(t, cases, server.URL)
	job := Job{ID: 1, Kind: JobCancelIssue, Payload: cancelJSON(cs.ID, 7009)}

	if err := tickets.RunCancel(context.Background(), job); err == nil {
		t.Fatal("первый заход обязан вернуть ошибку: снятие метки не прошло")
	}

	failDelete = false
	if err := tickets.RunCancel(context.Background(), job); err != nil {
		t.Fatalf("повтор: %v", err)
	}
	if !seen.has("PATCH /repos/galera-club/tg-intake/issues/70") {
		t.Errorf("повтор не закрыл issue; запросы: %v", seen.list())
	}
	if !seen.has("DELETE /repos/galera-club/tg-intake/issues/70/labels/status:in-progress") {
		t.Errorf("повтор не снял прежнюю метку; запросы: %v", seen.list())
	}
}

// TestCancelRecordedOnce: отмена записывается один раз, и держит это база.
// Проверка перед вставкой пропускала второе событие, когда две работы отмены
// шли одновременно: каждая видела пустой журнал до чужого коммита.
func TestCancelRecordedOnce(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7120, 71)

	record := func() error {
		_, err := pool.Exec(ctx, `
			INSERT INTO case_events (case_id, kind, payload)
			VALUES ($1, 'cancelled_by_author', '{}'::jsonb)`, cs.ID)
		return err
	}
	if err := record(); err != nil {
		t.Fatalf("первая запись отмены: %v", err)
	}
	if err := record(); err == nil {
		t.Fatal("вторая запись отмены прошла: тикет закрылся бы дважды")
	}

	var n int
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM case_events WHERE case_id = $1 AND kind = 'cancelled_by_author'`,
		cs.ID).Scan(&n)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 1 {
		t.Errorf("событий отмены: %d, ожидалось 1", n)
	}
}

// addProject заводит проект и убирает его за собой: testPool чистит обращения,
// но не проекты, и мусор утёк бы в соседние тесты.
func addProject(t *testing.T, pool *pgxpool.Pool, slug string) ProjectConfig {
	t.Helper()
	ctx := context.Background()

	p := ProjectConfig{Slug: slug, Title: "Другой " + slug, Owner: "acme", Repo: slug, Context: "тест"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := SyncProjects(ctx, pool, []ProjectConfig{p}, log); err != nil {
		t.Fatalf("sync projects: %v", err)
	}
	t.Cleanup(func() {
		// Тикеты теста ссылаются на проект внешним ключом: без снятия ссылки
		// удаление проекта отказывает (cases_project_id_fkey), и следующий тест
		// видит его в общем списке.
		if _, err := pool.Exec(ctx,
			`DELETE FROM cases WHERE project_id = (SELECT id FROM projects WHERE slug = $1)`, slug); err != nil {
			t.Logf("cleanup cases of project %s: %v", slug, err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM projects WHERE slug = $1`, slug); err != nil {
			t.Logf("cleanup project %s: %v", slug, err)
		}
	})
	return p
}
