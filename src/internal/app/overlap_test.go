package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOverlapKeepsOnlyRead: номер тикета и путь файла называет модель, а не
// человек. Пункт про то, чего сервис не читал, - выдумка: автор примет её за
// факт и не заведёт нужный тикет.
func TestOverlapKeepsOnlyRead(t *testing.T) {
	issues := []Issue{{Number: 57}, {Number: 68}}
	loaded := []docText{{Path: "CHANGELOG.md"}}

	kept, dropped := keepFound([]overlapItem{
		{Issue: 57, Note: "та же механика"},
		{Issue: 999, Note: "выдуманный номер"},
		{Path: "CHANGELOG.md", Note: "вышло в 0.4.2"},
		{Path: "docs/выдумка.md", Note: "выдуманный путь"},
		{Issue: 68, Note: ""},
		{Issue: 68, Path: "CHANGELOG.md", Note: "и тикет, и документ разом"},
	}, issues, loaded)

	if len(kept) != 2 {
		t.Fatalf("прошло пунктов: %d, ожидалось 2: %+v", len(kept), kept)
	}
	if kept[0].Issue != 57 || kept[1].Path != "CHANGELOG.md" {
		t.Errorf("прошли не те пункты: %+v", kept)
	}
	if dropped != 4 {
		t.Errorf("отброшено пунктов: %d, ожидалось 4", dropped)
	}
}

// TestOverlapCutsListAndText: список - подсказка автору, а не отчёт. Пункты
// сверх предела, повтор одного тикета и заметка на страницу вытолкнули бы кнопки
// саммари в отдельное сообщение, а перевод строки сломал бы пункт в теле issue.
func TestOverlapCutsListAndText(t *testing.T) {
	issues := []Issue{{Number: 1}, {Number: 2}, {Number: 3}, {Number: 4}, {Number: 5}}

	kept, _ := keepFound([]overlapItem{
		{Issue: 1, Note: "раз"}, {Issue: 2, Note: "два"}, {Issue: 1, Note: "повтор"},
		{Issue: 3, Note: "три"}, {Issue: 4, Note: "четыре"}, {Issue: 5, Note: "пять"},
	}, issues, nil)

	if len(kept) != maxOverlapItems {
		t.Fatalf("пунктов в списке: %d, ожидалось %d", len(kept), maxOverlapItems)
	}
	for _, item := range kept {
		if item.Note == "повтор" {
			t.Error("один тикет попал в список дважды")
		}
	}

	long, _ := keepFound([]overlapItem{
		{Issue: 1, Note: "первая строка\nвторая строка " + strings.Repeat("длинно ", 100)},
	}, issues, nil)
	if strings.Contains(long[0].Note, "\n") {
		t.Error("перевод строки остался в заметке")
	}
	if len([]rune(long[0].Note)) > overlapNoteChars {
		t.Errorf("заметка длиной %d рун при пределе %d", len([]rune(long[0].Note)), overlapNoteChars)
	}
}

// TestOverlapTitleCannotStealLink: заголовок тикета пишет посторонний человек, а
// ссылку собирает Go. Скобки внутри заголовка увели бы автора и того, кто возьмёт
// тикет, на чужой адрес.
func TestOverlapTitleCannotStealLink(t *testing.T) {
	issues := []Issue{{
		Number: 57, Title: "Отказы ](https://evil.example) [x",
		HTMLURL: "https://github.com/acme/proj/issues/57",
	}}

	list := overlapList([]overlapItem{{Issue: 57, Note: "та же механика"}},
		issues, Project{Owner: "acme", Repo: "proj"}, "main")

	if strings.Contains(list, "evil.example") {
		t.Errorf("чужой адрес из заголовка попал в список:\n%s", list)
	}
	if !strings.Contains(list, "(https://github.com/acme/proj/issues/57)") {
		t.Errorf("ссылка на тикет собрана не по ответу GitHub:\n%s", list)
	}
}

// TestOverlapSurvivesDeadGitHub: критерий приёмки среза - недоступный репозиторий
// не мешает саммари. Сверка обязана вернуть пустой список молча и не звать модель
// вовсе: клиент модели здесь nil, и обращение к нему уронило бы работу саммари.
func TestOverlapSurvivesDeadGitHub(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	overlap := NewOverlap(NewGitHub("token", server.URL, nil, testLog(t)), nil, testLog(t),
		DialogModel{Name: "test-model"})

	list := overlap.Check(context.Background(), "case-1",
		Project{Owner: "acme", Repo: "proj"}, "Заголовок", "", "## Случай\n\nтекст")

	if list != "" {
		t.Errorf("недоступный GitHub дал список пересечений: %q", list)
	}
}

// TestOverlapListBuildsLinks: адреса собирает Go по номеру и пути - модель их не
// пишет вовсе. Закрытый тикет помечается: для автора это разница между «уже
// делают» и «сделано или отклонено».
func TestOverlapListBuildsLinks(t *testing.T) {
	project := Project{Owner: "acme", Repo: "proj"}
	issues := []Issue{{
		Number: 57, State: "closed", Title: "Автологирование причин отказов",
		HTMLURL: "https://github.com/acme/proj/issues/57",
	}}

	list := overlapList([]overlapItem{
		{Issue: 57, Note: "та же механика"},
		{Path: "CHANGELOG.md", Note: "вышло в 0.4.2"},
	}, issues, project, "main")

	if !strings.Contains(list, "https://github.com/acme/proj/issues/57") {
		t.Errorf("нет ссылки на тикет:\n%s", list)
	}
	if !strings.Contains(list, "Автологирование причин отказов") || !strings.Contains(list, "закрыт") {
		t.Errorf("тикет без заголовка или без пометки о закрытии:\n%s", list)
	}
	if !strings.Contains(list, "https://github.com/acme/proj/blob/main/CHANGELOG.md") {
		t.Errorf("нет ссылки на документ:\n%s", list)
	}
}

// TestOverlapEmptyWithoutItems: сверка ничего не нашла - карточка саммари обязана
// выглядеть как до среза. Пустой список пунктов не имеет права стать блоком с
// заголовком и пустотой под ним.
func TestOverlapEmptyWithoutItems(t *testing.T) {
	kept, dropped := keepFound([]overlapItem{{Issue: 999, Note: "выдумка"}},
		[]Issue{{Number: 57}}, nil)

	if len(kept) != 0 || dropped != 1 {
		t.Fatalf("пунктов: %d, отброшено: %d", len(kept), dropped)
	}
	if list := overlapList(kept, nil, Project{}, "main"); list != "" {
		t.Errorf("пустой список пересечений дал текст: %q", list)
	}
	if strings.Contains(summaryMessage("Заголовок", "", "## Случай\n\nтекст", "", ""),
		"уже есть") {
		t.Error("карточка саммари обещает пересечения, которых нет")
	}
}

// TestIssueBodyKeepsOverlap: список пересечений уходит в тело тикета отдельным
// разделом - ради него срез и делается: берущий тикет видит найденный контекст,
// не восстанавливая его с нуля. Место раздела фиксировано: после ссылок автора и
// до строки «Не уточнено».
func TestIssueBodyKeepsOverlap(t *testing.T) {
	publisher := NewPublisher(nil, nil, testRules(t), testLog(t), 0, "")
	cs := &Case{
		Kind: "feature", Summary: "## Случай\n\nНужно логировать отказы",
		Overlap: "- [Тикет #57 Автологирование](https://github.com/acme/proj/issues/57), закрыт: та же механика",
		Filled:  map[string]string{"need": "логировать отказы"},
	}

	body := publisher.body(cs, User{First: "Иван"}, []string{"https://crm/lead/1"}, "<!-- marker -->")

	overlap := strings.Index(body, "## Пересечения")
	if overlap < 0 || !strings.Contains(body, "Тикет #57") {
		t.Fatalf("пересечений нет в теле тикета:\n%s", body)
	}
	if links := strings.Index(body, "## Ссылки"); links > overlap {
		t.Errorf("пересечения идут раньше ссылок автора:\n%s", body)
	}
	if gaps := strings.Index(body, "Не уточнено"); gaps < overlap {
		t.Errorf("пересечения идут после строки «Не уточнено»:\n%s", body)
	}

	cs.Overlap = ""
	if strings.Contains(publisher.body(cs, User{First: "Иван"}, nil, "<!-- marker -->"), "## Пересечения") {
		t.Error("пустая сверка оставила в тикете пустую рубрику")
	}
}

// TestSummaryShowsOverlapPlain: список хранится в markdown ради тела issue, а в
// чат уходит без разметки - parse_mode сервис не включает, и ссылка обязана
// остаться читаемой сама по себе.
func TestSummaryShowsOverlapPlain(t *testing.T) {
	overlap := "- [Тикет #57 Автологирование](https://github.com/acme/proj/issues/57): та же механика"

	card := summaryMessage("Заголовок", "", "## Случай\n\nтекст", "", overlap)

	if !strings.Contains(card, "Тикет #57 Автологирование (https://github.com/acme/proj/issues/57)") {
		t.Errorf("ссылка в карточке осталась разметкой:\n%s", card)
	}
	if strings.Index(card, "уже есть") > strings.Index(card, "Где я ошибся") {
		t.Errorf("пересечения показаны после вопроса о правке:\n%s", card)
	}
}
