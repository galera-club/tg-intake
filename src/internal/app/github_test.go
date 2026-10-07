package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func testLog(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestIssueBodyStartsWithBrief: краткое содержание обязано доехать до тела
// тикета первым разделом - его читает тот, кто возьмёт задачу, и по нему же
// автор узнаёт свой тикет в боте. Молчание модели тикет не останавливает, но
// тогда раздела нет вовсе, а не пустая рубрика.
func TestIssueBodyStartsWithBrief(t *testing.T) {
	publisher := NewPublisher(nil, nil, testRules(t), testLog(t), 0, "")
	cs := &Case{Kind: "bug", Brief: "Заявка не сохраняется у менеджеров с утра.",
		Summary: "## Случай\n\nФорма гасит кнопку"}

	body := publisher.body(cs, User{First: "Иван"}, nil, "<!-- marker -->")
	brief := strings.Index(body, "## Кратко")
	if brief < 0 || !strings.Contains(body, cs.Brief) {
		t.Fatalf("краткого содержания нет в теле тикета:\n%s", body)
	}
	if section := strings.Index(body, "## Случай"); section < brief {
		t.Errorf("кратко идёт не первым разделом:\n%s", body)
	}

	cs.Brief = ""
	if strings.Contains(publisher.body(cs, User{First: "Иван"}, nil, "<!-- marker -->"), "## Кратко") {
		t.Error("пустое краткое содержание оставило в тикете пустую рубрику")
	}
}

// TestTreeDocsFiltersMarkdown: отбор Lookup работает только с md, каталоги и
// прочие расширения в дереве репозитория ему не нужны.
func TestTreeDocsFiltersMarkdown(t *testing.T) {
	server := githubStub(t, map[string]string{
		"GET /repos/acme/proj/git/trees/main": `{
			"tree": [
				{"path": "docs/architecture.md", "type": "blob", "size": 120},
				{"path": "docs/img/diagram.png", "type": "blob", "size": 900},
				{"path": "docs/plans", "type": "tree", "size": 0},
				{"path": "README.MD", "type": "blob", "size": 40}
			],
			"truncated": false
		}`,
	})
	gh := NewGitHub("token", server.URL, testStatuses, testLog(t))

	docs, err := gh.TreeDocs(context.Background(), Project{Owner: "acme", Repo: "proj"}, "main")
	if err != nil {
		t.Fatalf("tree docs: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("файлов: %d, ожидалось 2: %+v", len(docs), docs)
	}
	sizes := map[string]int{}
	for _, d := range docs {
		sizes[d.Path] = d.Size
	}
	if sizes["docs/architecture.md"] != 120 || sizes["README.MD"] != 40 {
		t.Errorf("состав или размеры md-файлов не совпали: %+v", docs)
	}
}

// TestTreeDocsLogsTruncated: усечённое дерево не должно молчать - отбор
// увидел бы неполный список md-файлов, не зная об этом.
func TestTreeDocsLogsTruncated(t *testing.T) {
	server := githubStub(t, map[string]string{
		"GET /repos/acme/proj/git/trees/main": `{"tree": [], "truncated": true}`,
	})
	var buf bytes.Buffer
	gh := NewGitHub("token", server.URL, testStatuses, slog.New(slog.NewTextHandler(&buf, nil)))

	if _, err := gh.TreeDocs(context.Background(), Project{Owner: "acme", Repo: "proj"}, "main"); err != nil {
		t.Fatalf("tree docs: %v", err)
	}
	if !strings.Contains(buf.String(), "tree_truncated") {
		t.Errorf("предупреждение об усечённом дереве не залогировано: %s", buf.String())
	}
}

// TestFileRejectsUnsafePathWithoutRequest: путь проверяется до похода в сеть -
// модель называет его сама, и отказ не должен стоить лишнего запроса.
func TestFileRejectsUnsafePathWithoutRequest(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"пустой путь", ""},
		{"абсолютный путь", "/docs/architecture.md"},
		{"выход за пределы репозитория", "docs/../../etc/passwd"},
		{"непечатаемый символ", "docs/arch\x00itecture.md"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seen := &requestLog{}
			server := recordingStub(t, seen, nil)
			gh := NewGitHub("token", server.URL, testStatuses, testLog(t))

			_, err := gh.File(context.Background(), Project{Owner: "acme", Repo: "proj"}, c.path, "main")
			if err == nil {
				t.Fatal("небезопасный путь принят")
			}
			if len(seen.list()) != 0 {
				t.Errorf("запрос ушёл в сеть до проверки пути: %v", seen.list())
			}
		})
	}
}

// TestFileDecodesBase64WithNewlines: GitHub переносит base64 внутри JSON
// строками фиксированной длины, декодер обязан их снимать, как GetReadme.
func TestFileDecodesBase64WithNewlines(t *testing.T) {
	content := "# Заголовок\n\nПроверка декодирования содержимого."
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	var wrapped strings.Builder
	for i := 0; i < len(encoded); i += 20 {
		end := i + 20
		if end > len(encoded) {
			end = len(encoded)
		}
		wrapped.WriteString(encoded[i:end])
		wrapped.WriteString("\n")
	}

	server := githubStub(t, map[string]string{
		"GET /repos/acme/proj/contents/docs/architecture.md": fmt.Sprintf(
			`{"content": %q, "encoding": "base64"}`, wrapped.String()),
	})
	gh := NewGitHub("token", server.URL, testStatuses, testLog(t))

	got, err := gh.File(context.Background(), Project{Owner: "acme", Repo: "proj"}, "docs/architecture.md", "main")
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if got != content {
		t.Errorf("содержимое: %q, ожидалось %q", got, content)
	}
}

// TestFileTooLargeReturnsError: файлы больше 1 МБ Contents API отдаёт без
// содержимого - пустая строка читалась бы дальше как «файл пустой», а не как
// отказ.
func TestFileTooLargeReturnsError(t *testing.T) {
	server := githubStub(t, map[string]string{
		"GET /repos/acme/proj/contents/docs/big.md": `{"content": "", "encoding": "none"}`,
	})
	gh := NewGitHub("token", server.URL, testStatuses, testLog(t))

	got, err := gh.File(context.Background(), Project{Owner: "acme", Repo: "proj"}, "docs/big.md", "main")
	if err == nil {
		t.Fatal("файл больше 1 МБ должен вернуть ошибку, а не пустую строку")
	}
	if got != "" {
		t.Errorf("содержимое не пустое при ошибке: %q", got)
	}
}

// TestCheckReadNamesMissingPermission: отказ в праве читать содержимое должен
// называть само право - именно на нём молча встал режим «Спросить», и по
// сообщению владелец понимает, что выдать токену.
func TestCheckReadNamesMissingPermission(t *testing.T) {
	asked := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.Path
		w.Header().Set("X-Accepted-GitHub-Permissions", "contents=read")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message": "Resource not accessible by personal access token"}`)
	}))
	t.Cleanup(server.Close)
	gh := NewGitHub("token", server.URL, testStatuses, testLog(t))

	err := gh.CheckRead(context.Background(), Project{Owner: "acme", Repo: "proj"})
	if err == nil {
		t.Fatal("отказ в правах должен вернуть ошибку")
	}
	if asked != "GET /repos/acme/proj/contents/" {
		t.Errorf("проверка ушла не туда: %s", asked)
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "contents=read") {
		t.Errorf("в ошибке нет статуса или нужного права: %v", err)
	}
}

// TestLookupFailedTextSeparatesDenial: отказ в правах не лечится повтором, и
// звать автора спросить ещё раз значит гонять его по кругу навсегда.
func TestLookupFailedTextSeparatesDenial(t *testing.T) {
	denied := lookupFailedText(&githubError{status: 403, message: "Resource not accessible"})
	if !strings.Contains(denied, "владельцу") {
		t.Errorf("отказ в правах не отправляет к владельцу: %s", denied)
	}
	if strings.Contains(denied, "Спросите ещё раз") {
		t.Errorf("отказ в правах предлагает бесполезный повтор: %s", denied)
	}

	temporary := lookupFailedText(errors.New("read body: context deadline exceeded"))
	if !strings.Contains(temporary, "Спросите ещё раз") {
		t.Errorf("временный сбой должен звать спросить ещё раз: %s", temporary)
	}
}

// TestIssueBodyUnclear: незакрытое ядро уходит в тикет одной строкой «Не
// уточнено» перед маркером, а не списком «Не разобрано»; при закрытом ядре
// строки нет вовсе (R4).
func TestIssueBodyUnclear(t *testing.T) {
	publisher := NewPublisher(nil, nil, testRules(t), testLog(t), 0, "")
	const marker = "<!-- marker -->"

	tests := []struct {
		name   string
		kind   string
		filled map[string]string
		want   string
	}{
		{"баг без случая", "bug", map[string]string{"wrong": "дубль"},
			"\n\n---\nНе уточнено: конкретный случай.\n\n" + marker},
		{"смесь без случая и нужного", "mixed", map[string]string{"wrong": "дубль", "why": "руками долго"},
			"\n\n---\nНе уточнено: конкретный случай, что нужно.\n\n" + marker},
		{"ядро закрыто", "bug", map[string]string{"case": "сделка 1", "wrong": "дубль"},
			"## Случай\n\nтекст\n\n" + marker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := &Case{Kind: tt.kind, Filled: tt.filled, Summary: "## Случай\n\nтекст"}
			body := publisher.body(cs, User{First: "Иван"}, nil, marker)
			if !strings.HasSuffix(body, tt.want) {
				t.Errorf("хвост тела:\n%q\nожидался:\n%q", body, tt.want)
			}
			if strings.Contains(body, "Не разобрано") {
				t.Errorf("в теле старый список пробелов:\n%s", body)
			}
		})
	}
}

// TestPublishMixedLabels: смесь уходит одним тикетом с метками обоих типов
// (Р-2), а меток несуществующего type:mixed GitHub не получает. Проверка - по
// телу запроса, который принял GitHub. cases.incomplete здесь ложен нарочно:
// метку ставит тот же счёт по ядру, что и строку «Не уточнено».
func TestPublishMixedLabels(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	cs, _, err := cases.StartCase(ctx, User{ID: 7201, First: "Тест"}, "tg-intake", modeTicket)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	_, err = pool.Exec(ctx, `
		UPDATE cases SET status = 'publishing', kind = 'mixed', title = 'Напоминание',
		                 summary = '## Склонение', incomplete = false,
		                 contract = '{"wrong": "имя в неверном падеже", "need": "за час", "why": "за день поздно"}',
		                 gaps = '["case"]'
		WHERE id = $1`, cs.ID)
	if err != nil {
		t.Fatalf("mark publishing: %v", err)
	}

	var issue struct {
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/repos/galera-club/tg-intake/issues" {
			if err := json.NewDecoder(r.Body).Decode(&issue); err != nil {
				t.Errorf("decode issue: %v", err)
			}
			fmt.Fprint(w, `{"number": 78, "html_url": "https://github.com/galera-club/tg-intake/issues/78"}`)
			return
		}
		fmt.Fprint(w, "[]")
	}))
	t.Cleanup(server.Close)

	publisher := NewPublisher(cases, NewGitHub("token", server.URL, nil, testLog(t)), testRules(t), testLog(t), 0, "")
	job := Job{ID: 1, Kind: JobPublish, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := publisher.Run(ctx, job); err != nil {
		t.Fatalf("publish: %v", err)
	}

	for _, want := range []string{"type:bug", "type:feature", "incomplete"} {
		if !slices.Contains(issue.Labels, want) {
			t.Errorf("метки тикета %v без %q", issue.Labels, want)
		}
	}
	if slices.Contains(issue.Labels, "type:mixed") {
		t.Errorf("метка type:mixed ушла в GitHub: %v", issue.Labels)
	}
	if !strings.Contains(issue.Body, "Не уточнено: конкретный случай.") {
		t.Errorf("строки пробела нет в теле:\n%s", issue.Body)
	}
}

// TestPublishFindsIssueOnFirstAttempt: «Публикую» после исчерпанных повторов
// ставит новую работу с первой попыткой, а issue прошлой уже мог создаться -
// маркер ищется и тогда, второго тикета нет.
func TestPublishFindsIssueOnFirstAttempt(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	cs, _, err := cases.StartCase(ctx, User{ID: 7202, First: "Тест"}, "tg-intake", modeTicket)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	_, err = pool.Exec(ctx, `
		UPDATE cases SET status = 'publishing', kind = 'bug', title = 'Статус не сменился',
		                 summary = '## Сделка', contract = '{"case": "заказ 4821", "wrong": "статус"}'
		WHERE id = $1`, cs.ID)
	if err != nil {
		t.Fatalf("mark publishing: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/repos/galera-club/tg-intake/issues" {
			t.Error("создан второй issue")
		}
		if r.Method == http.MethodGet && r.URL.Path == "/repos/galera-club/tg-intake/issues" {
			found := []Issue{{Number: 77, HTMLURL: "https://github.com/galera-club/tg-intake/issues/77",
				Body: "тело\n" + caseMarker(cs.ID)}}
			if err := json.NewEncoder(w).Encode(found); err != nil {
				t.Errorf("encode issues: %v", err)
			}
			return
		}
		fmt.Fprint(w, "[]")
	}))
	t.Cleanup(server.Close)

	publisher := NewPublisher(cases, NewGitHub("token", server.URL, nil, testLog(t)), testRules(t), testLog(t), 0, "")
	job := Job{ID: 1, Kind: JobPublish, Attempts: 1, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := publisher.Run(ctx, job); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := reload(t, cases, cs.ID); got.IssueNumber != 77 {
		t.Errorf("обращение не привязано к найденному issue 77: %v", got.IssueNumber)
	}
}

// boardStub отвечает на чтение issue 77 и на GraphQL, тело мутации кладёт в
// mutation: проверяется, что на доску ушёл именно этот тикет.
func boardStub(t *testing.T, issue, graphql string, mutation *[]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/galera-club/tg-intake/issues/77":
			fmt.Fprint(w, issue)
		case "POST /graphql":
			*mutation, _ = io.ReadAll(r.Body)
			fmt.Fprint(w, graphql)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestAddToBoard: мутация получает доску из конфига и node_id тикета, ответ -
// id карточки.
func TestAddToBoard(t *testing.T) {
	var mutation []byte
	server := boardStub(t, `{"number": 77, "node_id": "I_77"}`,
		`{"data": {"addProjectV2ItemById": {"item": {"id": "PVTI_1"}}}}`, &mutation)
	gh := NewGitHub("token", server.URL, testStatuses, testLog(t))

	item, err := gh.AddToBoard(context.Background(), Project{Owner: "galera-club", Repo: "tg-intake"}, 77, "PVT_board")
	if err != nil {
		t.Fatalf("add to board: %v", err)
	}
	if item != "PVTI_1" {
		t.Errorf("карточка %q, ожидалась PVTI_1", item)
	}
	var sent struct {
		Query     string            `json:"query"`
		Variables map[string]string `json:"variables"`
	}
	if err := json.Unmarshal(mutation, &sent); err != nil {
		t.Fatalf("тело мутации не JSON: %v: %s", err, mutation)
	}
	if !strings.Contains(sent.Query, "projectId: $board, contentId: $issue") ||
		sent.Variables["board"] != "PVT_board" || sent.Variables["issue"] != "I_77" {
		t.Errorf("мутация: %s", mutation)
	}
}

// TestAddToBoardRejected: GitHub отвечает 200, но карточки нет - это ошибка, а
// не успех. Без node_id мутация не уходит вовсе.
func TestAddToBoardRejected(t *testing.T) {
	tests := []struct {
		name, issue, graphql string
		want                 string
	}{
		{"нет права", `{"number": 77, "node_id": "I_77"}`,
			`{"data": {"addProjectV2ItemById": null}, "errors": [{"type": "FORBIDDEN",
			"message": "Resource not accessible by personal access token"}]}`, "Resource not accessible"},
		{"пустой ответ", `{"number": 77, "node_id": "I_77"}`, `{"data": null}`, "no item"},
		{"без карточки", `{"number": 77, "node_id": "I_77"}`,
			`{"data": {"addProjectV2ItemById": {"item": {"id": ""}}}}`, "no item"},
		{"без node_id", `{"number": 77}`, `{}`, "node_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mutation []byte
			server := boardStub(t, tt.issue, tt.graphql, &mutation)
			gh := NewGitHub("token", server.URL, testStatuses, testLog(t))

			_, err := gh.AddToBoard(context.Background(), Project{Owner: "galera-club", Repo: "tg-intake"}, 77, "PVT_board")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ошибка %v, ожидалась с %q", err, tt.want)
			}
			if tt.name == "без node_id" && mutation != nil {
				t.Errorf("мутация ушла без node_id: %s", mutation)
			}
		})
	}
}
