package app

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	tele "gopkg.in/telebot.v4"
)

// Подписи нижней панели: хендлер telebot ловит их по тексту, поэтому значение
// не меняется этим срезом (рубеж 3a, TestPanelButtonsUnchanged).
const (
	buttonDone  = "Готово"
	buttonMenu  = "Меню"
	buttonReset = "Сброс"
)

// Описания команд бота (SetCommands).
const (
	commandMenu       = "Меню"
	commandTickets    = "Мои тикеты"
	commandAddProject = "Добавить проект"
	commandReset      = "Сброс"
)

const msgHandlerFailed = "Действие не прошло: техническая ошибка. Попробуйте ещё раз."

// homeText - начальный экран: один текст и для первого показа, и для
// возврата, иначе правка сообщения даёт другой заголовок на том же месте.
const homeText = "Выберите проект или добавьте новый:"

// projectHelp - подсказка. Один пример важнее описания синтаксиса: ссылку
// копируют со страницы репозитория и присылают как есть.
const projectHelp = "Пришлите ссылку на репозиторий проекта:\n" +
	"https://github.com/owner/repo\n\n" +
	"Название и описание я соберу сам по README. Можно задать их явно:\n" +
	"owner/repo Название | Чем занят сервис"

// newsTail - общий хвост новости по тикету: короткая и длинная новость ведут
// в одну карточку одним и тем же приглашением.
const newsTail = "Открыть - кнопкой ниже или в «Мои тикеты»."

// R9 (docs/specs/ticket-form.md §10, AGENTS.md принцип 6): все строки для
// пользователя на русском, адресованные чату - сообщения, правки экрана,
// toast, подписи кнопок, описания команд, уведомления владельцу. Ввод модели,
// тело issue, метки и словари сверки - texts_model.go.

// alertPublished и alertCancelled - уведомление владельцу: короткая шапка о
// движении тикета в отдельный чат, куда пишут и алерты контура. Адресат - чат
// из конфига, а не роль в сервисе: кто читает ленту, решается составом чата.
func alertPublished(p Project, cs *Case, author User, number int, url string, incomplete, onBoard bool) string {
	text := alertMessage("Новый тикет", p, cs, author, number, url)
	if incomplete {
		text += "\nКонтракт недобран: тикет помечен incomplete."
	}
	if !onBoard {
		text += "\nНа доску Galera не добавлен: добавьте вручную."
	}
	return text
}

func alertCancelled(p Project, cs *Case, author User, number int, url string) string {
	return alertMessage("Тикет отменён автором", p, cs, author, number, url)
}

func alertMessage(head string, p Project, cs *Case, author User, number int, url string) string {
	return fmt.Sprintf("%s: %s\n%s\nАвтор: %s\n#%d %s",
		head, p.Slug, cs.Title, authorName(author), number, url)
}

// Тексты новостей короткие намеренно: сообщение несёт факт и кнопку перехода, а
// содержание автор читает в карточке. Иначе десяток активных тикетов превращает
// чат в ленту, которую перестают читать.
func statusNews(p Project, number int, s Status) string {
	return fmt.Sprintf("Тикет #%d (%s): статус «%s».\n%s", number, p.Title, s.Title, newsTail)
}

func commentNews(p Project, number int) string {
	return fmt.Sprintf("По тикету #%d (%s) появился комментарий разработчика.\n%s",
		number, p.Title, newsTail)
}

func msgCancelIssueGone(number int) string {
	return fmt.Sprintf("Тикета #%d уже нет в GitHub, отменять нечего.", number)
}

func msgCancelAlreadyClosed(number int, status string) string {
	return fmt.Sprintf("Тикет #%d уже закрыт со статусом «%s», отменять нечего.", number, status)
}

func msgCancelled(number int) string {
	return fmt.Sprintf("Тикет #%d отменён и закрыт.", number)
}

// projectCard показывает и то, откуда взялось описание: контекст уходит в
// инструкцию интервью и определяет вопросы по всем будущим обращениям проекта,
// поэтому придуманное моделью автор должен отличать от своего.
func projectCard(p ProjectConfig, source string) string {
	return fmt.Sprintf("Проект «%s» заведён и появился в меню.\nРепозиторий: %s/%s\n\n%s\n\n%s\n"+
		"Переписать: /project %s/%s Название | Описание",
		p.Title, p.Owner, p.Repo, p.Context, projectSourceNote(source), p.Owner, p.Repo)
}

// Подписи кнопок навигации и действий бота (markup.Data, reply-клавиатура).
// Одно действие - одно название на всех экранах (§11): переход в меню
// проекта отовсюду зовётся «Меню проекта», а не «Назад» - так видно, куда
// ведёт кнопка, как «К списку».
const (
	buttonAddProject   = "Добавить проект"
	buttonAllTrue      = "Всё так"
	buttonAsk          = "Спросить"
	buttonBackToList   = "К списку"
	buttonCancelTicket = "Отменить тикет"
	buttonContinue     = "Продолжить"
	buttonCreateTicket = "Создать тикет"
	buttonEndAsk       = "Закончить разговор"
	buttonFix          = "Поправить"
	buttonNextPage     = "Следующие"
	buttonPrevPage     = "Предыдущие"
	buttonProjectMenu  = "Меню проекта"
	buttonPublish      = "Публикую"
	buttonResetNo      = "Оставить"
	buttonResetYes     = "Да, сбросить"
	buttonSkip         = "Отправить как есть"
	buttonToProjects   = "К проектам"
	buttonViewTickets  = "Посмотреть тикеты"
)

func buttonOpenTicket(number int) string {
	return fmt.Sprintf("Открыть тикет #%d", number)
}

// Мгновенные отклики на нажатие кнопки (toast).
const (
	toastAccepted         = "Принято"
	toastAwaitingFix      = "Жду правку"
	toastAwaitingLink     = "Жду ссылку"
	toastCancellingTicket = "Отменяю тикет"
	toastCollecting       = "Собираю обращение"
	toastContinuing       = "Продолжаем"
	toastEndingAsk        = "Заканчиваю"
	toastKeeping          = "Оставляю"
	toastListening        = "Слушаю вопрос"
	toastOpeningProject   = "Открываю проект"
	toastOpeningProjects  = "Открываю проекты"
	toastOpeningTicket    = "Открываю тикет"
	toastOpeningTickets   = "Открываю тикеты"
	toastPublishing       = "Публикую"
	toastResetting        = "Сбрасываю"
	toastStale            = "Этот экран устарел"
	toastToTicket         = "Перевожу в тикет"
)

// Сообщения бота в чате: экраны, ответы на команды и кнопки. Дубли значений
// (один текст на нескольких экранах) слиты в одну константу на этапе стиля:
// на S1 они проверялись отдельно по числу литералов, дальше это уже не нужно.
const (
	msgAcceptedItem      = "Принял. "
	msgAccessDenied      = "Доступ к боту закрыт. Напишите владельцу сервиса."
	msgAllTrueAccepted   = "Принято: всё так. Думаю дальше."
	msgAlreadyPublishing = "Уже публикую. Пришлю номер, как только тикет заведётся."
	msgAnswerAccepted    = "Ответ принят. Думаю дальше."
	msgAnswerNeedText    = "Ответьте текстом или голосовым. Скриншоты принимаются только до кнопки «Готово»."
	msgAnsweringDocs     = "Отвечаю по документации проекта."
	msgAskWhatToKnow     = "Что хотите узнать? Напишите или наговорите вопрос и нажмите «Готово»."
	msgAwaitingFixNote   = "Жду правку: напишите или наговорите, перепишу и покажу снова."
	msgBotIntro          = "Я завожу тикеты по обращениям сотрудников."
	msgCancelNotAuthor   = "Отменить тикет может только его автор."
	msgCaseClosed        = "Обращение уже закрыто."
	msgCaseMovedOn       = "Обращение уже ушло дальше. Нажмите «Меню» и заведите новое."
	msgCaseReset         = "Обращение отменено, файлы удалены."
	msgCollectingCase    = "Собираю обращение."
	msgConfirmReset      = "Обращение будет отменено безвозвратно, вместе с файлами. Сбросить?"
	msgEditNotSeen       = "Правку прежнего сообщения я не вижу. Пришлите исправленное " +
		"отдельным сообщением - оно добавится к обращению."
	msgKeptGoing     = "Оставил, продолжаем."
	msgLetsStart     = "Начнём."
	msgNoNewQuestion = "Нового вопроса не вижу. Напишите или наговорите его и нажмите «Готово»."
	msgNoProjectsYet = "Проекты ещё не заведены. Добавьте первый:"
	msgNoSuggestion  = "Догадок у меня нет, подтверждать нечего. Ответьте текстом или голосовым."
	msgNotSaved      = "Это сообщение я не сохраняю: сбор начинается кнопкой. Выберите проект, " +
		"нажмите «Создать тикет» и пришлите материал ещё раз."
	msgNothingToFinish     = "Сейчас нечего завершать. Нажмите «Меню» и начните с проекта."
	msgNothingToParse      = "Пока нечего разбирать. Пришлите текст, голосовое или скриншот."
	msgNothingToReset      = "Сбрасывать нечего."
	msgParsingMaterial     = "Разбираю материал. Отвечу, как закончу."
	msgPrivateOnly         = "Бот работает только в личной переписке."
	msgProjectDisabled     = "Проект недоступен: его выключили."
	msgPublishingNow       = "Публикую. Пришлю номер и ссылку."
	msgQuestionAccepted    = "Принял вопрос, смотрю документацию."
	msgReadyWhatNext       = "Готово. Что дальше?"
	msgResetDone           = "Сброшено."
	msgResumingCase        = "Продолжаем прежнее обращение."
	msgRoundQuestionsTitle = "Раунд вопросов"
	msgSendMaterial        = "Присылайте материал и нажмите «Готово»."
	msgSendMaterialCollect = "Присылайте текст, голосовые и скриншоты. Когда закончите, нажмите «Готово»."
	msgSendRepoLink        = "Пришлите ссылку на репозиторий следующим сообщением, " +
		"например https://github.com/owner/repo"
	msgSkippedSummary = "Отправляю как есть. Собираю итог."
	msgSlugTaken      = "Проект с таким именем уже заведён на другой репозиторий. " +
		"Напишите владельцу сервиса - выключить проект из бота пока может только он вручную."
	msgStartOver           = "Начнём заново."
	msgStillWorking        = "Всё ещё разбираю. Иногда это занимает до 5-10 минут - отвечу, как закончу."
	msgSummaryRewriting    = "Переписываю текст с вашей правкой. Покажу заново - тогда и опубликуем."
	msgTalkAlreadyEnded    = "Разговор уже закончен."
	msgTalkClosedToTicket  = "Разговор уже закрыт."
	msgTalkEnded           = "Разговор закончен."
	msgTicketLeaving       = "Тикет уже уходит в GitHub, отменить не получится. Пришлю номер."
	msgTicketNotFound      = "Тикет не найден."
	msgTicketsPageSuffix   = ", страница %d из %d"
	msgToTicketDone        = "Перевожу в тикет."
	msgTranscribing        = "Расшифровываю ответ."
	msgVoiceFailed         = "Не получилось принять запись. Ответьте ещё раз."
	msgVoiceTooBig         = "Запись тяжелее 20 МБ, Telegram не отдаёт её боту. Ответьте текстом или короче."
	msgWhichProjectTickets = "Тикеты какого проекта показать?"
	msgFullInTicket        = "...\n\nПолностью - в тикете по ссылке ниже."
	msgStatusUnavailable   = "статус недоступен"
	msgLastComment         = "\nПоследний комментарий:\n"
)

func msgAnswersAccepted(answers int) string {
	return fmt.Sprintf("Принято ответов: %d. Думаю дальше.", answers)
}

func msgCancellingTicket(number int) string {
	return fmt.Sprintf("Отменяю тикет #%d, сообщу, когда закроется.", number)
}

// msgContinueOtherProject - обращение уже собирается в другом проекте: голова
// и хвост вокруг названия проекта раньше были двумя константами (перенос S1),
// на стиле собраны в одну функцию.
func msgContinueOtherProject(title string) string {
	return "Это обращение уже собирается в проекте «" + title + "». " +
		"Продолжайте его или нажмите «Сброс», чтобы завести новое."
}

// msgProjectOpened - карточка проекта из меню: и выбор из списка, и возврат
// после /start показывают один и тот же текст.
func msgProjectOpened(title string) string {
	return "Проект «" + title + "». Что делаем?"
}

func msgProjectChosen(title string) string {
	return "Проект «" + title + "» выбран."
}

func msgNoTickets(title string) string {
	return "По проекту «" + title + "» тикетов ещё нет."
}

func msgTicketsOfProject(title string) string {
	return "Тикеты проекта «" + title + "»"
}

func msgItemsAccepted(count int) string {
	return fmt.Sprintf("Принято сообщений: %d. Закончите - нажмите «Готово».", count)
}

func msgTicketGoneOnCard(number int) string {
	return fmt.Sprintf("Тикета #%d больше нет в GitHub.", number)
}

// continueText - экран продолжения при живом обращении. Режим называется вслух,
// и выход назван по жанру, за которым автор пришёл: разговор по документации
// висит в сборе до «Закончить разговор», и «Продолжить» продолжает именно его.
func continueText(live, wanted string) string {
	now := "У вас уже собирается обращение для тикета."
	if live == modeAsk {
		now = "У вас уже идёт разговор по документации."
	}
	switch {
	case live == wanted:
		return now + " Продолжайте его или нажмите «Сброс», чтобы начать заново."
	case wanted == modeAsk:
		return now + " Продолжайте его или нажмите «Сброс», чтобы вместо него спросить по документации."
	}
	return now + " Продолжайте его или нажмите «Сброс», чтобы вместо него завести тикет."
}

func stateReply(status string) string {
	switch status {
	case statusCollecting:
		return "Идёт сбор обращения. Присылайте материал и нажмите «Готово»."
	case statusInterview:
		return "У вас идёт разбор обращения. Ответьте на последний вопрос - текстом " +
			"или голосовым. Если вопросов не видно, напишите, что хотели добавить, " +
			"и я спрошу заново."
	case statusSummary:
		return "Обращение ждёт вашего решения: «Публикую» или «Поправить» " +
			"под последним сообщением."
	case statusAnswering:
		return "Смотрю документацию проекта. Отвечу, как найду."
	case statusPublishing:
		return "Публикую тикет. Пришлю номер и ссылку, как только он заведётся."
	default:
		return "Разбираю обращение, минуту. Отвечу, как закончу."
	}
}

// itemReply - что ответить автору на неудачный приём и надо ли тащить ошибку
// наверх: известный отказ - не сбой хендлера.
func itemReply(err error, maxItems int) (text string, internal bool) {
	switch {
	case err == nil, errors.Is(err, errLimitReported):
		// Про исчерпанный лимит автору сказали один раз; повторять на каждое
		// следующее сообщение бот не должен.
		return "", false
	case errors.Is(err, ErrUnsupportedItem):
		return "Пришлите текстом, голосовым или скриншотом.", false
	case errors.Is(err, ErrTooManyItems):
		return fmt.Sprintf("В обращении уже %d сообщений. Нажмите «Готово».", maxItems), false
	case errors.Is(err, ErrFileTooBig):
		return "Файл тяжелее 20 МБ, Telegram не отдаёт его боту. Пришлите скриншот или фрагмент.", false
	case errors.Is(err, ErrNotCollecting):
		return "Сбор по этому обращению закрыт, разбираю материал.", false
	}
	// Скачивание не удалось либо отказала база: элемент уже помечен failed,
	// обращение живо, и молчать нельзя - автор считает, что отправил.
	return "Не получилось принять сообщение. Пришлите его иначе.", true
}

// projectAsk - зачем обращению проект. В режиме вопроса тикет не заводится, и
// обещать его нельзя: проект нужен ради своей документации.
func projectAsk(cs *Case) string {
	if cs.Mode == modeAsk {
		return "Выберите проект, по документации которого отвечать."
	}
	return "Выберите проект, в который заводим тикет."
}

// markAnswered дописывает исход к сообщению раунда: вопросы остаются читаемыми
// в истории, а под ними видно, что ответ принят и бот работает дальше.
func markAnswered(c tele.Context, note string) string {
	text := msgRoundQuestionsTitle
	if msg := c.Message(); msg != nil && msg.Text != "" {
		text = msg.Text
	}
	return text + "\n\n---\n" + note
}

func ticketLine(t Ticket) string {
	if t.News {
		return fmt.Sprintf("#%d (новое) - %s - %s", t.Number, statusTitle(t.Status), t.Title)
	}
	return fmt.Sprintf("#%d - %s - %s", t.Number, statusTitle(t.Status), t.Title)
}

// ticketButton - подпись кнопки списка. Отметка новости повторяется на кнопке:
// в длинном списке нажимают по кнопкам, не сверяясь с текстом выше.
func ticketButton(t Ticket) string {
	if t.News {
		return fmt.Sprintf("#%d новое - %s", t.Number, statusTitle(t.Status))
	}
	return fmt.Sprintf("#%d %s", t.Number, statusTitle(t.Status))
}

// statusTitle - что показать вместо статуса, когда GitHub не ответил. Пустая
// строка выглядела бы как «статус неизвестен сервису», а он просто недоступен.
func statusTitle(s Status) string {
	if s.Title == "" {
		return msgStatusUnavailable
	}
	return s.Title
}

// cutForCard готовит длинный текст к показу в карточке: снимает markdown и режет
// хвост с пометкой. Без пометки автор примет обрывок за весь текст, а
// полный текст живёт в issue - ссылка стоит в той же карточке.
func cutForCard(text string, limit int) string {
	text = plainText(strings.TrimSpace(text))
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return cutRunes(text, limit) + msgFullInTicket
}

func cardText(t *Ticket) string {
	var text strings.Builder
	fmt.Fprintf(&text, "Тикет #%d - %s\n%s\n\nАвтор: %s\n", t.Number, statusTitle(t.Status), t.Title, t.Author)
	// Краткое содержание, а у тикетов до его появления - начало тела: целиком тело
	// вытесняло с экрана статус и комментарий, за которыми автор и заходит.
	switch {
	case t.Brief != "":
		text.WriteString("\n" + cutForCard(t.Brief, briefLimit) + "\n")
	case t.Body != "":
		text.WriteString("\n" + cutForCard(firstSection(t.Body), briefLimit) + "\n")
	}
	if t.Comment != "" {
		text.WriteString(msgLastComment + cutForCard(t.Comment, cardComment) + "\n")
	}
	if t.URL != "" {
		text.WriteString("\n" + t.URL)
	}
	return text.String()
}

// projectFailText объясняет отказ словами автора, а не статусом API: для
// fine-grained токена «нет репозитория» и «нет доступа» неразличимы.
func projectFailText(err error) string {
	if isDenied(err) {
		return "Репозиторий недоступен: токен сервиса его не видит либо не может писать в Issues. " +
			"Проверьте адрес и права токена, потом повторите."
	}
	return "Не получилось завести проект. Попробуйте ещё раз, а если повторится - напишите владельцу сервиса."
}

// publishFailedText отделяет отказ в правах от временного сбоя. Советовать
// «нажмите ещё раз» там, где токену не хватает прав, значит гонять автора по
// кругу: повтор не поможет, пока владелец не выдаст право заводить тикеты.
func publishFailedText(cause error) string {
	if isDenied(cause) {
		return "Тикет не создан: у сервиса нет прав заводить задачи в этом проекте. " +
			"Материал сохранён. Напишите владельцу сервиса - когда право появится, " +
			"нажмите «Публикую» ещё раз."
	}
	return "Не удалось создать тикет в GitHub. Нажмите «Публикую» ещё раз - материал на месте."
}

// lookupFailedText - то же разделение для похода в документацию: право читать
// содержимое репозитория выдаётся отдельно от права заводить тикеты, и без него
// «спросите ещё раз своими словами» отправляет автора по кругу навсегда.
func lookupFailedText(cause error) string {
	if isDenied(cause) {
		return "У сервиса нет доступа к документации этого проекта, и повтор тут не " +
			"поможет - напишите владельцу сервиса. Тикет создать можно: нажмите " +
			"«Создать тикет»."
	}
	return "Не смог посмотреть документацию. Спросите ещё раз своими словами - " +
		"или нажмите «Создать тикет»."
}

func publishedMessage(number int, url string, incomplete bool) string {
	text := fmt.Sprintf("Готово. Тикет #%d: %s", number, url)
	if incomplete {
		text += "\n\nЧасть вопросов осталась без ответа - тикет помечен как неполный, " +
			"пробелы перечислены в теле."
	}
	return text
}

const msgWhatToChange = "Что бы вы хотели изменить?"

func remindText(status string) string {
	if status == statusCollecting {
		return "Обращение ждёт вас сутки. Пришлите остальное и нажмите «Готово» " +
			"либо нажмите «Сброс». Вложения уже удалены, текст на месте."
	}
	return "Обращение ждёт вашего ответа сутки. Ответьте, и я доведу его до тикета, " +
		"либо нажмите «Сброс». Вложения уже удалены, разбор на месте."
}

const msgProcessFailed = "Не смог обработать обращение. Пришлите материал иначе и нажмите «Готово» ещё раз."

const msgParseFailed = "Не смог разобрать обращение. Напишите ещё раз своими словами - или нажмите «Сброс»."

const msgCancelFailed = "Отменить тикет не получилось. Откройте его в списке и попробуйте ещё раз."

// lostNotifyText - шапка алерта о недоставленном сообщении. Текст потери идёт
// целиком: владелец должен видеть, что именно не дошло до автора.
func lostNotifyText(caseID, text string) string {
	return "Сообщение автору не доставлено, обращение " + caseID + ":\n\n" + text
}

// alertLookup* - строка владельцу о заданном вопросе: кто, проект, найден ли
// ответ и из каких файлов.
const (
	alertLookupNoAnswer          = "ответа в документации нет"
	alertLookupAnswerFoundPrefix = "ответ найден: "
)

func alertLookupQuestion(project, author, found string) string {
	return fmt.Sprintf("Вопрос по документации: %s\nАвтор: %s\n%s", project, author, found)
}

const msgNothingParsed = "Ничего не удалось разобрать. Пришлите материал иначе и нажмите «Готово» ещё раз."

func protocolMessage(protocol string) string {
	return "Разобрал материал. Вот что получилось:\n\n" + plainText(protocol) +
		"\n\nСейчас уточню недостающее."
}

const msgVoiceUnrecognized = "Не разобрал голосовое. Повторите текстом или запишите ещё раз."

func roundMessage(questions []Question) string {
	tail := "\n\nОтветьте своими словами - текстом или голосовым."
	if hasSuggestion(questions) {
		tail += " Если предположения верны, нажмите «Всё так»."
	}
	return "Уточню, чтобы тикет не пришлось переспрашивать:\n\n" + questionList(questions) + tail
}

func summaryMessage(title, brief, body, unclear, overlap string) string {
	var b strings.Builder
	b.WriteString("Вот что уйдёт в тикет.\n\n")
	b.WriteString(title + "\n\n")
	// Краткое содержание показывается вместе с разделами: оно уедет в тикет, а
	// подтверждает автор именно то, что уйдёт.
	if brief != "" {
		b.WriteString(brief + "\n\n")
	}
	b.WriteString(plainText(body))
	// Строка есть ровно тогда, когда тикет уйдёт с меткой неполноты: обе
	// считаются по незакрытому ядру.
	if unclear != "" {
		b.WriteString("\n\n" + unclear + " Тикет уйдёт с пометкой о неполноте.")
	}
	// Пересечения идут перед вопросом о правке: это то, чего автор не знал, и
	// решать ему сразу после - публиковать или бросить обращение.
	if overlap != "" {
		b.WriteString("\n\nПохоже, часть этого уже есть:\n\n" + plainText(overlap))
		b.WriteString("\n\nЕсли это оно - нажмите «Сброс», тикет не понадобится. " +
			"Если нет - напишите, чего не хватает.")
	}
	b.WriteString("\n\nГде я ошибся? Напишите правку - или публикуем.")
	return b.String()
}
