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
	"net/url"
	"strings"
	"time"
)

const (
	openRouterURL = "https://openrouter.ai/api/v1/chat/completions"
	// Бюджет одной попытки. Прежние две минуты держались за шаг, который думал
	// без предела: генерация уходила в размышления на 110 секунд, вторая попытка
	// в бюджет работы уже не влезала, и автор ждал повторов очереди по пять
	// минут (наблюдение 2026-08-12). С ограниченными размышлениями шаг
	// укладывается в полминуты, и минута - это запас на разброс, а не на новую
	// такую же потерю.
	llmTimeout = time.Minute
	// Остаток дедлайна, меньше которого повтор не начинаем: генерация под
	// остаток короче собственной длительности только спишет токены за работу,
	// которую оборвут. Следствие для синхронного пути (`/project`, бюджет 20
	// секунд): повтора там нет вовсе, команда быстрее уходит в свой фолбэк.
	llmMinBudget = 30 * time.Second
	llmRetries   = 3
	llmBodyLimit = 1 << 20
	// Потолок ответа диалогового шага - страховка от генерации без конца, а не
	// тюнинг. Он один на размышления и на сам ответ, а уровень размышлений
	// меняют переменной контура без релиза: впритык к наблюдавшимся 4476
	// токенам потолок обрезал бы каждый ход на `medium`, и обрезанный JSON не
	// разбирается вовсе. Медиа-шаги потолка не ставят: длину транскрипта задаёт
	// запись, а не модель, и голосовое на десятки минут упрётся в любой предел.
	llmMaxTokens = 8000
)

// OpenRouter - клиент chat/completions. Один эндпоинт не оправдывает SDK:
// чужая модель ошибок и повторов дороже своих ста строк.
type OpenRouter struct {
	key   string
	model string
	http  *http.Client
	log   *slog.Logger
}

// NewOpenRouter: proxy - адрес HTTP-прокси или пусто. Прокси задаётся только
// этому клиенту, а не через окружение процесса: на хосте прямой путь к
// OpenRouter закрыт, а Telegram и GitHub с него доступны напрямую, и уводить их
// в тот же туннель значит менять то, что работает.
func NewOpenRouter(key, model, proxy string, log *slog.Logger) *OpenRouter {
	// Без таймаута клиента: бюджет попытки ставит send через ctx, и там же
	// действует дедлайн работы. Два независимых предела на одно соединение
	// разошлись бы, и виновника обрыва пришлось бы угадывать по логу.
	client := &http.Client{}
	if proxy != "" {
		// Адрес разобран и проверен при загрузке конфига.
		parsed, _ := url.Parse(proxy)
		client.Transport = &http.Transport{Proxy: http.ProxyURL(parsed)}
	}
	return &OpenRouter{key: key, model: model, http: client, log: log}
}

// Request - один вызов модели.
//
// Порядок Messages сохраняется как есть, клиент ничего не вставляет между
// сообщениями: стабильный префикс обязан идти первым сообщением, и любая
// изменяющаяся строка перед ним молча гасит кэш провайдера.
type Request struct {
	Step string // имя шага для лога; содержимое запроса не логируется
	// CaseID - только для лога (llm_call, llm_retry, llm_invalid): по нему
	// разбор инцидента с моделью собирает цепочку конкретного обращения.
	// Пусто у шагов вне обращения (например заведение проекта).
	CaseID     string
	Model      string          // пусто - модель клиента по умолчанию
	Messages   []Message       // префикс первым, волатильное последним
	SchemaName string          // имя схемы в response_format, пусто - "response"
	Schema     json.RawMessage // тело JSON Schema ответа
	// Reasoning - уровень размышлений модели: пусто означает не передавать
	// параметр вовсе. Требовать его у провайдера без поддержки нельзя:
	// require_parameters сузит выбор до тех, кто умеет reasoning.
	Reasoning string
	// MaxTokens - потолок ответа: ноль означает не передавать параметр вовсе.
	// Ставят его диалоговые шаги, где длину ответа выбирает модель. Шагу, чья
	// длина задана материалом (транскрипт записи), потолок обрезал бы ответ на
	// середине, а обрезанный JSON не разбирается вовсе.
	MaxTokens int
}

// DialogModel - модель диалоговых шагов и её бюджет размышлений. Пара едет
// вместе: уровень размышлений имеет смысл только рядом с моделью, которой он
// адресован, и меняется тем же решением контура.
type DialogModel struct {
	Name      string
	Reasoning string
}

type Message struct {
	Role  string
	Parts []Part
}

// Part - кусок содержимого сообщения. Поля закрыты: вид куска задаётся
// конструктором, чтобы не собралось сообщение с аудио и mime картинки разом.
type Part struct {
	kind   string
	text   string
	data   []byte
	format string
}

func TextPart(text string) Part { return Part{kind: "text", text: text} }

// AudioPart принимает только байты: URL в input_audio OpenRouter не берёт.
// format - контейнер записи (ogg, mp3), он же подсказка провайдеру о кодеке.
func AudioPart(data []byte, format string) Part {
	return Part{kind: "audio", data: data, format: format}
}

// ImagePart уходит как data:<mime>;base64 в image_url.
func ImagePart(data []byte, mime string) Part {
	return Part{kind: "image", data: data, format: mime}
}

func (m Message) MarshalJSON() ([]byte, error) {
	wire := struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}{Role: m.Role}

	// Единственный текстовый кусок отдаём строкой: массив content-частей в
	// system-сообщении принимают не все провайдеры.
	if len(m.Parts) == 1 && m.Parts[0].kind == "text" {
		wire.Content = m.Parts[0].text
	} else {
		wire.Content = m.Parts
	}
	return json.Marshal(wire)
}

func (p Part) MarshalJSON() ([]byte, error) {
	switch p.kind {
	case "text":
		return json.Marshal(map[string]string{"type": "text", "text": p.text})
	case "audio":
		return json.Marshal(map[string]any{
			"type": "input_audio",
			"input_audio": map[string]string{
				"data":   base64.StdEncoding.EncodeToString(p.data),
				"format": p.format,
			},
		})
	case "image":
		return json.Marshal(map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url": "data:" + p.format + ";base64," + base64.StdEncoding.EncodeToString(p.data),
			},
		})
	default:
		return nil, fmt.Errorf("unknown content part %q", p.kind)
	}
}

type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []Message      `json:"messages"`
	ResponseFormat responseFormat `json:"response_format"`
	Provider       providerOpts   `json:"provider"`
	MaxTokens      int            `json:"max_tokens,omitempty"`
	Reasoning      *reasoningOpts `json:"reasoning,omitempty"`
}

// reasoningOpts: размышления модели считаются выходными токенами и оплачиваются
// как они же, а времени занимают больше самого ответа.
type reasoningOpts struct {
	Effort string `json:"effort"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

// providerOpts: без require_parameters запрос уедет к провайдеру без поддержки
// схемы и вернётся свободным текстом вместо структуры.
type providerOpts struct {
	RequireParameters bool `json:"require_parameters"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		// "length" означает, что ответ упёрся в потолок и оборван на середине.
		// Без этого признака обрыв виден только ошибкой разбора JSON, а она
		// читается как «модель не держит схему» - причину искали бы не там.
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		// Размышления входят в completion_tokens; отдельным числом видно, чем
		// занят шаг - генерацией ответа или раздумьем перед ним.
		CompletionDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type llmResult struct {
	content     json.RawMessage
	tokensIn    int
	tokensOut   int
	tokensThink int
}

// Complete возвращает content первого choice сырым: в схему его разбирает
// вызывающий, он же и валидирует - вывод модели недоверенный.
func (c *OpenRouter) Complete(ctx context.Context, req Request) (json.RawMessage, error) {
	if len(req.Messages) == 0 || len(req.Schema) == 0 {
		return nil, errors.New("openrouter: request needs messages and schema")
	}

	model := req.Model
	if model == "" {
		model = c.model
	}
	name := req.SchemaName
	if name == "" {
		name = "response"
	}

	wire := chatRequest{
		Model:          model,
		Messages:       req.Messages,
		ResponseFormat: responseFormat{Type: "json_schema", JSONSchema: jsonSchema{Name: name, Strict: true, Schema: req.Schema}},
		Provider:       providerOpts{RequireParameters: true},
		MaxTokens:      req.MaxTokens,
	}
	if req.Reasoning != "" {
		wire.Reasoning = &reasoningOpts{Effort: req.Reasoning}
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("openrouter %s: build request: %w", req.Step, err)
	}

	for attempt := 0; ; attempt++ {
		start := time.Now()
		out, retry, err := c.send(ctx, body)
		// Время попытки, а не всей цепочки: сумма прячет как раз то, ради чего
		// метрику читают - сколько занимает один проход модели.
		spent := time.Since(start)
		if err == nil {
			c.log.Info("llm_call",
				"step", req.Step,
				"case_id", req.CaseID,
				"model", model,
				"ms", spent.Milliseconds(),
				"attempt", attempt+1,
				"tokens_in", out.tokensIn,
				"tokens_out", out.tokensOut,
				"tokens_think", out.tokensThink)
			return out.content, nil
		}
		if !retry || attempt == llmRetries {
			return nil, fmt.Errorf("openrouter %s: %w", req.Step, err)
		}
		// Повтор генерирует ответ заново и стоит как первая попытка: начинать
		// его на исходе дедлайна значит списать токены за работу, которую всё
		// равно оборвут.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < llmMinBudget {
			return nil, fmt.Errorf("openrouter %s: no budget for attempt %d: %w", req.Step, attempt+2, err)
		}

		c.log.Warn("llm_retry", "step", req.Step, "case_id", req.CaseID, "model", model, "attempt", attempt+1,
			"ms", spent.Milliseconds(), "error", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("openrouter %s: %w", req.Step, ctx.Err())
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		}
	}
}

// send делает одну попытку. Второе значение - повторять ли: 429 и 5xx да, 4xx
// нет, разбитое соединение да - ответа не было, значит и запрос не отработал.
func (c *OpenRouter) send(ctx context.Context, body []byte) (llmResult, bool, error) {
	// Бюджет попытки. Дедлайн работы из ctx остаётся главным: если его осталось
	// меньше, победит он, и попытка оборвётся раньше своего предела.
	ctx, cancel := context.WithTimeout(ctx, llmTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterURL, bytes.NewReader(body))
	if err != nil {
		return llmResult{}, false, fmt.Errorf("build http request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.key)
	httpReq.Header.Set("Content-Type", "application/json")
	// Идентификация приложения: OpenRouter ждёт эти два заголовка, а дефолтный
	// User-Agent Go-клиента отличает нас от браузера для edge-защиты.
	httpReq.Header.Set("HTTP-Referer", "https://github.com/galera-club/tg-intake")
	httpReq.Header.Set("X-Title", "tg-intake")
	httpReq.Header.Set("User-Agent", "tg-intake/1.0")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return llmResult{}, true, fmt.Errorf("post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, llmBodyLimit))
	if err != nil {
		return llmResult{}, true, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return llmResult{}, retry, fmt.Errorf("status %d: %s", resp.StatusCode, errorMessage(raw))
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return llmResult{}, false, fmt.Errorf("decode response: %w", err)
	}
	// Ошибку провайдера OpenRouter кладёт в тело ответа с кодом 200.
	if out.Error != nil {
		retry := out.Error.Code == http.StatusTooManyRequests || out.Error.Code >= 500
		return llmResult{}, retry, fmt.Errorf("provider error %d: %s", out.Error.Code, cut(out.Error.Message))
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return llmResult{}, false, errors.New("empty content")
	}
	// Повтор не поможет: тот же запрос упрётся в тот же потолок. Ошибка названа
	// прямо, чтобы правка потолка не искалась через разбор JSON.
	if out.Choices[0].FinishReason == "length" {
		return llmResult{}, false, fmt.Errorf("response truncated at %d tokens", out.Usage.CompletionTokens)
	}

	return llmResult{
		content:     json.RawMessage(out.Choices[0].Message.Content),
		tokensIn:    out.Usage.PromptTokens,
		tokensOut:   out.Usage.CompletionTokens,
		tokensThink: out.Usage.CompletionDetails.ReasoningTokens,
	}, false, nil
}

// errorMessage берёт из ответа только error.message. Рядом в metadata провайдер
// возвращает отфильтрованный вход, а транскрипт не имеет права попасть ни в
// текст ошибки, ни следом в лог.
func errorMessage(raw []byte) string {
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		// Не-JSON отвечает не API, а что-то по дороге: шлюз, фильтр, блокировка.
		// Голое «non-json body» такой ответ не отличает, поэтому показываем его
		// начало - нашего запроса в нём нет.
		return "non-json body: " + cut(oneLine(string(raw)))
	}
	return cut(out.Error.Message)
}

func cut(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= 200 {
		return message
	}
	return strings.ToValidUTF8(message[:200], "")
}
