//go:build eval

package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Прогон набора реальных обращений через текущие промты: M1-M4 по спеке
// ticket-form, Р-9. Запуск - make eval, набор выгружает safe-ssh.sh eval-export.

const (
	// Корень репозитория: go test запускает тест из каталога пакета.
	evalDir = "../../../eval"
	// Раунды по Р-9 фиксированы: база и замер обязаны идти при одном пределе,
	// какой бы INTERVIEW_ROUNDS ни стоял в окружении.
	evalRounds = 2
	// Ходов интервью на обращение: при двух раундах их три, запас ловит
	// зацикливание, а не обрывает разговор.
	evalTurns = 6
	// Дефолты диалоговой модели из config.go, держать равными ему: сравнению до
	// и после нужна одна модель в обоих прогонах, а не модель прода.
	evalModel     = "deepseek/deepseek-v4-flash-0731"
	evalReasoning = "low"
	// Пауза между попытками шага.
	evalRetryDelay = 5 * time.Second
	// Ответ, когда в журнале исходника на этот раунд ответа нет (Р-12).
	evalNoAnswer = "Не знаю"
)

// evalCase - строка набора в формате safe-ssh.sh eval-export.
type evalCase struct {
	ID       string      `json:"id"`
	Project  string      `json:"project"`
	Context  string      `json:"context"`
	Protocol string      `json:"protocol"`
	Events   []evalEvent `json:"events"`
}

func TestEvalRun(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL is not set: eval truncates the database it runs on")
	}
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Fatal("OPENROUTER_API_KEY is not set")
	}
	runs := evalNumber(t, "EVAL_RUNS", fmt.Sprint(evalBaseRuns))
	parallel := evalNumber(t, "EVAL_PARALLEL", "6")
	model := valueOr(os.Getenv("EVAL_MODEL"), evalModel)
	reasoning, err := parseReasoning(valueOr(os.Getenv("EVAL_REASONING"), evalReasoning))
	if err != nil {
		t.Fatalf("EVAL_REASONING: %v", err)
	}
	out := valueOr(os.Getenv("EVAL_OUT"), time.Now().Format("2006-01-02"))
	baseName := os.Getenv("EVAL_BASE")
	if baseName != "" && filepath.Clean(baseName) == filepath.Clean(out) {
		t.Fatal("EVAL_OUT must differ from EVAL_BASE: the run would overwrite the base it compares to")
	}

	all := loadEvalCases(t)
	// Из «Спросить» модель видела ответ по документации, которого в наборе нет:
	// прогон такого обращения мерил бы другой разговор.
	cases := slices.DeleteFunc(slices.Clone(all), func(c evalCase) bool {
		return slices.ContainsFunc(c.Events, func(e evalEvent) bool { return e.Kind == "switched_to_ticket" })
	})
	t.Logf("cases=%d excluded=%d model=%s reasoning=%s rounds=%d runs=%d parallel=%d",
		len(cases), len(all)-len(cases), model, reasoning, evalRounds, runs, parallel)

	var base evalResult
	haveBase := baseName != ""
	if haveBase {
		base = readEvalResult(t, baseName)
		// До первого вызова модели: дорогой прогон не запускается на сломанную
		// базу или заведомо несравнимый замер (другая модель, раунды, набор).
		if !runsValid(base.Runs) {
			t.Fatalf("EVAL_BASE %s: base broken (want %d runs with failed <= %.0f%%)",
				baseName, evalBaseRuns, evalFailLimit*100)
		}
		ids := make([]string, len(cases))
		for i, c := range cases {
			ids[i] = c.ID
		}
		if err := sameSetup(base, model, reasoning, evalRounds, ids); err != nil {
			t.Fatalf("EVAL_BASE %s: %v", baseName, err)
		}
	}

	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	cfg.MaxConns = int32(parallel) + 2 //nolint:gosec // parallel - малое число из EVAL_PARALLEL
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	upsertEvalProjects(t, pool, cases)

	proxy := os.Getenv("OPENROUTER_PROXY")
	// NewOpenRouter считает адрес уже проверенным и ошибку разбора не видит.
	if err := checkProxy("OPENROUTER_PROXY", proxy); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	llm := newCappedLLM(t, NewOpenRouter(key, model, proxy, log))
	interview := NewInterview(NewCases(pool, nil, log, 30, 0), llm, log, testRules(t),
		DialogModel{Name: model, Reasoning: reasoning}, evalRounds, nil)

	var broken string
	result := evalResult{Model: model, Reasoning: reasoning, Rounds: evalRounds,
		Total: len(all), Excluded: len(all) - len(cases)}
	for n := 1; n <= runs && broken == ""; n++ {
		if _, err := pool.Exec(ctx,
			`TRUNCATE cases, case_items, case_events, jobs, users RESTART IDENTITY CASCADE`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		done := make([]caseRun, len(cases))
		sem := make(chan struct{}, parallel)
		var wg sync.WaitGroup
		for i, c := range cases {
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				done[i] = runEvalCase(ctx, pool, interview, int64(i+1), c)
				t.Logf("run %d case %d/%d %s kind=%s questions=%v incomplete=%t failed=%q",
					n, i+1, len(cases), cutRunes(c.ID, 8), done[i].Kind, done[i].Questions,
					done[i].Incomplete, done[i].Failed)
			})
		}
		wg.Wait()

		run := evalRun{Metrics: measure(done), Kinds: measureKinds(done), Cases: done}
		result.Runs = append(result.Runs, run)
		t.Logf("run %d: %s", n, metricsLine(run.Metrics))
		broken = checkFailed(n, done)
	}
	result.Mean = meanOf(result.Runs)

	t.Logf("mean: %s", metricsLine(result.Mean.Metrics))
	for _, kind := range slices.Sorted(maps.Keys(result.Mean.Kinds)) {
		t.Logf("mean %s: %s", kind, metricsLine(result.Mean.Kinds[kind]))
	}
	// Файл пишется и у сломанного прогона: по нему разбирают, что сломалось.
	writeEvalResult(t, out, result)
	if broken != "" {
		t.Fatal(broken)
	}

	if !haveBase {
		return
	}
	table, reasons := compareRuns(base, result)
	for _, line := range strings.Split(strings.TrimRight(table, "\n"), "\n") {
		t.Log(line)
	}
	if len(reasons) == 0 {
		t.Log("verdict: pass")
		return
	}
	// Порог не взят - код выхода не 0 (docs/specs/ticket-form.md), последняя
	// строка называет причины: t.Fatalf логирует их сам, второй раз не пишем.
	t.Fatalf("verdict: fail: %s", strings.Join(reasons, "; "))
}

func evalNumber(t *testing.T, name, fallback string) int {
	t.Helper()
	n, err := parsePositive(name, valueOr(os.Getenv(name), fallback))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func loadEvalCases(t *testing.T) []evalCase {
	t.Helper()
	f, err := os.Open(filepath.Join(evalDir, "cases.jsonl"))
	if err != nil {
		t.Fatalf("open set: %v (выгрузка: deploy/safe-ssh.sh eval-export > eval/cases.jsonl)", err)
	}
	defer f.Close()

	var cases []evalCase
	scanner := bufio.NewScanner(f)
	// Строка - обращение с протоколом и журналом целиком, это десятки килобайт.
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for line := 1; scanner.Scan(); line++ {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var c evalCase
		if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
			t.Fatalf("cases.jsonl line %d: %v", line, err)
		}
		cases = append(cases, c)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read set: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("cases.jsonl is empty")
	}
	return cases
}

// upsertEvalProjects кладёт проекты набора с их контекстом: контекст входит в
// префикс промта и меняет разговор. Репозиторий - заглушка, GitHub прогон не
// вызывает.
func upsertEvalProjects(t *testing.T, pool *pgxpool.Pool, cases []evalCase) {
	t.Helper()
	for _, c := range cases {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO projects (slug, title, github_owner, github_repo, context)
			VALUES ($1, $1, 'eval', 'eval', $2)
			ON CONFLICT (slug) DO UPDATE SET context = EXCLUDED.context, updated_at = now()`,
			c.Project, c.Context)
		if err != nil {
			t.Fatalf("upsert project %s: %v", c.Project, err)
		}
	}
}

// runEvalCase ведёт одно обращение от протокола до саммари так же, как это
// делают воркер и бот: ход, ответ автора, следующий ход, саммари.
func runEvalCase(ctx context.Context, pool *pgxpool.Pool, iv *Interview, user int64, c evalCase) caseRun {
	run := caseRun{ID: c.ID}
	caseID, err := insertEvalCase(ctx, pool, user, c)
	if err != nil {
		run.Failed = err.Error()
		return run
	}
	payload, err := json.Marshal(casePayload{CaseID: caseID})
	if err != nil {
		run.Failed = err.Error()
		return run
	}
	answers := roundAnswers(c.Events)
	// Написанное до первого раунда модель на проде видела в истории первым же
	// ходом: без него разговор шёл бы по другому сырью.
	if answers[0] != "" {
		cs, err := loadEvalCase(ctx, iv, caseID)
		if err == nil {
			err = iv.cases.AddAnswer(ctx, cs, answers[0])
		}
		if err != nil {
			run.Failed = "answer: " + err.Error()
			return run
		}
	}

	var seen int64
	for range evalTurns {
		attempts, err := evalStep(ctx, iv.Run, Job{Kind: JobInterview, Payload: payload})
		run.Attempts += attempts
		if err != nil {
			run.Failed = "interview: " + err.Error()
			return run
		}
		id, kind, questions, err := lastTurn(ctx, pool, caseID)
		if err != nil {
			run.Failed = err.Error()
			return run
		}
		if id == seen {
			run.Failed = "no progress"
			return run
		}
		seen = id

		cs, err := loadEvalCase(ctx, iv, caseID)
		if err != nil {
			run.Failed = err.Error()
			return run
		}
		if kind == "round_asked" {
			run.Questions = append(run.Questions, questions)
			answer := valueOr(answers[cs.Round], evalNoAnswer)
			if err := iv.cases.AddAnswer(ctx, cs, answer); err != nil {
				run.Failed = "answer: " + err.Error()
				return run
			}
			continue
		}

		attempts, err = evalStep(ctx, iv.Summarize, Job{Kind: JobSummarize, Payload: payload})
		run.Attempts += attempts
		if err != nil {
			run.Failed = "summary: " + err.Error()
			return run
		}
		cs, err = loadEvalCase(ctx, iv, caseID)
		if err != nil {
			run.Failed = err.Error()
			return run
		}
		if cs.Status != statusSummary {
			run.Failed = "no summary"
			return run
		}
		run.Kind, run.Incomplete = cs.Kind, cs.Incomplete
		return run
	}
	run.Failed = fmt.Sprintf("no summary after %d turns", evalTurns)
	return run
}

// loadEvalCase - Load, у которого пропажа обращения тоже ошибка: прогон
// заводит его сам, и nil значит поломку, а не отмену автором.
func loadEvalCase(ctx context.Context, iv *Interview, caseID string) (*Case, error) {
	cs, err := iv.cases.Load(ctx, caseID)
	if err != nil {
		return nil, err
	}
	if cs == nil {
		return nil, fmt.Errorf("case %s is gone", caseID)
	}
	return cs, nil
}

// evalStep - шаг как работа очереди: бюджет воркера на попытку и повтор до
// maxAttempts попыток. На проде таймаут модели и невалидный ответ повторяет
// очередь, и без повтора прогон мерил бы сеть, а не промты. Состояние
// обращения между попытками не трогаем: Run и Summarize сверяют версию сами.
// Возвращает число потраченных попыток - caseRun.Attempts копит их по шагам.
func evalStep(ctx context.Context, step JobHandler, job Job) (int, error) {
	var err error
	for job.Attempts = 1; job.Attempts <= maxAttempts; job.Attempts++ {
		if err = evalAttempt(ctx, step, job); err == nil {
			return job.Attempts, nil
		}
		// Готовой функции отсрочки у воркера нет (она в SQL FailJob), а
		// растущая пауза до 16 с на пять попыток прогону не нужна.
		if job.Attempts < maxAttempts && !wait(ctx, evalRetryDelay) {
			break
		}
	}
	attempts := min(job.Attempts, maxAttempts)
	return attempts, fmt.Errorf("%d attempts, last: %w", attempts, err)
}

func evalAttempt(ctx context.Context, step JobHandler, job Job) error {
	ctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	return step(ctx, job)
}

// insertEvalCase заводит обращение сразу в интервью. Автор свой у каждого
// обращения: активное обращение у автора одно по уникальному индексу.
func insertEvalCase(ctx context.Context, pool *pgxpool.Pool, user int64, c evalCase) (string, error) {
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (telegram_id, first_name, slug) VALUES ($1, 'eval', 'eval')`, user); err != nil {
		return "", fmt.Errorf("insert user: %w", err)
	}
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO cases (user_id, project_id, status, mode, protocol, round)
		VALUES ($1, (SELECT id FROM projects WHERE slug = $2), 'interview', 'ticket', $3, 0)
		RETURNING id`, user, c.Project, c.Protocol).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert case: %w", err)
	}
	return id, nil
}

// lastTurn - последнее событие хода и число вопросов в нём. Ноль в id - хода
// ещё не было.
func lastTurn(ctx context.Context, pool *pgxpool.Pool, caseID string) (int64, string, int, error) {
	var id int64
	var kind string
	var payload []byte
	err := pool.QueryRow(ctx, `
		SELECT id, kind, payload FROM case_events
		WHERE case_id = $1 AND kind IN ('round_asked', 'interview_done')
		ORDER BY id DESC LIMIT 1`, caseID).Scan(&id, &kind, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", 0, nil
	}
	if err != nil {
		return 0, "", 0, fmt.Errorf("last turn: %w", err)
	}
	var p struct {
		Questions []Question `json:"questions"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0, "", 0, fmt.Errorf("decode turn: %w", err)
	}
	return id, kind, len(p.Questions), nil
}

// checkFailed - причина остановить прогоны, когда failed больше предела: такие
// цифры мерят поломку, а не промты. Пусто - прогон годен.
func checkFailed(n int, done []caseRun) string {
	var failed []caseRun
	for _, r := range done {
		if r.Failed != "" {
			failed = append(failed, r)
		}
	}
	if float64(len(failed)) > evalFailLimit*float64(len(done)) {
		return fmt.Sprintf("run %d: failed %d of %d, first: %s: %s",
			n, len(failed), len(done), failed[0].ID, failed[0].Failed)
	}
	return ""
}

func writeEvalResult(t *testing.T, name string, result evalResult) {
	t.Helper()
	dir := filepath.Join(evalDir, "results")
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // каталог результатов eval читается людьми
		t.Fatalf("create results dir: %v", err)
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil { //nolint:gosec // файл результата eval читается людьми
		t.Fatalf("write result: %v", err)
	}
	t.Logf("result: eval/results/%s.json", name)
}

// readEvalResult - база для EVAL_BASE: без файла сравнивать не с чем, и
// прогон замера был бы часом без смысла.
func readEvalResult(t *testing.T, name string) evalResult {
	t.Helper()
	path := filepath.Join(evalDir, "results", name+".json")
	data, err := os.ReadFile(path) //nolint:gosec // путь собран из имени запуска оператора
	if err != nil {
		t.Fatalf("open base %s: %v (снят ли make eval EVAL_OUT=%s?)", path, err, name)
	}
	var result evalResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode base %s: %v", path, err)
	}
	return result
}
