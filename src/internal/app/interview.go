package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	tele "gopkg.in/telebot.v4"
)

const (
	stepInterview = "interview"
	stepSummary   = "summary"
	// Больше трёх вопросов за раз человек не читает, а отвечает на первый.
	maxQuestions = 3
	// Сколько раз один пункт контракта вообще может стать вопросом. Раунд
	// задаёт несколько вопросов, а человек отвечает одной репликой на один из
	// них - остальные модель законно спрашивает снова. Второй заход уместен,
	// третий автор читает как испорченную пластинку.
	maxAsks  = 2
	maxTitle = 80
	// Разделов саммари не больше шести, заголовок - одна строка: длиннее уже
	// не заголовок, а пересказ (Р-6 спеки ticket-form).
	maxSections = 6
	maxHeading  = 60
	// detailKey - вопрос-уточнение вне ядра: адрес объекта, дословный образец.
	// Одно на обращение и только в первом раунде, держит это Go (dropDetails).
	detailKey = "detail"
	// Предел краткого содержания. Два-три предложения о сути помещаются с
	// запасом; всё, что длиннее, - уже пересказ разделов.
	briefLimit = 400
)

var (
	interviewPrompt = mustPrompt("interview.md")
	summaryPrompt   = mustPrompt("summary.md")

	ErrNotInterview = errors.New("case is not in interview")
	ErrStaleRound   = errors.New("round is not current")
	// Ответ по текущему раунду уже принят: кнопку нажали второй раз, пока ход
	// ещё думает. Для автора это не «прошлый вопрос», а тот же самый.
	ErrRoundAnswered = errors.New("round is already answered")
	ErrNoSummary     = errors.New("case has no summary to confirm")
	ErrNoSuggestion  = errors.New("round has no suggestions to accept")
)

// Interview - шаги разговора: добивание контракта раундами вопросов и сборка
// саммари. Побочные эффекты выполняет Go: модель возвращает структуру, Go
// валидирует её и только потом пишет в БД.
type Interview struct {
	cases   *Cases
	llm     Completer
	log     *slog.Logger
	rules   Contract
	model   DialogModel
	rounds  int
	overlap *Overlap

	// Готовые куски системного сообщения: собираются один раз, потому что
	// стабильный префикс не имеет права меняться между вызовами.
	askPrefix  string
	sumPrefix  string
	turnSchema json.RawMessage
}

func NewInterview(cases *Cases, llm Completer, log *slog.Logger, rules Contract, model DialogModel, rounds int, overlap *Overlap) *Interview {
	return &Interview{
		cases:      cases,
		llm:        llm,
		log:        log,
		rules:      rules,
		model:      model,
		rounds:     rounds,
		overlap:    overlap,
		askPrefix:  strings.ReplaceAll(interviewPrompt, "{{CONTRACT}}", rules.Prompt()),
		sumPrefix:  strings.ReplaceAll(summaryPrompt, "{{CONTRACT}}", rules.Prompt()),
		turnSchema: turnSchema(rules),
	}
}

type keyValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Question - вопрос раунда. Suggested - догадка модели: автор подтверждает её
// одной кнопкой, и это экономит ему раунд переписки.
type Question struct {
	Key       string `json:"key"`
	Text      string `json:"text"`
	Suggested string `json:"suggested"`
}

type interviewTurn struct {
	Kind      string     `json:"kind"`
	Filled    []keyValue `json:"filled"`
	Gaps      []string   `json:"gaps"`
	Questions []Question `json:"questions"`
	Ready     bool       `json:"ready"`
}

// Section - раздел саммари под заголовком модели: форма тикета идёт от
// материала, а не от анкеты. Key - пункт ядра, который раздел покрывает, или
// пусто: по нему Go видит, какой закрытый пункт модель не упомянула.
type Section struct {
	Key     string `json:"key"`
	Heading string `json:"heading"`
	Text    string `json:"text"`
}

type summaryOut struct {
	Title    string    `json:"title"`
	Brief    string    `json:"brief"`
	Sections []Section `json:"sections"`
}

// turnSchema строится из правил: список типов обращения задаётся ими же, и
// захардкоженный enum разошёлся бы с контрактом при первой правке.
func turnSchema(rules Contract) json.RawMessage {
	kinds, err := json.Marshal(kindList(rules))
	if err != nil {
		panic("interview schema: " + err.Error())
	}
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"kind": {"type": "string", "enum": ` + string(kinds) + `},
			"filled": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {"key": {"type": "string"}, "value": {"type": "string"}},
					"required": ["key", "value"],
					"additionalProperties": false
				}
			},
			"gaps": {"type": "array", "items": {"type": "string"}},
			"questions": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"key": {"type": "string"},
						"text": {"type": "string"},
						"suggested": {"type": "string"}
					},
					"required": ["key", "text", "suggested"],
					"additionalProperties": false
				}
			},
			"ready": {"type": "boolean"}
		},
		"required": ["kind", "filled", "gaps", "questions", "ready"],
		"additionalProperties": false
	}`)
}

var summarySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"title": {"type": "string"},
		"brief": {"type": "string"},
		"sections": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"key": {"type": "string"},
					"heading": {"type": "string"},
					"text": {"type": "string"}
				},
				"required": ["key", "heading", "text"],
				"additionalProperties": false
			}
		}
	},
	"required": ["title", "brief", "sections"],
	"additionalProperties": false
}`)

// turnsCount - версия разговора: сколько ответов автора он уже вобрал. Ход
// читает её перед вызовом модели и требует неизменности при записи. Пока модель
// думает, автор может дописать - тогда работа хода уже заменена новой, и
// устаревший результат не имеет права лечь в базу поверх свежего.
func (c *Cases) turnsCount(ctx context.Context, db txRunner, caseID string) (int, error) {
	var n int
	err := db.QueryRow(ctx, `
		SELECT count(*) FROM case_events
		WHERE case_id = $1 AND kind = 'answer_given'`, caseID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count answers of case %s: %w", caseID, err)
	}
	return n, nil
}

// isFix - ход идёт после показанного саммари, то есть автор прислал правку.
// Признак выводится из журнала, а не переносится в payload работы: работу
// заменяет каждое следующее сообщение автора, и признак в ней терялся бы на
// втором сообщении подряд.
func (c *Cases) isFix(ctx context.Context, caseID string) (bool, error) {
	var kind string
	err := c.pool.QueryRow(ctx, `
		SELECT kind FROM case_events
		WHERE case_id = $1 AND kind IN ('round_asked', 'summary_ready')
		ORDER BY id DESC LIMIT 1`, caseID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check fix of case %s: %w", caseID, err)
	}
	return kind == "summary_ready", nil
}

// Run - один ход интервью: спросить модель, что уже собрано и чего не хватает,
// и либо задать раунд вопросов, либо перейти к саммари.
func (i *Interview) Run(ctx context.Context, job Job) error {
	var p casePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("payload of %s: %w", job.Kind, err)
	}

	cs, err := i.cases.Load(ctx, p.CaseID)
	if err != nil {
		return err
	}
	// Обращение отменили или разговор ушёл дальше: работа устарела.
	if cs == nil || cs.Status != statusInterview {
		return nil
	}
	if cs.ProjectID == nil {
		return fmt.Errorf("case %s has no project", cs.ID)
	}

	version, err := i.cases.turnsCount(ctx, i.cases.pool, cs.ID)
	if err != nil {
		return err
	}
	fix, err := i.cases.isFix(ctx, cs.ID)
	if err != nil {
		return err
	}
	// Раньше askTurn: checkTurn сверяет по нему, спрашивать ли ещё можно
	// (allExhausted), а не только фильтрует готовый ход после него.
	var asked map[string]int
	if !fix {
		asked, err = i.cases.askedKeys(ctx, cs.ID)
		if err != nil {
			return err
		}
	}

	// После пропуска (Р-15) раунд не откроется, и ход без вопросов при открытом
	// ядре законен: правка саммари иначе упала бы в отказ формата.
	skipped, err := i.cases.skipped(ctx, i.cases.pool, cs.ID)
	if err != nil {
		return err
	}

	messages, _, err := i.dialog(ctx, cs, i.askPrefix)
	if err != nil {
		return err
	}

	turn, err := i.askTurn(ctx, cs, messages, fix, skipped, asked)
	if err != nil {
		return err
	}

	// Исчерпанный пункт снимается с вопросов и остаётся пробелом: тикет уйдёт с
	// пометкой о неполноте, и это честнее третьего повтора. Правка саммари
	// предела не знает, как и предела раундов: автор пришёл уточнять именно
	// этот пункт, и молчание в ответ обесценило бы правку.
	if !fix {
		kept := slices.DeleteFunc(turn.Questions, func(q Question) bool { return asked[q.Key] >= maxAsks })
		if len(kept) < len(turn.Questions) {
			i.log.Info("questions_exhausted", "case_id", cs.ID, "dropped", len(turn.Questions)-len(kept))
		}
		turn.Questions = kept
	}
	// Правка саммари тоже может открыть раунд, и уточнение там подчиняется
	// тому же пределу: номер раунда модели не сообщается.
	kept, dropped := dropDetails(turn.Questions, cs.Round)
	if dropped > 0 {
		i.log.Info("detail_dropped", "case_id", cs.ID, "round", cs.Round+1, "dropped", dropped)
	}
	turn.Questions = kept

	// Предел считается по уже заданным раундам: исчерпав их, ход не спрашивает
	// ничего, а собирает саммари с тем, что есть. Правка саммари предел не
	// проверяет - иначе автор, заметивший ошибку на последнем раунде, не может
	// её исправить.
	toSummary := turn.Ready || len(turn.Questions) == 0 || (!fix && cs.Round >= i.rounds)
	round := cs.Round
	if !toSummary {
		round++
	}
	filled := i.mergeFilled(cs.Filled, turn)
	// Смена типа обращения снимает ключи чужого контракта: собранные ответы
	// исчезают из состояния. Промт менять тип без повода запрещает, но проверить
	// повод нечем, а вот увидеть саму пропажу обязаны - иначе разговор
	// необъяснимо начинает спрашивать заново.
	if cs.Kind != "" && cs.Kind != turn.Kind {
		i.log.Warn("case_kind_changed", "case_id", cs.ID, "from", cs.Kind, "to", turn.Kind,
			"lost_keys", strings.Join(lostKeys(cs.Filled, filled), ","))
	}

	saved, actualRound, actualToSummary, err := i.saveTurn(ctx, cs, turn, filled, round, toSummary, version)
	if err != nil {
		return err
	}
	if !saved {
		// Автор дописал, пока модель думала: его ответ уже поставил свежий ход,
		// и этот результат устарел целиком.
		i.log.Info("interview_turn_stale", "case_id", cs.ID, "round", cs.Round)
		return nil
	}

	// Вопросов в правдивом логе нет, если ход всё же ушёл в саммари (в том
	// числе из-за пропуска, обнаруженного уже внутри saveTurn) - раунда с ними
	// не было.
	questions := len(turn.Questions)
	if actualToSummary {
		questions = 0
	}
	// Ключи пробелов, а не только их число: решение «оставлять ли пункт
	// обязательным» принимается по тому, какой из них не закрывается чаще
	// прочих, и по счётчику этого не увидеть. Ключ - имя пункта контракта,
	// содержимого обращения в нём нет.
	i.log.Info("interview_round", "case_id", cs.ID, "round", actualRound, "kind", turn.Kind,
		"questions", questions, "gaps", len(turn.Gaps),
		"gap_keys", strings.Join(turn.Gaps, ","), "to_summary", actualToSummary)
	return nil
}

// saveTurn кладёт ход разговора: состояние контракта, событие раунда и то, что
// уходит автору либо в следующую работу. Одной транзакцией - иначе вопрос
// уходит автору, а раунд в базе не сохранён.
// saved - лёг ли результат в базу. Ложь означает, что ход устарел: обращение
// отменили или автор дописал, пока модель думала - actualRound/actualToSummary
// тогда не определены. Иначе они называют то, что реально записано: пропуск,
// случившийся, пока модель думала, эта же транзакция обязана увидеть раньше
// записи round (Р-15) - round остаётся прежним, а не round+1 из аргумента.
func (i *Interview) saveTurn(ctx context.Context, cs *Case, turn interviewTurn, filled map[string]string, round int, toSummary bool, version int) (saved bool, actualRound int, actualToSummary bool, err error) {
	err = i.cases.inTx(ctx, func(tx pgx.Tx) error {
		// Версия разговора сверяется внутри той же транзакции: между её чтением
		// и записью автор мог прислать ещё один ответ, и тогда писать этот ход
		// поверх свежего нельзя.
		current, err := i.cases.turnsCount(ctx, tx, cs.ID)
		if err != nil {
			return err
		}
		if current != version {
			return nil
		}

		// Строка блокируется раньше решения "какой раунд писать": так пропуск,
		// случившийся конкурентно, обязан лечь в эту же транзакцию до того, как
		// мы выберем round и toSummary, а не после.
		var status string
		err = tx.QueryRow(ctx, `SELECT status FROM cases WHERE id = $1 FOR UPDATE`, cs.ID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock case %s for turn: %w", cs.ID, err)
		}
		// Обращение отменили, пока модель думала: ни вопроса, ни саммари.
		if status != statusInterview {
			return nil
		}

		actualRound, actualToSummary = round, toSummary
		if !toSummary {
			skipped, err := i.cases.skipped(ctx, tx, cs.ID)
			if err != nil {
				return err
			}
			if skipped {
				actualToSummary = true
				actualRound = cs.Round
				i.log.Info("skip_dropped", "case_id", cs.ID, "dropped", len(turn.Questions))
			}
		}

		contract, err := json.Marshal(filled)
		if err != nil {
			return fmt.Errorf("encode contract of case %s: %w", cs.ID, err)
		}
		gaps, err := json.Marshal(turn.Gaps)
		if err != nil {
			return fmt.Errorf("encode gaps of case %s: %w", cs.ID, err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE cases SET kind = $2, contract = $3, gaps = $4, round = $5, updated_at = now()
			WHERE id = $1`, cs.ID, turn.Kind, contract, gaps, actualRound); err != nil {
			return fmt.Errorf("save turn of case %s: %w", cs.ID, err)
		}
		saved = true

		if actualToSummary {
			if err := addEvent(ctx, tx, cs.ID, "interview_done", map[string]any{
				"round": actualRound, "gaps": turn.Gaps,
			}); err != nil {
				return err
			}
			return replaceJob(ctx, tx, JobSummarize, cs.ID, casePayload{CaseID: cs.ID})
		}

		if err := addEvent(ctx, tx, cs.ID, "round_asked", map[string]any{
			"round": actualRound, "questions": turn.Questions,
		}); err != nil {
			return err
		}
		// Кнопка идёт только под раундом, где есть догадки: обещание подтвердить
		// их одним нажатием обязано совпадать с тем, что автор видит в тексте.
		keys := keysAsk
		if hasSuggestion(turn.Questions) {
			keys = keysRound
		}
		return putNotifyRound(ctx, tx, cs.ID, actualRound, roundMessage(turn.Questions), keys)
	})
	return saved, actualRound, actualToSummary, err
}

// askTurn спрашивает модель и проверяет её ответ. Невалидный ответ - один
// повтор: модель промахивается разово, второй такой же промах означает, что
// дело не в случайности, и работа уходит в повтор очередью.
func (i *Interview) askTurn(ctx context.Context, cs *Case, messages []Message, fix, skipped bool, asked map[string]int) (interviewTurn, error) {
	req := Request{
		Step:       stepInterview,
		CaseID:     cs.ID,
		Model:      i.model.Name,
		Reasoning:  i.model.Reasoning,
		MaxTokens:  llmMaxTokens,
		Messages:   messages,
		SchemaName: "interview_turn",
		Schema:     i.turnSchema,
	}

	var lastErr error
	// Ход без единой догадки годен, но стоит автору лишних минут: кнопке нечего
	// подтверждать. Тратим на догадки первую попытку из двух, а ход держим:
	// второй заход может кончиться и невалидным ответом, и терять из-за этого
	// готовые вопросы дороже, чем отдать раунд без кнопки.
	var noSuggestion *interviewTurn
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := i.llm.Complete(ctx, req)
		if err != nil {
			return interviewTurn{}, err
		}

		var turn interviewTurn
		if err := json.Unmarshal(raw, &turn); err != nil {
			lastErr = fmt.Errorf("decode turn: %w", err)
		} else if err := i.checkTurn(cs.Filled, turn, skipped || !fix && (cs.Round >= i.rounds || allExhausted(turn.Gaps, asked))); err != nil {
			lastErr = err
		} else if attempt == 0 && len(turn.Questions) > 0 && !hasSuggestion(turn.Questions) {
			i.log.Warn("turn_without_suggestion", "step", stepInterview, "case_id", cs.ID,
				"questions", len(turn.Questions))
			noSuggestion = &turn
			continue
		} else {
			return turn, nil
		}
		i.log.Warn("llm_invalid", "step", stepInterview, "case_id", cs.ID,
			"attempt", attempt+1, "error", lastErr)
	}
	if noSuggestion != nil {
		return *noSuggestion, nil
	}
	return interviewTurn{}, fmt.Errorf("interview turn of case %s: %w", cs.ID, lastErr)
}

// hasSuggestion - в раунде есть что подтверждать кнопкой. Вопрос без догадки
// кнопка не закрывает (AcceptRound его пропускает), поэтому раунд из одних
// таких вопросов не должен ни обещать подтверждение, ни показывать кнопку.
func hasSuggestion(questions []Question) bool {
	return slices.ContainsFunc(questions, func(q Question) bool { return !isStub(q.Suggested) })
}

// dropDetails снимает лишние уточнения вне ядра: одно на обращение и только в
// первом раунде (Р-3). round - номер последнего заданного раунда до этого хода,
// так что первый раунд задаётся только при round == 0.
func dropDetails(questions []Question, round int) ([]Question, int) {
	allowed := round == 0
	kept := make([]Question, 0, len(questions))
	for _, q := range questions {
		if q.Key == detailKey {
			if !allowed {
				continue
			}
			allowed = false
		}
		kept = append(kept, q)
	}
	return kept, len(questions) - len(kept)
}

// allExhausted - по каждому пробелу уже спрошено maxAsks раз: дальше спрашивать
// нечем, и ход без единого вопроса - не тупик модели, а законный переход в
// саммари с пометкой о неполноте. Пустой gaps сюда не попадает: это другая
// ошибка (ядро не назвало пробел молчанием), а не исчерпанный лимит.
func allExhausted(gaps []string, asked map[string]int) bool {
	if len(gaps) == 0 {
		return false
	}
	for _, key := range gaps {
		if asked[key] < maxAsks {
			return false
		}
	}
	return true
}

// mergeFilled - состояние контракта после хода: накопленное прошлыми раундами
// плюс свежее. Ключ в gaps переоткрывает пункт, смена типа обращения снимает
// ключи чужого контракта.
func (i *Interview) mergeFilled(prior map[string]string, turn interviewTurn) map[string]string {
	filled := make(map[string]string, len(prior)+len(turn.Filled))
	for key, value := range prior {
		if i.rules.Title(turn.Kind, key) != "" {
			filled[key] = value
		}
	}
	for _, kv := range turn.Filled {
		filled[kv.Key] = strings.TrimSpace(kv.Value)
	}
	for _, key := range turn.Gaps {
		delete(filled, key)
	}
	return filled
}

// lostKeys - пункты, которые были закрыты и после хода закрытыми быть
// перестали. Имена пунктов контракта, содержимого обращения в них нет.
func lostKeys(prior, filled map[string]string) []string {
	var lost []string
	for key := range prior {
		if _, ok := filled[key]; !ok {
			lost = append(lost, key)
		}
	}
	slices.Sort(lost)
	return lost
}

// checkTurn - проверки недоверенного вывода модели. Схема гарантирует форму, а
// смысл проверяет Go: ключи вне ядра, вопрос про закрытый пункт и готовность
// при незакрытом ядре прошли бы схему насквозь. Ядро считается по слитому
// состоянию: контракт копится, и пункт, закрытый прошлым раундом, модель
// повторять не обязана.
func (i *Interview) checkTurn(prior map[string]string, turn interviewTurn, stuck bool) error {
	items := i.rules.Items(turn.Kind)
	if len(items) == 0 {
		return fmt.Errorf("unknown case kind %q", turn.Kind)
	}
	if len(turn.Questions) > maxQuestions {
		return fmt.Errorf("turn has %d questions", len(turn.Questions))
	}

	// Повтор ключа в Filled иначе прошёл бы молча: mergeFilled взял бы
	// последнее значение и потерял первую идею, ту же проверку questions
	// уже делает строкой ниже.
	seenFilled := make(map[string]bool, len(turn.Filled))
	for _, kv := range turn.Filled {
		if i.rules.Title(turn.Kind, kv.Key) == "" {
			return fmt.Errorf("filled key %q is not in contract", kv.Key)
		}
		if seenFilled[kv.Key] {
			return fmt.Errorf("two values for filled key %q", kv.Key)
		}
		seenFilled[kv.Key] = true
	}
	for _, key := range turn.Gaps {
		if i.rules.Title(turn.Kind, key) == "" {
			return fmt.Errorf("gap key %q is not in contract", key)
		}
	}
	// Два вопроса об одном пункте в одном раунде сожгли бы его предел за раз:
	// счётчик заданных вопросов считает по журналу, а не по раундам.
	// Уточнение вне ядра ключа в gaps не имеет, а лишние уточнения снимает
	// dropDetails: ход из-за них не отклоняется.
	seen := make(map[string]bool, len(turn.Questions))
	for _, q := range turn.Questions {
		if q.Key != detailKey && !slices.Contains(turn.Gaps, q.Key) {
			return fmt.Errorf("question about closed key %q", q.Key)
		}
		if strings.TrimSpace(q.Text) == "" {
			return fmt.Errorf("question about %q is empty", q.Key)
		}
		if seen[q.Key] && q.Key != detailKey {
			return fmt.Errorf("two questions about key %q", q.Key)
		}
		// Отписку вместо догадки промт запрещает прямо, а ловил её только
		// момент нажатия «Всё так» - автор к тому времени уже прочитал
		// «Предполагаю: не указано» и потерял доверие к кнопке.
		if strings.TrimSpace(q.Suggested) != "" && isStub(q.Suggested) {
			return fmt.Errorf("question about %q suggests a stub", q.Key)
		}
		seen[q.Key] = true
	}
	missing := i.rules.Missing(turn.Kind, i.mergeFilled(prior, turn))
	if turn.Ready && len(missing) > 0 {
		return fmt.Errorf("turn is ready with %d core gaps", len(missing))
	}
	// Готовность обрывает разговор, и заданное тем же ходом уточнение автору
	// уже не уйдёт: уточнение допустимо и при закрытом ядре.
	if turn.Ready && len(turn.Questions) > 0 {
		return fmt.Errorf("turn is ready with %d questions", len(turn.Questions))
	}
	// Иначе разговор встаёт: не готово, а спросить нечего. Раунды или лимит
	// повторов по оставшимся пробелам исчерпаны (stuck) - ход уходит в
	// саммари с incomplete (toSummary это уже учитывает по пустым Questions),
	// а не крутится в отказах, пока не кончатся попытки очереди.
	if !turn.Ready && len(turn.Questions) == 0 {
		if !stuck {
			return errors.New("turn is not ready and has no questions")
		}
	} else if !turn.Ready && len(turn.Gaps) > 0 &&
		// Открытое ядро спрашивается раньше уточнения: раунд из одного detail
		// при пробеле в ядре тратит вопрос автора мимо того, без чего тикет
		// неполон. Не относится к stuck: там вопросов нет вовсе.
		!slices.ContainsFunc(turn.Questions, func(q Question) bool { return slices.Contains(turn.Gaps, q.Key) }) {
		return errors.New("turn has core gaps but no question about them")
	}
	// Пункт ядра, не закрытый и не названный пробелом, ушёл бы в тикет
	// молчанием. Признаваться в непрочитанном модель обязана.
	for _, key := range missing {
		if !slices.Contains(turn.Gaps, key) {
			return fmt.Errorf("core key %q is neither filled nor in gaps", key)
		}
	}
	return nil
}

// Summarize собирает саммари и показывает его автору. Это последняя точка, где
// ловится неверно прочитанный скриншот: файлы ещё не удалены.
func (i *Interview) Summarize(ctx context.Context, job Job) error {
	var p casePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("payload of %s: %w", job.Kind, err)
	}

	cs, err := i.cases.Load(ctx, p.CaseID)
	if err != nil {
		return err
	}
	if cs == nil || cs.Status != statusInterview {
		return nil
	}
	if cs.ProjectID == nil {
		return fmt.Errorf("case %s has no project", cs.ID)
	}

	version, err := i.cases.turnsCount(ctx, i.cases.pool, cs.ID)
	if err != nil {
		return err
	}

	messages, project, err := i.dialog(ctx, cs, i.sumPrefix)
	if err != nil {
		return err
	}

	out, err := i.askSummary(ctx, cs, messages)
	if err != nil {
		return err
	}

	title := scrubContacts(strings.TrimSpace(out.Title))
	body := i.renderSections(cs, out.Sections)
	brief := briefOf(out.Brief, body)
	// Метку неполноты и строку «Не уточнено» считает Go по ядру, а не модель.
	unclear := i.rules.Unclear(cs.Kind, cs.Filled)
	incomplete := unclear != ""
	// Пусто только при пустом протоколе: показывать автору нечего, и работа
	// уходит в повторы, а исчерпав их - скажет ему об этом.
	if body == "" {
		return fmt.Errorf("summary of case %s has no content", cs.ID)
	}

	// Сверка стоит между готовым саммари и его показом: точка подтверждения у
	// автора остаётся одна, а сверять раньше нечего - черновика ещё нет.
	overlap := i.checkOverlap(ctx, cs, project, version, title, brief, body)

	moved := false
	err = i.cases.inTx(ctx, func(tx pgx.Tx) error {
		// Автор дописал, пока собиралось саммари: его ответ уже поставил новый
		// ход интервью, и показывать саммари без этой правки нельзя.
		current, err := i.cases.turnsCount(ctx, tx, cs.ID)
		if err != nil {
			return err
		}
		if current != version {
			return nil
		}

		tag, err := tx.Exec(ctx, `
			UPDATE cases SET status = 'summary', title = $2, summary = $3, brief = $4,
			                 incomplete = $5, overlap = $6, updated_at = now()
			WHERE id = $1 AND status = 'interview'`, cs.ID, title, body, brief, incomplete, overlap)
		if err != nil {
			return fmt.Errorf("save summary of case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		moved = true

		// Текст саммари ложится снимком в событие, а не читается потом из
		// колонки: колонка держит только последнюю версию, а цепочку правок
		// модель обязана видеть целиком.
		if err := addEvent(ctx, tx, cs.ID, "summary_ready", map[string]any{
			"incomplete": incomplete, "sections": len(out.Sections),
			"title": title, "brief": brief, "body": body, "overlap": overlap,
		}); err != nil {
			return err
		}
		// Ключ по работе, а не по раунду: правка саммари раунд не двигает, и
		// переписанное саммари упёрлось бы в ключ прошлого - автор не увидел бы
		// собственную правку.
		return putNotifyKey(ctx, tx, cs.ID, strconv.FormatInt(job.ID, 10),
			summaryMessage(title, brief, body, unclear, overlap), keysSummary)
	})
	if err != nil {
		return err
	}
	if !moved {
		return nil
	}

	if strings.TrimSpace(out.Brief) == "" {
		i.log.Warn("brief_missing", "case_id", cs.ID, "replaced", brief != "")
	}
	i.log.Info("summary_ready", "case_id", cs.ID, "incomplete", incomplete,
		"overlap", overlap != "", "gap_keys", strings.Join(cs.Gaps, ","),
		"sections", len(out.Sections), "chars", utf8.RuneCountInString(body))
	return nil
}

// checkOverlap сверяет готовый черновик с состоянием проекта. Сначала смотрит,
// не дописал ли автор, пока собиралось саммари: сверка стоит запросов к GitHub и
// хода модели, а показывать это саммари всё равно уже нельзя.
func (i *Interview) checkOverlap(ctx context.Context, cs *Case, project Project,
	version int, title, brief, body string) string {
	if i.overlap == nil {
		return ""
	}
	current, err := i.cases.turnsCount(ctx, i.cases.pool, cs.ID)
	if err != nil || current != version {
		return ""
	}
	return i.overlap.Check(ctx, cs.ID, project, title, brief, body)
}

func (i *Interview) askSummary(ctx context.Context, cs *Case, messages []Message) (summaryOut, error) {
	req := Request{
		Step:       stepSummary,
		CaseID:     cs.ID,
		Model:      i.model.Name,
		Reasoning:  i.model.Reasoning,
		MaxTokens:  llmMaxTokens,
		Messages:   messages,
		SchemaName: "case_summary",
		Schema:     summarySchema,
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := i.llm.Complete(ctx, req)
		if err != nil {
			return summaryOut{}, err
		}

		var out summaryOut
		if err := json.Unmarshal(raw, &out); err != nil {
			lastErr = fmt.Errorf("decode summary: %w", err)
		} else if err := i.checkSummary(cs, out); err != nil {
			lastErr = err
		} else {
			return out, nil
		}
		i.log.Warn("llm_invalid", "step", stepSummary, "case_id", cs.ID,
			"attempt", attempt+1, "error", lastErr)
	}
	return summaryOut{}, fmt.Errorf("summary of case %s: %w", cs.ID, lastErr)
}

func (i *Interview) checkSummary(cs *Case, out summaryOut) error {
	title := strings.TrimSpace(out.Title)
	if title == "" {
		return errors.New("summary has no title")
	}
	if utf8.RuneCountInString(title) > maxTitle {
		return fmt.Errorf("summary title is %d runes long", utf8.RuneCountInString(title))
	}
	// Заголовок «Проблема: не сохраняется форма» тратит место списка на слово,
	// которое и так известно из типа тикета. Промт это запрещает, проверять
	// было некому.
	if word := strings.ToLower(strings.Fields(title)[0]); slices.Contains(titleStopWords, strings.Trim(word, ":-")) {
		return fmt.Errorf("summary title starts with %q", word)
	}

	// Разросшееся краткое содержание перестаёт быть кратким и вытесняет статус с
	// комментарием за край экрана. Пустое не отклоняется: молчание модели не
	// имеет права остановить тикет (решение 2026-08-09), краткое достраивается из
	// первого раздела, а пробел виден в логе.
	brief := strings.TrimSpace(out.Brief)
	if utf8.RuneCountInString(brief) > briefLimit {
		return fmt.Errorf("summary brief is %d runes long", utf8.RuneCountInString(brief))
	}

	// Пустой список разделов не ошибка, пока тело есть из чего собрать: из
	// закрытого ядра. Без ядра модель обязана дать разделы - протокол сырья в
	// тело не идёт, он не обезличен.
	if len(out.Sections) > maxSections {
		return fmt.Errorf("summary has %d sections", len(out.Sections))
	}
	if len(out.Sections) == 0 && !slices.ContainsFunc(i.rules.Items(cs.Kind), func(item ContractItem) bool {
		return strings.TrimSpace(cs.Filled[item.Key]) != ""
	}) {
		return errors.New("summary has no sections and no filled core")
	}
	for idx := range out.Sections {
		s := &out.Sections[idx]
		if err := checkHeading(s.Heading); err != nil {
			return fmt.Errorf("section %d: %w", idx+1, err)
		}
		if s.Key != "" && i.rules.Title(cs.Kind, s.Key) == "" {
			// Тип менялся по ходу интервью (case_kind_changed): ключ из
			// контракта прежнего типа - не повод отклонять весь ответ и
			// жечь попытки, текст остаётся обычным разделом.
			i.log.Warn("section_key_dropped", "case_id", cs.ID, "key", s.Key)
			s.Key = ""
		}
		// В ошибке номер раздела, а не заголовок: она уходит в лог и события, а
		// заголовок собран из материала и может нести имя клиента.
		if strings.TrimSpace(s.Text) == "" {
			return fmt.Errorf("section %d is empty", idx+1)
		}
		// Строка «## Ссылки» внутри текста стала бы в теле тикета вторым
		// заголовком и спорила бы с разделом, который пишет Go.
		if headingLineRe.MatchString(s.Text) {
			return fmt.Errorf("section %d text has a heading line", idx+1)
		}
	}
	return nil
}

// checkHeading - заголовок раздела от модели становится строкой «## ...» в
// теле issue: перевод строки, решётка или разметка в нём ломают тело, а
// занятое имя спорит с разделом, который пишет Go.
func checkHeading(heading string) error {
	heading = strings.TrimSpace(heading)
	switch {
	case heading == "":
		return errors.New("section has no heading")
	case utf8.RuneCountInString(heading) > maxHeading:
		return fmt.Errorf("section heading is %d runes long", utf8.RuneCountInString(heading))
	case strings.ContainsAny(heading, "\n\r#<"):
		return errors.New("section heading has markup")
	case slices.ContainsFunc(reservedHeadings, func(r string) bool { return strings.EqualFold(r, heading) }):
		return fmt.Errorf("section heading %q is reserved", heading)
	}
	return nil
}

// dialog собирает сообщения запроса. Порядок обязателен: стабильный префикс
// первым сообщением, протокол сырья вторым, история раундов последней. Любая
// изменяющаяся строка перед промтом молча гасит кэш провайдера.
func (i *Interview) dialog(ctx context.Context, cs *Case, prefix string) ([]Message, Project, error) {
	project, err := LoadProject(ctx, i.cases.pool, *cs.ProjectID)
	if err != nil {
		return nil, Project{}, err
	}

	messages := dialogMessages(prefix, project.Context, cs.Protocol)

	history, err := i.cases.history(ctx, cs.ID)
	if err != nil {
		return nil, Project{}, err
	}
	return append(messages, history...), project, nil
}

// AddAnswer принимает ответ автора: текстом, расшифровкой голосового или
// подтверждением раунда. Живёт рядом с состоянием, а не с моделью: ответ надо
// сохранить и поставить следующий ход, а спрашивать модель будет работа.
//
// Ответ при показанном саммари - это правка: обращение возвращается в интервью
// и получает ход без проверки предела раундов.
func (c *Cases) AddAnswer(ctx context.Context, cs *Case, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if cs.Status != statusInterview && cs.Status != statusSummary {
		return ErrNotInterview
	}

	moved := false
	err := c.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE cases SET status = 'interview', updated_at = now()
			WHERE id = $1 AND status IN ('interview', 'summary')`, cs.ID)
		if err != nil {
			return fmt.Errorf("accept answer of case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		moved = true

		if err := addEvent(ctx, tx, cs.ID, "answer_given", map[string]any{
			"round": cs.Round, "text": text,
		}); err != nil {
			return err
		}

		// Человек дописывает вторым сообщением: замена снимает ещё не начатый
		// ход, и модель отвечает один раз на всё сразу. Правка это или обычный
		// ответ, решает сам ход по журналу - в работе этот признак терялся бы
		// при замене.
		return replaceJob(ctx, tx, JobInterview, cs.ID, casePayload{CaseID: cs.ID})
	})
	if err != nil {
		return err
	}
	if !moved {
		return ErrNotInterview
	}

	cs.Status = statusInterview
	c.log.Info("answer_given", "case_id", cs.ID, "round", cs.Round,
		"chars", utf8.RuneCountInString(text))
	return nil
}

// AcceptRound - кнопка «Всё так»: предположения модели становятся ответом
// целиком. Номер раунда приходит из callback_data и сверяется с текущим: кнопка
// прошлого раунда осталась в чате и не должна закрывать чужие вопросы.
func (c *Cases) AcceptRound(ctx context.Context, cs *Case, round int) error {
	if cs.Status != statusInterview {
		return ErrNotInterview
	}
	if round != cs.Round {
		return ErrStaleRound
	}

	// На раунд уже отвечали: кнопка нажата второй раз, пока первый ход ещё
	// думает. Номер раунда этого не ловит - он меняется только следующим ходом,
	// поэтому смотрим, что было последним событием разговора.
	answered, err := c.roundAnswered(ctx, cs.ID)
	if err != nil {
		return err
	}
	if answered {
		return ErrRoundAnswered
	}

	questions, err := c.lastQuestions(ctx, cs.ID)
	if err != nil {
		return err
	}
	if len(questions) == 0 {
		return ErrStaleRound
	}

	// Вопрос без догадки кнопка не закрывает: отписка стала бы подтверждённым
	// ответом, которого автор не видел. Такой пункт остаётся открытым.
	var b strings.Builder
	for _, q := range questions {
		if isStub(q.Suggested) {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", q.Text, strings.TrimSpace(q.Suggested))
	}
	if b.Len() == 0 {
		return ErrNoSuggestion
	}
	return c.AddAnswer(ctx, cs, b.String())
}

// isStub - догадки нет: пусто или отписка вроде «не указано». Промт такие
// строки запрещает, но кнопка «Всё так» превращает догадку в слова автора, а
// выдуманный ответ дороже лишнего вопроса: сомнительную строку лучше не
// принять, чем принять.
func isStub(text string) bool {
	text = strings.ToLower(strings.Trim(text, " .,:;-"))
	if text == "" {
		return true
	}
	// Отписка на этих словах обрывается: «дата не указана». Тот же оборот
	// внутри фразы - факт о сервисе: «в заказе не указан адрес».
	for _, tail := range stubTails {
		if strings.HasSuffix(text, tail) {
			return true
		}
	}
	return slices.ContainsFunc(stubPhrases, func(p string) bool { return strings.Contains(text, p) })
}

// roundAnswered - последним событием разговора идёт ответ или пропуск, а не
// вопрос. Значит текущий раунд закрыт и подтверждать в нём нечего.
func (c *Cases) roundAnswered(ctx context.Context, caseID string) (bool, error) {
	kind, err := lastRoundEvent(ctx, c.pool, caseID)
	if err != nil {
		return false, err
	}
	return kind != "" && kind != "round_asked", nil
}

// lastRoundEvent - последнее событие раунда среди троицы (docs/specs/ticket-form.md):
// вопрос, ответ, пропуск. Пустая строка - раунда с таким событием ещё не было.
func lastRoundEvent(ctx context.Context, db txRunner, caseID string) (string, error) {
	var kind string
	err := db.QueryRow(ctx, `
		SELECT kind FROM case_events
		WHERE case_id = $1 AND kind IN ('round_asked', 'answer_given', 'questions_skipped')
		ORDER BY id DESC LIMIT 1`, caseID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check last round event of case %s: %w", caseID, err)
	}
	return kind, nil
}

// skipped - в обращении уже случился пропуск вопросов (Р-15): раунд не
// открывается больше ни ходом, ни правкой саммари до самой публикации.
// db - пул или транзакция: saveTurn обязан увидеть пропуск в своей же
// транзакции, остальные вызовы читают вне неё.
func (c *Cases) skipped(ctx context.Context, db txRunner, caseID string) (bool, error) {
	var exists bool
	err := db.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM case_events WHERE case_id = $1 AND kind = 'questions_skipped')`,
		caseID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check skip of case %s: %w", caseID, err)
	}
	return exists, nil
}

// SkipQuestions - «Отправить как есть»: раунд закрывается без ответа, дальше
// работу довершает саммари (Р-5 ticket-form: саммари - последняя точка перед
// публикацией). Статус и cases.round не трогает - их меняет Summarize.
func (c *Cases) SkipQuestions(ctx context.Context, cs *Case, round int) error {
	if cs.Status != statusInterview {
		c.log.Info("skip_refused", "case_id", cs.ID, "round", round, "reason", "not_interview")
		return ErrNotInterview
	}
	if round != cs.Round {
		c.log.Info("skip_refused", "case_id", cs.ID, "round", round, "reason", "stale_round")
		return ErrStaleRound
	}

	err := c.inTx(ctx, func(tx pgx.Tx) error {
		// Блокировка строки через updated_at, как у ответа: пропуск - тоже
		// действие автора, таймер черновика сдвигается так же.
		tag, err := tx.Exec(ctx, `
			UPDATE cases SET updated_at = now()
			WHERE id = $1 AND status = 'interview' AND round = $2`, cs.ID, round)
		if err != nil {
			return fmt.Errorf("lock case %s for skip: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrStaleRound
		}

		kind, err := lastRoundEvent(ctx, tx, cs.ID)
		if err != nil {
			return err
		}
		switch kind {
		case "":
			return ErrStaleRound
		case "round_asked":
		default:
			return ErrRoundAnswered
		}

		if err := addEvent(ctx, tx, cs.ID, "questions_skipped", map[string]any{"round": round}); err != nil {
			return err
		}
		return replaceJob(ctx, tx, JobSummarize, cs.ID, casePayload{CaseID: cs.ID})
	})
	switch {
	case errors.Is(err, ErrStaleRound):
		c.log.Info("skip_refused", "case_id", cs.ID, "round", round, "reason", "stale_round")
		return err
	case errors.Is(err, ErrRoundAnswered):
		c.log.Info("skip_refused", "case_id", cs.ID, "round", round, "reason", "round_answered")
		return err
	case err != nil:
		return err
	}

	c.log.Info("questions_skipped", "case_id", cs.ID, "round", round)
	return nil
}

// askedKeys - сколько раз каждый пункт контракта уже становился вопросом.
// Считается по журналу: раунды переживают рестарт, а память процесса нет.
func (c *Cases) askedKeys(ctx context.Context, caseID string) (map[string]int, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT payload FROM case_events
		WHERE case_id = $1 AND kind = 'round_asked'`, caseID)
	if err != nil {
		return nil, fmt.Errorf("query asked rounds of case %s: %w", caseID, err)
	}
	defer rows.Close()

	asked := map[string]int{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("scan asked round: %w", err)
		}
		var p struct {
			Questions []Question `json:"questions"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("decode asked round: %w", err)
		}
		for _, q := range p.Questions {
			asked[q.Key]++
		}
	}
	return asked, rows.Err()
}

func (c *Cases) lastQuestions(ctx context.Context, caseID string) ([]Question, error) {
	var payload []byte
	err := c.pool.QueryRow(ctx, `
		SELECT payload FROM case_events
		WHERE case_id = $1 AND kind = 'round_asked'
		ORDER BY id DESC LIMIT 1`, caseID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load last round of case %s: %w", caseID, err)
	}

	var p struct {
		Questions []Question `json:"questions"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("decode last round of case %s: %w", caseID, err)
	}
	return p.Questions, nil
}

// RoundView - вопросы последнего раунда вместе с числом ответов после него:
// живая версия того, что markRound показывает на экране. Живёт в БД, а не в
// памяти процесса - экран обязан пережить рестарт.
func (c *Cases) RoundView(ctx context.Context, caseID string) ([]Question, int, error) {
	questions, err := c.lastQuestions(ctx, caseID)
	if err != nil {
		return nil, 0, err
	}

	var answers int
	err = c.pool.QueryRow(ctx, `
		SELECT count(*) FROM case_events
		WHERE case_id = $1 AND kind = 'answer_given'
		  AND id > COALESCE(
		      (SELECT max(id) FROM case_events WHERE case_id = $1 AND kind = 'round_asked'), 0)`,
		caseID).Scan(&answers)
	if err != nil {
		return nil, 0, fmt.Errorf("count round answers of case %s: %w", caseID, err)
	}
	return questions, answers, nil
}

// ConfirmSummary - кнопка «Публикую». Ключ работы без счётчика: issue у
// обращения ровно один, и повторное нажатие обязано упереться в тот же ключ.
func (c *Cases) ConfirmSummary(ctx context.Context, cs *Case) error {
	if cs.Status != statusSummary {
		return ErrNoSummary
	}

	confirmed := false
	err := c.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE cases SET status = 'publishing', updated_at = now()
			WHERE id = $1 AND status = 'summary'`, cs.ID)
		if err != nil {
			return fmt.Errorf("confirm summary of case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		confirmed = true

		if err := addEvent(ctx, tx, cs.ID, "summary_confirmed", nil); err != nil {
			return err
		}

		// Прошлая попытка могла исчерпать повторы и остаться в очереди
		// погашенной: замена возвращает публикацию в работу.
		return replaceJob(ctx, tx, JobPublish, cs.ID, casePayload{CaseID: cs.ID})
	})
	if err != nil {
		return err
	}
	if !confirmed {
		return ErrNoSummary
	}

	cs.Status = statusPublishing
	c.log.Info("summary_confirmed", "case_id", cs.ID, "user_id", cs.UserID)
	return nil
}

// AddVoiceAnswer принимает голосовой ответ на вопрос интервью. Расшифровка
// уходит работой, а не синхронным вызовом: апдейты Telegram обрабатываются
// последовательно, и минута ожидания модели остановила бы бота для всех авторов.
func (c *Cases) AddVoiceAnswer(ctx context.Context, bot *tele.Bot, cs *Case, msg *tele.Message) error {
	if cs.Status != statusInterview && cs.Status != statusSummary {
		return ErrNotInterview
	}
	file := msg.Voice.File
	if file.FileSize > maxFileSize {
		return ErrFileTooBig
	}

	var itemID int64
	err := c.pool.QueryRow(ctx, `
		INSERT INTO case_items (case_id, kind, tg_message_id, tg_file_id, source_text, mime)
		VALUES ($1, 'voice', $2, $3, $4, $5) RETURNING id`,
		cs.ID, msg.ID, file.FileID, msg.Caption, valueOr(msg.Voice.MIME, "audio/ogg")).Scan(&itemID)
	if err != nil {
		return fmt.Errorf("insert answer of case %s: %w", cs.ID, err)
	}

	if err := c.download(ctx, bot, cs, itemID, file); err != nil {
		return err
	}
	key := fmt.Sprintf("%s:%s:%d", JobNormalizeVoice, cs.ID, itemID)
	return PutJob(ctx, c.pool, JobNormalizeVoice, key, itemPayload{CaseID: cs.ID, ItemID: itemID})
}

// AfterVoice решает, куда двигаться после расшифровки. Один и тот же шаг
// нормализации обслуживает и сырьё сбора, и ответ в интервью: сырьё
// превращается в текст одинаково, а что с ним делать - зависит от состояния
// разговора, а не от самой записи.
func (c *Cases) AfterVoice(ctx context.Context, caseID, text string) error {
	cs, err := c.Load(ctx, caseID)
	if err != nil {
		return err
	}
	if cs == nil {
		return nil
	}
	if cs.Status == statusInterview || cs.Status == statusSummary {
		return c.AddAnswer(ctx, cs, text)
	}
	return c.AdvanceNormalize(ctx, caseID)
}

// AfterVoiceFail - та же развилка для нераспознанной записи. В сборе провал
// виден автору строкой протокола, а в разговоре его заметить нечем: молчание
// бота автор читает как «он думает».
func (c *Cases) AfterVoiceFail(ctx context.Context, caseID string, itemID int64) error {
	cs, err := c.Load(ctx, caseID)
	if err != nil {
		return err
	}
	if cs == nil {
		return nil
	}
	if cs.Status == statusInterview || cs.Status == statusSummary {
		return putNotifyKey(ctx, c.pool, caseID, fmt.Sprintf("voicefail-%d", itemID),
			msgVoiceUnrecognized, "")
	}
	return c.AdvanceNormalize(ctx, caseID)
}

// briefOf - краткое содержание: своё от модели или начало первого раздела
// саммари, когда модель промолчала. Молчание не имеет права остановить тикет
// (решение 2026-08-09), а пустая рубрика в issue и карточка без описания хуже
// пересказа.
//
// Замена берётся из готового тела, а не из сырых разделов ответа: в теле они уже
// обезличены и расставлены по контракту. Резать раньше обезличивания нельзя -
// телефон, разорванный границей среза, перестал бы совпадать с шаблоном и уехал
// бы в тикет.
func briefOf(brief, body string) string {
	if text := strings.TrimSpace(brief); text != "" {
		return scrubContacts(text)
	}
	first := firstSection(body)
	// Заголовок раздела в краткое содержание не идёт: рубрика в issue уже
	// называется «Кратко», а карточка печатает текст без названия поля.
	if _, text, found := strings.Cut(first, "\n\n"); found {
		first = text
	}
	return cutRunes(strings.TrimSpace(first), briefLimit)
}

// firstSection - первый раздел саммари вместе с его названием: обычно «Какую
// задачу это решает». Карточка тикета без краткого содержания показывает его, а
// не начало всего тела: за пределом в 400 символов иначе оказывался хвост одного
// раздела и обрывок следующего.
func firstSection(body string) string {
	body = strings.TrimSpace(body)
	if next := strings.Index(body, "\n## "); next > 0 {
		return body[:next]
	}
	return body
}

// cutRunes режет текст по числу символов, а не байтов: русская строка весит по
// два байта на символ, и байтовый предел обрезал бы её вдвое раньше.
func cutRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit])
}

// plainText убирает markdown: Telegram его не рендерит, а parse_mode включать
// нельзя - тексты приходят из GitHub, и непарная звёздочка в чужом комментарии
// уронила бы отправку. Таблиц у Telegram нет вовсе, поэтому строка таблицы
// становится перечислением через дефис.
func plainText(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, " ")
		if isTableRule(line) {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "|") {
			line = tableRow(line)
		}
		line = headingRe.ReplaceAllString(line, "")
		line = mdLinkRe.ReplaceAllString(line, "$1 ($2)")
		line = strings.ReplaceAll(line, "**", "")
		line = strings.ReplaceAll(line, "`", "")
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// isTableRule - строка-разделитель шапки таблицы: показывать её нечем, в
// перечислении она превратилась бы в строку из дефисов.
func isTableRule(line string) bool {
	return strings.Contains(line, "---") && tableRuleRe.MatchString(line)
}

// tableRow превращает строку таблицы в перечисление: ячейки через дефис.
func tableRow(line string) string {
	cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
	for n, cell := range cells {
		cells[n] = strings.TrimSpace(cell)
	}
	return strings.Join(cells, " - ")
}

var (
	headingRe = regexp.MustCompile(`^#{1,6}\s+`)
	// headingLineRe - строка текста, которую markdown прочтёт заголовком.
	headingLineRe = regexp.MustCompile(`(?m)^\s*#{1,6}(\s|$)`)
	tableRuleRe   = regexp.MustCompile(`^[\s|:-]+$`)
	mdLinkRe      = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
)

// Структурные персональные данные вырезаются детерминированно до записи в
// тикет; остальное (ФИО, переписка) - забота промтов, последняя защита - автор
// видит саммари. Шаблоны намеренно узкие: широкий съел бы номера заказов.
var (
	emailRe = regexp.MustCompile(`[\p{L}\d._%+-]+@[\p{L}\d.-]+\.[\p{L}]{2,}`)
	// Телефон опознаётся по форме, а не по длине: либо разделители внутри
	// номера, либо одиннадцать цифр с 7 или 8 в начале. Голая цепочка цифр
	// телефоном не считается - это номер заказа.
	phoneRe = regexp.MustCompile(`(?:\+\d{1,3}[\s(-]?)?\d{3}[\s)-]\d{3}[\s-]\d{2}[\s-]\d{2}|\b[78]\d{10}\b`)
	cardRe  = regexp.MustCompile(`\b\d{4}[\s-]\d{4}[\s-]\d{4}[\s-]\d{4}\b`)
)

func kindList(rules Contract) []string {
	kinds := make([]string, 0, len(rules))
	for _, kind := range caseKinds {
		if len(rules[kind]) > 0 {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}
