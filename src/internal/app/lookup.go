package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	stepPick   = "lookup_pick"
	stepAnswer = "lookup_answer"

	// Потолки похода в документацию. Три файла - предел отбора: ответ, размазанный
	// по большему числу документов, лучше не собирать вовсе. Остальные страхуют от
	// файла и дерева, которые не влезут в контекст модели.
	maxDocs      = 3
	maxDocChars  = 80000
	maxAllChars  = 200000
	maxTreePaths = 300
)

// Карта комплекта документов - часть стабильного префикса обоих ходов: она
// объясняет модели, что лежит в документе и кому он адресован, а список файлов
// репозитория приходит волатильной частью.
var (
	docMap       = mustPrompt("docmap.md")
	pickPrefix   = strings.ReplaceAll(mustPrompt("lookup-pick.md"), "{{DOCMAP}}", docMap)
	answerPrefix = strings.ReplaceAll(mustPrompt("lookup-answer.md"), "{{DOCMAP}}", docMap)
)

// Схемы ответа. Поля перечислены в required целиком: strict-режим OpenRouter
// требует этого, а Go всё равно перепроверяет вывод модели как недоверенный.
var (
	pickSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"files": {"type": "array", "items": {"type": "string"}},
			"reason": {"type": "string"}
		},
		"required": ["files", "reason"],
		"additionalProperties": false
	}`)

	answerSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"answer": {"type": "string"},
			"sources": {"type": "array", "items": {"type": "string"}},
			"found": {"type": "boolean"},
			"wants_ticket": {"type": "boolean"}
		},
		"required": ["answer", "sources", "found", "wants_ticket"],
		"additionalProperties": false
	}`)
)

type lookupPick struct {
	Files  []string `json:"files"`
	Reason string   `json:"reason"`
}

type lookupAnswer struct {
	Answer      string   `json:"answer"`
	Sources     []string `json:"sources"`
	Found       bool     `json:"found"`
	WantsTicket bool     `json:"wants_ticket"`
}

// docText - скачанный файл документации: путь нужен и ссылке на источник, и
// проверке того, что модель ссылается на прочитанное.
type docText struct {
	Path string
	Text string
}

// Lookup - шаг ответа по документации проекта: два хода модели, отбор файлов и
// ответ по ним. Побочные эффекты выполняет Go: модель возвращает структуру, Go
// валидирует её и только потом пишет в БД и отвечает автору.
type Lookup struct {
	cases *Cases
	gh    *GitHub
	llm   Completer
	log   *slog.Logger
	model DialogModel
}

func NewLookup(cases *Cases, gh *GitHub, llm Completer, log *slog.Logger, model DialogModel) *Lookup {
	return &Lookup{cases: cases, gh: gh, llm: llm, log: log, model: model}
}

// Run - работа очереди. Провал называется своим именем: в общем job_failed не
// видно, на каком шаге разговор потерял ответ.
func (l *Lookup) Run(ctx context.Context, job Job) error {
	var p casePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("payload of %s: %w", job.Kind, err)
	}
	if err := l.answer(ctx, job.ID, p.CaseID); err != nil {
		l.log.Warn("lookup_failed", "case_id", p.CaseID, "error", err)
		return err
	}
	return nil
}

func (l *Lookup) answer(ctx context.Context, jobID int64, caseID string) error {
	cs, err := l.cases.Load(ctx, caseID)
	if err != nil {
		return err
	}
	// Обращение отменили, ответ уже показан или разговор ушёл в тикет: работа
	// устарела.
	if cs == nil || cs.Mode != modeAsk || cs.Status != statusAnswering {
		return nil
	}
	if cs.ProjectID == nil {
		return fmt.Errorf("case %s has no project", cs.ID)
	}

	project, err := LoadProject(ctx, l.cases.pool, *cs.ProjectID)
	if err != nil {
		return err
	}
	history, err := l.cases.askHistory(ctx, cs.ID)
	if err != nil {
		return err
	}

	start := time.Now()
	l.log.Info("lookup_start", "case_id", cs.ID, "project", project.Slug)

	repo, err := l.gh.GetRepo(ctx, project.Owner, project.Repo)
	if err != nil {
		return err
	}
	docs, err := l.gh.TreeDocs(ctx, project, repo.DefaultBranch)
	if err != nil {
		return err
	}

	files, dropped, err := l.pick(ctx, project, docs, history)
	if err != nil {
		return err
	}
	loaded, chars := l.load(ctx, cs, project, repo.DefaultBranch, files)
	l.log.Info("lookup_pick", "case_id", cs.ID, "files", len(loaded),
		"dropped", dropped, "chars", chars)

	out, err := l.askAnswer(ctx, cs, project, loaded, history)
	if err != nil {
		return err
	}

	body := answerBody(out)
	text := withLinks(body, sourceLinks(project, repo.DefaultBranch, out.Sources))
	alert, err := l.alertText(ctx, cs, project, out)
	if err != nil {
		return err
	}

	saved, err := l.save(ctx, cs, jobID, out, body, text, alert)
	if err != nil {
		return err
	}
	if !saved {
		// Разговор ушёл из-под хода, пока модель думала: автор перевёл его в тикет
		// кнопкой или закончил разговор. Ответ на прошлое состояние не показываем.
		l.log.Info("lookup_dropped", "case_id", cs.ID)
		return nil
	}

	l.log.Info("lookup_done", "case_id", cs.ID, "found", out.Found,
		"wants_ticket", out.WantsTicket, "sources", len(out.Sources),
		"ms", time.Since(start).Milliseconds())
	return nil
}

// pick - ход отбора: модель называет файлы по карте документов и дереву
// репозитория. Второе значение - сколько путей отброшено как несуществующие.
func (l *Lookup) pick(ctx context.Context, project Project, docs []DocFile, history []Message) ([]string, int, error) {
	if len(docs) == 0 {
		return nil, 0, nil
	}

	var out lookupPick
	messages := lookupMessages(pickPrefix, project.Context, treeMessage(docs), history)
	if err := complete(ctx, l.llm, l.model, stepPick, pickSchema, messages, &out); err != nil {
		return nil, 0, err
	}

	files, dropped := keepInTree(docs, out.Files)
	return files, dropped, nil
}

// load скачивает отобранные файлы, второе значение - сколько символов прочитано.
// Нечитаемый файл не отменяет ответ: молчание хуже ответа по двум файлам из трёх.
func (l *Lookup) load(ctx context.Context, cs *Case, project Project, ref string, files []string) ([]docText, int) {
	loaded := make([]docText, 0, len(files))
	chars := 0
	for _, path := range files {
		left := maxAllChars - chars
		if left <= 0 {
			break
		}
		text, err := l.gh.File(ctx, project, path, ref)
		if err != nil {
			l.log.Warn("lookup_file_failed", "case_id", cs.ID, "path", path, "error", err)
			continue
		}
		text = cutDoc(text, min(maxDocChars, left))
		chars += utf8.RuneCountInString(text)
		loaded = append(loaded, docText{Path: path, Text: text})
	}
	return loaded, chars
}

// askAnswer - ход ответа по скачанным файлам с проверкой вывода модели.
func (l *Lookup) askAnswer(ctx context.Context, cs *Case, project Project, loaded []docText, history []Message) (lookupAnswer, error) {
	var out lookupAnswer
	messages := lookupMessages(answerPrefix, project.Context,
		docsMessage(loaded, modelSelectedFilesHeading), history)
	if err := complete(ctx, l.llm, l.model, stepAnswer, answerSchema, messages, &out); err != nil {
		return lookupAnswer{}, err
	}

	checked := checkAnswer(out, loaded)
	if out.Found && !checked.Found {
		// Ответ по файлу, которого модель не читала, - выдумка: сотрудник примет
		// его за факт, а проверить будет нечем.
		l.log.Warn("lookup_answer_unsourced", "case_id", cs.ID, "sources", len(out.Sources))
	}
	return checked, nil
}

// complete зовёт диалоговую модель по схеме и разбирает ответ. Повтора на
// невалидный ответ нет: схема strict, а смысл проверяет вызывающий - непригодный
// ответ становится честным «не нашёл», а не второй генерацией.
func complete(ctx context.Context, llm Completer, model DialogModel, step string,
	schema json.RawMessage, messages []Message, out any) error {
	raw, err := llm.Complete(ctx, Request{
		Step:       step,
		Model:      model.Name,
		Reasoning:  model.Reasoning,
		MaxTokens:  llmMaxTokens,
		Messages:   messages,
		SchemaName: step,
		Schema:     schema,
	})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", step, err)
	}
	return nil
}

// save кладёт исход хода: одной транзакцией, иначе ответ уходит автору, а
// разговор остаётся ждать его вечно. Второе значение - лёг ли результат в базу.
func (l *Lookup) save(ctx context.Context, cs *Case, jobID int64, out lookupAnswer, body, text, alert string) (bool, error) {
	saved := false
	err := l.cases.inTx(ctx, func(tx pgx.Tx) error {
		// Замок хода: строка держится до конца транзакции, а разговор мог уйти в
		// тикет кнопкой или закрыться, пока модель думала.
		var status string
		err := tx.QueryRow(ctx, `
			SELECT status FROM cases WHERE id = $1 AND mode = 'ask' FOR UPDATE`, cs.ID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock case %s: %w", cs.ID, err)
		}
		if status != statusAnswering {
			return nil
		}
		saved = true

		// Автор просит правку: ответ ему уже не нужен, разговор продолжит интервью
		// с той же историей.
		if out.WantsTicket {
			return switchToTicket(ctx, tx, cs.ID)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE cases SET status = 'collecting', updated_at = now()
			WHERE id = $1`, cs.ID); err != nil {
			return fmt.Errorf("save answer of case %s: %w", cs.ID, err)
		}

		// Текст ответа ложится снимком в событие: это реплика бота, и следующий
		// ход разговора обязан видеть её целиком.
		if err := addEvent(ctx, tx, cs.ID, "answer_ready", map[string]any{
			"found": out.Found, "sources": out.Sources, "text": body,
		}); err != nil {
			return err
		}
		if err := putNotifyKey(ctx, tx, cs.ID, strconv.FormatInt(jobID, 10), text, keysAnswer); err != nil {
			return err
		}
		if alert == "" {
			return nil
		}
		return putAlert(ctx, tx, cs.ID, "ask:"+strconv.FormatInt(jobID, 10), alert, l.cases.alertChat)
	})
	return saved, err
}

// alertText - строка владельцу о заданном вопросе: кто, проект, найден ли ответ
// и из каких файлов. Пустой чат уведомлений - пустая строка.
func (l *Lookup) alertText(ctx context.Context, cs *Case, project Project, out lookupAnswer) (string, error) {
	if l.cases.alertChat == 0 || out.WantsTicket {
		return "", nil
	}
	author, err := LoadUser(ctx, l.cases.pool, cs.UserID)
	if err != nil {
		return "", err
	}

	found := alertLookupNoAnswer
	if out.Found {
		found = alertLookupAnswerFoundPrefix + strings.Join(out.Sources, ", ")
	}
	return alertLookupQuestion(project.Slug, authorName(author), found), nil
}

// keepInTree оставляет только пути, которые есть в дереве репозитория. Второе
// значение - сколько отброшено: вывод модели недоверенный, и придуманный путь
// не должен ни уходить в GitHub, ни становиться ссылкой в ответе.
func keepInTree(docs []DocFile, files []string) ([]string, int) {
	known := make(map[string]bool, len(docs))
	for _, d := range docs {
		known[d.Path] = true
	}

	var kept []string
	dropped := 0
	for _, path := range files {
		path = strings.TrimSpace(path)
		if !known[path] {
			dropped++
			continue
		}
		// Названный дважды файл берём один раз: второй экземпляр стоил бы запроса
		// к GitHub и места в контексте.
		delete(known, path)
		if len(kept) < maxDocs {
			kept = append(kept, path)
		}
	}
	return kept, dropped
}

// checkAnswer - проверки недоверенного вывода модели. Схема гарантирует форму, а
// смысл проверяет Go: ответ без источника и источник, которого модель не читала,
// прошли бы схему насквозь, а сотрудник принял бы такой ответ за факт.
func checkAnswer(out lookupAnswer, loaded []docText) lookupAnswer {
	read := make(map[string]bool, len(loaded))
	for _, d := range loaded {
		read[d.Path] = true
	}

	var sources []string
	for _, path := range out.Sources {
		if read[path] {
			sources = append(sources, path)
		}
	}

	checked := lookupAnswer{
		Answer:      scrubContacts(strings.TrimSpace(out.Answer)),
		Sources:     sources,
		WantsTicket: out.WantsTicket,
	}
	checked.Found = out.Found && len(sources) > 0 && checked.Answer != ""
	if !checked.Found {
		checked.Sources = nil
	}
	return checked
}

// sourceLinks собирает адреса источников. Ссылку строит Go по ветке из ответа
// GitHub: модель адресов не пишет - тот же довод, что и у раздела «Ссылки» в
// теле issue.
func sourceLinks(project Project, ref string, sources []string) []string {
	links := make([]string, 0, len(sources))
	for _, path := range sources {
		links = append(links, fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s",
			project.Owner, project.Repo, ref, path))
	}
	return links
}

// answerBody - ответ автору без ссылок. Он же ложится снимком в журнал: адреса
// собирает Go при отправке, а промт ответа запрещает модели писать их самой -
// история разговора не должна учить её обратному.
func answerBody(out lookupAnswer) string {
	if !out.Found {
		return msgLookupNoAnswer
	}
	// Разметку снимаем здесь же, где текст модели превращается в реплику бота:
	// Telegram markdown не рендерит, а parse_mode на чужом тексте роняет отправку.
	return plainText(out.Answer)
}

// cutDoc режет документ по остатку бюджета и помечает обрез: без пометки модель
// ответит по оборванной середине, не зная, что конца файла не видела.
func cutDoc(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return cutRunes(text, limit) + modelDocCutMark
}

// askHistory восстанавливает разговор режима вопроса из журнала: реплики автора
// и показанные ему ответы. Протокол сырья целиком сюда не идёт - прошлые вопросы
// пришли бы в контекст модели дважды.
func (c *Cases) askHistory(ctx context.Context, caseID string) ([]Message, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT kind, COALESCE(payload->>'text', '') FROM case_events
		WHERE case_id = $1 AND kind IN ('question_asked', 'answer_ready')
		ORDER BY id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("query ask history of case %s: %w", caseID, err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var kind, text string
		if err := rows.Scan(&kind, &text); err != nil {
			return nil, fmt.Errorf("scan ask history event: %w", err)
		}
		if text == "" {
			continue
		}
		role := "user"
		if kind == "answer_ready" {
			role = "assistant"
		}
		messages = append(messages, Message{Role: role, Parts: []Part{TextPart(text)}})
	}
	return messages, rows.Err()
}
