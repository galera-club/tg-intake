package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Config читается один раз при старте и дальше передаётся значением.
type Config struct {
	DatabaseURL     string
	BotToken        string
	AllowedIDs      []int64
	LogLevel        slog.Level
	Env             string
	OpenRouterKey   string
	OpenRouterProxy string
	TelegramProxy   string
	ModelMedia      string
	ModelDialog     string
	// Уровень размышлений диалоговых шагов. Пусто - параметр не передаётся
	// вовсе: модели без reasoning он не нужен, а требование его поддержки сузило
	// бы выбор провайдеров.
	ReasoningDialog string
	GitHubToken     string
	// Node id доски организации (PVT_...); пусто - тикеты на доску не ставятся.
	BoardID         string
	MediaDir        string
	MaxItems        int
	InterviewRounds int
	Projects        []ProjectConfig
	// Чат уведомлений владельца и токен бота, которым туда пишут. Пустой чат
	// выключает уведомления целиком, пустой токен отдаёт отправку боту сервиса.
	AlertBotToken string
	AlertChatID   int64
	// Тема группы уведомлений (message_thread_id Bot API). 0 - без темы:
	// сообщение в общий чат, как до среза.
	AlertThreadID int
}

// ProjectConfig - проект из переменной окружения. Список живёт в конфиге
// контура, а не в git: репозиторий публичный, и адреса внутренних сервисов туда
// попадать не должны.
type ProjectConfig struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	Context string `json:"context"`
	// Указатель, чтобы отличить «не указано» от «выключен»: по умолчанию проект
	// активен, и отсутствие поля не должно прятать его из меню.
	Active *bool `json:"active"`
}

func (p ProjectConfig) IsActive() bool { return p.Active == nil || *p.Active }

// LoadConfig собирает конфиг из окружения. Ошибка перечисляет все проблемы
// разом: иначе поднятие контура идёт циклом «запустил, узнал про одну
// переменную, запустил снова».
func LoadConfig() (Config, error) {
	var problems []string

	cfg := Config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		BotToken:      os.Getenv("TELEGRAM_BOT_TOKEN"),
		Env:           valueOr(os.Getenv("ENV"), "dev"),
		OpenRouterKey: os.Getenv("OPENROUTER_API_KEY"),
		// Пусто - идём напрямую. Нужен там, где хосту закрыт прямой путь к
		// OpenRouter: без прокси запрос возвращает 403 от фильтра по дороге, а
		// не ответ API.
		OpenRouterProxy: os.Getenv("OPENROUTER_PROXY"),
		// Прокси Telegram отдельный и другой схемы: у соседних сервисов это
		// socks5, тогда как OpenRouter ходит через HTTP-прокси.
		TelegramProxy: os.Getenv("TELEGRAM_PROXY"),
		ModelMedia:    valueOr(os.Getenv("OPENROUTER_MODEL_MEDIA"), "google/gemini-3.1-flash-lite"),
		ModelDialog:   valueOr(os.Getenv("OPENROUTER_MODEL_DIALOG"), "deepseek/deepseek-v4-flash-0731"),
		GitHubToken:   os.Getenv("GITHUB_TOKEN"),
		BoardID:       os.Getenv("GITHUB_BOARD_ID"),
		// Дефолт низкий, а не выключенный: интервью выигрывает от короткого
		// раздумья, но не от того, которое дольше самого ответа.
		ReasoningDialog: valueOr(os.Getenv("OPENROUTER_REASONING_DIALOG"), "low"),
		MediaDir:        valueOr(os.Getenv("MEDIA_DIR"), "/tmp/intake"),
		AlertBotToken:   os.Getenv("ALERT_BOT_TOKEN"),
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL is empty")
	}
	if cfg.BotToken == "" {
		problems = append(problems, "TELEGRAM_BOT_TOKEN is empty")
	}
	if cfg.OpenRouterKey == "" {
		problems = append(problems, "OPENROUTER_API_KEY is empty")
	}
	// Без токена сервис доводит обращение до саммари и упирается в публикацию:
	// весь разговор с автором оказался бы напрасным.
	if cfg.GitHubToken == "" {
		problems = append(problems, "GITHUB_TOKEN is empty")
	}

	ids, err := parseIDs(os.Getenv("TELEGRAM_ALLOWED_IDS"))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.AllowedIDs = ids

	level, err := parseLevel(valueOr(os.Getenv("LOG_LEVEL"), "info"))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.LogLevel = level

	maxItems, err := parsePositive("MAX_ITEMS", valueOr(os.Getenv("MAX_ITEMS"), "30"))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.MaxItems = maxItems

	rounds, err := parsePositive("INTERVIEW_ROUNDS", valueOr(os.Getenv("INTERVIEW_ROUNDS"), "2"))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.InterviewRounds = rounds

	chat, err := parseChatID(os.Getenv("ALERT_CHAT_ID"))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.AlertChatID = chat

	thread, err := parseThreadID(os.Getenv("ALERT_THREAD_ID"))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.AlertThreadID = thread

	projects, err := ParseProjects(os.Getenv("PROJECTS"))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.Projects = projects

	reasoning, err := parseReasoning(cfg.ReasoningDialog)
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.ReasoningDialog = reasoning

	// Разбираем при старте: битый адрес прокси иначе всплыл бы первым вызовом
	// наружу, то есть в середине живого разговора.
	for name, value := range map[string]string{
		"OPENROUTER_PROXY": cfg.OpenRouterProxy,
		"TELEGRAM_PROXY":   cfg.TelegramProxy,
	} {
		if err := checkProxy(name, value); err != nil {
			problems = append(problems, err.Error())
		}
	}

	if len(problems) > 0 {
		return Config{}, errors.New("config: " + strings.Join(problems, "; "))
	}
	return cfg, nil
}

// parseIDs разбирает белый список. Пустой список - ошибка: бот без белого
// списка пускал бы в приватные репозитории любого, кто нашёл его в поиске.
func parseIDs(raw string) ([]int64, error) {
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("TELEGRAM_ALLOWED_IDS has non-numeric entry %q", part)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, errors.New("TELEGRAM_ALLOWED_IDS is empty")
	}
	return ids, nil
}

// parseReasoning разбирает уровень размышлений. `off` означает не передавать
// параметр вовсе и отличается от `none`: `none` просит модель не думать, а это
// требование к провайдеру, которое он должен уметь исполнить.
func parseReasoning(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "off":
		return "", nil
	case "none", "minimal", "low", "medium", "high":
		return value, nil
	}
	return "", fmt.Errorf("OPENROUTER_REASONING_DIALOG is %q, want off, none, minimal, low, medium or high", raw)
}

// parseChatID разбирает чат уведомлений. Пусто - уведомления выключены, это не
// ошибка. Знаковый разбор обязателен: у группы и канала chat_id отрицательный.
func parseChatID(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ALERT_CHAT_ID is not a number: %q", raw)
	}
	return id, nil
}

// parseThreadID разбирает тему группы уведомлений. Пусто - без темы, это не
// ошибка: тема - настройка канала, а не обязательный параметр.
func parseThreadID(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	return parsePositive("ALERT_THREAD_ID", raw)
}

// projectSlug повторяет CHECK из схемы: slug уезжает в callback_data, где всего
// 64 байта, и должен переживать разбор кнопки.
var projectSlug = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ParseProjects разбирает список проектов. Пустая строка - не ошибка: локальный
// запуск живёт на seed, и пустое значение не должно опустошать таблицу.
func ParseProjects(raw string) ([]ProjectConfig, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	var list []ProjectConfig
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("PROJECTS is not a json array of projects: %v", err)
	}

	seen := make(map[string]bool, len(list))
	for _, p := range list {
		switch {
		case !projectSlug.MatchString(p.Slug):
			return nil, fmt.Errorf("PROJECTS has slug %q outside of [a-z0-9-]{1,32}", p.Slug)
		case seen[p.Slug]:
			return nil, fmt.Errorf("PROJECTS has duplicate slug %q", p.Slug)
		case strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Owner) == "" ||
			strings.TrimSpace(p.Repo) == "":
			return nil, fmt.Errorf("PROJECTS entry %q needs title, owner and repo", p.Slug)
		// Пустой контекст оставил бы интервью без представления о проекте: вопросы
		// станут общими, а тикет - бесполезным.
		case strings.TrimSpace(p.Context) == "":
			return nil, fmt.Errorf("PROJECTS entry %q has no context", p.Slug)
		}
		seen[p.Slug] = true
	}
	return list, nil
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("LOG_LEVEL %q is not debug, info, warn or error", raw)
	}
}

// parsePositive разбирает счётные настройки: MAX_ITEMS ограничивает пачку сырья
// числом элементов, INTERVIEW_ROUNDS - число раундов вопросов. Ноль в обоих
// случаях означал бы сервис, который ничего не принимает или ничего не
// спрашивает.
func parsePositive(name, raw string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s %q is not a positive integer", name, raw)
	}
	return n, nil
}

// checkProxy проверяет адрес прокси. Пустой означает прямой путь и ошибкой не
// является: локально и на хостах без блокировок прокси не нужен.
func checkProxy(name, raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Scheme == "" {
		return fmt.Errorf("%s %q is not a proxy url like http://host:port", name, raw)
	}
	return nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
