package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestWatchTellsAboutStatus: смена метки на будящую доходит до автора одним
// сообщением с переходом в карточку и оставляет отметку в списке.
func TestWatchTellsAboutStatus(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7101, 70)
	setTold(t, pool, cs.ID, labelNew)

	watch := newTestWatch(t, pool, githubStub(t, map[string]string{
		"GET /repos/galera-club/tg-intake/issues": `[{"number": 70, "labels": [{"name": "status:in-progress"}]}]`,
	}).URL)
	if err := watch.Run(ctx); err != nil {
		t.Fatalf("watch: %v", err)
	}

	if n := countJobs(t, pool, JobNotify, cs.ID); n != 1 {
		t.Fatalf("уведомлений: %d, ожидалось 1", n)
	}
	var buttons, told string
	var news bool
	err := pool.QueryRow(ctx, `
		SELECT (SELECT COALESCE(payload->>'buttons', '') FROM jobs
		        WHERE kind = $1 AND payload->>'case_id' = $2::text),
		       told_status, has_news
		FROM cases WHERE id = $2::uuid`, JobNotify, cs.ID).Scan(&buttons, &told, &news)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if buttons != keysTicket {
		t.Errorf("набор кнопок новости: %q, ожидался %q", buttons, keysTicket)
	}
	if told != "status:in-progress" || !news {
		t.Errorf("состояние доставки: told=%q news=%v", told, news)
	}

	// Второй обход без изменений: автору сказать нечего.
	if err := watch.Run(ctx); err != nil {
		t.Fatalf("watch again: %v", err)
	}
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 1 {
		t.Errorf("повторный обход задублировал уведомление: %d", n)
	}
}

// TestWatchSilentOnFirstSight: тикет, заведённый до появления слежения,
// наблюдается молча - иначе выкат разослал бы новость по каждому старому тикету.
func TestWatchSilentOnFirstSight(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7102, 71)

	watch := newTestWatch(t, pool, githubStub(t, map[string]string{
		"GET /repos/galera-club/tg-intake/issues": `[{"number": 71, "labels": [{"name": "status:prod"}]}]`,
	}).URL)
	if err := watch.Run(ctx); err != nil {
		t.Fatalf("watch: %v", err)
	}

	if n := countJobs(t, pool, JobNotify, cs.ID); n != 0 {
		t.Errorf("первое наблюдение разбудило автора: работ %d", n)
	}
	if told, news := state(t, pool, cs.ID); told != "status:prod" || news {
		t.Errorf("состояние после первого наблюдения: told=%q news=%v", told, news)
	}
}

// TestWatchQuietStatus: промежуточный статус автора не будит, но виден в списке
// при следующем заходе - отметки новости у него нет.
func TestWatchQuietStatus(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7103, 72)
	setTold(t, pool, cs.ID, labelNew)

	watch := newTestWatch(t, pool, githubStub(t, map[string]string{
		"GET /repos/galera-club/tg-intake/issues": `[{"number": 72, "labels": [{"name": "status:dev"}]}]`,
	}).URL)
	if err := watch.Run(ctx); err != nil {
		t.Fatalf("watch: %v", err)
	}

	if n := countJobs(t, pool, JobNotify, cs.ID); n != 0 {
		t.Errorf("незначимый статус разбудил автора: работ %d", n)
	}
	if told, news := state(t, pool, cs.ID); told != "status:dev" || news {
		t.Errorf("состояние после незначимой смены: told=%q news=%v", told, news)
	}
}

// TestWatchTellsAboutComment: комментарий разработчика доходит до автора, а
// повторный обход того же комментария не дублирует уведомление.
func TestWatchTellsAboutComment(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7104, 73)
	setTold(t, pool, cs.ID, labelNew)
	movedBack := time.Now().Add(-time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE projects SET comments_since = $1 WHERE slug = 'tg-intake'`,
		movedBack); err != nil {
		t.Fatalf("set border: %v", err)
	}

	// Комментарий заведён раньше границы окна, а правился после неё: since у
	// GitHub считает по времени правки, и граница обязана двигаться по нему же.
	created := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	updated := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	watch := newTestWatch(t, pool, githubStub(t, map[string]string{
		"GET /repos/galera-club/tg-intake/issues/comments": fmt.Sprintf(`[
			{"id": 501, "body": "Смотрю", "created_at": %q, "updated_at": %q,
			 "issue_url": "https://api.github.com/repos/galera-club/tg-intake/issues/73"},
			{"id": 502, "body": "Чужой тикет", "created_at": %q, "updated_at": %q,
			 "issue_url": "https://api.github.com/repos/galera-club/tg-intake/issues/999"}]`,
			created, updated, created, updated),
	}).URL)
	for range 2 {
		if err := watch.Run(ctx); err != nil {
			t.Fatalf("watch: %v", err)
		}
	}

	if n := countJobs(t, pool, JobNotify, cs.ID); n != 1 {
		t.Fatalf("уведомлений о комментарии: %d, ожидалось 1", n)
	}
	var key string
	var mark int64
	err := pool.QueryRow(ctx, `
		SELECT (SELECT key FROM jobs WHERE kind = $1 AND payload->>'case_id' = $2::text),
		       told_comment_id
		FROM cases WHERE id = $2::uuid`, JobNotify, cs.ID).Scan(&key, &mark)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if want := fmt.Sprintf("notify:%s:comment:501", cs.ID); key != want {
		t.Errorf("ключ работы: %q, ожидался %q", key, want)
	}
	if mark != 501 {
		t.Errorf("отметка доставленного комментария: %d", mark)
	}

	// Граница окна двинулась: следующий обход не перечитывает доставленное.
	var border time.Time
	if err := pool.QueryRow(ctx, `SELECT comments_since FROM projects WHERE slug = 'tg-intake'`).
		Scan(&border); err != nil {
		t.Fatalf("read border: %v", err)
	}
	if !border.After(movedBack) {
		t.Errorf("граница окна не двинулась: %s", border)
	}
}

// TestMarkSeenOnlyAuthor: отметку новости гасит только автор тикета. Просмотр
// общий, и чужой заход не должен прятать новость от того, кто её ждёт.
func TestMarkSeenOnlyAuthor(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7105, 74)
	if _, err := pool.Exec(ctx, `UPDATE cases SET has_news = true WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("set news: %v", err)
	}
	tickets := newTestTickets(t, cases, githubStub(t, nil).URL)

	// Карточка обязана видеть отметку: без неё открытие тикета её не погасит.
	card, err := tickets.Load(ctx, testProject(t, pool), 74)
	if err != nil {
		t.Fatalf("load card: %v", err)
	}
	if !card.News {
		t.Error("карточка не видит отметку новости")
	}

	if err := tickets.MarkSeen(ctx, cs.ID, 9999); err != nil {
		t.Fatalf("mark seen by stranger: %v", err)
	}
	if _, news := state(t, pool, cs.ID); !news {
		t.Error("чужой заход погасил отметку новости")
	}
	if err := tickets.MarkSeen(ctx, cs.ID, 7105); err != nil {
		t.Fatalf("mark seen: %v", err)
	}
	if _, news := state(t, pool, cs.ID); news {
		t.Error("автор открыл карточку, а отметка осталась")
	}
}

// TestListNewsIsAuthors: отметка новости авторская. Чужая ничего не говорит тому,
// кто открыл список, а гасит её только автор.
func TestListNewsIsAuthors(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	stranger := publishCase(t, cases, 7106, 90)
	mine := publishCase(t, cases, 7107, 80)
	for _, cs := range []*Case{stranger, mine} {
		if _, err := pool.Exec(ctx, `UPDATE cases SET has_news = true WHERE id = $1`, cs.ID); err != nil {
			t.Fatalf("set news: %v", err)
		}
	}

	tickets := newTestTickets(t, cases, githubStub(t, nil).URL)
	list, total, err := tickets.List(ctx, testProject(t, pool), 7107, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || total != 2 {
		t.Fatalf("тикетов: %d, всего %d, ожидалось 2 и 2", len(list), total)
	}
	// Порядок по номеру: 90 свежее 80.
	if list[0].Number != 90 || list[0].News {
		t.Errorf("чужой тикет: %+v", list[0])
	}
	if list[1].Number != 80 || !list[1].News {
		t.Errorf("свой тикет без отметки: %+v", list[1])
	}
}

// TestListPaging: вторая страница отдаёт следующую десятку, а не повторяет
// первую. Без листания старый тикет автору недостижим - аккаунта GitHub у него
// нет.
func TestListPaging(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	for n := range ticketsLimit + 3 {
		publishCase(t, cases, int64(7200+n), 200+n)
	}

	tickets := newTestTickets(t, cases, githubStub(t, nil).URL)
	first, total, err := tickets.List(ctx, testProject(t, pool), 7200, 0)
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if total != ticketsLimit+3 || len(first) != ticketsLimit {
		t.Fatalf("страница 1: %d из %d", len(first), total)
	}
	second, _, err := tickets.List(ctx, testProject(t, pool), 7200, 1)
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(second) != 3 {
		t.Fatalf("страница 2: %d, ожидалось 3", len(second))
	}
	if second[0].Number != first[len(first)-1].Number-1 {
		t.Errorf("страницы пересекаются или рвутся: %d после %d",
			second[0].Number, first[len(first)-1].Number)
	}

	// Страница за концом списка: пусто, но без ошибки - экран вернёт на первую.
	empty, _, err := tickets.List(ctx, testProject(t, pool), 7200, 9)
	if err != nil || len(empty) != 0 {
		t.Errorf("страница за концом: %d тикетов, ошибка %v", len(empty), err)
	}

	// Кнопка «К списку» считает страницу по номеру тикета: возврат обязан попасть
	// туда, откуда автор ушёл в карточку.
	page, err := tickets.Page(ctx, testProject(t, pool), second[0].Number)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if page != 1 {
		t.Errorf("страница тикета #%d: %d, ожидалась 1", second[0].Number, page)
	}
	if page, err = tickets.Page(ctx, testProject(t, pool), first[0].Number); err != nil || page != 0 {
		t.Errorf("страница свежего тикета: %d, ошибка %v", page, err)
	}
}

func newTestWatch(t *testing.T, pool *pgxpool.Pool, api string) *Watch {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWatch(pool, NewGitHub("token", api, testStatuses, log), testStatuses, log)
}

func setTold(t *testing.T, pool *pgxpool.Pool, caseID, label string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE cases SET told_status = $2 WHERE id = $1`, caseID, label); err != nil {
		t.Fatalf("set told status: %v", err)
	}
}

// state - что слежение уже сказало автору по этому тикету.
func state(t *testing.T, pool *pgxpool.Pool, caseID string) (string, bool) {
	t.Helper()
	var told *string
	var news bool
	err := pool.QueryRow(context.Background(),
		`SELECT told_status, has_news FROM cases WHERE id = $1`, caseID).Scan(&told, &news)
	if err != nil {
		t.Fatalf("read case state: %v", err)
	}
	if told == nil {
		return "", news
	}
	return *told, news
}
