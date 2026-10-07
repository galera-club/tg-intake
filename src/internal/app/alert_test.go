package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

	got := alertPublished(project, cs, author, 42, url, true)
	for _, want := range []string{"Новый тикет: crm-bot", "Не грузится карточка",
		"Иван Петров (@ivan)", "#42 " + url, "incomplete"} {
		if !strings.Contains(got, want) {
			t.Errorf("в уведомлении нет %q:\n%s", want, got)
		}
	}

	cs.Incomplete = true
	if strings.Contains(alertPublished(project, cs, author, 42, url, false), "incomplete") {
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
		NewPublisher(cases, NewGitHub("token", server.URL, nil, log), testRules(t), log, alertChat)
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
