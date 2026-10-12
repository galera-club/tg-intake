package app

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"
)

// caseRun - итог одного обращения в прогоне eval.
type caseRun struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`               // тип после саммари
	Questions  []int  `json:"questions"`          // число вопросов по раундам, пусто - без вопросов
	Incomplete bool   `json:"incomplete"`         // cases.incomplete после Summarize
	Attempts   int    `json:"attempts,omitempty"` // сумма попыток шагов обращения
	Failed     string `json:"failed,omitempty"`
}

// evalRun - метрики и обращения одного прогона.
type evalRun struct {
	Metrics evalMetrics            `json:"metrics"`
	Kinds   map[string]evalMetrics `json:"kinds"`
	Cases   []caseRun              `json:"cases,omitempty"`
}

// evalResult - итог набора прогонов: база или замер (Р-9).
type evalResult struct {
	Model     string    `json:"model"`
	Reasoning string    `json:"reasoning"`
	Rounds    int       `json:"rounds"`
	Total     int       `json:"total"`
	Excluded  int       `json:"excluded"`
	Runs      []evalRun `json:"runs"`
	Mean      evalRun   `json:"mean"`
}

// evalEvent - событие журнала исходника в наборе eval.
type evalEvent struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// evalBaseRuns - прогонов базы и финала (Р-9): меньше не даёт устойчивого
// среднего, больше тратит ключ без выигрыша в точности.
const evalBaseRuns = 3

// evalFailLimit - доля failed, выше которой прогон не мерит промты, а
// упирается в поломку. Общая для проверки одного прогона (checkFailed, тег
// eval) и для оценки готовности базы (runsValid, здесь). База среза 1 дала
// 13% (7 из 54, отказы модели и сети после maxAttempts) - честное качество
// старого кода, а не поломка; 10% резало бы годную базу.
const evalFailLimit = 0.2

// evalEps - допуск сравнения средних метрик с порогом: защита от шума float,
// не от реальной разницы (Р-9, сценарий на границе).
const evalEps = 1e-9

// roundAnswers - ответы автора исходника по номеру раунда до первого саммари:
// правки саммари отвечают на другое и в ответ раунда не попадают (Р-12).
// Раунд 0 - написанное до первого вопроса.
func roundAnswers(events []evalEvent) map[int]string {
	answers := map[int]string{}
	for _, e := range events {
		if e.Kind == "interview_done" || e.Kind == "summary_ready" {
			break
		}
		if e.Kind != "answer_given" {
			continue
		}
		var p struct {
			Round int    `json:"round"`
			Text  string `json:"text"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || strings.TrimSpace(p.Text) == "" {
			continue
		}
		if answers[p.Round] != "" {
			answers[p.Round] += "\n"
		}
		answers[p.Round] += p.Text
	}
	return answers
}

// evalMetrics - M1-M4 по спеке ticket-form, Р-9. Знаменатели лежат рядом:
// доля от пяти обращений и доля от пятидесяти сравниваются по-разному.
type evalMetrics struct {
	M1     float64 `json:"m1"` // доля обращений без вопросов
	M2     float64 `json:"m2"` // среднее число раундов до саммари
	M3     float64 `json:"m3"` // доля bug и mixed с закрытым ядром
	M4     float64 `json:"m4"` // среднее число вопросов в раунде 1
	Cases  int     `json:"cases"`
	Bugs   int     `json:"bugs"`   // знаменатель M3
	Asked  int     `json:"asked"`  // знаменатель M4
	Failed int     `json:"failed"` // в метрики не входят
}

func measure(runs []caseRun) evalMetrics {
	var m evalMetrics
	var silent, rounds, complete, firstRound int
	for _, r := range runs {
		if r.Failed != "" {
			m.Failed++
			continue
		}
		m.Cases++
		rounds += len(r.Questions)
		if len(r.Questions) == 0 {
			silent++
		} else {
			m.Asked++
			firstRound += r.Questions[0]
		}
		if r.Kind == "bug" || r.Kind == "mixed" {
			m.Bugs++
			if !r.Incomplete {
				complete++
			}
		}
	}
	m.M1 = share(silent, m.Cases)
	m.M2 = share(rounds, m.Cases)
	m.M3 = share(complete, m.Bugs)
	m.M4 = share(firstRound, m.Asked)
	return m
}

// share - частное с нулём на пустом знаменателе: JSON не умеет NaN, а пустой
// знаменатель виден рядом в самих метриках.
func share(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

func metricsLine(m evalMetrics) string {
	return fmt.Sprintf("M1=%.3f M2=%.3f M3=%.3f (of %d) M4=%.3f (of %d) cases=%d failed=%d",
		m.M1, m.M2, m.M3, m.Bugs, m.M4, m.Asked, m.Cases, m.Failed)
}

// failedShare - доля failed от всех обращений прогона. Обе стороны могут
// уложиться в evalFailLimit порознь и всё равно разъехаться: замер не должен
// разваливаться относительно базы больше чем на 5 п.п. (решение диспетчера
// после базового прогона, docs/specs/ticket-form.md).
func failedShare(m evalMetrics) float64 {
	return share(m.Failed, m.Cases+m.Failed)
}

// runsValid - прогонов evalBaseRuns и в каждом failed не больше
// evalFailLimit: сравнение с шумной или недособранной базой хуже отказа от
// сравнения.
func runsValid(runs []evalRun) bool {
	if len(runs) != evalBaseRuns {
		return false
	}
	for _, r := range runs {
		total := r.Metrics.Cases + r.Metrics.Failed
		if total == 0 || float64(r.Metrics.Failed) > evalFailLimit*float64(total) {
			return false
		}
	}
	return true
}

// sameSetup - модель, reasoning, раунды и набор ID обращений совпадают с
// базой: проверка до хода модели, чтобы не тратить дорогой прогон на
// несравнимый замер.
func sameSetup(base evalResult, model, reasoning string, rounds int, ids []string) error {
	if base.Model != model || base.Reasoning != reasoning {
		return fmt.Errorf("base is %s/%s, got %s/%s", base.Model, base.Reasoning, model, reasoning)
	}
	if base.Rounds != rounds {
		return fmt.Errorf("base rounds=%d, got %d", base.Rounds, rounds)
	}
	if len(base.Runs) == 0 {
		return fmt.Errorf("base has no runs")
	}
	want := caseIDs(base.Runs)
	got := slices.Clone(ids)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		return fmt.Errorf("case set differs from base: %d ids in base, %d given", len(want), len(got))
	}
	return nil
}

// caseIDs - набор ID обращений первого прогона: набор входа не меняется между
// прогонами, только их исход (failed, вопросы).
func caseIDs(runs []evalRun) []string {
	if len(runs) == 0 {
		return nil
	}
	ids := make([]string, len(runs[0].Cases))
	for i, c := range runs[0].Cases {
		ids[i] = c.ID
	}
	return ids
}

// compareRuns - таблица база/замер и вердикт по порогу Р-9. Вызывающий уже
// отсеял сломанную базу (runsValid, до хода модели): здесь она годна. reasons
// пуст - pass; иначе каждая причина - непройденная проверка (docs/specs/ticket-form.md).
func compareRuns(base, after evalResult) (string, []string) {
	var b strings.Builder
	var reasons []string

	if len(after.Runs) != evalBaseRuns {
		reasons = append(reasons, fmt.Sprintf("runs %d of %d", len(after.Runs), evalBaseRuns))
	}

	bm, am := base.Mean.Metrics, after.Mean.Metrics
	noBugs := bm.Bugs == 0 || am.Bugs == 0
	m3ok, m3rule := am.M3 >= bm.M3-0.05-evalEps, ">= base-0.05"
	if noBugs {
		m3ok, m3rule = false, "n/a"
		reasons = append(reasons, "M3: no bug cases")
	}
	rows := []struct {
		name string
		ok   bool
		rule string
		get  func(evalMetrics) float64
	}{
		{"M1", am.M1 >= bm.M1+0.10-evalEps, ">= base+0.10", func(m evalMetrics) float64 { return m.M1 }},
		{"M2", am.M2 <= bm.M2+0.10+evalEps, "<= base+0.10", func(m evalMetrics) float64 { return m.M2 }},
		{"M3", m3ok, m3rule, func(m evalMetrics) float64 { return m.M3 }},
		{"M4", am.M4 < bm.M4-evalEps, "< base", func(m evalMetrics) float64 { return m.M4 }},
		{"failed", failedShare(am) <= failedShare(bm)+0.05+evalEps, "<= base+0.05", failedShare},
	}
	for _, row := range rows {
		switch {
		case row.name == "M3" && noBugs:
			// причина уже добавлена одной строкой выше.
		case !row.ok:
			reasons = append(reasons, fmt.Sprintf("%s: %.3f vs base %.3f (need %s)",
				row.name, row.get(am), row.get(bm), row.rule))
		}
		baseMin, baseMax := minMax(base.Runs, row.get)
		afterMin, afterMax := minMax(after.Runs, row.get)
		status := "ok"
		if !row.ok {
			status = "miss"
		}
		fmt.Fprintf(&b, "%-2s base=%.3f [%.3f-%.3f] after=%.3f [%.3f-%.3f] diff=%+.3f rule %s: %s\n",
			row.name, row.get(bm), baseMin, baseMax, row.get(am), afterMin, afterMax,
			row.get(am)-row.get(bm), row.rule, status)
	}

	b.WriteString("-- reference, not part of the verdict --\n")
	writeKindRows(&b, base, after)
	m3, ids := m3Intersection(base, after)
	fmt.Fprintf(&b, "M3 bug(base)->bug/mixed(after): %.3f over %d ids\n", m3, ids)
	fmt.Fprintf(&b, "attempts mean: base=- after=%.3f\n", meanAttempts(after))
	fmt.Fprintf(&b, "asked in base, silent in after: %s\n", joinIDs(silentInAfter(base, after)))
	fmt.Fprintf(&b, "failed on one side only: %s\n", joinIDs(failedOnlyOneSide(base, after)))

	return b.String(), reasons
}

func joinIDs(ids []string) string {
	if len(ids) == 0 {
		return "-"
	}
	return strings.Join(ids, ", ")
}

// minMax - разброс метрики по прогонам (max-min по трём прогонам, Р-9).
func minMax(runs []evalRun, get func(evalMetrics) float64) (float64, float64) {
	if len(runs) == 0 {
		return 0, 0
	}
	lo, hi := get(runs[0].Metrics), get(runs[0].Metrics)
	for _, r := range runs[1:] {
		v := get(r.Metrics)
		lo = min(lo, v)
		hi = max(hi, v)
	}
	return lo, hi
}

// writeKindRows - M1-M4 по типам, тип без базы отмечен «-» в её колонке.
func writeKindRows(b *strings.Builder, base, after evalResult) {
	kinds := map[string]bool{}
	for k := range base.Mean.Kinds {
		kinds[k] = true
	}
	for k := range after.Mean.Kinds {
		kinds[k] = true
	}
	for _, kind := range slices.Sorted(maps.Keys(kinds)) {
		baseLine, afterLine := "-", "-"
		if m, ok := base.Mean.Kinds[kind]; ok {
			baseLine = metricsLine(m)
		}
		if m, ok := after.Mean.Kinds[kind]; ok {
			afterLine = metricsLine(m)
		}
		fmt.Fprintf(b, "type=%s base=%s after=%s\n", kind, baseLine, afterLine)
	}
}

// majorityKind - тип обращения по большинству прогонов, где оно не упало:
// один прогон может ошибиться с типом, большинство - нет.
func majorityKind(runs []evalRun) map[string]string {
	votes := map[string]map[string]int{}
	order := map[string][]string{}
	for _, r := range runs {
		for _, c := range r.Cases {
			if c.Failed != "" || c.Kind == "" {
				continue
			}
			if votes[c.ID] == nil {
				votes[c.ID] = map[string]int{}
			}
			if votes[c.ID][c.Kind] == 0 {
				order[c.ID] = append(order[c.ID], c.Kind)
			}
			votes[c.ID][c.Kind]++
		}
	}
	out := make(map[string]string, len(votes))
	for id, counts := range votes {
		best, bestN := "", 0
		for _, k := range order[id] {
			if counts[k] > bestN {
				best, bestN = k, counts[k]
			}
		}
		out[id] = best
	}
	return out
}

// m3Intersection - M3 на пересечении: ID с типом bug в базе и bug/mixed в
// замере, доля закрытого ядра по всем прогонам замера этих ID. Ломается
// знаменатель M3 у обычного расчёта, когда часть bug/feature уходит в mixed -
// это подмножество остаётся сравнимым (docs/specs/ticket-form.md).
func m3Intersection(base, after evalResult) (float64, int) {
	baseKind := majorityKind(base.Runs)
	afterKind := majorityKind(after.Runs)
	var ids []string
	for id, bk := range baseKind {
		if bk != "bug" {
			continue
		}
		if ak := afterKind[id]; ak == "bug" || ak == "mixed" {
			ids = append(ids, id)
		}
	}
	var closed, total int
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	for _, r := range after.Runs {
		for _, c := range r.Cases {
			if c.Failed != "" || !idSet[c.ID] {
				continue
			}
			total++
			if !c.Incomplete {
				closed++
			}
		}
	}
	return share(closed, total), len(ids)
}

// meanAttempts - среднее Attempts на обращение замера. У базы поля нет
// (записана срезом 1): колонка базы - «-» безусловно, а не 0 (plan §4).
func meanAttempts(after evalResult) float64 {
	var sum, n int
	for _, r := range after.Runs {
		for _, c := range r.Cases {
			if c.Failed == "" {
				sum += c.Attempts
				n++
			}
		}
	}
	return share(sum, n)
}

// majorityBy - результат предиката yes по большинству прогонов для ID среди
// записей, прошедших include; seen ложно, если include не подошёл ни разу.
func majorityBy(runs []evalRun, id string, include, yes func(caseRun) bool) (majority, seen bool) {
	var y, n int
	for _, r := range runs {
		for _, c := range r.Cases {
			if c.ID != id || !include(c) {
				continue
			}
			seen = true
			if yes(c) {
				y++
			} else {
				n++
			}
		}
	}
	return y > n, seen
}

func caseNotFailed(c caseRun) bool { return c.Failed == "" }
func caseAsked(c caseRun) bool     { return len(c.Questions) > 0 }
func caseAny(caseRun) bool         { return true }
func caseFailed(c caseRun) bool    { return c.Failed != "" }

// silentInAfter - ID, у которых база большинством прогонов спрашивала, а
// замер большинством прогонов молчит.
func silentInAfter(base, after evalResult) []string {
	var ids []string
	for _, id := range caseIDs(base.Runs) {
		baseAsked, baseSeen := majorityBy(base.Runs, id, caseNotFailed, caseAsked)
		afterAsked, afterSeen := majorityBy(after.Runs, id, caseNotFailed, caseAsked)
		if baseSeen && afterSeen && baseAsked && !afterAsked {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// failedOnlyOneSide - ID, упавшие большинством прогонов только у базы или
// только у замера.
func failedOnlyOneSide(base, after evalResult) []string {
	var ids []string
	for _, id := range caseIDs(base.Runs) {
		bf, _ := majorityBy(base.Runs, id, caseAny, caseFailed)
		af, _ := majorityBy(after.Runs, id, caseAny, caseFailed)
		if bf != af {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// measureKinds - метрики отдельно по типу обращения.
func measureKinds(runs []caseRun) map[string]evalMetrics {
	byKind := map[string][]caseRun{}
	for _, r := range runs {
		if r.Failed == "" {
			byKind[r.Kind] = append(byKind[r.Kind], r)
		}
	}
	out := make(map[string]evalMetrics, len(byKind))
	for kind, list := range byKind {
		out[kind] = measure(list)
	}
	return out
}

// meanOf - среднее метрик по прогонам (Р-9 сравнивает средние), по типу - по
// прогонам, где тип встретился. Счётчики - суммы по прогонам.
func meanOf(runs []evalRun) evalRun {
	all := make([]evalMetrics, 0, len(runs))
	byKind := map[string][]evalMetrics{}
	for _, r := range runs {
		all = append(all, r.Metrics)
		for kind, m := range r.Kinds {
			byKind[kind] = append(byKind[kind], m)
		}
	}
	mean := evalRun{Metrics: average(all), Kinds: map[string]evalMetrics{}}
	for kind, list := range byKind {
		mean.Kinds[kind] = average(list)
	}
	return mean
}

func average(list []evalMetrics) evalMetrics {
	var sum evalMetrics
	for _, m := range list {
		sum.M1 += m.M1
		sum.M2 += m.M2
		sum.M3 += m.M3
		sum.M4 += m.M4
		sum.Cases += m.Cases
		sum.Bugs += m.Bugs
		sum.Asked += m.Asked
		sum.Failed += m.Failed
	}
	n := float64(len(list))
	sum.M1, sum.M2, sum.M3, sum.M4 = sum.M1/n, sum.M2/n, sum.M3/n, sum.M4/n
	return sum
}

// TestEvalMetrics: обращения без failed - без вопросов, два раунда, bug с
// неполным ядром; failed в знаменатели не входит.
func TestEvalMetrics(t *testing.T) {
	got := measure([]caseRun{
		{ID: "silent", Kind: "feature"},
		{ID: "two-rounds", Kind: "bug", Questions: []int{3, 1}},
		{ID: "incomplete", Kind: "bug", Questions: []int{2}, Incomplete: true},
		{ID: "broken", Kind: "bug", Questions: []int{3, 3}, Failed: "no progress"},
	})
	want := evalMetrics{
		M1: 1.0 / 3, M2: 1, M3: 0.5, M4: 2.5,
		Cases: 3, Bugs: 2, Asked: 2, Failed: 1,
	}
	const eps = 1e-9
	if math.Abs(got.M1-want.M1) > eps || math.Abs(got.M2-want.M2) > eps ||
		math.Abs(got.M3-want.M3) > eps || math.Abs(got.M4-want.M4) > eps ||
		got.Cases != want.Cases || got.Bugs != want.Bugs || got.Asked != want.Asked || got.Failed != want.Failed {
		t.Fatalf("measure:\n got %+v\nwant %+v", got, want)
	}
}

// TestRoundAnswers: ответ раунда по журналу исходника (Р-12).
func TestRoundAnswers(t *testing.T) {
	answer := func(round int, text string) evalEvent {
		payload, _ := json.Marshal(map[string]any{"round": round, "text": text})
		return evalEvent{Kind: "answer_given", Payload: payload}
	}
	asked := evalEvent{Kind: "round_asked", Payload: json.RawMessage(`{"round":1}`)}
	tests := []struct {
		name   string
		events []evalEvent
		want   map[int]string
	}{
		{"fix after summary is dropped",
			[]evalEvent{asked, answer(1, "a"), {Kind: "summary_ready"}, answer(1, "fix")},
			map[int]string{1: "a"}},
		{"fix after interview done is dropped",
			[]evalEvent{asked, answer(1, "a"), {Kind: "interview_done"}, answer(1, "fix")},
			map[int]string{1: "a"}},
		{"two answers of a round are joined",
			[]evalEvent{asked, answer(1, "a"), answer(1, "b")},
			map[int]string{1: "a\nb"}},
		{"empty text is skipped",
			[]evalEvent{asked, answer(1, " "), answer(1, "a")},
			map[int]string{1: "a"}},
		{"round zero is collected",
			[]evalEvent{answer(0, "before"), asked, answer(1, "a")},
			map[int]string{0: "before", 1: "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := roundAnswers(tt.events); !maps.Equal(got, tt.want) {
				t.Fatalf("roundAnswers = %v, want %v", got, tt.want)
			}
		})
	}
}

// evalCaseFixture - валидный прогон из трёх обращений для сборки evalResult в
// тестах compareRuns: один silent, один bug с закрытым ядром, один mixed.
func evalCaseFixture() []caseRun {
	return []caseRun{
		{ID: "c1", Kind: "feature"},
		{ID: "c2", Kind: "bug", Questions: []int{2}},
		{ID: "c3", Kind: "mixed", Questions: []int{1}},
	}
}

// evalResultFixture - evalResult из evalBaseRuns одинаковых валидных
// прогонов: удобная база для сценариев, которые правят одну метрику.
func evalResultFixture(model, reasoning string, rounds int) evalResult {
	runs := make([]evalRun, evalBaseRuns)
	for i := range runs {
		cases := evalCaseFixture()
		runs[i] = evalRun{Metrics: measure(cases), Kinds: measureKinds(cases), Cases: cases}
	}
	res := evalResult{Model: model, Reasoning: reasoning, Rounds: rounds, Total: 3, Runs: runs}
	res.Mean = meanOf(res.Runs)
	return res
}

func hasReason(reasons []string, substr string) bool {
	return slices.ContainsFunc(reasons, func(r string) bool { return strings.Contains(r, substr) })
}

// TestEvalThreshold - сценарии проверки eval-compare, docs/specs/ticket-form.md.
func TestEvalThreshold(t *testing.T) {
	t.Run("boundaries pass exactly at the threshold (сценарий 1)", func(t *testing.T) {
		base := evalResultFixture("m", "low", 2)
		// Литералы, не арифметика правила: 0.1+0.2 != 0.3 в float64, и ровно на
		// этих парах база+порог и буквальная граница расходятся на пару ULP.
		// Удаление evalEps превращает все четыре ok в miss - тест это ловит.
		// Failed/Cases 12/88 и 17/83 дают ту же ULP-границу для доли failed
		// (12/100=0.12, 17/100=0.17=база+0.05 с плавающей неточностью).
		base.Mean.Metrics = evalMetrics{M1: 0.2, M2: 0.7, M3: 0.34, M4: 0.3, Bugs: 10, Cases: 88, Failed: 12}
		after := base
		after.Mean.Metrics = evalMetrics{M1: 0.3, M2: 0.8, M3: 0.29, M4: 0.299, Bugs: 10, Cases: 83, Failed: 17}
		_, reasons := compareRuns(base, after)
		if len(reasons) != 0 {
			t.Fatalf("want pass, got reasons: %v", reasons)
		}
	})

	t.Run("one metric off at a time names only that metric (сценарий 2)", func(t *testing.T) {
		base := evalResultFixture("m", "low", 2)
		// passing - все четыре метрики с запасом внутри порога: подмена ровно
		// одной ниже переводит в miss только её.
		passing := evalMetrics{
			M1: base.Mean.Metrics.M1 + 0.20, M2: base.Mean.Metrics.M2, M3: base.Mean.Metrics.M3,
			M4: base.Mean.Metrics.M4 - 0.20, Bugs: base.Mean.Metrics.Bugs,
			// База без failed (фикстура их не сеет): 2 из 100 - доля 0.02,
			// с запасом под порог база+0.05.
			Cases: 98, Failed: 2,
		}
		m4 := passing
		m4.M4 = base.Mean.Metrics.M4
		m1 := passing
		m1.M1 = base.Mean.Metrics.M1 + 0.09
		m2 := passing
		m2.M2 = base.Mean.Metrics.M2 + 0.11
		m3 := passing
		m3.M3 = base.Mean.Metrics.M3 - 0.06
		failed := passing
		failed.Cases, failed.Failed = 90, 10 // доля 0.10 > база(0)+0.05

		cases := []struct {
			name  string
			after evalMetrics
		}{
			{"M4", m4}, {"M1", m1}, {"M2", m2}, {"M3", m3}, {"failed", failed},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				after := base
				after.Mean.Metrics = c.after
				_, reasons := compareRuns(base, after)
				if len(reasons) != 1 || !strings.HasPrefix(reasons[0], c.name) {
					t.Fatalf("want exactly one reason for %s, got %v", c.name, reasons)
				}
			})
		}
	})

	t.Run("after has fewer runs than the base (сценарий 3)", func(t *testing.T) {
		base := evalResultFixture("m", "low", 2)
		after := base
		after.Runs = after.Runs[:1]
		after.Mean = meanOf(after.Runs)
		// Пороги пройдены сами по себе: причина только в числе прогонов.
		after.Mean.Metrics.M1 = base.Mean.Metrics.M1 + 0.2
		after.Mean.Metrics.M4 = 0
		_, reasons := compareRuns(base, after)
		if len(reasons) != 1 || reasons[0] != "runs 1 of 3" {
			t.Fatalf("want single reason \"runs 1 of 3\", got %v", reasons)
		}
	})

	t.Run("broken or single-run base is caught by runsValid (сценарий 4)", func(t *testing.T) {
		// runsValid проверяется в eval_test.go рядом с sameSetup, до первого
		// вызова модели (не тратить ключ на сравнение с шумной базой) - здесь
		// проверяем саму функцию отбора.
		t.Run("single run", func(t *testing.T) {
			base := evalResultFixture("m", "low", 2)
			base.Runs = base.Runs[:1]
			if runsValid(base.Runs) {
				t.Fatal("want invalid for a single-run base")
			}
		})
		t.Run("failed over the limit", func(t *testing.T) {
			base := evalResultFixture("m", "low", 2)
			base.Runs[0].Metrics.Failed = 10
			base.Runs[0].Metrics.Cases = 0
			if runsValid(base.Runs) {
				t.Fatal("want invalid for a run with failed over the limit")
			}
		})
	})

	t.Run("no bug cases on either side (сценарий 5)", func(t *testing.T) {
		base := evalResultFixture("m", "low", 2)
		noBugs := base.Mean.Metrics
		noBugs.Bugs = 0
		t.Run("after has no bugs", func(t *testing.T) {
			after := base
			after.Mean.Metrics = noBugs
			_, reasons := compareRuns(base, after)
			if !hasReason(reasons, "M3: no bug cases") {
				t.Fatalf("want \"M3: no bug cases\", got %v", reasons)
			}
		})
		t.Run("base has no bugs", func(t *testing.T) {
			b := base
			b.Mean.Metrics = noBugs
			_, reasons := compareRuns(b, base)
			if !hasReason(reasons, "M3: no bug cases") {
				t.Fatalf("want \"M3: no bug cases\", got %v", reasons)
			}
		})
	})

	t.Run("kind seen only in after keeps its row with a dash for base (сценарий 6)", func(t *testing.T) {
		base := evalResultFixture("m", "low", 2)
		delete(base.Mean.Kinds, "mixed")
		after := evalResultFixture("m", "low", 2)
		table, _ := compareRuns(base, after)
		if !strings.Contains(table, "type=mixed base=-") {
			t.Fatalf("want mixed row with base=-, got:\n%s", table)
		}
	})

	t.Run("M3 intersection keeps only bug-base cases (сценарий 7)", func(t *testing.T) {
		base := evalResult{Runs: []evalRun{
			{Cases: []caseRun{{ID: "bug1", Kind: "bug"}, {ID: "feat1", Kind: "feature"}}},
		}}
		after := evalResult{Runs: []evalRun{
			{Cases: []caseRun{{ID: "bug1", Kind: "mixed"}, {ID: "feat1", Kind: "mixed"}}},
		}}
		_, ids := m3Intersection(base, after)
		if ids != 1 {
			t.Fatalf("want 1 id in the intersection (bug1 only), got %d", ids)
		}
	})

	t.Run("both silent-in-after and failed-only-one-side ids are reported (сценарий 8)", func(t *testing.T) {
		base := evalResult{Runs: []evalRun{
			{Cases: []caseRun{{ID: "asked1", Questions: []int{1}}, {ID: "ok1"}}},
			{Cases: []caseRun{{ID: "asked1", Questions: []int{1}}, {ID: "ok1"}}},
			{Cases: []caseRun{{ID: "asked1", Questions: []int{1}}, {ID: "ok1"}}},
		}}
		after := evalResult{Runs: []evalRun{
			{Cases: []caseRun{{ID: "asked1"}, {ID: "ok1", Failed: "err"}}},
			{Cases: []caseRun{{ID: "asked1"}, {ID: "ok1", Failed: "err"}}},
			{Cases: []caseRun{{ID: "asked1"}, {ID: "ok1", Failed: "err"}}},
		}}
		silent := silentInAfter(base, after)
		if !slices.Contains(silent, "asked1") {
			t.Fatalf("want asked1 in silent-in-after, got %v", silent)
		}
		failedOnly := failedOnlyOneSide(base, after)
		if !slices.Contains(failedOnly, "ok1") {
			t.Fatalf("want ok1 in failed-only-one-side, got %v", failedOnly)
		}
		if slices.Contains(silent, "ok1") || slices.Contains(failedOnly, "asked1") {
			t.Fatalf("ids mixed up between rows: silent=%v failedOnly=%v", silent, failedOnly)
		}
	})

	t.Run("sameSetup rejects a mismatched setup or case set (сценарий 9)", func(t *testing.T) {
		base := evalResultFixture("m1", "low", 2)
		t.Run("different model", func(t *testing.T) {
			if err := sameSetup(base, "m2", "low", 2, caseIDs(base.Runs)); err == nil {
				t.Fatal("want error for a different model")
			}
		})
		t.Run("different reasoning", func(t *testing.T) {
			if err := sameSetup(base, "m1", "high", 2, caseIDs(base.Runs)); err == nil {
				t.Fatal("want error for a different reasoning")
			}
		})
		t.Run("different rounds", func(t *testing.T) {
			if err := sameSetup(base, "m1", "low", 3, caseIDs(base.Runs)); err == nil {
				t.Fatal("want error for a different rounds count")
			}
		})
		t.Run("case set missing an id", func(t *testing.T) {
			ids := caseIDs(base.Runs)[:len(caseIDs(base.Runs))-1]
			if err := sameSetup(base, "m1", "low", 2, ids); err == nil {
				t.Fatal("want error for a case set missing an id")
			}
		})
	})
}
