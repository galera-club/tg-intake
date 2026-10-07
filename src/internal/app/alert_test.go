package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"testing"
)

const testAlertChat = -1001234567890

// TestAlertMessages: шапка уведомления несёт проект, заголовок, автора и ссылку,
// а строка про недобранный контракт появляется только у неполного тикета.
// Метка incomplete приходит параметром (Unclear), а не из cs.Incomplete - поле
// в этом тесте нарочно расходится с параметром, чтобы падать, если алерт
// снова начнёт читать cs.Incomplete (github.go: метка issue и алерт обязаны
// считать одной и той же функцией).
func TestAlertMessages(t *testing.T) {
	project := Project{Slug: "crm-bot"}
	author := User{First: "Иван", Last: "Петров", Username: "ivan"}
	cs := &Case{Title: "Не грузится карточка", Incomplete: false}
	url := "https://github.com/o/r/issues/42"

	got := alertPublished(project, cs, author, 42, url, true, true)
	for _, want := range []string{"Новый тикет: crm-bot", "Не грузится карточка",
		"Иван Петров (@ivan)", "#42 " + url, "incomplete"} {
		if !strings.Contains(got, want) {
			t.Errorf("в уведомлении нет %q:\n%s", want, got)
		}
	}

	cs.Incomplete = true
	if strings.Contains(alertPublished(project, cs, author, 42, url, false, true), "incomplete") {
		t.Error("полный тикет (по параметру) помечен недобранным контрактом")
	}

	cancelled := alertCancelled(project, cs, author, 42, url)
	if !strings.HasPrefix(cancelled, "Тикет отменён автором: crm-bot") {
		t.Errorf("шапка отмены: %q", cancelled)
	}
}

// TestLostNotifyAlertsOwner: сообщение автору, исчерпавшее повторы, обязано
// всплыть у владельца. Молча погашенная работа уносила с собой вопрос раунда
// или номер заведённого тикета, и следа не оставалось нигде.
func TestLostNotifyAlertsOwner(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cases.alertChat = testAlertChat

	cs, _, err := cases.StartCase(ctx, User{ID: 7110, First: "Тест"}, "tg-intake", modeTicket)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	if err := putNotifyKey(ctx, pool, cs.ID, "round-1", "Уточню: что было на экране?", keysRound); err != nil {
		t.Fatalf("put notify: %v", err)
	}

	lost := Job{
		ID:       jobID(t, pool, JobNotify+":"+cs.ID+":round-1"),
		Kind:     JobNotify,
		Payload:  []byte(`{"case_id":"` + cs.ID + `","text":"Уточню: что было на экране?"}`),
		Attempts: maxAttempts + 1,
	}
	cause := errors.New("telegram unreachable")
	cases.HandleFailedJob(ctx, lost, cause)

	var text string
	err = pool.QueryRow(ctx, `
		SELECT payload->>'text' FROM jobs
		WHERE kind = $1 AND payload->>'case_id' = $2 AND (payload->>'chat_id')::bigint = $3`,
		JobNotify, cs.ID, int64(testAlertChat)).Scan(&text)
	if err != nil {
		t.Fatalf("владелец не узнал о потере: %v", err)
	}
	if !strings.Contains(text, "что было на экране") {
		t.Errorf("текст потери не дошёл до владельца: %q", text)
	}

	// Потерянный алерт второго алерта не порождает: недоступный Telegram иначе
	// кормил бы очередь собственными провалами.
	alert := Job{
		ID:       jobID(t, pool, JobNotify+":"+cs.ID+":lost:"+strconv.FormatInt(lost.ID, 10)),
		Kind:     JobNotify,
		Payload:  []byte(`{"case_id":"` + cs.ID + `","text":"потеря","chat_id":` + strconv.Itoa(testAlertChat) + `}`),
		Attempts: maxAttempts + 1,
	}
	cases.HandleFailedJob(ctx, alert, cause)
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 2 {
		t.Errorf("сообщений в очереди: %d, ожидалось 2 - провал алерта породил новый", n)
	}
}

// TestPublishAlertsOwner: публикация кладёт в очередь два сообщения - автору и в
// чат владельца. Второе адресовано явным chat_id и живёт под своим ключом.
func TestPublishAlertsOwner(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs, publisher := publishThrough(t, cases, 7100, testAlertChat)

	job := Job{ID: 1, Kind: JobPublish, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := publisher.Run(ctx, job); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if n := countJobs(t, pool, JobNotify, cs.ID); n != 2 {
		t.Fatalf("сообщений в очереди: %d, ожидалось 2 (автору и владельцу)", n)
	}
	var key, text string
	err := pool.QueryRow(ctx, `
		SELECT key, payload->>'text' FROM jobs
		WHERE kind = $1 AND payload->>'case_id' = $2 AND (payload->>'chat_id')::bigint = $3`,
		JobNotify, cs.ID, int64(testAlertChat)).Scan(&key, &text)
	if err != nil {
		t.Fatalf("уведомление владельцу не поставлено: %v", err)
	}
	if key != JobNotify+":"+cs.ID+":alert" {
		t.Errorf("ключ уведомления: %q", key)
	}
	if !strings.Contains(text, "#77") {
		t.Errorf("в уведомлении нет номера тикета: %q", text)
	}

	// Повтор работы второго сообщения владельцу не даёт: ключ занят.
	if err := publisher.Run(ctx, job); err != nil {
		t.Fatalf("publish twice: %v", err)
	}
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 2 {
		t.Errorf("после повтора сообщений в очереди: %d, ожидалось 2", n)
	}
}

// TestPublishWithoutAlert: пустой ALERT_CHAT_ID оставляет прежний сервис -
// автору сообщение уходит, лишних работ в очереди нет.
func TestPublishWithoutAlert(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs, publisher := publishThrough(t, cases, 7102, 0)

	job := Job{ID: 1, Kind: JobPublish, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := publisher.Run(ctx, job); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 1 {
		t.Errorf("сообщений в очереди: %d, ожидалось 1 (только автору)", n)
	}
}

// publishThrough готовит обращение к публикации и издателя с заглушкой GitHub.
func publishThrough(t *testing.T, cases *Cases, userID, alertChat int64) (*Case, *Publisher) {
	t.Helper()
	ctx := context.Background()

	cs, _, err := cases.StartCase(ctx, User{ID: userID, First: "Тест"}, "tg-intake", modeTicket)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	_, err = cases.pool.Exec(ctx, `
		UPDATE cases SET status = 'publishing', kind = 'bug', title = 'Форма не сохраняется',
		                 summary = '## Случай' WHERE id = $1`, cs.ID)
	if err != nil {
		t.Fatalf("mark publishing: %v", err)
	}

	server := githubStub(t, map[string]string{
		"POST /repos/galera-club/tg-intake/issues": `{"number": 77,
			"html_url": "https://github.com/galera-club/tg-intake/issues/77"}`,
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return reload(t, cases, cs.ID),
		NewPublisher(cases, NewGitHub("token", server.URL, nil, log), testRules(t), log, alertChat, "")
}

// TestCancelAlertsOwner: отмена тикета автором доходит до владельца тем же
// способом - иначе в ленте остаётся тикет, которого в GitHub уже нет.
func TestCancelAlertsOwner(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7101, 60)

	server := githubStub(t, map[string]string{
		"GET /repos/galera-club/tg-intake/issues/60": `{"number": 60,
			"html_url": "https://github.com/galera-club/tg-intake/issues/60", "labels": []}`,
	})
	tickets := newTestTickets(t, cases, server.URL)
	tickets.alertChat = testAlertChat

	job := Job{ID: 1, Kind: JobCancelIssue, Payload: cancelJSON(cs.ID, 7101)}
	if err := tickets.RunCancel(ctx, job); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	var text string
	err := pool.QueryRow(ctx, `
		SELECT payload->>'text' FROM jobs
		WHERE kind = $1 AND payload->>'case_id' = $2 AND (payload->>'chat_id')::bigint = $3`,
		JobNotify, cs.ID, int64(testAlertChat)).Scan(&text)
	if err != nil {
		t.Fatalf("уведомление владельцу не поставлено: %v", err)
	}
	if !strings.HasPrefix(text, "Тикет отменён автором") {
		t.Errorf("текст уведомления: %q", text)
	}

	// Повтор работы отмены упирается в записанное событие и второго сообщения
	// владельцу не ставит.
	if err := tickets.RunCancel(ctx, job); err != nil {
		t.Fatalf("cancel twice: %v", err)
	}
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 2 {
		t.Errorf("после повтора сообщений в очереди: %d, ожидалось 2", n)
	}
}

// publishToBoard - издатель с доской, записью запросов к GitHub и логом в logs.
func publishToBoard(t *testing.T, cases *Cases, userID int64, board string, routes map[string]string, logs *bytes.Buffer) (*Case, *Publisher, *requestLog) {
	t.Helper()
	cs, _ := publishThrough(t, cases, userID, testAlertChat)
	seen := &requestLog{}
	server := recordingStub(t, seen, routes)
	log := slog.New(slog.NewTextHandler(logs, nil))
	return cs, NewPublisher(cases, NewGitHub("token", server.URL, nil, log), testRules(t), log, testAlertChat, board), seen
}

func alertText(t *testing.T, cases *Cases, caseID string) string {
	t.Helper()
	var text string
	err := cases.pool.QueryRow(context.Background(), `
		SELECT payload->>'text' FROM jobs
		WHERE kind = $1 AND payload->>'case_id' = $2 AND (payload->>'chat_id')::bigint = $3`,
		JobNotify, caseID, int64(testAlertChat)).Scan(&text)
	if err != nil {
		t.Fatalf("уведомление владельцу не поставлено: %v", err)
	}
	return text
}

var createdIssue = map[string]string{
	"POST /repos/galera-club/tg-intake/issues": `{"number": 77,
		"html_url": "https://github.com/galera-club/tg-intake/issues/77"}`,
	"GET /repos/galera-club/tg-intake/issues/77": `{"number": 77, "node_id": "I_77"}`,
}

// TestPublishBoardFailureKeepsTicket: доска отказала (нет права, неверный id) -
// тикет всё равно опубликован, работа без ошибки, владелец видит строку о доске.
func TestPublishBoardFailureKeepsTicket(t *testing.T) {
	ctx := context.Background()
	cases := newTestCases(t, testPool(t), t.TempDir())
	routes := maps.Clone(createdIssue)
	routes["POST /graphql"] = `{"data": {"addProjectV2ItemById": null},
		"errors": [{"type": "FORBIDDEN", "message": "Resource not accessible by personal access token"}]}`
	var logs bytes.Buffer
	cs, publisher, _ := publishToBoard(t, cases, 7110, "PVT_board", routes, &logs)

	job := Job{ID: 1, Kind: JobPublish, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := publisher.Run(ctx, job); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := reload(t, cases, cs.ID); got.Status != "published" || got.IssueNumber != 77 {
		t.Errorf("обращение: статус %s, issue %d; ожидалось published, 77", got.Status, got.IssueNumber)
	}
	if text := alertText(t, cases, cs.ID); !strings.Contains(text, "На доску Galera не добавлен") {
		t.Errorf("в уведомлении нет строки о доске: %q", text)
	}
	if !strings.Contains(logs.String(), "board_add_failed") {
		t.Errorf("сбой доски не в логе: %s", logs.String())
	}
}

// TestPublishRetryAddsSameIssue: ответ на создание потерян, повтор находит тикет
// по маркеру - второго тикета нет, на доску идёт тот же, владельцу одно сообщение.
func TestPublishRetryAddsSameIssue(t *testing.T) {
	ctx := context.Background()
	cases := newTestCases(t, testPool(t), t.TempDir())
	cs, _ := publishThrough(t, cases, 7111, testAlertChat)
	routes := map[string]string{
		"GET /repos/galera-club/tg-intake/issues": `[{"number": 77,
			"html_url": "https://github.com/galera-club/tg-intake/issues/77",
			"body": "` + caseMarker(cs.ID) + `"}]`,
		"GET /repos/galera-club/tg-intake/issues/77": `{"number": 77, "node_id": "I_77"}`,
		"POST /graphql": `{"data": {"addProjectV2ItemById": {"item": {"id": "PVTI_1"}}}}`,
	}
	seen := &requestLog{}
	server := recordingStub(t, seen, routes)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	publisher := NewPublisher(cases, NewGitHub("token", server.URL, nil, log), testRules(t), log, testAlertChat, "PVT_board")

	job := Job{ID: 1, Kind: JobPublish, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := publisher.Run(ctx, job); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if seen.has("POST /repos/galera-club/tg-intake/issues") {
		t.Error("повтор создал второй тикет")
	}
	if !seen.has("POST /graphql") {
		t.Error("найденный тикет не ушёл на доску")
	}
	if text := alertText(t, cases, cs.ID); strings.Contains(text, "доску") {
		t.Errorf("доска приняла тикет, а уведомление о сбое: %q", text)
	}
	if n := countJobs(t, cases.pool, JobNotify, cs.ID); n != 2 {
		t.Errorf("сообщений в очереди: %d, ожидалось 2 (автору и владельцу)", n)
	}
}

// TestPublishWithoutBoard: пустой GITHUB_BOARD_ID - к GraphQL ни одного
// запроса, публикация как до доски.
func TestPublishWithoutBoard(t *testing.T) {
	ctx := context.Background()
	cases := newTestCases(t, testPool(t), t.TempDir())
	cs, publisher, seen := publishToBoard(t, cases, 7112, "", createdIssue, &bytes.Buffer{})

	job := Job{ID: 1, Kind: JobPublish, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := publisher.Run(ctx, job); err != nil {
		t.Fatalf("publish: %v", err)
	}
	for _, request := range seen.list() {
		if strings.Contains(request, "/graphql") || strings.HasSuffix(request, "/issues/77") {
			t.Errorf("без доски ушёл запрос %s", request)
		}
	}
	if text := alertText(t, cases, cs.ID); strings.Contains(text, "доску") {
		t.Errorf("без доски в уведомлении строка о ней: %q", text)
	}
}
