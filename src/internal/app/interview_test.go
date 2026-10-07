package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testRules(t *testing.T) Contract {
	t.Helper()

	rules, err := LoadContract()
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	return rules
}

func newTestInterview(t *testing.T, cases *Cases, rounds int) *Interview {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewInterview(cases, nil, log, testRules(t), DialogModel{Name: "test-model"}, rounds, nil)
}

// TestLoadContract: правила едут в бинарь и обязаны быть рабочими. Тип из
// CHECK cases.kind без пунктов в правилах, как и повтор ключа, должен ронять
// старт, а не обнаруживаться на живом диалоге.
func TestLoadContract(t *testing.T) {
	rules := testRules(t)

	if !slices.Contains(caseKinds, "mixed") {
		t.Errorf("тип mixed не в списке типов: %v", caseKinds)
	}
	for _, kind := range caseKinds {
		if len(rules.Items(kind)) == 0 {
			t.Errorf("тип %q остался без пунктов", kind)
		}
	}

	err := checkItems("bug", []ContractItem{
		{Key: "case", Title: "Случай"},
		{Key: "case", Title: "Второй раз"},
	})
	if err == nil {
		t.Error("повтор ключа принят, ожидался отказ")
	}
}

// TestContractCore: правила - только ядро (Р-1). Правка данных легко вернула
// бы анкету: каждый лишний пункт - лишний вопрос автору и метка неполноты.
func TestContractCore(t *testing.T) {
	rules := testRules(t)

	want := map[string][]string{
		"bug":      {"case", "wrong"},
		"feature":  {"need", "why"},
		"question": {"question"},
		"mixed":    {"case", "wrong", "need", "why"},
	}
	for kind, keys := range want {
		var got []string
		for _, it := range rules.Items(kind) {
			got = append(got, it.Key)
		}
		if !slices.Equal(got, keys) {
			t.Errorf("ядро %s: %v, ожидалось %v", kind, got, keys)
		}
	}
	filled := map[string]string{"case": "сделка 59767187", "wrong": "закрыта дублем"}
	if gaps := rules.Missing("mixed", filled); !slices.Equal(gaps, []string{"need", "why"}) {
		t.Errorf("пробелы смеси: %v", gaps)
	}
}

// TestCheckTurn: схема гарантирует форму ответа, смысл проверяет Go. Каждое
// нарушение здесь прошло бы схему насквозь и испортило бы тикет молча.
func TestCheckTurn(t *testing.T) {
	i := newTestInterview(t, nil, 2)

	full := []keyValue{
		{Key: "case", Value: "сделка 59767187"},
		{Key: "wrong", Value: "закрыта «Дублем», ожидали «Встреча назначена»"},
	}
	wish := []keyValue{
		{Key: "need", Value: "отказ нерелевантным лидам автоматически"},
		{Key: "why", Value: "сейчас отказывают руками"},
	}

	tests := []struct {
		name  string
		prior map[string]string
		turn  interviewTurn
		stuck bool
		ok    bool
	}{
		{
			name: "готовый ход",
			turn: interviewTurn{Kind: "bug", Filled: full, Ready: true},
			ok:   true,
		},
		{
			name:  "пункт закрыт прошлым раундом",
			prior: map[string]string{"case": "сделка 59767187"},
			turn:  interviewTurn{Kind: "bug", Filled: full[1:], Ready: true},
			ok:    true,
		},
		{
			name: "смесь с ядром бага и пожелания",
			turn: interviewTurn{Kind: "mixed", Filled: append(slices.Clone(full), wish...), Ready: true},
			ok:   true,
		},
		{
			name: "четыре вопроса",
			turn: interviewTurn{
				Kind: "mixed",
				Gaps: []string{"case", "wrong", "need", "why"},
				Questions: []Question{
					{Key: "case", Text: "а"}, {Key: "wrong", Text: "б"},
					{Key: "need", Text: "в"}, {Key: "why", Text: "г"},
				},
			},
		},
		{
			// Иначе mergeFilled молча взял бы последнее значение и потерял
			// первую идею - та же проверка, что и у повтора вопроса. Ядро
			// иначе закрыто и готово (как "готовый ход"), чтобы отказ был
			// именно от повтора ключа, а не от чего-то ещё.
			name: "два значения одного ключа",
			turn: interviewTurn{
				Kind: "bug",
				Filled: []keyValue{
					{Key: "case", Value: "сделка 59767187"},
					{Key: "case", Value: "сделка 60000000"},
					{Key: "wrong", Value: "закрыта «Дублем», ожидали «Встреча назначена»"},
				},
				Ready: true,
			},
		},
		{
			// Старый ключ из памяти модели: ядро его не знает.
			name: "ключ вне ядра",
			turn: interviewTurn{
				Kind:      "bug",
				Filled:    full[:1],
				Gaps:      []string{"wrong", "expected"},
				Questions: []Question{{Key: "expected", Text: "что ожидали?"}},
			},
		},
		{
			name: "уточнение при закрытом ядре",
			turn: interviewTurn{
				Kind:   "feature",
				Filled: wish,
				Questions: []Question{{Key: detailKey, Text: "по какому признаку лид нерелевантен?",
					Suggested: "нет бюджета"}},
			},
			ok: true,
		},
		{
			// Лишние уточнения снимает Go после проверки, ход из-за них не
			// отклоняется (R6).
			name: "два уточнения",
			turn: interviewTurn{
				Kind:   "bug",
				Filled: full,
				Questions: []Question{
					{Key: detailKey, Text: "а"}, {Key: detailKey, Text: "б"},
				},
			},
			ok: true,
		},
		{
			// Раунд из одного уточнения при открытом ядре тратит вопрос мимо
			// того, без чего тикет неполон.
			name: "уточнение при открытом ядре без вопроса о нём",
			turn: interviewTurn{
				Kind:      "bug",
				Filled:    full[:1],
				Gaps:      []string{"wrong"},
				Questions: []Question{{Key: detailKey, Text: "где смотрели?"}},
			},
		},
		{
			name: "уточнение рядом с вопросом о ядре",
			turn: interviewTurn{
				Kind:   "bug",
				Filled: full[:1],
				Gaps:   []string{"wrong"},
				Questions: []Question{
					{Key: "wrong", Text: "что пошло не так?"}, {Key: detailKey, Text: "где смотрели?"},
				},
			},
			ok: true,
		},
		{
			name: "готов при незакрытом пункте ядра",
			turn: interviewTurn{Kind: "bug", Filled: full[:1], Gaps: []string{"wrong"}, Ready: true},
		},
		{
			// Готовность обрывает разговор: заданный тем же ходом вопрос автору
			// уже не уйдёт, и молча потерять его нельзя.
			name: "готов и всё же уточняет",
			turn: interviewTurn{
				Kind:      "bug",
				Filled:    full,
				Questions: []Question{{Key: detailKey, Text: "где смотрели?"}},
				Ready:     true,
			},
		},
		{
			name: "пункт ядра не закрыт и не назван пробелом",
			turn: interviewTurn{
				Kind:      "bug",
				Filled:    full[:1],
				Questions: []Question{{Key: detailKey, Text: "где смотрели?"}},
			},
		},
		{
			name: "не готов и спросить нечего",
			turn: interviewTurn{Kind: "bug", Filled: full[:1], Gaps: []string{"wrong"}},
		},
		{
			// Автор отказался уточнять, лимит повторов и раундов исчерпан:
			// ход без вопросов - не тупик, а переход в саммари с incomplete.
			name:  "не готов и спросить нечего, но лимит исчерпан",
			turn:  interviewTurn{Kind: "bug", Filled: full[:1], Gaps: []string{"wrong"}},
			stuck: true,
			ok:    true,
		},
		{
			// stuck снимает только запрет на пустые Questions - раунд из
			// одного detail мимо открытого ядра остаётся отказом.
			name: "уточнение при открытом ядре без вопроса о нём, лимит исчерпан",
			turn: interviewTurn{
				Kind:      "bug",
				Filled:    full[:1],
				Gaps:      []string{"wrong"},
				Questions: []Question{{Key: detailKey, Text: "где смотрели?"}},
			},
			stuck: true,
		},
		{
			name: "вопрос про закрытый пункт",
			turn: interviewTurn{
				Kind:      "bug",
				Filled:    full,
				Questions: []Question{{Key: "case", Text: "а какая сделка?"}},
			},
		},
		{
			// Отписка вместо догадки обесценивает кнопку «Всё так»: автор
			// читает «Предполагаю: не указано» и перестаёт ей верить.
			name: "отписка вместо предположения",
			turn: interviewTurn{
				Kind:      "bug",
				Filled:    full[:1],
				Gaps:      []string{"wrong"},
				Questions: []Question{{Key: "wrong", Text: "что пошло не так?", Suggested: "не указано"}},
			},
		},
		{
			name: "предположение допустимо пустым",
			turn: interviewTurn{
				Kind:      "bug",
				Filled:    full[:1],
				Gaps:      []string{"wrong"},
				Questions: []Question{{Key: "wrong", Text: "что пошло не так?"}},
			},
			ok: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := i.checkTurn(tt.prior, tt.turn, tt.stuck)
			if tt.ok && err != nil {
				t.Errorf("ход отклонён: %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("ход принят, ожидался отказ")
			}
		})
	}
}

// TestAllExhausted: askTurn считает stuck по этой функции, а TestCheckTurn
// проверяет stuck напрямую булевым значением - без этого теста ошибка в
// подсчёте лимита (askedKeys, maxAsks) прошла бы незамеченной.
func TestAllExhausted(t *testing.T) {
	tests := []struct {
		name  string
		gaps  []string
		asked map[string]int
		want  bool
	}{
		{"пустой gaps - другая ошибка, не исчерпание", nil, map[string]int{"wrong": maxAsks}, false},
		{"один пробел ниже лимита", []string{"wrong"}, map[string]int{"wrong": maxAsks - 1}, false},
		{"один пробел на лимите", []string{"wrong"}, map[string]int{"wrong": maxAsks}, true},
		{"из двух один ниже лимита", []string{"wrong", "case"}, map[string]int{"wrong": maxAsks, "case": 0}, false},
		{"оба на лимите или выше", []string{"wrong", "case"}, map[string]int{"wrong": maxAsks, "case": maxAsks + 1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allExhausted(tt.gaps, tt.asked); got != tt.want {
				t.Errorf("allExhausted(%v, %v) = %v, want %v", tt.gaps, tt.asked, got, tt.want)
			}
		})
	}
}

// TestDropDetails: уточнение вне ядра - одно на обращение и только в первом
// раунде (Р-3). Модель номера раунда не знает, предел держит Go.
func TestDropDetails(t *testing.T) {
	questions := []Question{
		{Key: detailKey, Text: "первое"},
		{Key: "wrong", Text: "что пошло не так?"},
		{Key: detailKey, Text: "второе"},
	}

	tests := []struct {
		name    string
		round   int
		want    []string
		dropped int
	}{
		{"первый раунд", 0, []string{"первое", "что пошло не так?"}, 1},
		{"второй раунд", 1, []string{"что пошло не так?"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kept, dropped := dropDetails(slices.Clone(questions), tt.round)
			var got []string
			for _, q := range kept {
				got = append(got, q.Text)
			}
			if !slices.Equal(got, tt.want) || dropped != tt.dropped {
				t.Errorf("round=%d: оставлено %v, снято %d; ожидалось %v, %d",
					tt.round, got, dropped, tt.want, tt.dropped)
			}
		})
	}
}

// TestMergeFilled: контракт копится между раундами. Пункт, не повторённый
// моделью, не пропадает; ключ в gaps переоткрывает пункт; ключ вне ядра
// текущего типа снимается, а переход бага в смесь ядро бага сохраняет.
func TestMergeFilled(t *testing.T) {
	i := newTestInterview(t, nil, 2)

	tests := []struct {
		name  string
		prior map[string]string
		turn  interviewTurn
		want  map[string]string
	}{
		{
			name:  "баг",
			prior: map[string]string{"case": "сделка 1", "need": "ключ другого типа"},
			turn: interviewTurn{Kind: "bug",
				Filled: []keyValue{{Key: "wrong", Value: " закрыта дублем "}}, Gaps: []string{"case"}},
			want: map[string]string{"wrong": "закрыта дублем"},
		},
		{
			name:  "баг стал смесью",
			prior: map[string]string{"case": "сделка 1", "wrong": "закрыта дублем"},
			turn:  interviewTurn{Kind: "mixed", Filled: []keyValue{{Key: "need", Value: "напоминание за час"}}},
			want:  map[string]string{"case": "сделка 1", "wrong": "закрыта дублем", "need": "напоминание за час"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := i.mergeFilled(tt.prior, tt.turn); !maps.Equal(got, tt.want) {
				t.Errorf("слито %v, ожидалось %v", got, tt.want)
			}
		})
	}
}

// TestScrubContacts: структурные персональные данные вырезаются до записи в
// тикет, а идентификаторы карточек остаются - без них тикет бесполезен.
func TestScrubContacts(t *testing.T) {
	in := "Клиент писал с ivan.petrov@example.com и звонил на +7 916 123-45-67, " +
		"карта 4276 3800 1234 5678, заказ 4821000123 в статусе «оплачен»"
	got := scrubContacts(in)

	for _, secret := range []string{"ivan.petrov@example.com", "916 123-45-67", "4276 3800"} {
		if strings.Contains(got, secret) {
			t.Errorf("персональные данные остались: %q в %q", secret, got)
		}
	}
	for _, keep := range []string{"4821000123", "оплачен"} {
		if !strings.Contains(got, keep) {
			t.Errorf("нужное вырезано: %q пропал из %q", keep, got)
		}
	}
}

// TestSummaryTitle: заголовок держит форму, по которой автор узнаёт свой тикет
// в списке. Служебное слово в начале тратит место на то, что и так видно по
// метке типа.
func TestSummaryTitle(t *testing.T) {
	i := newTestInterview(t, nil, 3)
	cs := &Case{Kind: "bug", Filled: map[string]string{"case": "заявка 4821"}}

	const brief = "Заявка не сохраняется после нажатия «Готово», данные теряются."
	if err := i.checkSummary(cs, summaryOut{Title: "Заявка не сохраняется", Brief: brief}); err != nil {
		t.Errorf("годный заголовок отклонён: %v", err)
	}
	for _, title := range []string{"Проблема: заявка не сохраняется", "Баг в форме заявки",
		"просьба добавить фильтр"} {
		if err := i.checkSummary(cs, summaryOut{Title: title, Brief: brief}); err == nil {
			t.Errorf("заголовок %q принят", title)
		}
	}
	if err := i.checkSummary(cs, summaryOut{Title: "   ", Brief: brief}); err == nil {
		t.Error("пустой заголовок принят")
	}
}

// TestSummaryBrief: разросшееся краткое содержание отклоняется - оно вытеснило
// бы статус и комментарий за край экрана. Пустое проходит: молчание модели не
// останавливает тикет, краткое достраивается из первого раздела саммари.
func TestSummaryBrief(t *testing.T) {
	i := newTestInterview(t, nil, 3)
	cs := &Case{Kind: "bug", Filled: map[string]string{"case": "заявка 4821"}}
	title := "Заявка не сохраняется"

	if err := i.checkSummary(cs, summaryOut{Title: title, Brief: "  "}); err != nil {
		t.Errorf("пустое краткое содержание остановило тикет: %v", err)
	}
	long := strings.Repeat("и", briefLimit+1)
	if err := i.checkSummary(cs, summaryOut{Title: title, Brief: long}); err == nil {
		t.Error("краткое содержание длиннее предела принято")
	}

	// Модель промолчала: суть берётся из первого раздела готового тела - там
	// разделы уже обезличены и расставлены по контракту. Иначе тикет уйдёт без
	// краткого содержания, а карточка в боте покажет один заголовок.
	body := "## Конкретный случай\n\n" + strings.Repeat("к", briefLimit+50) +
		"\n\n## Что должно было произойти\n\nдолжна сохраняться"
	brief := briefOf("", body)
	if utf8.RuneCountInString(brief) != briefLimit {
		t.Errorf("замена краткого не обрезана по пределу: %d символов", utf8.RuneCountInString(brief))
	}
	if strings.Contains(brief, "Конкретный случай") || strings.Contains(brief, "должна сохраняться") {
		t.Errorf("замена взята не из текста первого раздела: %q", brief)
	}
	if got := briefOf("Заявка не сохраняется", body); got != "Заявка не сохраняется" {
		t.Errorf("своё краткое содержание подменено: %q", got)
	}

	// Обрезка идёт после обезличивания: телефон, разорванный границей среза, не
	// совпал бы с шаблоном и уехал бы в тикет.
	dirty := "## Случай\n\n" + strings.Repeat("к", briefLimit-8) + " +7 916 123-45-67 хвост"
	if got := briefOf("", dirty); strings.Contains(got, "916 123") {
		t.Errorf("телефон пережил обрезку краткого содержания: %q", got)
	}
}

// TestScrubbedTranscript: обезличивание не держится на одном промте. Расшифровка
// и разбор экрана уходят автору протоколом раньше саммари, и промах модели
// показал бы телефон клиента в чате.
func TestScrubbedTranscript(t *testing.T) {
	spoken := "клиент Пётр звонил с +7 916 123-45-67, заказ 4821000123 не оплачен"
	got := scrubContacts(spoken)
	if strings.Contains(got, "916 123-45-67") {
		t.Errorf("телефон остался в расшифровке: %q", got)
	}
	if !strings.Contains(got, "4821000123") {
		t.Errorf("номер заказа вырезан вместе с телефоном: %q", got)
	}

	extract := screenshotExtract{
		Screen:     "карточка клиента",
		Facts:      []screenshotFact{{Label: "почта", Value: "ivan@example.com"}},
		Relevant:   "та самая карточка",
		Unreadable: &[]string{},
	}
	if card := scrubContacts(formatExtract(extract)); strings.Contains(card, "ivan@example.com") {
		t.Errorf("почта осталась в разборе экрана: %q", card)
	}
}

// TestLostKeys: смена типа обращения уносит собранные ответы, и увидеть это
// нужно по именам пунктов - иначе разговор необъяснимо спрашивает заново.
func TestLostKeys(t *testing.T) {
	prior := map[string]string{"case": "заказ 4821", "expected": "статус меняется"}
	filled := map[string]string{"case": "заказ 4821"}

	got := lostKeys(prior, filled)
	if len(got) != 1 || got[0] != "expected" {
		t.Errorf("потерянные пункты: %v, ожидался [expected]", got)
	}
	if n := len(lostKeys(prior, prior)); n != 0 {
		t.Errorf("без смены типа потеряно %d пунктов", n)
	}
}

// TestSummaryWithoutSections: ответ модели без разделов не отклоняется и не
// повторяется. Именно эта проверка уводила работу в повторы и оставляла автора
// без единого слова на минуты, пока модель не отвечала «как надо».
func TestSummaryWithoutSections(t *testing.T) {
	i := newTestInterview(t, nil, 2)
	i.llm = fakeLLM(t, `{"title":"Форма не сохраняется","brief":"","sections":[]}`)
	cs := &Case{ID: "case-1", Kind: "bug", Filled: map[string]string{"case": "заказ 4821"}}

	messages := []Message{{Role: "system", Parts: []Part{TextPart("промт")}}}
	out, err := i.askSummary(context.Background(), cs, messages)
	if err != nil {
		t.Fatalf("саммари без разделов отклонено: %v", err)
	}
	if out.Title != "Форма не сохраняется" {
		t.Errorf("заголовок саммари: %q", out.Title)
	}
}

// TestCheckSummary: заголовки разделов пишет модель, а тело issue собирает Go.
// Чужой markdown в заголовке, занятое имя или раздел про пункт другого типа
// ломали бы тело тикета молча (Р-6).
func TestCheckSummary(t *testing.T) {
	i := newTestInterview(t, nil, 2)
	cs := &Case{Kind: "bug", Filled: map[string]string{"case": "сделка 59767187"}}

	section := func(heading string) Section { return Section{Heading: heading, Text: "текст"} }
	many := func(n int) []Section {
		out := make([]Section, n)
		for k := range out {
			out[k] = section(fmt.Sprintf("Раздел %d", k+1))
		}
		return out
	}

	tests := []struct {
		name     string
		sections []Section
		ok       bool
	}{
		{"без разделов", nil, true},
		{"шесть разделов", many(6), true},
		{"раздел пункта ядра", []Section{{Key: "case", Heading: "Сделка", Text: "59767187"}}, true},
		{"семь разделов", many(7), false},
		{"заголовок длиннее 60", []Section{section(strings.Repeat("з", 61))}, false},
		{"пустой заголовок", []Section{section("  ")}, false},
		{"перевод строки", []Section{section("Шаги\nи ещё")}, false},
		{"решётка", []Section{section("Шаги # два")}, false},
		{"угловая скобка", []Section{section("Шаги <b>")}, false},
		{"занято Кратко", []Section{section("Кратко")}, false},
		{"занято ссылки", []Section{section(" ссылки ")}, false},
		{"занято Пересечения", []Section{section("Пересечения")}, false},
		{"пустой текст", []Section{{Heading: "Шаги", Text: " "}}, false},
		{"заголовок внутри текста", []Section{{Heading: "Шаги", Text: "раз\n## Ссылки\nдва"}}, false},
		{"номер тикета в начале строки", []Section{{Heading: "Шаги", Text: "раз\n#57 уже закрыт"}}, true},
		{"ключ другого типа становится свободным разделом", []Section{{Key: "need", Heading: "Что нужно", Text: "фильтр"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := i.checkSummary(cs, summaryOut{Title: "Сделка закрыта дублем", Sections: tt.sections})
			if tt.ok && err != nil {
				t.Errorf("саммари отклонено: %v", err)
			}
			if !tt.ok && err == nil {
				t.Errorf("саммари с разделами %q принято", tt.sections)
			}
		})
	}
}

// TestCheckSummaryNeedsBody: без разделов и без ядра тело собрать не из чего,
// а сырой протокол в него не идёт - он не обезличен моделью.
func TestCheckSummaryNeedsBody(t *testing.T) {
	i := newTestInterview(t, nil, 2)
	if err := i.checkSummary(&Case{Kind: "bug"}, summaryOut{Title: "Сделка закрыта дублем"}); err == nil {
		t.Fatal("саммари без разделов и ядра принято")
	}
}

// TestCheckSummary_ForeignKeyDropped: тип менялся по ходу интервью
// (case_kind_changed), и раздел под ключ прежнего типа не должен ронять
// саммари на пустом месте - ключ снимается, а текст остаётся в теле тикета.
func TestCheckSummary_ForeignKeyDropped(t *testing.T) {
	i := newTestInterview(t, nil, 2)
	cs := &Case{Kind: "bug"}
	out := summaryOut{
		Title:    "Сделка закрыта дублем",
		Sections: []Section{{Key: "need", Heading: "Что нужно", Text: "фильтр по статусу"}},
	}

	if err := i.checkSummary(cs, out); err != nil {
		t.Fatalf("саммари с чужим ключом отклонено: %v", err)
	}
	if out.Sections[0].Key != "" {
		t.Fatalf("ключ %q не снят", out.Sections[0].Key)
	}
	if body := i.renderSections(cs, out.Sections); !strings.Contains(body, "фильтр по статусу") {
		t.Fatalf("текст раздела потерян в теле:\n%s", body)
	}
}

// TestSectionsFallBackToContract: содержание ядра уже собрано интервью, и
// молчание модели не имеет права остановить тикет.
func TestSectionsFallBackToContract(t *testing.T) {
	i := newTestInterview(t, nil, 2)
	cs := &Case{
		Kind:   "bug",
		Filled: map[string]string{"case": "сделка 59767187", "wrong": "закрыта «Дублем»"},
	}

	body := i.renderSections(cs, nil)

	want := "## Конкретный случай\n\nсделка 59767187\n\n## Что пошло не так\n\nзакрыта «Дублем»"
	if body != want {
		t.Errorf("тело саммари:\nполучено:\n%s\nожидалось:\n%s", body, want)
	}
}

// TestSectionsKeepCore: закрытый пункт ядра, который модель не покрыла
// разделом, дописывается из собранного - ни одна идея не выбрасывается, и
// держит это Go, а не промт.
func TestSectionsKeepCore(t *testing.T) {
	i := newTestInterview(t, nil, 2)
	cs := &Case{
		Kind:   "bug",
		Filled: map[string]string{"case": "сделка 59767187", "wrong": "закрыта «Дублем»"},
	}

	body := i.renderSections(cs, []Section{{Key: "case", Heading: "Сделка", Text: "59767187 от вторника"}})

	if !strings.Contains(body, "## Что пошло не так\n\nзакрыта «Дублем»") {
		t.Errorf("закрытый пункт потерян:\n%s", body)
	}
	if strings.Contains(body, "## Конкретный случай") {
		t.Errorf("покрытый разделом пункт повторён:\n%s", body)
	}
}

// TestRenderSections: разделы идут в порядке модели под её заголовками, раздел
// про пункт из пробелов остаётся (это слова автора, пробел назовёт строка «Не
// уточнено»), а заголовок обезличивается так же, как текст.
func TestRenderSections(t *testing.T) {
	i := newTestInterview(t, nil, 2)
	cs := &Case{Kind: "bug", Filled: map[string]string{"case": "сделка 59767187"}, Gaps: []string{"wrong"}}

	body := i.renderSections(cs, []Section{
		{Heading: "Звонок +7 916 123-45-67", Text: "клиент перезвонил"},
		{Key: "case", Heading: "Сделка", Text: "59767187"},
		{Key: "wrong", Heading: "Что не так", Text: "слова автора"},
	})

	want := "## Звонок [телефон]\n\nклиент перезвонил\n\n## Сделка\n\n59767187\n\n" +
		"## Что не так\n\nслова автора"
	if body != want {
		t.Errorf("тело саммари:\nполучено:\n%s\nожидалось:\n%s", body, want)
	}
	if strings.Contains(plainText(body), "## ") {
		t.Error("в сообщении автору остались markdown-заголовки")
	}
}

// TestSummaryMessageUnclear: автор видит незакрытое ядро до «Публикую» одной
// строкой, а при закрытом ядре не видит ни строки, ни обещания пометки (R4).
func TestSummaryMessageUnclear(t *testing.T) {
	rules := testRules(t)

	open := summaryMessage("Заголовок", "", "## Случай\n\nтекст",
		rules.Unclear("bug", map[string]string{"case": "сделка 1"}), "")
	if !strings.Contains(open, "Не уточнено: что пошло не так. Тикет уйдёт с пометкой о неполноте.") {
		t.Errorf("строки пробела нет:\n%s", open)
	}
	if strings.Contains(open, "Остались пробелы") {
		t.Errorf("старый список пробелов:\n%s", open)
	}

	closed := summaryMessage("Заголовок", "", "## Случай\n\nтекст",
		rules.Unclear("bug", map[string]string{"case": "сделка 1", "wrong": "дубль"}), "")
	if strings.Contains(closed, "Не уточнено") || strings.Contains(closed, "неполноте") {
		t.Errorf("пробел при закрытом ядре:\n%s", closed)
	}
}

// startInterview готовит обращение в интервью: тесту нужен разговор, а не
// прогон нормализации.
func startInterview(t *testing.T, cases *Cases, userID int64, round int) *Case {
	t.Helper()
	ctx := context.Background()

	cs, _, err := cases.StartCase(ctx, User{ID: userID, First: "Тест"}, "tg-intake", modeTicket)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	_, err = cases.pool.Exec(ctx, `
		UPDATE cases SET status = 'interview', kind = 'bug', round = $2,
		                 protocol = 'текст: форма не сохраняется'
		WHERE id = $1`, cs.ID, round)
	if err != nil {
		t.Fatalf("move to interview: %v", err)
	}
	return reload(t, cases, cs.ID)
}

func countJobs(t *testing.T, pool *pgxpool.Pool, kind, caseID string) int {
	t.Helper()

	var n int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM jobs WHERE kind = $1 AND payload->>'case_id' = $2`, kind, caseID).Scan(&n)
	if err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

// TestAcceptRound: «Всё так» принимает раунд целиком, а кнопка прошлого раунда
// не закрывает текущие вопросы - иначе автор подтверждает не то, что видит.
func TestAcceptRound(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6001, 1)

	questions := []Question{
		{Key: "case", Text: "какой заказ?", Suggested: "заказ 4821"},
		{Key: "wrong", Text: "что пошло не так?", Suggested: "статус не сменился на «оплачен»"},
		{Key: detailKey, Text: "где смотрели?", Suggested: "в карточке заказа"},
	}
	err := addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{"round": 1, "questions": questions})
	if err != nil {
		t.Fatalf("add round: %v", err)
	}

	if err := cases.AcceptRound(ctx, cs, 0); !errors.Is(err, ErrStaleRound) {
		t.Fatalf("кнопка прошлого раунда: получено %v, ожидалось ErrStaleRound", err)
	}
	if err := cases.AcceptRound(ctx, reload(t, cases, cs.ID), 1); err != nil {
		t.Fatalf("accept round: %v", err)
	}

	history, err := cases.history(ctx, cs.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("история разговора: сообщений %d, ожидалось 2", len(history))
	}
	answer := history[1].Parts[0].text
	for _, q := range questions {
		if !strings.Contains(answer, q.Suggested) {
			t.Errorf("предположение потеряно: %q", q.Suggested)
		}
	}
	if n := countJobs(t, pool, JobInterview, cs.ID); n != 1 {
		t.Errorf("работ интервью после ответа: %d, ожидалась 1", n)
	}

	// Следующий ход идёт секунды, и человек жмёт кнопку ещё раз. Второе нажатие
	// не должно давать ни второго ответа в истории, ни второго хода модели. И
	// это не устаревшая кнопка: вопрос тот же, ответ по нему уже принят - автору
	// про «прошлый вопрос» говорить неправда.
	if err := cases.AcceptRound(ctx, reload(t, cases, cs.ID), 1); !errors.Is(err, ErrRoundAnswered) {
		t.Fatalf("повторное «Всё так»: получено %v, ожидалось ErrRoundAnswered", err)
	}
	if n := countJobs(t, pool, JobInterview, cs.ID); n != 1 {
		t.Errorf("повтор нажатия добавил работу: работ %d, ожидалась 1", n)
	}
}

// TestAcceptRoundSkipsStub: «Всё так» превращает догадки в слова автора, и
// отписка модели «не указано» стала бы подтверждённым фактом. Такой пункт
// остаётся открытым.
func TestAcceptRoundSkipsStub(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6010, 1)

	questions := []Question{
		{Key: "case", Text: "какой заказ?", Suggested: "заказ 4821"},
		{Key: "wrong", Text: "что вышло?", Suggested: "Не указано"},
	}
	err := addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{"round": 1, "questions": questions})
	if err != nil {
		t.Fatalf("add round: %v", err)
	}
	if err := cases.AcceptRound(ctx, reload(t, cases, cs.ID), 1); err != nil {
		t.Fatalf("accept round: %v", err)
	}

	history, err := cases.history(ctx, cs.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	answer := history[len(history)-1].Parts[0].text
	if strings.Contains(strings.ToLower(answer), "не указано") {
		t.Errorf("отписка ушла в ответ автора: %q", answer)
	}
	if !strings.Contains(answer, "заказ 4821") {
		t.Errorf("содержательная догадка потеряна: %q", answer)
	}
}

// TestExhaustedQuestionGoesToSummary: пункт, о котором уже спрашивали дважды,
// третьего вопроса не получает. Ход не задаёт раунд из воздуха, а собирает
// саммари с тем, что есть: повтор одного и того же автор читает как поломку.
func TestExhaustedQuestionGoesToSummary(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6011, 1)

	turn := `{"kind":"bug","filled":[{"key":"case","value":"заказ 4821"}],` +
		`"gaps":["wrong"],"ready":false,` +
		`"questions":[{"key":"wrong","text":"что ожидали?","suggested":"статус «оплачен»"}]}`
	i := newTestInterview(t, cases, 3)
	i.llm = fakeLLM(t, turn)

	// Два раунда об одном пункте: предел исчерпан, третьего вопроса быть не
	// должно, даже если модель его предлагает.
	for n := 1; n <= maxAsks; n++ {
		err := addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{
			"round": n, "questions": []Question{{Key: "wrong", Text: "что ожидали?"}},
		})
		if err != nil {
			t.Fatalf("add round: %v", err)
		}
	}

	job := Job{ID: 1, Kind: JobInterview, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := i.Run(ctx, job); err != nil {
		t.Fatalf("run interview: %v", err)
	}

	if n := countJobs(t, pool, JobSummarize, cs.ID); n != 1 {
		t.Errorf("работ саммари: %d, ожидалась 1", n)
	}
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 0 {
		t.Errorf("исчерпанный пункт ушёл автору третьим вопросом: уведомлений %d", n)
	}
	if got := reload(t, cases, cs.ID).Gaps; len(got) != 1 {
		t.Errorf("пробелы не сохранены: %v", got)
	}
}

// TestRoundWithoutSuggestionHasNoButton: раунд, где модель не дала ни одной
// догадки, доходит до автора вопросами, но без кнопки «Всё так» и без обещания
// подтвердить предположения. Обещание без догадок автор проверяет нажатием и
// получает отказ - ровно тот случай, что был в контуре 2026-08-14.
func TestRoundWithoutSuggestionHasNoButton(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6021, 1)

	turn := `{"kind":"bug","filled":[{"key":"case","value":"заказ 4821"}],` +
		`"gaps":["wrong"],"ready":false,` +
		`"questions":[{"key":"wrong","text":"что ожидали?","suggested":""}]}`
	i := newTestInterview(t, cases, 3)
	i.llm = fakeLLM(t, turn)

	job := Job{ID: 1, Kind: JobInterview, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := i.Run(ctx, job); err != nil {
		t.Fatalf("run interview: %v", err)
	}

	var raw []byte
	err := pool.QueryRow(ctx, `SELECT payload FROM jobs
		WHERE kind = $1 AND payload->>'case_id' = $2`, JobNotify, cs.ID).Scan(&raw)
	if err != nil {
		t.Fatalf("notify job: %v", err)
	}
	var p notifyPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.Buttons != keysAsk {
		t.Errorf("кнопки раунда без догадок: %q, ожидалось %q", p.Buttons, keysAsk)
	}
	if strings.Contains(p.Text, "Всё так") {
		t.Errorf("обещание кнопки без догадок: %q", p.Text)
	}
	if !strings.Contains(p.Text, "что ожидали?") {
		t.Errorf("вопрос потерян: %q", p.Text)
	}
}

// fakeLLM подменяет транспорт клиента: ответ модели приходит из теста, сеть не
// нужна. Тест в том же пакете, поэтому обходится без параметра адреса.
func fakeLLM(t *testing.T, content string) *OpenRouter {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"content": content}}},
	})
	if err != nil {
		t.Fatalf("encode llm response: %v", err)
	}

	llm := NewOpenRouter("test-key", "test-model", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	llm.http = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})}
	return llm
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestAskedKeys: раунд задаёт несколько вопросов, а человек отвечает на один -
// остальные модель спрашивает снова. Счётчик по журналу решает, когда пункт
// исчерпан: третий заход по одному ключу автор читает как поломку бота.
func TestAskedKeys(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6009, 1)

	rounds := [][]Question{
		{{Key: "case", Text: "какой заказ?"}, {Key: "wrong", Text: "что ожидали?"}},
		{{Key: "wrong", Text: "а всё-таки, что должно было выйти?"}},
	}
	for n, questions := range rounds {
		err := addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{
			"round": n + 1, "questions": questions,
		})
		if err != nil {
			t.Fatalf("add round: %v", err)
		}
	}

	asked, err := cases.askedKeys(ctx, cs.ID)
	if err != nil {
		t.Fatalf("asked keys: %v", err)
	}
	if asked["wrong"] < maxAsks {
		t.Errorf("пункт wrong спрошен %d раз, ожидалось не меньше %d", asked["wrong"], maxAsks)
	}
	if asked["case"] >= maxAsks {
		t.Errorf("пункт case исчерпан после одного вопроса: %d", asked["case"])
	}
}

// TestReplaceJobRevivesPublish: работа обращения существует в единственном
// экземпляре, и новая заменяет прежнюю. Без этого исчерпавшая повторы
// публикация оставляла бы свой ключ в очереди, повторное «Публикую» молча не
// вставало бы, а обращение застряло бы в publishing навсегда.
func TestReplaceJobRevivesPublish(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6005, 3)

	if err := replaceJob(ctx, pool, JobPublish, cs.ID, casePayload{CaseID: cs.ID}); err != nil {
		t.Fatalf("put publish job: %v", err)
	}
	_, err := pool.Exec(ctx, `
		UPDATE jobs SET status = 'failed', attempts = 6 WHERE kind = $1`, JobPublish)
	if err != nil {
		t.Fatalf("fail publish job: %v", err)
	}

	_, err = pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID)
	if err != nil {
		t.Fatalf("move to summary: %v", err)
	}
	if err := cases.ConfirmSummary(ctx, reload(t, cases, cs.ID)); err != nil {
		t.Fatalf("confirm summary: %v", err)
	}

	var status string
	var attempts int
	err = pool.QueryRow(ctx, `
		SELECT status, attempts FROM jobs WHERE kind = $1 AND payload->>'case_id' = $2`,
		JobPublish, cs.ID).Scan(&status, &attempts)
	if err != nil {
		t.Fatalf("load publish job: %v", err)
	}
	if status != "pending" || attempts != 0 {
		t.Errorf("публикация не вернулась в очередь: status=%s attempts=%d", status, attempts)
	}
}

// TestAnswerAfterSummaryIsFix: правка саммари возвращает обращение в интервью, и
// следующий ход узнаёт в ней правку по журналу. Без этого автор, заметивший
// ошибку на последнем раунде, не может её исправить: предел раундов исчерпан, и
// правка молча ушла бы в новое саммари.
func TestAnswerAfterSummaryIsFix(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6002, 3)

	err := addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{
		"round": 3, "questions": []Question{{Key: "case", Text: "какой заказ?"}},
	})
	if err != nil {
		t.Fatalf("add round: %v", err)
	}
	if err := addEvent(ctx, pool, cs.ID, "summary_ready", map[string]any{"incomplete": false}); err != nil {
		t.Fatalf("add summary event: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("move to summary: %v", err)
	}

	fix, err := cases.isFix(ctx, cs.ID)
	if err != nil {
		t.Fatalf("is fix: %v", err)
	}
	if !fix {
		t.Error("ход после показанного саммари не опознан как правка")
	}

	if err := cases.AddAnswer(ctx, reload(t, cases, cs.ID), "заказ был 4821, а не 4812"); err != nil {
		t.Fatalf("add answer: %v", err)
	}
	if got := reload(t, cases, cs.ID).Status; got != statusInterview {
		t.Errorf("статус после правки: %s, ожидался %s", got, statusInterview)
	}
	if n := countJobs(t, pool, JobInterview, cs.ID); n != 1 {
		t.Errorf("работ интервью после правки: %d, ожидалась 1", n)
	}
}

// TestShownSummaryIsInHistory: автор правит текст, который бот ему показал.
// Саммари обязано лежать в истории репликой бота, иначе правка ссылается в
// пустоту. Событие без снимка текста (обращение старше выката) пропускается.
func TestShownSummaryIsInHistory(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6012, 1)

	err := addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{
		"round": 1, "questions": []Question{{Key: "case", Text: "какой заказ?"}},
	})
	if err != nil {
		t.Fatalf("add round: %v", err)
	}
	if err := addEvent(ctx, pool, cs.ID, "answer_given", map[string]any{"text": "заказ 4821"}); err != nil {
		t.Fatalf("add answer: %v", err)
	}
	err = addEvent(ctx, pool, cs.ID, "summary_ready", map[string]any{
		"title": "Заказ 4821 не уходит в доставку", "body": "## Конкретный случай\n\nЗаказ 4821.",
	})
	if err != nil {
		t.Fatalf("add summary: %v", err)
	}
	if err := addEvent(ctx, pool, cs.ID, "answer_given", map[string]any{"text": "заказ 4812"}); err != nil {
		t.Fatalf("add fix: %v", err)
	}

	history, err := cases.history(ctx, cs.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 4 {
		t.Fatalf("сообщений в истории: %d, ожидалось 4", len(history))
	}
	shown := history[2]
	if shown.Role != "assistant" {
		t.Errorf("роль показанного саммари: %s, ожидалась assistant", shown.Role)
	}
	if !strings.Contains(shown.Parts[0].text, "Заказ 4821 не уходит в доставку") {
		t.Errorf("в истории нет текста показанного саммари: %q", shown.Parts[0].text)
	}

	if err := addEvent(ctx, pool, cs.ID, "summary_ready", map[string]any{"incomplete": false}); err != nil {
		t.Fatalf("add legacy summary: %v", err)
	}
	history, err = cases.history(ctx, cs.ID)
	if err != nil {
		t.Fatalf("history after legacy: %v", err)
	}
	if len(history) != 4 {
		t.Errorf("событие без текста саммари попало в историю: %d сообщений", len(history))
	}
}

// TestAskAnswerIsInHistory: разговор пришёл в тикет из режима вопроса, и «как
// сейчас» уже установлено по документации. Ответ бота обязан лежать в истории,
// иначе первый же раунд переспросит то же. Слова автора несёт протокол сырья,
// поэтому вопрос в историю не идёт - он пришёл бы в контекст дважды.
func TestAskAnswerIsInHistory(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6015, 0)

	err := addEvent(ctx, pool, cs.ID, "question_asked", map[string]any{
		"text": "через сколько срабатывает опрос",
	})
	if err != nil {
		t.Fatalf("add question: %v", err)
	}
	if err := addEvent(ctx, pool, cs.ID, "answer_ready", map[string]any{"text": "Раз в сутки."}); err != nil {
		t.Fatalf("add answer: %v", err)
	}

	history, err := cases.history(ctx, cs.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1 || history[0].Role != "assistant" {
		t.Fatalf("история после перехода в тикет: %+v", history)
	}
	if !strings.Contains(history[0].Parts[0].text, "Раз в сутки") {
		t.Errorf("ответ по документации не попал в историю: %q", history[0].Parts[0].text)
	}
}

// TestStaleTurnIsDropped: пока модель думает, автор дописывает - и его ответ уже
// поставил свежий ход. Устаревший результат не имеет права лечь поверх: иначе
// автор получает два раунда вопросов на один свой ответ.
func TestStaleTurnIsDropped(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	i := newTestInterview(t, cases, 3)
	cs := startInterview(t, cases, 6006, 1)

	version, err := cases.turnsCount(ctx, pool, cs.ID)
	if err != nil {
		t.Fatalf("turns count: %v", err)
	}
	if err := cases.AddAnswer(ctx, cs, "и ещё вот что: падает только в Safari"); err != nil {
		t.Fatalf("add answer: %v", err)
	}

	turn := interviewTurn{
		Kind:      "bug",
		Filled:    []keyValue{{Key: "case", Value: "заказ 4821"}},
		Gaps:      []string{"wrong"},
		Questions: []Question{{Key: "wrong", Text: "что ожидали?"}},
	}
	saved, _, _, err := i.saveTurn(ctx, cs, turn, map[string]string{"case": "заказ 4821"}, 2, false, version)
	if err != nil {
		t.Fatalf("save turn: %v", err)
	}
	if saved {
		t.Error("ход, устаревший на ответе автора, записан в базу")
	}
	if got := reload(t, cases, cs.ID).Round; got != 1 {
		t.Errorf("раунд сдвинут устаревшим ходом: %d, ожидался 1", got)
	}
}

// TestRoundLimitGoesToSummary: исчерпав раунды, ход не спрашивает больше ничего,
// а собирает саммари с тем, что есть. Недобранный контракт даёт тикет с
// пробелами, а не отказ.
func TestRoundLimitGoesToSummary(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	i := newTestInterview(t, cases, 3)
	cs := startInterview(t, cases, 6007, 3)

	version, err := cases.turnsCount(ctx, pool, cs.ID)
	if err != nil {
		t.Fatalf("turns count: %v", err)
	}
	turn := interviewTurn{
		Kind:      "bug",
		Filled:    []keyValue{{Key: "case", Value: "заказ 4821"}},
		Gaps:      []string{"wrong"},
		Questions: []Question{{Key: "wrong", Text: "что ожидали?"}},
	}
	// Предел исчерпан (round=3 при пределе 3), поэтому ход идёт в саммари, а не
	// задаёт четвёртый раунд.
	saved, _, _, err := i.saveTurn(ctx, cs, turn, map[string]string{"case": "заказ 4821"}, 3, true, version)
	if err != nil {
		t.Fatalf("save turn: %v", err)
	}
	if !saved {
		t.Fatal("ход не записан")
	}

	if n := countJobs(t, pool, JobSummarize, cs.ID); n != 1 {
		t.Errorf("работ саммари: %d, ожидалась 1", n)
	}
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 0 {
		t.Errorf("на пределе раундов автору ушли вопросы: уведомлений %d", n)
	}
	if got := reload(t, cases, cs.ID); len(got.Gaps) != 1 {
		t.Errorf("пробелы не сохранены: %v", got.Gaps)
	}
}

// TestPublishSkipsCancelled: отменённое обращение не уходит в GitHub, повторная
// работа не создаёт второго тикета. Оба выхода срабатывают до первого запроса,
// поэтому клиент здесь пустой.
func TestPublishSkipsCancelled(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	publisher := NewPublisher(cases, NewGitHub("", GitHubAPI, nil, log), testRules(t), log, 0, "")

	cs := startInterview(t, cases, 6003, 3)
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'cancelled' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("cancel case: %v", err)
	}

	job := Job{ID: 1, Kind: JobPublish, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := publisher.Run(ctx, job); err != nil {
		t.Fatalf("publish cancelled case: %v", err)
	}
	if got := reload(t, cases, cs.ID); got.IssueNumber != 0 {
		t.Errorf("отменённое обращение получило тикет #%d", got.IssueNumber)
	}

	_, err := pool.Exec(ctx, `
		UPDATE cases SET status = 'publishing', issue_number = 42, issue_url = 'u' WHERE id = $1`, cs.ID)
	if err != nil {
		t.Fatalf("mark published: %v", err)
	}
	if err := publisher.Run(ctx, job); err != nil {
		t.Fatalf("publish twice: %v", err)
	}
	if got := reload(t, cases, cs.ID).IssueNumber; got != 42 {
		t.Errorf("повтор работы переписал тикет: #%d", got)
	}
}

// TestRecoverStuck: обращение, потерявшее свою работу, двигаться нечем - из
// publishing нет даже отмены. Восстановление возвращает работу в очередь, а
// обращения, ждущие человека, не трогает.
func TestRecoverStuck(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	stuck := startInterview(t, cases, 6008, 3)
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'publishing' WHERE id = $1`, stuck.ID); err != nil {
		t.Fatalf("move to publishing: %v", err)
	}
	waiting := startInterview(t, cases, 6009, 1)

	if err := cases.RecoverStuck(ctx); err != nil {
		t.Fatalf("recover stuck: %v", err)
	}

	if n := countJobs(t, pool, JobPublish, stuck.ID); n != 1 {
		t.Errorf("публикация не возвращена в очередь: работ %d, ожидалась 1", n)
	}
	// Обращение в интервью ждёт ответа автора: работы там нет и быть не должно.
	if n := countJobs(t, pool, JobInterview, waiting.ID); n != 0 {
		t.Errorf("восстановление тронуло ждущее обращение: работ %d", n)
	}
}

// TestPrefixStable: стабильный префикс обязан идти первым сообщением и не
// меняться от хода к ходу. Любая изменяющаяся строка перед промтом молча гасит
// кэш провайдера, и заметить это по ответам модели нельзя.
func TestPrefixStable(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	i := newTestInterview(t, cases, 3)
	cs := startInterview(t, cases, 6004, 1)

	first, _, err := i.dialog(ctx, cs, i.askPrefix)
	if err != nil {
		t.Fatalf("dialog: %v", err)
	}

	err = addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{
		"round": 1, "questions": []Question{{Key: "case", Text: "какой заказ?"}},
	})
	if err != nil {
		t.Fatalf("add round: %v", err)
	}
	if err := cases.AddAnswer(ctx, reload(t, cases, cs.ID), "заказ 4821"); err != nil {
		t.Fatalf("add answer: %v", err)
	}

	second, _, err := i.dialog(ctx, reload(t, cases, cs.ID), i.askPrefix)
	if err != nil {
		t.Fatalf("dialog after round: %v", err)
	}

	if len(second) <= len(first) {
		t.Fatalf("история не выросла: было %d сообщений, стало %d", len(first), len(second))
	}
	if first[0].Role != "system" || second[0].Role != "system" {
		t.Fatal("первым сообщением обязан идти системный промт")
	}
	if first[0].Parts[0].text != second[0].Parts[0].text {
		t.Error("системный префикс изменился между ходами")
	}
}

// TestDetailOnlyGoesToSummary: уточнение во втором раунде Go снимает (R6), и
// если спросить больше нечего, ход идёт в саммари, а не шлёт автору пустой
// раунд.
func TestDetailOnlyGoesToSummary(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6031, 1)

	turn := `{"kind":"bug","filled":[{"key":"case","value":"сделка 59767187"},` +
		`{"key":"wrong","value":"закрыта дублем"}],"gaps":[],"ready":false,` +
		`"questions":[{"key":"detail","text":"где смотрели?","suggested":"в карточке сделки"}]}`
	i := newTestInterview(t, cases, 2)
	i.llm = fakeLLM(t, turn)

	job := Job{ID: 1, Kind: JobInterview, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := i.Run(ctx, job); err != nil {
		t.Fatalf("run interview: %v", err)
	}

	if n := countJobs(t, pool, JobSummarize, cs.ID); n != 1 {
		t.Errorf("работ саммари: %d, ожидалась 1", n)
	}
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 0 {
		t.Errorf("уточнение второго раунда ушло автору: уведомлений %d", n)
	}
}

// TestFixKeepsOneDetail: правка саммари, пришедшего без раундов, может
// открыть первый раунд, и предел уточнений держится и там: одно, а не два.
func TestFixKeepsOneDetail(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6032, 0)

	if err := addEvent(ctx, pool, cs.ID, "summary_ready", map[string]any{"incomplete": false}); err != nil {
		t.Fatalf("add summary event: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("move to summary: %v", err)
	}
	if err := cases.AddAnswer(ctx, reload(t, cases, cs.ID), "сделка не та"); err != nil {
		t.Fatalf("add answer: %v", err)
	}

	turn := `{"kind":"bug","filled":[{"key":"case","value":"сделка 59767187"},` +
		`{"key":"wrong","value":"закрыта дублем"}],"gaps":[],"ready":false,"questions":[` +
		`{"key":"detail","text":"первое","suggested":"а"},{"key":"detail","text":"второе","suggested":"б"}]}`
	i := newTestInterview(t, cases, 2)
	i.llm = fakeLLM(t, turn)

	job := Job{ID: 1, Kind: JobInterview, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := i.Run(ctx, job); err != nil {
		t.Fatalf("run interview: %v", err)
	}

	questions, err := cases.lastQuestions(ctx, cs.ID)
	if err != nil {
		t.Fatalf("last questions: %v", err)
	}
	if len(questions) != 1 || questions[0].Text != "первое" {
		t.Errorf("вопросы раунда: %+v, ожидалось одно первое уточнение", questions)
	}
}

// TestSummarizeUnclear: метку неполноты и строку «Не уточнено» автору считает
// Go по ядру, и они совпадают. Модель без разделов при пустом ядре тикет не
// останавливает: тело собирается из протокола сырья.
func TestSummarizeUnclear(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	tests := []struct {
		name       string
		kind       string
		contract   string
		gaps       string
		incomplete bool
		unclear    string
		inBody     string
	}{
		{"ядро закрыто", "bug", `{"case": "заказ 4821", "wrong": "статус не сменился"}`, `[]`,
			false, "", "заказ 4821"},
		{"ядро открыто", "bug", `{"case": "заказ 4821"}`, `["wrong"]`,
			true, "Не уточнено: что пошло не так.", "заказ 4821"},
		{"вопрос без ядра", "question", `{}`, `["question"]`,
			true, "Не уточнено: вопрос.", "форма не сохраняется"},
	}
	for n, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := startInterview(t, cases, int64(6040+n), 1)
			_, err := pool.Exec(ctx, `UPDATE cases SET kind = $2, contract = $3, gaps = $4 WHERE id = $1`,
				cs.ID, tt.kind, tt.contract, tt.gaps)
			if err != nil {
				t.Fatalf("set contract: %v", err)
			}
			i := newTestInterview(t, cases, 2)
			i.llm = fakeLLM(t, `{"title":"Форма не сохраняется","brief":"","sections":[{"key":"","heading":"Суть","text":"форма не сохраняется"}]}`)

			job := Job{ID: int64(100 + n), Kind: JobSummarize, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
			if err := i.Summarize(ctx, job); err != nil {
				t.Fatalf("summarize: %v", err)
			}

			got := reload(t, cases, cs.ID)
			if got.Status != statusSummary || got.Incomplete != tt.incomplete {
				t.Errorf("статус %s, incomplete %v; ожидалось summary, %v", got.Status, got.Incomplete, tt.incomplete)
			}
			if !strings.Contains(got.Summary, tt.inBody) {
				t.Errorf("тело без %q:\n%s", tt.inBody, got.Summary)
			}
			var text string
			err = pool.QueryRow(ctx, `SELECT payload->>'text' FROM jobs
				WHERE kind = $1 AND payload->>'case_id' = $2`, JobNotify, cs.ID).Scan(&text)
			if err != nil {
				t.Fatalf("notify job: %v", err)
			}
			if tt.unclear == "" && strings.Contains(text, "Не уточнено") {
				t.Errorf("строка пробела при закрытом ядре:\n%s", text)
			}
			if tt.unclear != "" && !strings.Contains(text, tt.unclear) {
				t.Errorf("нет строки %q:\n%s", tt.unclear, text)
			}
		})
	}
}
