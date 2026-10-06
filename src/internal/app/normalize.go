package app

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Промты - блок «Изменяемое»: правятся без чтения Go. В бинарь уезжают
// встроенными, потому что distroless-образ не несёт каталога с ресурсами.
//
//go:embed prompts/*.md
var promptFiles embed.FS

var (
	transcribePrompt = mustPrompt("transcribe.md")
	screenshotPrompt = mustPrompt("screenshot.md")
)

const (
	stepTranscribe = "transcribe"
	stepScreenshot = "screenshot"
)

// Схемы ответа. Поля перечислены в required целиком: strict-режим OpenRouter
// требует этого, а Go всё равно перепроверяет вывод модели как недоверенный.
var (
	transcriptSchema = json.RawMessage(`{
		"type": "object",
		"properties": {"text": {"type": "string"}},
		"required": ["text"],
		"additionalProperties": false
	}`)

	screenshotSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"screen": {"type": "string"},
			"facts": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"label": {"type": "string"},
						"value": {"type": "string"}
					},
					"required": ["label", "value"],
					"additionalProperties": false
				}
			},
			"relevant": {"type": "string"},
			"unreadable": {"type": "array", "items": {"type": "string"}}
		},
		"required": ["screen", "facts", "relevant", "unreadable"],
		"additionalProperties": false
	}`)
)

// Normalizer - шаги нормализации сырья: транскрипт голосового и разбор
// скриншота с протоколом в контексте.
//
// Побочные эффекты выполняет Go: модель возвращает структуру, Go валидирует её
// и только потом пишет в БД.
type Normalizer struct {
	cases *Cases
	llm   Completer
	log   *slog.Logger
}

func NewNormalizer(cases *Cases, llm Completer, log *slog.Logger) *Normalizer {
	return &Normalizer{cases: cases, llm: llm, log: log}
}

// RunNormalizeVoice расшифровывает одно голосовое. Работа ставится по элементу,
// поэтому провал одной записи не трогает остальные.
func (n *Normalizer) RunNormalizeVoice(ctx context.Context, job Job) error {
	var p itemPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("payload of %s: %w", job.Kind, err)
	}

	items, err := n.cases.Items(ctx, p.CaseID)
	if err != nil {
		return err
	}
	item := findItem(items, p.ItemID)
	// Элемент уже разобран или погашен: повтор работы не переделывает сделанное,
	// но цепочку двинуть обязан - иначе обращение зависнет в normalizing.
	if item == nil || item.Status != "pending" {
		return n.cases.AdvanceNormalize(ctx, p.CaseID)
	}

	audio, format, err := n.readAudio(*item)
	if err != nil {
		// Файла нет или он не читается: повтор этого не исправит.
		n.log.Warn("item_rejected", "case_id", p.CaseID, "item_id", item.ID,
			"reason", "unreadable_file", "error", err)
		return n.failAndMoveOn(ctx, p.CaseID, item.ID, modelErrFileUnread)
	}

	raw, err := n.llm.Complete(ctx, Request{
		Step:   stepTranscribe,
		CaseID: p.CaseID,
		Messages: []Message{
			{Role: "system", Parts: []Part{TextPart(transcribePrompt)}},
			{Role: "user", Parts: []Part{AudioPart(audio, format)}},
		},
		SchemaName: "transcript",
		Schema:     transcriptSchema,
	})
	if err != nil {
		return err
	}

	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decode transcript: %w", err)
	}

	// Обезличивание перестаёт держаться на одном промте: расшифровка идёт и в
	// протокол, который сразу показывают автору, и ответом в интервью. Промт
	// остаётся первой линией, регулярка - второй.
	text := scrubContacts(strings.TrimSpace(out.Text))
	if text == "" {
		// О нераспознанной речи надо сказать прямо: пустота, ушедшая дальше,
		// читается моделью как «человек ничего не сказал».
		n.log.Info("item_rejected", "case_id", p.CaseID, "item_id", item.ID, "reason", "no_speech")
		return n.failAndMoveOn(ctx, p.CaseID, item.ID, modelErrSpeechUnrecognized)
	}

	if err := n.cases.SaveNormalized(ctx, item.ID, text); err != nil {
		return err
	}
	// Следующий шаг ставит сам воркер: цепочка живёт в БД, а не в памяти.
	// AfterVoice смотрит на состояние разговора - та же расшифровка идёт либо
	// дальше по нормализации, либо ответом на вопрос интервью.
	return n.cases.AfterVoice(ctx, p.CaseID, text)
}

// RunNormalizeImage разбирает один скриншот. Работа ставится по элементу, как и
// расшифровка: бюджет работы принадлежит одному вызову модели, и деградация
// провайдера на первом экране не оставляет следующие без времени.
func (n *Normalizer) RunNormalizeImage(ctx context.Context, job Job) error {
	var p itemPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("payload of %s: %w", job.Kind, err)
	}

	cs, err := n.cases.Load(ctx, p.CaseID)
	if err != nil {
		return err
	}
	// Обращение отменили либо разбор уже прошёл: работа повторяться не должна.
	if cs == nil || cs.Status != statusNormalizing {
		return nil
	}

	items, err := n.cases.Items(ctx, p.CaseID)
	if err != nil {
		return err
	}
	item := findItem(items, p.ItemID)
	// Элемент уже разобран или погашен: повтор работы не переделывает сделанное,
	// но цепочку двинуть обязан - иначе обращение зависнет в normalizing.
	if item != nil && item.Status == "pending" {
		// Протокол здесь контекст, а не результат: в базу его кладёт шаг
		// закрытия нормализации, когда разбирать больше нечего.
		if err := n.readScreenshot(ctx, BuildProtocol(items), *item); err != nil {
			return err
		}
	}
	return n.cases.AdvanceNormalize(ctx, p.CaseID)
}

// RunFinishNormalize закрывает нормализацию: сырья не осталось, протокол
// собран целиком. Ставится всегда, даже без единого вложения - обращение из
// одного текста проходит тем же путём.
func (n *Normalizer) RunFinishNormalize(ctx context.Context, job Job) error {
	var p casePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("payload of %s: %w", job.Kind, err)
	}

	cs, err := n.cases.Load(ctx, p.CaseID)
	if err != nil {
		return err
	}
	if cs == nil || cs.Status != statusNormalizing {
		return nil
	}

	items, err := n.cases.Items(ctx, p.CaseID)
	if err != nil {
		return err
	}
	if !hasContent(items) {
		return n.reopen(ctx, cs, job.ID)
	}
	return n.finish(ctx, cs, job.ID, BuildProtocol(items), len(items))
}

// readScreenshot разбирает один скриншот и записывает результат. Ошибку
// возвращает только при сорванном вызове: отказ по содержимому гасит элемент, а
// не работу.
func (n *Normalizer) readScreenshot(ctx context.Context, protocol string, item Item) error {
	data, err := readFile(item.FilePath)
	if err != nil {
		n.log.Warn("item_rejected", "item_id", item.ID, "reason", "unreadable_file", "error", err)
		return failItem(ctx, n.cases.pool, item.ID, modelErrFileUnreadScreenshot)
	}

	mime := item.Mime
	if mime == "" {
		mime = "image/jpeg"
	}

	// Порядок фиксирован: стабильный префикс первым сообщением, волатильное
	// следом, изображение последним. Любая изменяющаяся строка перед промтом
	// молча гасит кэш провайдера.
	req := Request{
		Step: stepScreenshot,
		Messages: []Message{
			{Role: "system", Parts: []Part{TextPart(screenshotPrompt)}},
			{Role: "user", Parts: []Part{TextPart(modelRawProtocolPrefix + protocol)}},
			{Role: "user", Parts: []Part{ImagePart(data, mime)}},
		},
		SchemaName: "screenshot_extract",
		Schema:     screenshotSchema,
	}

	// Невалидный ответ - один повтор: модель промахивается разово, а второй
	// такой же ответ означает, что скриншот ей не даётся.
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := n.llm.Complete(ctx, req)
		if err != nil {
			return err
		}
		extract, err := parseExtract(raw)
		if err == nil {
			return n.cases.SaveNormalized(ctx, item.ID, scrubContacts(formatExtract(extract)))
		}
		n.log.Warn("llm_invalid", "step", stepScreenshot, "item_id", item.ID,
			"attempt", attempt+1, "error", err)
	}
	return failItem(ctx, n.cases.pool, item.ID, modelErrScreenshotSchema)
}

// screenshotFact - пара «поле экрана и его значение».
type screenshotFact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// screenshotExtract - контракт разбора скриншота (architecture.md, раздел 7).
// Unreadable указателем не ради nil: отсутствие поля и пустой список - разные
// ответы, и первый недопустим. Правдоподобная выдумка хуже явного пробела.
type screenshotExtract struct {
	Screen     string           `json:"screen"`
	Facts      []screenshotFact `json:"facts"`
	Relevant   string           `json:"relevant"`
	Unreadable *[]string        `json:"unreadable"`
}

func parseExtract(raw json.RawMessage) (screenshotExtract, error) {
	var e screenshotExtract
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, fmt.Errorf("decode extract: %w", err)
	}
	if e.Unreadable == nil {
		return e, errors.New("unreadable is missing")
	}
	if len(e.Facts) == 0 && len(*e.Unreadable) == 0 {
		return e, errors.New("facts and unreadable are both empty")
	}
	return e, nil
}

// finish закрывает нормализацию: протокол, статус, событие и уведомление автору
// живут или не живут вместе, файлы уходят следом.
//
// Здесь единственная развилка по режиму: сырьё превращается в текст одинаково
// для любого разговора, а следующий шаг выбирает режим. Разговор начинает
// работа, а не этот шаг, - это и делает второй режим бота наслаиванием, а не
// переписью.
func (n *Normalizer) finish(ctx context.Context, cs *Case, jobID int64, protocol string, items int) error {
	chars := utf8.RuneCountInString(protocol)

	status, job := statusInterview, JobInterview
	text := protocolMessage(protocol)
	if cs.Mode == modeAsk {
		// Сказать автору тут нечего: «смотрю документацию» он получил на «Готово»,
		// а следующее слово бота - сам ответ.
		status, job, text = statusAnswering, JobLookup, ""
	}

	moved := false
	err := n.cases.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE cases SET status = $3, protocol = $2, updated_at = now()
			WHERE id = $1 AND status = 'normalizing'`, cs.ID, protocol, status)
		if err != nil {
			return fmt.Errorf("finish normalize of case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		moved = true

		if err := addEvent(ctx, tx, cs.ID, "normalized", map[string]any{
			"items": items, "chars": chars,
		}); err != nil {
			return err
		}
		if cs.Mode == modeAsk {
			// Реплика автора для истории разговора: в журнал идёт только новая
			// часть протокола, иначе прошлые вопросы придут в контекст дважды.
			if err := addEvent(ctx, tx, cs.ID, "question_asked", map[string]any{
				"text": protocolDelta(cs.Protocol, protocol),
			}); err != nil {
				return err
			}
		}
		if text != "" {
			if err := putNotify(ctx, tx, cs.ID, jobID, text); err != nil {
				return err
			}
		}
		return replaceJob(ctx, tx, job, cs.ID, casePayload{CaseID: cs.ID})
	})
	if err != nil {
		return err
	}
	if !moved {
		return nil
	}

	cs.Status = status
	n.log.Info("normalized", "case_id", cs.ID, "items", items, "chars", chars, "mode", cs.Mode)
	// Файлы живут до публикации: саммари - последняя точка, где автор ловит
	// неверно прочитанный скриншот, и стирать медиа раньше нельзя.
	return nil
}

// protocolDelta - часть протокола, появившаяся с прошлого захода. Протокол
// собирается append-порядком по id элементов, поэтому прежнее значение всегда
// его префикс, и вычитание точное.
func protocolDelta(previous, protocol string) string {
	return strings.TrimSpace(strings.TrimPrefix(protocol, previous))
}

// reopen возвращает обращение в сбор, когда разобрать не удалось ничего: иначе
// слот активного обращения занят навсегда, и автор не заведёт даже новое.
func (n *Normalizer) reopen(ctx context.Context, cs *Case, jobID int64) error {
	reopened := false
	err := n.cases.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		reopened, err = reopenCase(ctx, tx, cs.ID)
		if err != nil || !reopened {
			return err
		}

		if err := addEvent(ctx, tx, cs.ID, "case_reopened", map[string]any{"reason": "nothing parsed"}); err != nil {
			return err
		}
		return putNotify(ctx, tx, cs.ID, jobID, msgNothingParsed)
	})
	if err != nil {
		return err
	}
	if !reopened {
		return nil
	}

	cs.Status = statusCollecting
	n.log.Warn("case_reopened", "case_id", cs.ID, "reason", "nothing parsed")
	return nil
}

// hasContent - осталось ли в сырье хоть что-то разобранное. Пустого протокола
// для этой проверки не хватает: BuildProtocol пишет провал строкой «не удалось
// разобрать», и обращение из одних провалов дало бы непустой текст без единого
// факта.
func hasContent(items []Item) bool {
	for _, it := range items {
		if it.Status == "failed" {
			continue
		}
		if strings.TrimSpace(it.Normalized) != "" || strings.TrimSpace(it.SourceText) != "" {
			return true
		}
	}
	return false
}

// readAudio отдаёт байты записи и значение format для input_audio.
func (n *Normalizer) readAudio(item Item) ([]byte, string, error) {
	data, err := readFile(item.FilePath)
	if err != nil {
		return nil, "", err
	}
	return data, audioFormat(item.Mime), nil
}

// audioFormat переводит mime Telegram в поле format запроса. Голосовое всегда
// приходит контейнером ogg, поэтому он же фолбэк для пустого mime.
func audioFormat(mime string) string {
	switch {
	case strings.Contains(mime, "mpeg"), strings.Contains(mime, "mp3"):
		return "mp3"
	case strings.Contains(mime, "wav"):
		return "wav"
	default:
		return "ogg"
	}
}

// readFile отделяет «пути нет в БД» от «файла нет на диске»: после DropFiles
// путь обнулён, и это не сбой чтения, а стёртое медиа.
func readFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("item has no file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("file is empty")
	}
	return data, nil
}

func findItem(items []Item, id int64) *Item {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
	}
	return nil
}

// failAndMoveOn гасит элемент и двигает цепочку: провал одной записи не имеет
// права держать обращение в normalizing.
func (n *Normalizer) failAndMoveOn(ctx context.Context, caseID string, itemID int64, reason string) error {
	if err := failItem(ctx, n.cases.pool, itemID, reason); err != nil {
		return err
	}
	return n.cases.AfterVoiceFail(ctx, caseID, itemID)
}

func mustPrompt(name string) string {
	data, err := promptFiles.ReadFile("prompts/" + name)
	if err != nil {
		panic("prompt " + name + ": " + err.Error())
	}
	return string(data)
}
