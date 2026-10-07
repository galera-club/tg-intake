//go:build live

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tele "gopkg.in/telebot.v4"
)

// live_test.go - срез 8 ticket-form (docs/specs/ticket-form.md §7, §10): сквозной
// прогон диалогов через настоящие хендлеры бота и воркер. Telegram - фейковый
// (fakeTelegram/screenBot из screen_harness_test.go), OpenRouter и GitHub -
// настоящие, песочница daniil4545/intake-sandbox. Маршруты NewBot тестом не
// покрыты (§9 плана): хендлеры зовутся напрямую, как в остальных тестах пакета,
// а не через tb.ProcessUpdate - таблица кнопка->хендлер лежит в голове теста,
// а не в коде.
//
// Модель недетерминирована и её выбор (был ли раунд, какие ключи спросила,
// закрыла ли пункт ядра) в тесте не проверяется - только логируется: выбор
// модели уже измерен статистически в eval (docs/specs/ticket-form.md).
// Живой прогон проверяет ПРОДУКТ - инварианты §2.1/Р-15/R4 архитектуры,
// которые обязаны держаться при любом выборе модели (assertScreenStripped,
// pressButton, assertGapConsistency, assertTypeLabels, assertIdeasKept,
// assertHeadings). Провайдер, не ответивший вовремя, - не повод валить
// сценарий: waitForCase различает зависший продукт (FAIL) и таймауты модели
// (Skip) по журналу llm_retry/job_failed (см. решение диспетчера по итогам
// второго живого прогона, docs/acceptance/ticket-form.md).

const (
	// Единственная база, с которой работает live: TRUNCATE в начале не должен
	// иметь шанса задеть что-то ещё.
	liveHost     = "localhost"
	livePort     = uint16(5434)
	liveDatabase = "intake_live"
	// Проект зашит, а не читается из окружения: второе условие защиты (§2 плана)
	// выполняется самим устройством теста - другой репозиторий взять неоткуда.
	liveOwner        = "daniil4545"
	liveRepoName     = "intake-sandbox"
	liveProjectSlug  = "sandbox"
	liveProjectTitle = "Qualifier (песочница)"
	liveAuthorID     = int64(1)
	liveRounds       = 2
	liveMaxItems     = 30
	// Дедлайн одного шага (§3 плана): ход модели, публикация в GitHub. Не
	// меньше двух подряд попыток работы воркера (jobTimeout каждая, worker.go)
	// с паузой между ними - одной попытки живому прогону не хватает.
	liveStepDeadline = 2*jobTimeout + 30*time.Second
	livePoll         = 2 * time.Second
	// Запас перед дедлайном самого теста (t.Deadline, из -timeout): без него
	// зависший шаг ловит не наш Fatalf, а -timeout убивает процесс мимо
	// t.Cleanup - отменённое обращение и открытый issue в песочнице остались бы
	// висеть.
	liveDeadlineMargin = 30 * time.Second
)

// liveContext - контекст проекта для промта интервью. eval/cases.jsonl (Р-10
// ticket-form) не читается: набор untracked и в этом дереве отсутствует
// (worktree отдельный от того, где его выгружали). Вместо реального контекста
// qualifier - краткое собственное описание того же домена, которого достаточно
// модели для интервью; решение записано в отчёте среза 8.
const liveContext = `Qualifier - бот-квалификатор лидов клуба поверх Bitrix24: ` +
	`встречает нового лида, ведёт диалог по сценарию, ставит сделку и её статус, ` +
	`шлёт напоминания о встречах. Пример сущностей: сделка (deal), лид, статус ` +
	`сделки, напоминание.`

type liveEnv struct {
	ft      *fakeTelegram
	tb      *tele.Bot
	b       *Bot
	cases   *Cases
	gh      *GitHub
	project Project
	// rules - то же ядро контракта, что видит продукт (Publisher, Interview):
	// gap-инварианты сверяются с ним же, а не с самоотчётом модели (cs.Gaps).
	rules Contract
	// logBuf - копия лога прогона для providerTimeouts: различить зависший
	// продукт и молчащего провайдера можно только по журналу вызовов модели.
	logBuf *liveLogBuf
}

// liveLogBuf копит лог прогона (сверх обычной печати в stderr) для
// providerTimeouts: слог не персистит llm_retry/job_failed в БД, только в
// журнал, а тесту после таймаута шага нужно посчитать их по конкретному
// case_id. slog.Handler сам сериализует вызовы Handle одного хендлера, но
// строку сюда пишет один хендлер, а читает - другая горутина (тест), поэтому
// свой мьютекс всё равно нужен.
type liveLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *liveLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = os.Stderr.Write(p)
	return l.buf.Write(p)
}

// countTimeouts - строки лога этого обращения, где не ответил провайдер:
// llm_retry (сам факт повтора - таймаут или 5xx) и job_failed с явным
// «deadline exceeded» в причине - остальные job_failed (невалидный ответ
// модели, дубль ключа) провайдер ни при чём, это выбор модели или наш же
// checkTurn, и в счёт не идут.
func (l *liveLogBuf) countTimeouts(caseID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	marker := "case_id=" + caseID
	n := 0
	for _, line := range strings.Split(l.buf.String(), "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		switch {
		case strings.Contains(line, "msg=llm_retry"):
			n++
		case strings.Contains(line, "msg=job_failed") && strings.Contains(line, "deadline exceeded"):
			n++
		}
	}
	return n
}

// scenarioResult - исход одного сценария для итоговой строки TestLiveRun.
type scenarioResult struct {
	name    string
	skipped bool
	passed  bool
}

// runScenario - t.Run с учётом отличия «пропущен провайдером» от «прошёл» и
// «упал»: обычный t.Run.ok не различает pass и skip.
func runScenario(t *testing.T, results *[]scenarioResult, name string, fn func(t *testing.T)) {
	var skipped bool
	ok := t.Run(name, func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		fn(t)
	})
	*results = append(*results, scenarioResult{name: name, skipped: skipped, passed: ok && !skipped})
}

func TestLiveRun(t *testing.T) {
	dsn := requireLiveDatabase(t)
	openrouterKey := requireEnv(t, "OPENROUTER_API_KEY")
	githubToken := requireEnv(t, "GITHUB_TOKEN")

	ctx := context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx,
		`TRUNCATE cases, case_items, case_events, jobs, users, projects RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Info, не Warn: разбор живого прогона нужен interview_round (gap_keys) и
	// llm_call, оба уровня Info; providerTimeouts читает тот же журнал.
	logBuf := &liveLogBuf{}
	log := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := SyncProjects(ctx, pool, []ProjectConfig{{
		Slug: liveProjectSlug, Title: liveProjectTitle,
		Owner: liveOwner, Repo: liveRepoName, Context: liveContext,
	}}, log); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	projects, err := ListProjects(ctx, pool)
	if err != nil || len(projects) != 1 {
		t.Fatalf("load seeded project: projects=%v err=%v", projects, err)
	}
	project := projects[0]

	media, err := NewMedia(t.TempDir(), log)
	if err != nil {
		t.Fatalf("new media: %v", err)
	}
	rules, err := LoadContract()
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	statuses, err := LoadStatuses()
	if err != nil {
		t.Fatalf("load statuses: %v", err)
	}

	proxy := os.Getenv("OPENROUTER_PROXY")
	if err := checkProxy("OPENROUTER_PROXY", proxy); err != nil {
		t.Fatal(err)
	}
	model := valueOr(os.Getenv("OPENROUTER_MODEL_DIALOG"), "deepseek/deepseek-v4-flash-0731")
	reasoning, err := parseReasoning(valueOr(os.Getenv("OPENROUTER_REASONING_DIALOG"), "low"))
	if err != nil {
		t.Fatal(err)
	}
	dialog := DialogModel{Name: model, Reasoning: reasoning}

	cases := NewCases(pool, media, log, liveMaxItems, 0)
	llm := newCappedLLM(t, NewOpenRouter(openrouterKey, dialog.Name, proxy, log))
	// GITHUB_TOKEN живёт только в этой переменной и в клиенте: в лог и в код
	// он не идёт нигде дальше.
	gh := NewGitHub(githubToken, GitHubAPI, statuses, log)
	overlap := NewOverlap(gh, llm, log, dialog)
	interview := NewInterview(cases, llm, log, rules, dialog, liveRounds, overlap)
	publisher := NewPublisher(cases, gh, rules, log, 0, "")
	normalizer := NewNormalizer(cases, llm, log)
	ticketsSvc := NewTickets(cases, gh, statuses, log, 0)
	projectsSvc := NewProjects(cases, gh, llm, dialog, log)
	lookup := NewLookup(cases, gh, llm, log, dialog)

	ft, tb := newFakeTelegram(t)
	b := screenBot(tb, pool, cases, log, ticketsSvc)
	b.projects = projectsSvc
	b.allowed = []int64{liveAuthorID}

	handlers := map[string]JobHandler{
		JobNormalizeVoice:  normalizer.RunNormalizeVoice,
		JobNormalizeImage:  normalizer.RunNormalizeImage,
		JobFinishNormalize: normalizer.RunFinishNormalize,
		JobInterview:       interview.Run,
		JobSummarize:       interview.Summarize,
		JobPublish:         publisher.Run,
		JobNotify:          b.Notify,
		JobCancelIssue:     ticketsSvc.RunCancel,
		JobLookup:          lookup.Run,
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	t.Cleanup(cancelWorker)
	go RunWorker(workerCtx, pool, log, handlers, cases.HandleFailedJob)

	env := &liveEnv{ft: ft, tb: tb, b: b, cases: cases, gh: gh, project: project, rules: rules, logBuf: logBuf}

	// Один автор (id 1, единственный в белом списке) ведёт сценарии по очереди:
	// активное обращение у него одно, и следующий сценарий начинается только
	// когда предыдущее опубликовано (или отменено уборкой упавшего/пропущенного
	// - см. startCase/cancelIfActive).
	var results []scenarioResult
	runScenario(t, &results, "S1_bug_R2", func(t *testing.T) { runBugScenario(t, env) })
	runScenario(t, &results, "S2_feature_R2", func(t *testing.T) { runFeatureScenario(t, env) })
	runScenario(t, &results, "S3_mixed_R1", func(t *testing.T) {
		runMixedScenario(t, env, mixedScenario{
			name:          "S3",
			material:      []string{mixedR1Text},
			facts:         mixedR1Facts,
			materialIdeas: []string{"склонени"},
			factIdeas:     []string{"Анны", "забыва"},
		})
	})
	runScenario(t, &results, "S4_mixed_gate_b", func(t *testing.T) {
		runMixedScenario(t, env, mixedScenario{
			name:          "S4",
			material:      mixedSecondMaterial,
			facts:         mixedR1Facts,
			materialIdeas: []string{"склонени", "коммент"},
			factIdeas:     []string{"Анны", "забыва"},
		})
	})
	runScenario(t, &results, "S5_skip_R5", func(t *testing.T) { runSkipScenario(t, env) })

	passed, skipped, failed := 0, 0, 0
	for _, r := range results {
		switch {
		case r.skipped:
			skipped++
		case r.passed:
			passed++
		default:
			failed++
		}
	}
	t.Logf("итог: пройдено %d/%d, пропущено провайдером %d, упало %d",
		passed, len(results), skipped, failed)
	if passed == 0 {
		t.Fatalf("ни один сценарий не дошёл до конца: пройдено 0 из %d (провайдер пропустил %d, упало %d)",
			len(results), skipped, failed)
	}
}

// requireLiveDatabase - защита §2 плана: только intake_live на localhost:5434,
// прод-адрес из .env сюда попасть не должен. Печатает хост/порт/базу, а не сам
// DSN - в нём пароль.
func requireLiveDatabase(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv("LIVE_DATABASE_URL")
	if dsn == "" {
		t.Fatal("LIVE_DATABASE_URL is not set: live truncates the database it runs on")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse LIVE_DATABASE_URL: %v", err)
	}
	host := cfg.ConnConfig.Host
	if host != liveHost && host != "127.0.0.1" {
		t.Fatalf("LIVE_DATABASE_URL host %q is not %s: live runs only against the local sandbox", host, liveHost)
	}
	if cfg.ConnConfig.Port != livePort {
		t.Fatalf("LIVE_DATABASE_URL port %d is not %d", cfg.ConnConfig.Port, livePort)
	}
	if cfg.ConnConfig.Database != liveDatabase {
		t.Fatalf("LIVE_DATABASE_URL database %q is not %q: refusing to truncate anything else",
			cfg.ConnConfig.Database, liveDatabase)
	}
	return dsn
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set", name)
	}
	return v
}

func must(t *testing.T, err error, action string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

// startCase - вход каждого сценария (§4 плана): /start, проект, «Создать
// тикет». Экран, правящийся до заведения обращения (домашний список, меню
// проекта), фейк не проверяет по id - фейковый Bot API не отличает
// существующее сообщение от нового, а живой экран обращения (Screen)
// появляется только вместе с самим обращением.
func startCase(t *testing.T, env *liveEnv) string {
	t.Helper()
	ctx := context.Background()

	must(t, env.b.onStart(textCtx(env.tb, liveAuthorID, "/start")), "onStart")
	pressButton(t, env, env.b.onProject, 1, env.project.Slug, "")
	pressButton(t, env, env.b.onCreate, 1, env.project.Slug, "")

	cs, err := env.cases.Active(ctx, liveAuthorID)
	if err != nil {
		t.Fatalf("active case after start: %v", err)
	}
	if cs == nil {
		t.Fatal("нет активного обращения после «Создать тикет»")
	}
	// Упавший или пропущенный по провайдеру сценарий не должен держать
	// активное обращение автора: следующий startCase получил бы его вместо
	// нового и упал бы на чужом состоянии.
	t.Cleanup(func() { cancelIfActive(t, env, cs.ID) })
	return cs.ID
}

// cancelIfActive - уборка: CancelCase - no-op на уже опубликованном или
// отменённом обращении, так что вызов безопасен и после успеха сценария тоже.
func cancelIfActive(t *testing.T, env *liveEnv, caseID string) {
	t.Helper()
	cs, err := env.cases.Load(context.Background(), caseID)
	if err != nil || cs == nil {
		return
	}
	if err := env.cases.CancelCase(context.Background(), cs, "live run cleanup"); err != nil {
		t.Logf("не удалось отменить обращение %s при уборке: %v", caseID, err)
	}
}

// sendMaterial отправляет материал сценария по одному сообщению - так его
// присылал бы автор - и возвращает обращение с заведённым счётчиком (тоже
// живой экран, Р-8): от него отсчитывается первая смена экрана.
func sendMaterial(t *testing.T, env *liveEnv, caseID string, lines []string) *Case {
	t.Helper()
	for _, line := range lines {
		must(t, env.b.onItem(textCtx(env.tb, liveAuthorID, line)), "onItem")
	}
	return reload(t, env.cases, caseID)
}

func finishCollect(t *testing.T, env *liveEnv) {
	t.Helper()
	must(t, env.b.onDone(textCtx(env.tb, liveAuthorID, "Готово")), "onDone")
}

// stepDeadline - дедлайн одного ожидания: не больше liveStepDeadline и не
// позже дедлайна самого теста (t.Deadline из -timeout) минус запас на
// Cleanup - иначе завершение по -timeout прибьёт процесс мимо t.Fatalf, и
// отменить обращение да закрыть issue в песочнице будет некому.
func stepDeadline(t *testing.T) time.Time {
	t.Helper()
	deadline := time.Now().Add(liveStepDeadline)
	if td, ok := t.Deadline(); ok {
		if safe := td.Add(-liveDeadlineMargin); safe.Before(deadline) {
			deadline = safe
		}
	}
	return deadline
}

// waitForCase ждёт, пока обращение дойдёт до состояния match и его живой
// экран сменится (screen_msg отличается от prevScreen): смена экрана значит,
// что доставку взял на себя воркер (работа notify), а не только сохранил
// статус - до неё автору нечего было бы нажимать. Как только новый экран
// найден, прошлый (prevScreen) обязан быть уже снят - assertScreenStripped:
// это и есть проверяемая часть инварианта §3 «не больше одного живого
// inline-экрана».
//
// Не дождались за liveStepDeadline: если в логе этого обращения есть следы
// того, что провайдер не отвечал (llm_retry, job_failed с deadline exceeded) -
// это не продукт виноват, Skip; иначе - зависание продукта, Fatal.
func waitForCase(t *testing.T, env *liveEnv, caseID string, prevScreen int, match func(*Case) bool) *Case {
	t.Helper()

	deadline := stepDeadline(t)
	for {
		cs := reload(t, env.cases, caseID)
		if match(cs) && cs.Screen != prevScreen {
			if prevScreen != 0 {
				assertScreenStripped(t, env.ft, prevScreen)
			}
			return cs
		}
		if time.Now().After(deadline) {
			if n := env.logBuf.countTimeouts(caseID); n > 0 {
				t.Skipf("провайдер не ответил: %d таймаутов (case %s)", n, caseID)
			}
			t.Fatalf("не дождались шага обращения %s за %s: status=%s round=%d screen=%d",
				caseID, liveStepDeadline, cs.Status, cs.Round, cs.Screen)
		}
		time.Sleep(livePoll)
	}
}

// assertScreenStripped - §3 плана: прошлый живой экран обязан получить
// editMessageReplyMarkup (снятие кнопок) до того, как новый стал живым -
// иначе оба остались бы кликабельны разом, что и запрещает «не больше одного
// сообщения с inline_keyboard».
func assertScreenStripped(t *testing.T, ft *fakeTelegram, screenID int) {
	t.Helper()
	if n := len(ft.stripsOf(screenID)); n == 0 {
		t.Errorf("экран %d не снят при смене живого экрана: кнопки прошлого шага остались активны", screenID)
	}
}

// waitForRoundOrSummary - шаг §3 плана: следующий раунд вопросов либо саммари
// без раунда. Второе значение - был ли раунд.
func waitForRoundOrSummary(t *testing.T, env *liveEnv, caseID string, priorRound, prevScreen int) (*Case, bool) {
	t.Helper()
	cs := waitForCase(t, env, caseID, prevScreen, func(cs *Case) bool {
		return cs.Status == statusSummary || (cs.Status == statusInterview && cs.Round > priorRound)
	})
	return cs, cs.Status == statusInterview
}

func waitForPublished(t *testing.T, env *liveEnv, caseID string, prevScreen int) *Case {
	t.Helper()
	return waitForCase(t, env, caseID, prevScreen, func(cs *Case) bool {
		return cs.Status == statusPublished && cs.IssueNumber != 0
	})
}

// publish жмёт «Публикую», ждёт issue и сразу - пока не случилось ничего
// дальше - логирует номер и регистрирует уборку: паспорт прогона обязан
// показать номер даже если следующая проверка сценария зафейлится. closeAfter
// - S1/S2/S5 закрывают тикет в песочнице сами (§8 плана), S3/S4 оставляют
// открытым для гейта B.
func publish(t *testing.T, env *liveEnv, caseID string, screenID int, closeAfter bool) *Case {
	t.Helper()
	pressPublish(t, env, screenID)
	cs := waitForPublished(t, env, caseID, screenID)
	t.Logf("issue #%d создан в %s/%s", cs.IssueNumber, env.project.Owner, env.project.Repo)
	if closeAfter {
		t.Cleanup(func() { closeSandboxIssue(t, env, cs.IssueNumber) })
	}
	return cs
}

// factKeyOrder - порядок ядра плюс уточнение, для стабильного порядка строк
// собранного ответа.
var factKeyOrder = []string{"case", "wrong", "need", "why", "detail"}

// answerWithFacts - раунд отвечается всеми подготовленными фактами разом, а
// не только тем, что совпадает с ключом конкретного вопроса модели: модель
// может спросить не под тем ключом, под которым заготовлен факт (в живом
// прогоне спрашивала под case то, что было заготовлено под wrong), или
// повторно попросить то, что уже есть в материале. Какой именно раунд и
// какие ключи задала модель - решение модели, оно идёт в t.Logf вызывающим,
// не сюда. Ответ - одним текстом (§3 плана).
func answerWithFacts(facts map[string]string) string {
	lines := make([]string, 0, len(facts))
	for _, key := range factKeyOrder {
		if v := facts[key]; v != "" {
			lines = append(lines, v)
		}
	}
	if len(lines) == 0 {
		return "Не знаю"
	}
	return strings.Join(lines, "\n")
}

// driveToSummary ведёt обращение до саммари, отвечая на любые раунды
// подготовленными фактами и логируя, что именно спросила модель (§4 плана,
// решение диспетчера по итогам второго прогона: выбор модели не проверяется,
// только логируется). firstRoundScreen/firstRound - экран и номер первого
// раунда, 0 - раунда не было вовсе.
func driveToSummary(t *testing.T, env *liveEnv, name, caseID string, prevScreen int, facts map[string]string,
) (cs *Case, firstRoundScreen, firstRound int) {
	t.Helper()

	priorRound := 0
	for {
		var roundOccurred bool
		cs, roundOccurred = waitForRoundOrSummary(t, env, caseID, priorRound, prevScreen)
		if !roundOccurred {
			return cs, firstRoundScreen, firstRound
		}
		questions := mustQuestions(t, env.cases, caseID)
		t.Logf("%s: раунд %d, вопросы модели %v", name, cs.Round, questionKeys(questions))
		if firstRoundScreen == 0 {
			firstRoundScreen, firstRound = cs.Screen, cs.Round
		}
		must(t, env.b.onItem(textCtx(env.tb, liveAuthorID, answerWithFacts(facts))), "onItem answer")
		prevScreen, priorRound = cs.Screen, cs.Round
	}
}

func pressButton(t *testing.T, env *liveEnv, handler func(tele.Context) error, screenID int, data, wantToast string) {
	t.Helper()

	before := len(env.ft.methodCalls("answerCallbackQuery"))
	must(t, handler(callbackCtx(env.tb, liveAuthorID, screenID, data)), "press")

	// §2.1: ровно один answerCallbackQuery на нажатие - ноль (не ответили) и
	// два (двойной ответ - его ещё отдельно ловит cleanup фейка,
	// assertSingleAnswers) одинаково провал.
	calls := env.ft.methodCalls("answerCallbackQuery")
	if len(calls) != before+1 {
		t.Fatalf("answerCallbackQuery на нажатие: было %d, стало %d, ожидался прирост ровно на 1", before, len(calls))
	}
	if wantToast != "" {
		got, _ := calls[len(calls)-1].body["text"].(string)
		if got != wantToast {
			t.Errorf("toast: %q, ожидалось %q", got, wantToast)
		}
	}
}

func pressSkip(t *testing.T, env *liveEnv, screenID, round int, wantToast string) {
	t.Helper()
	pressButton(t, env, env.b.onSkip, screenID, strconv.Itoa(round), wantToast)
}

func pressPublish(t *testing.T, env *liveEnv, screenID int) {
	t.Helper()
	pressButton(t, env, env.b.onPublish, screenID, "", "")
}

func fetchIssue(t *testing.T, env *liveEnv, cs *Case) Issue {
	t.Helper()
	issue, err := env.gh.GetIssue(context.Background(), env.project, cs.IssueNumber, false)
	if err != nil {
		t.Fatalf("read issue #%d: %v", cs.IssueNumber, err)
	}
	return issue
}

// assertGapConsistency - R4: метка incomplete и строка «Не уточнено:» в теле
// есть тогда и только тогда, когда ядро открыто по rules.Unclear - тому же
// расчёту, которым Publisher решает это на публикации, а не по самоотчёту
// модели (cs.Gaps может с ним разойтись, и это не повод отказа - см.
// logCoreGaps). Старая строка «Не разобрано» (R4) не должна встречаться.
func assertGapConsistency(t *testing.T, rules Contract, cs *Case, issue Issue) {
	t.Helper()

	wantIncomplete := rules.Unclear(cs.Kind, cs.Filled) != ""
	if cs.Incomplete != wantIncomplete {
		t.Errorf("cs.incomplete=%t, по открытому ядру (Unclear) ожидалось %t", cs.Incomplete, wantIncomplete)
	}
	hasLabel := slices.Contains(issue.LabelNames(), "incomplete")
	if hasLabel != wantIncomplete {
		t.Errorf("метка incomplete=%t, ожидалась %t", hasLabel, wantIncomplete)
	}
	hasLine := strings.Contains(issue.Body, "Не уточнено:")
	if hasLine != wantIncomplete {
		t.Errorf("строка «Не уточнено:» в теле=%t, ожидалась %t", hasLine, wantIncomplete)
	}
	if strings.Contains(issue.Body, "Не разобрано") {
		t.Errorf("тело несёт старую строку «Не разобрано» (R4): %s", issue.Body)
	}
}

// assertTypeLabels - метки типа отражают cs.Kind, каким бы он ни оказался
// (выбор модели логируется отдельно, не здесь): mixed несёт обе метки, любой
// другой kind - ровно свою и не чужую.
func assertTypeLabels(t *testing.T, cs *Case, issue Issue) {
	t.Helper()

	labels := issue.LabelNames()
	want := typeLabels(cs.Kind)
	for _, w := range want {
		if !slices.Contains(labels, w) {
			t.Errorf("метки issue: %v, ожидалась %q по kind=%q", labels, w, cs.Kind)
		}
	}
	for _, other := range []string{"type:bug", "type:feature", "type:question"} {
		if !slices.Contains(want, other) && slices.Contains(labels, other) {
			t.Errorf("метки issue: %v несёт лишнюю %q при kind=%q", labels, other, cs.Kind)
		}
	}
}

// assertIdeasKept - «Ни одна идея не выбрасывается» (architecture.md):
// ключевая фраза каждой мысли, которую тест отправил как автор, обязана
// остаться в теле итогового issue - не дословно всей репликой (саммари
// пересказывает своими словами), а этим коротким литеральным фрагментом
// (номер сделки, точная цитата статуса, конкретная деталь), который
// формулировка модели меняет с наименьшей вероятностью.
func assertIdeasKept(t *testing.T, name string, phrases []string, issue Issue) {
	t.Helper()
	for _, phrase := range phrases {
		if !strings.Contains(strings.ToLower(issue.Body), strings.ToLower(phrase)) {
			t.Errorf("%s: идея автора потеряна - %q нет в теле issue", name, phrase)
		}
	}
}

// logCoreGaps - какие пункты ядра модель не закрыла. Это выбор модели, не
// инвариант продукта (см. заголовок файла) - строка в лог, не Errorf.
func logCoreGaps(t *testing.T, name string, rules Contract, cs *Case) {
	t.Helper()
	if missing := rules.Missing(cs.Kind, cs.Filled); len(missing) > 0 {
		t.Logf("%s: отклонение модели от R1/R2 - ядро не закрыто по %v (gaps модели=%v)", name, missing, cs.Gaps)
	}
}

// summaryReadyPayload - то, что кладёт событие summary_ready (interview.go,
// Summarize): sections - число разделов модели (len(out.Sections), до
// вставки Go-разделов ядра и до Publisher.body, который дописывает Кратко,
// Ссылки и Пересечения), body - собранное тело без них же. Оба поля - то, что
// нужно §10: «заголовков модели от 1 до 6, без повторов».
type summaryReadyPayload struct {
	Sections int    `json:"sections"`
	Body     string `json:"body"`
}

func lastSummaryReady(t *testing.T, cases *Cases, caseID string) summaryReadyPayload {
	t.Helper()
	var raw []byte
	err := cases.pool.QueryRow(context.Background(), `
		SELECT payload FROM case_events
		WHERE case_id = $1 AND kind = 'summary_ready'
		ORDER BY id DESC LIMIT 1`, caseID).Scan(&raw)
	if err != nil {
		t.Fatalf("read summary_ready of case %s: %v", caseID, err)
	}
	var p summaryReadyPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode summary_ready of case %s: %v", caseID, err)
	}
	return p
}

var liveHeadingRe = regexp.MustCompile(`(?m)^## (.+)$`)

// assertHeadings - §10 плана: заголовков модели от 1 до 6, без повторов.
// Читает последнее summary_ready - его body ещё без Кратко/Ссылок/
// Пересечений (их дописывает только Publisher.body при публикации), поэтому
// исключать их из подсчёта отдельно не нужно.
func assertHeadings(t *testing.T, cases *Cases, caseID string) {
	t.Helper()

	p := lastSummaryReady(t, cases, caseID)
	if p.Sections < 1 || p.Sections > 6 {
		t.Errorf("разделов модели: %d, ожидалось от 1 до 6", p.Sections)
	}
	seen := map[string]bool{}
	for _, m := range liveHeadingRe.FindAllStringSubmatch(p.Body, -1) {
		heading := strings.TrimSpace(m[1])
		if seen[heading] {
			t.Errorf("заголовок %q повторяется в теле саммари", heading)
		}
		seen[heading] = true
	}
}

// closeSandboxIssue - уборка §8 плана для S1, S2, S5: тикет в песочнице не
// нужен после прогона. Отказ не валит тест - тикет можно закрыть руками.
func closeSandboxIssue(t *testing.T, env *liveEnv, number int) {
	t.Helper()
	if err := env.gh.CloseIssue(context.Background(), env.project, number); err != nil {
		t.Logf("не удалось закрыть issue #%d в песочнице: %v (закрыть вручную)", number, err)
	}
}

func eventCounts(t *testing.T, cases *Cases, caseID string) string {
	t.Helper()
	kinds := []string{"round_asked", "questions_skipped", "answer_given", "summary_ready", "published"}
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = k + "=" + strconv.Itoa(countEventKind(t, cases, caseID, k))
	}
	return strings.Join(parts, ",")
}

// runBugScenario - S1, R2 баг: материал уже закрывает case и wrong. Раунда
// может и не быть (round_asked=0 - happy path R2), а может, ниже не
// проверяется (см. заголовок файла) - если раунд случился, отвечаем теми же
// фактами и идём дальше.
func runBugScenario(t *testing.T, env *liveEnv) {
	const name = "S1"
	caseID := startCase(t, env)
	cs := sendMaterial(t, env, caseID, []string{
		"https://crm.example.com/deal/59767187",
		"Сделка 59767187 закрылась статусом «Дублем», хотя должна была перейти " +
			"в «Встреча назначена». Так было только вчера, один раз - больше нигде не повторялось.",
	})
	prevScreen := cs.Screen
	finishCollect(t, env)

	facts := map[string]string{
		"case":  "Сделка 59767187, вчера",
		"wrong": "Закрылась статусом «Дублем», хотя должна была перейти в «Встреча назначена»",
	}
	cs, _, _ = driveToSummary(t, env, name, caseID, prevScreen, facts)
	logCoreGaps(t, name, env.rules, cs)
	assertHeadings(t, env.cases, caseID)

	cs = publish(t, env, caseID, cs.Screen, true)

	issue := fetchIssue(t, env, cs)
	assertTypeLabels(t, cs, issue)
	assertGapConsistency(t, env.rules, cs, issue)
	assertIdeasKept(t, name, []string{"59767187", "Дублем"}, issue)

	t.Logf("%s: issue=#%d kind=%s gaps=%v labels=%v events(%s) failed=%t",
		name, cs.IssueNumber, cs.Kind, cs.Gaps, issue.LabelNames(), eventCounts(t, env.cases, caseID), t.Failed())
}

// runFeatureScenario - S2, R2 пожелание. Раундов не бывает больше 2
// (INTERVIEW_ROUNDS, config.go) - продукт сам форсирует саммари; сколько их
// было и какие ключи спросила модель - в t.Logf. Устаревшая кнопка «Отправить
// как есть» на уже отвеченном раунде - продуктовый инвариант, проверяется
// независимо от того, что именно спросила модель.
func runFeatureScenario(t *testing.T, env *liveEnv) {
	const name = "S2"
	caseID := startCase(t, env)
	cs := sendMaterial(t, env, caseID, []string{
		"Руками отказываю лидам не под портрет, например финансовый аутсорсинг. " +
			"Хочу, чтобы бот сам отказывал таким по скрипту и ставил сделке \"нерелевантен лид\"",
	})
	prevScreen := cs.Screen
	finishCollect(t, env)

	facts := map[string]string{
		"need":   "Бот сам отказывает лидам вне портрета по скрипту и ставит сделке статус «нерелевантен лид»",
		"why":    "Сейчас отказывают руками",
		"detail": "Признак - деятельность вне маркетинговых агентств и студий разработки",
	}
	cs, roundScreen, round := driveToSummary(t, env, name, caseID, prevScreen, facts)

	ideas := []string{"нерелевантен лид"}
	if roundScreen != 0 {
		ideas = append(ideas, "маркетинговых агентств")
		// Р-15/правило 3 §2.1: кнопка «Отправить как есть» на уже отвеченном
		// раунде - устаревшая, без нового события пропуска.
		before := countEventKind(t, env.cases, caseID, "questions_skipped")
		pressSkip(t, env, roundScreen, round, "Этот экран устарел")
		if after := countEventKind(t, env.cases, caseID, "questions_skipped"); after != before {
			t.Errorf("%s: skip на отвеченном раунде завёл событие пропуска: было %d, стало %d", name, before, after)
		}
	} else {
		t.Logf("%s: раунда не было - модель закрыла ядро сразу материалом", name)
	}
	logCoreGaps(t, name, env.rules, cs)
	assertHeadings(t, env.cases, caseID)

	cs = publish(t, env, caseID, cs.Screen, true)

	issue := fetchIssue(t, env, cs)
	assertTypeLabels(t, cs, issue)
	assertGapConsistency(t, env.rules, cs, issue)
	assertIdeasKept(t, name, ideas, issue)

	t.Logf("%s: issue=#%d kind=%s gaps=%v labels=%v events(%s) failed=%t",
		name, cs.IssueNumber, cs.Kind, cs.Gaps, issue.LabelNames(), eventCounts(t, env.cases, caseID), t.Failed())
}

// mixedR1Text - дословный пример R1 из §10 глобальной спеки.
const mixedR1Text = "Напоминание пришло с неверным склонением имени, и заодно " +
	"пусть напоминание уходит за час, а не за день."

// mixedR1Facts - заготовки на case, wrong и why, которых материалу не
// хватает (§4 плана): need закрывается материалом («пусть напоминание уходит
// за час») без отдельного вопроса.
var mixedR1Facts = map[string]string{
	"case":  "Вчера, напоминание о встрече",
	"wrong": `Имя пришло как "Анны" вместо "Анна"`,
	"why":   "Чтобы клиент не забыл: за день забывают",
}

// mixedSecondMaterial - S4, вторая смесь для гейта B: тот же случай, что и
// mixedR1Text, но продиктованный (не переписанный дословно), плюс отдельная
// вторая идея без своего пункта ядра - проверяет, что бот не роняет её
// (инвариант «Ни одна идея не выбрасывается»), а не собирает ответ по ней.
var mixedSecondMaterial = []string{
	"Слушай, вот что было: напоминание пришло с неверным склонением имени, " +
		"спутало Анну с Анной, и вдобавок неплохо бы, чтобы оно уходило за час " +
		"до встречи, а не за день, как сейчас.",
	"И заодно пусть бот при закрытии сделки пишет причину комментарием в сделку.",
}

// mixedScenario - вход S3/S4: материал, факты на случай раунда, ключевые
// фразы материала (проверяются всегда) и фактов (только если раунд
// действительно случился и факты ушли автору), и остаётся ли issue открытым
// для гейта B.
type mixedScenario struct {
	name          string
	material      []string
	facts         map[string]string
	materialIdeas []string
	factIdeas     []string
	closeAfter    bool
}

// runMixedScenario - S3 и S4: смесь бага и пожелания, issue с двумя метками
// типа. Issue остаётся открытым - его читает владелец на гейте B (§7 плана,
// не автоматизируется).
func runMixedScenario(t *testing.T, env *liveEnv, sc mixedScenario) {
	caseID := startCase(t, env)
	cs := sendMaterial(t, env, caseID, sc.material)
	prevScreen := cs.Screen
	finishCollect(t, env)

	cs, roundScreen, _ := driveToSummary(t, env, sc.name, caseID, prevScreen, sc.facts)
	ideas := slices.Clone(sc.materialIdeas)
	if roundScreen != 0 {
		ideas = append(ideas, sc.factIdeas...)
	} else {
		t.Logf("%s: раунда не было - материал закрыл ядро сразу", sc.name)
	}
	logCoreGaps(t, sc.name, env.rules, cs)
	assertHeadings(t, env.cases, caseID)

	cs = publish(t, env, caseID, cs.Screen, sc.closeAfter)

	issue := fetchIssue(t, env, cs)
	assertTypeLabels(t, cs, issue)
	assertGapConsistency(t, env.rules, cs, issue)
	assertIdeasKept(t, sc.name, ideas, issue)

	tag := ""
	if !sc.closeAfter {
		tag = "ГЕЙТ B - "
	}
	t.Logf("%s: %sissue=#%d %s kind=%s gaps=%v labels=%v events(%s) failed=%t",
		sc.name, tag, cs.IssueNumber, issue.HTMLURL, cs.Kind, cs.Gaps, issue.LabelNames(),
		eventCounts(t, env.cases, caseID), t.Failed())
}

// runSkipScenario - S5, R5 пропуск. Раунд нужен, чтобы вообще было что
// пропускать кнопкой «Отправить как есть»: если модель ушла в саммари без
// раунда, пропуск проверить не на чем - это логируется, а не отказ, и
// сценарий идёт к публикации сразу. Если раунд был, инварианты продукта:
// повтор той же кнопки на уже неживом экране - «Этот экран устарел», а
// правка текстом после пропуска пересобирает саммари без нового раунда
// (Р-15) - раунда не добавляет.
func runSkipScenario(t *testing.T, env *liveEnv) {
	const name = "S5"
	caseID := startCase(t, env)
	cs := sendMaterial(t, env, caseID, []string{"Напоминания опять приходят неправильно"})
	prevScreen := cs.Screen
	finishCollect(t, env)

	cs, roundOccurred := waitForRoundOrSummary(t, env, caseID, 0, prevScreen)
	ideas := []string{"приходят неправильно"}

	if roundOccurred {
		questions := mustQuestions(t, env.cases, caseID)
		t.Logf("%s: раунд %d, вопросы модели %v", name, cs.Round, questionKeys(questions))
		roundScreen, round := cs.Screen, cs.Round

		pressSkip(t, env, roundScreen, round, "Принято")
		cs = waitForCase(t, env, caseID, roundScreen, func(cs *Case) bool { return cs.Status == statusSummary })
		if n := countEventKind(t, env.cases, caseID, "questions_skipped"); n != 1 {
			t.Errorf("%s: событий пропуска: %d, ожидалась 1", name, n)
		}
		summaryScreen := cs.Screen

		// Повтор той же кнопки на уже неживом (раундовом) экране - устарел.
		pressSkip(t, env, roundScreen, round, "Этот экран устарел")
		if n := countEventKind(t, env.cases, caseID, "questions_skipped"); n != 1 {
			t.Errorf("%s: повторный пропуск завёл второе событие: событий %d, ожидалась 1", name, n)
		}

		must(t, env.b.onItem(textCtx(env.tb, liveAuthorID, "Речь про напоминание о встрече за день")), "onItem fix")
		cs = waitForCase(t, env, caseID, summaryScreen, func(cs *Case) bool { return cs.Status == statusSummary })
		if n := countEventKind(t, env.cases, caseID, "round_asked"); n != 1 {
			t.Errorf("%s: правка после пропуска открыла новый раунд: событий round_asked %d, ожидалась 1", name, n)
		}
		ideas = append(ideas, "встрече за день")
	} else {
		t.Logf("%s: раунда не было - пропуск проверить не на чем, модель ушла сразу в саммари", name)
	}
	logCoreGaps(t, name, env.rules, cs)
	assertHeadings(t, env.cases, caseID)

	cs = publish(t, env, caseID, cs.Screen, true)

	issue := fetchIssue(t, env, cs)
	assertTypeLabels(t, cs, issue)
	assertGapConsistency(t, env.rules, cs, issue)
	assertIdeasKept(t, name, ideas, issue)

	t.Logf("%s: issue=#%d kind=%s gaps=%v labels=%v events(%s) failed=%t",
		name, cs.IssueNumber, cs.Kind, cs.Gaps, issue.LabelNames(), eventCounts(t, env.cases, caseID), t.Failed())
}

func mustQuestions(t *testing.T, cases *Cases, caseID string) []Question {
	t.Helper()
	questions, err := cases.lastQuestions(context.Background(), caseID)
	if err != nil {
		t.Fatalf("read last round questions of case %s: %v", caseID, err)
	}
	return questions
}

// questionKeys - ключи вопросов раунда для лога: разбор живого прогона хочет
// видеть, что именно спросила модель, а сценарии саму формулировку ключа не
// проверяют (недетерминировано).
func questionKeys(questions []Question) []string {
	keys := make([]string, len(questions))
	for i, q := range questions {
		keys[i] = q.Key
	}
	return keys
}
