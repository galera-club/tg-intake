package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLookupDropsUnknownPath: путь называет модель, а не человек. Файл, которого
// в дереве репозитория нет, стоил бы лишнего запроса к GitHub и стал бы ссылкой
// на несуществующий документ.
func TestLookupDropsUnknownPath(t *testing.T) {
	docs := []DocFile{{Path: "docs/prd.md"}, {Path: "README.md"}}

	kept, dropped := keepInTree(docs, []string{"docs/prd.md", "docs/выдумка.md", "README.md"})

	if len(kept) != 2 || kept[0] != "docs/prd.md" || kept[1] != "README.md" {
		t.Errorf("отобранные файлы: %v", kept)
	}
	if dropped != 1 {
		t.Errorf("отброшено путей вне дерева: %d, ожидался 1", dropped)
	}
}

// TestLookupFoundNeedsSources: ссылка на источник - единственный способ автора
// проверить ответ. Найденным считается только ответ, опёртый на прочитанный
// файл: иначе выдумку модели примут за факт из документации.
func TestLookupFoundNeedsSources(t *testing.T) {
	loaded := []docText{{Path: "docs/prd.md", Text: "опрос раз в сутки"}}

	tests := []struct {
		name string
		out  lookupAnswer
		want bool
	}{
		{
			name: "источник прочитан",
			out:  lookupAnswer{Answer: "Раз в сутки.", Sources: []string{"docs/prd.md"}, Found: true},
			want: true,
		},
		{
			name: "источников нет",
			out:  lookupAnswer{Answer: "Раз в сутки.", Found: true},
		},
		{
			name: "источник не читали",
			out:  lookupAnswer{Answer: "Раз в сутки.", Sources: []string{"docs/state.md"}, Found: true},
		},
		{
			name: "ответ пуст",
			out:  lookupAnswer{Sources: []string{"docs/prd.md"}, Found: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checked := checkAnswer(tc.out, loaded)
			if checked.Found != tc.want {
				t.Errorf("найден: %v, ожидалось %v", checked.Found, tc.want)
			}
			if !checked.Found && len(checked.Sources) != 0 {
				t.Errorf("у ненайденного ответа остались источники: %v", checked.Sources)
			}
		})
	}
}

// TestLookupAnswersWithLink: ссылку на источник строит Go по ветке из ответа
// GitHub - модель адресов не пишет. Ответ уходит автору с кнопками разговора и
// строкой владельцу, а сам разговор возвращается за следующей репликой.
func TestLookupAnswersWithLink(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cases.alertChat = testAlertChat
	cs := startAsk(t, cases, 5101, "через сколько срабатывает опрос")

	llm := lookupLLM(t, nil,
		`{"files":["docs/prd.md"],"reason":"продуктовое описание"}`,
		`{"answer":"Опрос уходит раз в сутки.","sources":["docs/prd.md"],"found":true,"wants_ticket":false}`)
	lookup := newTestLookup(t, cases, llm)

	if err := lookup.Run(ctx, lookupJob(cs.ID)); err != nil {
		t.Fatalf("run lookup: %v", err)
	}

	if got := reload(t, cases, cs.ID).Status; got != statusCollecting {
		t.Errorf("статус после ответа: %s, ожидался %s", got, statusCollecting)
	}
	sent := notifiesOf(t, pool, cs.ID)
	if len(sent) != 2 {
		t.Fatalf("сообщений из очереди: %d, ожидались ответ автору и строка владельцу", len(sent))
	}
	want := "https://github.com/galera-club/tg-intake/blob/prod/docs/prd.md"
	if !strings.Contains(sent[0].Text, want) {
		t.Errorf("ссылка на источник: %q, ожидалась %s", sent[0].Text, want)
	}
	if sent[0].Buttons != keysAnswer {
		t.Errorf("кнопки под ответом: %q, ожидалось %q", sent[0].Buttons, keysAnswer)
	}
	if sent[1].ChatID != testAlertChat || !strings.Contains(sent[1].Text, "docs/prd.md") {
		t.Errorf("строка владельцу: %+v", sent[1])
	}
	history, err := cases.askHistory(ctx, cs.ID)
	if err != nil {
		t.Fatalf("ask history: %v", err)
	}
	if len(history) != 2 || history[1].Role != "assistant" {
		t.Errorf("ответ не лёг в историю разговора: %+v", history)
	}
}

// TestLookupDenialOffersTicket: отказ по правам зовёт автора создать тикет, и
// кнопка обязана прийти с ним же. Панель сбора её не даёт, а разговор в
// документацию больше не вернётся - без кнопки текст указывает в тупик.
func TestLookupDenialOffersTicket(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startAsk(t, cases, 5107, "как часто уходит опрос")

	job := lookupJob(cs.ID)
	job.Attempts = maxAttempts + 1
	cases.HandleFailedJob(ctx, job, &githubError{status: 403, message: "Resource not accessible"})

	if got := reload(t, cases, cs.ID).Status; got != statusCollecting {
		t.Errorf("статус после провала: %s, ожидался %s", got, statusCollecting)
	}
	sent := notifiesOf(t, pool, cs.ID)
	if len(sent) != 1 {
		t.Fatalf("сообщений автору: %d, ожидалось одно", len(sent))
	}
	if sent[0].Buttons != keysAnswer {
		t.Errorf("кнопки под отказом: %q, ожидалось %q", sent[0].Buttons, keysAnswer)
	}
	if !strings.Contains(sent[0].Text, "владельцу") {
		t.Errorf("текст отказа не отправляет к владельцу: %q", sent[0].Text)
	}
}

// TestLookupSwitchesToTicket: реплика «давай поменяем» - уже не вопрос. Разговор
// переходит в тикет с той же историей, и «как сейчас» не переспрашивается.
func TestLookupSwitchesToTicket(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startAsk(t, cases, 5102, "давай поменяем срок опроса на 12 часов")

	llm := lookupLLM(t, nil,
		`{"files":["docs/prd.md"],"reason":"продуктовое описание"}`,
		`{"answer":"","sources":[],"found":false,"wants_ticket":true}`)
	lookup := newTestLookup(t, cases, llm)

	job := lookupJob(cs.ID)
	if err := lookup.Run(ctx, job); err != nil {
		t.Fatalf("run lookup: %v", err)
	}

	done := reload(t, cases, cs.ID)
	if done.Mode != modeTicket || done.Status != statusInterview {
		t.Errorf("режим %s, статус %s: ожидались %s и %s",
			done.Mode, done.Status, modeTicket, statusInterview)
	}
	if jobID(t, pool, JobInterview+":"+cs.ID) == 0 {
		t.Error("ход интервью не поставлен")
	}
	// Ключ ответа автору строится по идентификатору работы, ключ перевода - свой:
	// так одно сообщение отличается от другого надёжнее, чем счётчиком.
	if jobID(t, pool, fmt.Sprintf("%s:%s:%d", JobNotify, cs.ID, job.ID)) != 0 {
		t.Error("автору ушёл ответ из документации вместо перехода в тикет")
	}
	if jobID(t, pool, JobNotify+":"+cs.ID+":to-ticket") == 0 {
		t.Error("автор не узнал о переводе в тикет")
	}
}

// TestLookupDropsLateAnswer: пока модель ищет ответ, автор жмёт «Закончить
// разговор». Ответ на закрытый разговор не имеет права лечь поверх и уйти
// автору - обращение уже отпустило слот.
func TestLookupDropsLateAnswer(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startAsk(t, cases, 5103, "через сколько срабатывает опрос")

	end := func() {
		if err := cases.EndAsk(ctx, cs); err != nil {
			t.Fatalf("end ask: %v", err)
		}
	}
	llm := lookupLLM(t, end,
		`{"files":["docs/prd.md"],"reason":"продуктовое описание"}`,
		`{"answer":"Опрос уходит раз в сутки.","sources":["docs/prd.md"],"found":true,"wants_ticket":false}`)
	lookup := newTestLookup(t, cases, llm)

	if err := lookup.Run(ctx, lookupJob(cs.ID)); err != nil {
		t.Fatalf("run lookup: %v", err)
	}

	if got := reload(t, cases, cs.ID).Status; got != statusAnswered {
		t.Errorf("статус закрытого разговора: %s, ожидался %s", got, statusAnswered)
	}
	if countJobs(t, pool, JobNotify, cs.ID) != 0 {
		t.Error("ответ ушёл автору после конца разговора")
	}
}

// startAsk готовит разговор режима вопроса, дошедший до похода в документацию:
// тесту нужен сам ход, а не прогон сбора и нормализации.
func startAsk(t *testing.T, cases *Cases, userID int64, question string) *Case {
	t.Helper()
	ctx := context.Background()

	cs, _, err := cases.StartCase(ctx, User{ID: userID, First: "Тест"}, "tg-intake", modeAsk)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	_, err = cases.pool.Exec(ctx, `
		UPDATE cases SET status = 'answering', protocol = $2 WHERE id = $1`,
		cs.ID, "текст: "+question)
	if err != nil {
		t.Fatalf("move to answering: %v", err)
	}
	if err := addEvent(ctx, cases.pool, cs.ID, "question_asked", map[string]any{"text": question}); err != nil {
		t.Fatalf("add question: %v", err)
	}
	return reload(t, cases, cs.ID)
}

func newTestLookup(t *testing.T, cases *Cases, llm *OpenRouter) *Lookup {
	t.Helper()

	server := githubStub(t, map[string]string{
		"GET /repos/daniil4545/tg-intake": `{"full_name": "daniil4545/tg-intake", "default_branch": "prod"}`,
		"GET /repos/daniil4545/tg-intake/git/trees/prod": `{"tree": [
			{"path": "docs/prd.md", "type": "blob", "size": 4200},
			{"path": "docs/architecture.md", "type": "blob", "size": 9100}
		], "truncated": false}`,
		"GET /repos/daniil4545/tg-intake/contents/docs/prd.md": fmt.Sprintf(
			`{"content": %q, "encoding": "base64"}`,
			base64.StdEncoding.EncodeToString([]byte("# Продукт\n\nОпрос уходит раз в сутки."))),
	})
	gh := NewGitHub("token", server.URL, testStatuses, testLog(t))
	return NewLookup(cases, gh, llm, testLog(t), DialogModel{Name: "test-model"})
}

func lookupJob(caseID string) Job {
	return Job{ID: 1, Kind: JobLookup, Payload: []byte(`{"case_id":"` + caseID + `"}`)}
}

// lookupLLM отдаёт ответы ходов по порядку. before зовётся перед ответом модели:
// им тест вклинивает реплику автора, пока шаг «думает».
func lookupLLM(t *testing.T, before func(), contents ...string) *OpenRouter {
	t.Helper()

	call := 0
	llm := NewOpenRouter("test-key", "test-model", "", testLog(t))
	llm.http = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		if before != nil {
			before()
		}
		content := contents[min(call, len(contents)-1)]
		call++

		body, err := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": content}}},
		})
		if err != nil {
			t.Fatalf("encode llm response: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})}
	return llm
}

// notifiesOf - сообщения из очереди по обращению в порядке постановки: сначала
// ответ автору, следом строка владельцу.
func notifiesOf(t *testing.T, pool *pgxpool.Pool, caseID string) []notifyPayload {
	t.Helper()

	rows, err := pool.Query(context.Background(), `
		SELECT payload FROM jobs WHERE kind = $1 AND payload->>'case_id' = $2 ORDER BY id`,
		JobNotify, caseID)
	if err != nil {
		t.Fatalf("notify jobs of case %s: %v", caseID, err)
	}
	defer rows.Close()

	var sent []notifyPayload
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan notify job: %v", err)
		}
		var p notifyPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode notify payload: %v", err)
		}
		sent = append(sent, p)
	}
	return sent
}
