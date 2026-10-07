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
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	GitHubAPI     = "https://api.github.com"
	githubTimeout = 20 * time.Second
	// Просмотр ждёт человек, и ждёт он же за всех остальных авторов: бюджет
	// короткий, повторов нет, отказ сразу превращается в «статус недоступен».
	githubFastTimeout = 5 * time.Second
	githubRetries     = 3
	githubLimit       = 1 << 20
	// Сколько последних комментариев страниц просматриваем в поиске последнего.
	githubCommentPages = 5
	// Сколько последних issue просматриваем в поиске своего маркера. Дубль
	// ищется сразу после потерянного ответа, поэтому нужный тикет лежит в самом
	// начале списка.
	githubScan = 30
)

// Порядок в списке issue. По созданию ходят просмотр и поиск маркера, по
// изменению - слежение: тикет, которому сменили метку, свежим не становится.
const (
	sortCreated = "created"
	sortUpdated = "updated"
)

// GitHub - клиент Issues API на net/http, без SDK (раздел 1 architecture.md).
// Клиента два: рабочий с повторами для очереди и быстрый для хендлеров, где
// отказ GitHub морозил бы бота (раздел 6). Прокси нет: GitHub идёт напрямую.
type GitHub struct {
	token    string
	api      string
	http     *http.Client
	fast     *http.Client
	statuses Statuses
	log      *slog.Logger
}

// NewGitHub: api - базовый адрес, параметром ради тестов на httptest.Server.
func NewGitHub(token, api string, statuses Statuses, log *slog.Logger) *GitHub {
	return &GitHub{
		token:    token,
		api:      api,
		http:     &http.Client{Timeout: githubTimeout},
		fast:     &http.Client{Timeout: githubFastTimeout},
		statuses: statuses,
		log:      log,
	}
}

const authorLabelColor = "ededed"

// PrepareProject заводит метки проекта и этим же проверяет право писать:
// метка создаётся записью, отказ в правах виден до того, как автор потратил
// интервью. Почему не чтение и почему каждый проект - раздел 6 architecture.md.
func (g *GitHub) PrepareProject(ctx context.Context, p Project) error {
	for _, l := range baseLabels {
		if err := g.createLabel(ctx, p, l.Name, l.Color, l.Desc); err != nil {
			return err
		}
	}
	for _, s := range g.statuses {
		if err := g.createLabel(ctx, p, s.Label, s.Color, modelStatusLabelPrefix+s.Title); err != nil {
			return err
		}
	}
	return nil
}

// EnsureLabels заводит недостающие метки проекта. Автосоздание метки при
// создании issue документацией не обещано, поэтому шаг явный.
func (g *GitHub) EnsureLabels(ctx context.Context, p Project) error {
	if err := g.PrepareProject(ctx, p); err != nil {
		return err
	}
	g.log.Info("labels_created", "project", p.Slug, "labels", len(baseLabels)+len(g.statuses))
	return nil
}

// CheckWrite проверяет право писать в Issues одной меткой и коротким бюджетом.
// Полный bootstrap здесь не годится: десять меток рабочим клиентом - это до
// двух минут с повторами, а зовут отсюда хендлер, который держит очередь
// апдейтов всех авторов. Остальные метки заведёт старт либо первая публикация.
func (g *GitHub) CheckWrite(ctx context.Context, p Project) error {
	l := baseLabels[0]
	body, err := json.Marshal(map[string]string{"name": l.Name, "color": l.Color, "description": l.Desc})
	if err != nil {
		return fmt.Errorf("build label %s: %w", l.Name, err)
	}

	_, _, err = g.send(ctx, g.fast, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/labels", p.Owner, p.Repo), body)
	var apiErr *githubError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusUnprocessableEntity {
		return nil
	}
	return err
}

// CheckRead проверяет право читать содержимое репозитория - им живёт режим
// «Спросить». Право отдельное от Issues: токен, которым тикеты заводятся, о
// документации проекта может не знать вовсе, и раньше это выяснял автор
// посреди разговора.
//
// Листинг корня, а не README: README есть не в каждом репозитории, и его 404 не
// отличить от отказа в правах. Клиент рабочий, хотя проверка и не в очереди:
// зовут её со старта, никто не ждёт, а сетевой сбой на быстром клиенте
// записался бы в лог отказом в правах. Настоящий отказ повторов не стоит - 403
// без Retry-After возвращается сразу.
func (g *GitHub) CheckRead(ctx context.Context, p Project) error {
	_, err := g.call(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/contents/", p.Owner, p.Repo), nil)
	return err
}

// createLabel: существующая метка возвращает 422, и это не ошибка - именно
// такого состояния мы и добивались.
func (g *GitHub) createLabel(ctx context.Context, p Project, name, color, desc string) error {
	body, err := json.Marshal(map[string]string{"name": name, "color": color, "description": desc})
	if err != nil {
		return fmt.Errorf("build label %s: %w", name, err)
	}

	_, err = g.call(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/labels", p.Owner, p.Repo), body)
	var apiErr *githubError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusUnprocessableEntity {
		return nil
	}
	return err
}

// CreateIssue заводит тикет и возвращает его номер и ссылку.
func (g *GitHub) CreateIssue(ctx context.Context, p Project, title, body string, labels []string) (int, string, error) {
	payload, err := json.Marshal(map[string]any{"title": title, "body": body, "labels": labels})
	if err != nil {
		return 0, "", fmt.Errorf("build issue: %w", err)
	}

	// Единственный неидемпотентный запрос сервиса, и повторов у него нет.
	// Оборванный ответ на успешный POST означает, что тикет уже создан: слепой
	// повтор дал бы второй. Повторяет очередь, а она перед этим ищет маркер.
	raw, _, err := g.send(ctx, g.http, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues", p.Owner, p.Repo), payload)
	if err != nil {
		return 0, "", err
	}

	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, "", fmt.Errorf("decode issue: %w", err)
	}
	if out.Number == 0 {
		return 0, "", errors.New("github returned issue without number")
	}
	return out.Number, out.HTMLURL, nil
}

// AddToBoard кладёт тикет проекта на доску организации и возвращает id карточки.
// Мутация идемпотентна: тикет, который уже на доске, возвращает свою карточку.
// GitHub отвечает 200 и при отказе, поэтому ошибка - это errors или пустой item.
func (g *GitHub) AddToBoard(ctx context.Context, p Project, number int, board string) (string, error) {
	// Бюджет на весь шаг: повторы двух запросов не должны съесть время работы,
	// в которое ещё укладывается транзакция публикации.
	ctx, cancel := context.WithTimeout(ctx, githubTimeout)
	defer cancel()

	issue, err := g.GetIssue(ctx, p, number, false)
	if err != nil {
		return "", err
	}
	if issue.NodeID == "" {
		return "", fmt.Errorf("issue %d has no node_id", number)
	}

	payload, err := json.Marshal(map[string]any{
		"query": `mutation($board: ID!, $issue: ID!) {
			addProjectV2ItemById(input: {projectId: $board, contentId: $issue}) { item { id } }
		}`,
		"variables": map[string]string{"board": board, "issue": issue.NodeID},
	})
	if err != nil {
		return "", fmt.Errorf("build board mutation: %w", err)
	}
	raw, err := g.call(ctx, http.MethodPost, "/graphql", payload)
	if err != nil {
		return "", err
	}

	var out struct {
		Data struct {
			Add *struct {
				Item struct {
					ID string `json:"id"`
				} `json:"item"`
			} `json:"addProjectV2ItemById"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode board mutation: %w", err)
	}
	if len(out.Errors) > 0 {
		return "", fmt.Errorf("add issue %d to board: %s", number, out.Errors[0].Message)
	}
	if out.Data.Add == nil || out.Data.Add.Item.ID == "" {
		return "", fmt.Errorf("add issue %d to board: no item in response", number)
	}
	return out.Data.Add.Item.ID, nil
}

// FindIssue ищет свой маркер среди последних тикетов репозитория. Нужен на
// повторе: ответ на успешный запрос мог потеряться, и без проверки обращение
// уехало бы в GitHub вторым тикетом.
//
// Список, а не поиск: индекс search обновляется с задержкой, и только что
// созданный issue он не покажет, а список отдаёт его сразу.
func (g *GitHub) FindIssue(ctx context.Context, p Project, marker string) (int, string, error) {
	issues, err := g.listIssues(ctx, p, githubScan, sortCreated, false)
	if err != nil {
		return 0, "", err
	}
	for _, issue := range issues {
		if strings.Contains(issue.Body, marker) {
			return issue.Number, issue.HTMLURL, nil
		}
	}
	return 0, "", nil
}

// Issue - тикет в том виде, в каком его читает сервис. Labels нужны просмотру,
// Body - поиску маркера, Title и State - сверке черновика с состоянием проекта,
// PullRequest - отсеву: REST GitHub считает issue каждый pull request, и в
// активном репозитории они вытесняют тикеты из окна.
type Issue struct {
	Number      int    `json:"number"`
	NodeID      string `json:"node_id"`
	HTMLURL     string `json:"html_url"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Body        string `json:"body"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// LabelNames - имена меток тикета.
func (i Issue) LabelNames() []string {
	names := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		names = append(names, l.Name)
	}
	return names
}

// listIssues читает последние тикеты репозитория. Pull request'ы отсеиваются
// здесь, чтобы ни один вызывающий не забыл про них. sort выбирает, какие
// «последние» нужны: по созданию - просмотру и поиску маркера, по изменению -
// слежению.
func (g *GitHub) listIssues(ctx context.Context, p Project, limit int, sort string, fast bool) ([]Issue, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues?state=all&per_page=%d&sort=%s&direction=desc",
		p.Owner, p.Repo, limit, sort)
	raw, err := g.get(ctx, path, fast)
	if err != nil {
		return nil, err
	}

	var issues []Issue
	if err := json.Unmarshal(raw, &issues); err != nil {
		return nil, fmt.Errorf("decode issue list: %w", err)
	}
	kept := issues[:0]
	for _, issue := range issues {
		if issue.PullRequest == nil {
			kept = append(kept, issue)
		}
	}
	return kept, nil
}

// ListIssues - список для просмотра: короткий бюджет, без повторов.
func (g *GitHub) ListIssues(ctx context.Context, p Project, limit int) ([]Issue, error) {
	return g.listIssues(ctx, p, limit, sortCreated, true)
}

// ListUpdated - тикеты в порядке последнего изменения: слежению нужны те, где
// метки могли поменяться, а не самые новые. Идёт рабочим клиентом: это фон, и
// пара лишних секунд ему ничего не стоит.
func (g *GitHub) ListUpdated(ctx context.Context, p Project, limit int) ([]Issue, error) {
	return g.listIssues(ctx, p, limit, sortUpdated, false)
}

// GetIssue читает один тикет. fast решает, чей это вызов: просмотр из хендлера
// или работа из очереди.
func (g *GitHub) GetIssue(ctx context.Context, p Project, number int, fast bool) (Issue, error) {
	raw, err := g.get(ctx, fmt.Sprintf("/repos/%s/%s/issues/%d", p.Owner, p.Repo, number), fast)
	if err != nil {
		return Issue{}, err
	}

	var issue Issue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return Issue{}, fmt.Errorf("decode issue %d: %w", number, err)
	}
	return issue, nil
}

// LastComment - последний комментарий тикета. Комментарии приходят по
// возрастанию идентификатора, поэтому нужна последняя страница: листаем, пока
// страница полна, но не дальше пяти - пятьсот комментариев у внутреннего тикета
// означают, что что-то пошло не так, и это видно в логе.
func (g *GitHub) LastComment(ctx context.Context, p Project, number int) (string, error) {
	const perPage = 100
	// Бюджет на всю операцию: страниц может быть пять, а ждёт её человек, и
	// вместе с ним все остальные авторы.
	ctx, cancel := context.WithTimeout(ctx, githubFastTimeout)
	defer cancel()
	last := ""
	for page := 1; page <= githubCommentPages; page++ {
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=%d&page=%d",
			p.Owner, p.Repo, number, perPage, page)
		raw, err := g.get(ctx, path, true)
		if err != nil {
			return "", err
		}

		var comments []struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(raw, &comments); err != nil {
			return "", fmt.Errorf("decode comments of issue %d: %w", number, err)
		}
		if len(comments) > 0 {
			last = comments[len(comments)-1].Body
		}
		if len(comments) < perPage {
			return last, nil
		}
	}
	g.log.Warn("comments_truncated", "repo", p.Owner+"/"+p.Repo, "issue", number)
	return last, nil
}

// Comment - комментарий тикета в том виде, в каком его читает слежение. Номер
// issue отдельным полем не приходит: в ответе есть только адрес, из него номер и
// берётся.
type Comment struct {
	ID       int64  `json:"id"`
	Body     string `json:"body"`
	IssueURL string `json:"issue_url"`
	// UpdatedAt, а не время создания, двигает границу окна: since у GitHub
	// фильтрует именно по нему, и правка старого комментария иначе держала бы
	// окно на месте вечно.
	UpdatedAt time.Time `json:"updated_at"`
}

// IssueNumber - номер тикета из адреса комментария. Ноль означает адрес, который
// разобрать не удалось: такой комментарий слежение пропускает, а не гадает.
func (c Comment) IssueNumber() int {
	tail := c.IssueURL[strings.LastIndex(c.IssueURL, "/")+1:]
	number, err := strconv.Atoi(tail)
	if err != nil {
		return 0
	}
	return number
}

// ListComments - комментарии всего репозитория, изменённые после since. Один
// запрос на проект вместо запроса на каждый тикет: слежению нужны новые
// комментарии, а не переписка конкретного issue.
func (g *GitHub) ListComments(ctx context.Context, p Project, since time.Time) ([]Comment, error) {
	const perPage = 100
	var all []Comment
	for page := 1; page <= githubCommentPages; page++ {
		path := fmt.Sprintf(
			"/repos/%s/%s/issues/comments?since=%s&per_page=%d&page=%d&sort=created&direction=asc",
			p.Owner, p.Repo, url.QueryEscape(since.UTC().Format(time.RFC3339)), perPage, page)
		raw, err := g.get(ctx, path, false)
		if err != nil {
			return nil, err
		}

		var comments []Comment
		if err := json.Unmarshal(raw, &comments); err != nil {
			return nil, fmt.Errorf("decode repo comments: %w", err)
		}
		all = append(all, comments...)
		if len(comments) < perPage {
			return all, nil
		}
	}
	// Усечение не ошибка: граница окна не двинется дальше разобранного, и
	// остаток доедет следующим тиком.
	g.log.Warn("repo_comments_truncated", "repo", p.Owner+"/"+p.Repo, "since", since)
	return all, nil
}

// AddLabel добавляет метку, не трогая остальные: PUT затёр бы всё, включая
// проставленное владельцем вручную.
func (g *GitHub) AddLabel(ctx context.Context, p Project, number int, label string) error {
	body, err := json.Marshal(map[string][]string{"labels": {label}})
	if err != nil {
		return fmt.Errorf("build label request: %w", err)
	}
	_, err = g.call(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/issues/%d/labels", p.Owner, p.Repo, number), body)
	return err
}

// RemoveLabel снимает одну метку. 404 означает, что её и не было, - это не
// ошибка, а то состояние, которого мы добивались.
func (g *GitHub) RemoveLabel(ctx context.Context, p Project, number int, label string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels/%s",
		p.Owner, p.Repo, number, url.PathEscape(label))
	_, err := g.call(ctx, http.MethodDelete, path, nil)
	var apiErr *githubError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
		return nil
	}
	return err
}

// CloseIssue закрывает тикет как незапланированный: автор от него отказался, а
// не работа была сделана.
func (g *GitHub) CloseIssue(ctx context.Context, p Project, number int) error {
	body, err := json.Marshal(map[string]string{"state": "closed", "state_reason": "not_planned"})
	if err != nil {
		return fmt.Errorf("build close request: %w", err)
	}
	_, err = g.call(ctx, http.MethodPatch,
		fmt.Sprintf("/repos/%s/%s/issues/%d", p.Owner, p.Repo, number), body)
	return err
}

// githubError несёт код ответа: 422 на метке означает «уже есть», и отличить
// его от настоящего отказа можно только по статусу.
type githubError struct {
	status  int
	message string
}

func (e *githubError) Error() string { return fmt.Sprintf("github status %d: %s", e.status, e.message) }

// get - чтение с выбором бюджета. fast означает «зовут из хендлера»: одна
// попытка коротким клиентом, потому что автор ждёт ответа, а с ним и все
// остальные авторы.
func (g *GitHub) get(ctx context.Context, path string, fast bool) (json.RawMessage, error) {
	if fast {
		raw, _, err := g.send(ctx, g.fast, http.MethodGet, path, nil)
		return raw, err
	}
	return g.call(ctx, http.MethodGet, path, nil)
}

func (g *GitHub) call(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		raw, retry, err := g.send(ctx, g.http, method, path, body)
		if err == nil {
			return raw, nil
		}
		if !retry || attempt == githubRetries {
			return nil, err
		}

		g.log.Warn("github_retry", "path", path, "attempt", attempt+1, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		}
	}
}

// send делает одну попытку. Второе значение - повторять ли: 429, 5xx и
// вторичный лимит (403 с Retry-After) да, остальные 4xx нет.
func (g *GitHub) send(ctx context.Context, client *http.Client, method, path string, body []byte) (json.RawMessage, bool, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.api+path, reader)
	if err != nil {
		return nil, false, fmt.Errorf("build github request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		// Ответа не было, значит и запрос мог не отработать: повтор безопасен,
		// от дубля защищает поиск маркера.
		return nil, true, fmt.Errorf("github %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, githubLimit))
	if err != nil {
		return nil, true, fmt.Errorf("read github body: %w", err)
	}
	if resp.StatusCode >= 300 {
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 ||
			(resp.StatusCode == http.StatusForbidden && resp.Header.Get("Retry-After") != "")
		message := githubMessage(raw)
		// GitHub называет недостающее право заголовком. Без него «Resource not
		// accessible» не отличает «прав нет» от «токен не выдан на этот
		// репозиторий», и разбор упирается в догадки.
		if need := resp.Header.Get("X-Accepted-GitHub-Permissions"); need != "" {
			message += modelGithubPermissionNeeded + need + ")"
		}
		return nil, retry, &githubError{status: resp.StatusCode, message: message}
	}
	return raw, false, nil
}

func githubMessage(raw []byte) string {
	var out struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "non-json body"
	}
	return cut(out.Message)
}

// Publisher - последний шаг обращения: подтверждённое саммари становится issue.
type Publisher struct {
	cases *Cases
	gh    *GitHub
	rules Contract
	log   *slog.Logger
	// Чат уведомлений владельца; 0 - уведомления выключены.
	alertChat int64
	// Node id доски организации; пусто - тикет на доску не ставится.
	board string
}

func NewPublisher(cases *Cases, gh *GitHub, rules Contract, log *slog.Logger, alertChat int64, board string) *Publisher {
	return &Publisher{cases: cases, gh: gh, rules: rules, log: log, alertChat: alertChat, board: board}
}

// Run создаёт issue по обращению. Идемпотентен трижды: по ключу работы, по уже
// записанному номеру issue и по маркеру в теле тикета.
func (p *Publisher) Run(ctx context.Context, job Job) error {
	var payload casePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("payload of %s: %w", job.Kind, err)
	}

	cs, err := p.cases.Load(ctx, payload.CaseID)
	if err != nil {
		return err
	}
	// Проверка статуса обязательна: без неё отменённое обращение всё равно
	// уехало бы в GitHub.
	if cs == nil || cs.IssueNumber != 0 || cs.Status != statusPublishing {
		return nil
	}
	if cs.ProjectID == nil {
		return fmt.Errorf("case %s has no project", cs.ID)
	}

	project, err := LoadProject(ctx, p.cases.pool, *cs.ProjectID)
	if err != nil {
		return err
	}
	author, err := LoadUser(ctx, p.cases.pool, cs.UserID)
	if err != nil {
		return err
	}
	items, err := p.cases.Items(ctx, cs.ID)
	if err != nil {
		return err
	}

	if !project.LabelsReady {
		if err := p.gh.EnsureLabels(ctx, project); err != nil {
			return err
		}
		if err := MarkLabelsReady(ctx, p.cases.pool, project.ID); err != nil {
			return err
		}
	}
	// Метка автора живёт вне базового набора: она появляется вместе с первым
	// обращением человека.
	if err := p.gh.createLabel(ctx, project, "author:"+author.Slug, authorLabelColor, modelAuthorLabelDesc); err != nil {
		return err
	}

	marker := caseMarker(cs.ID)
	number, url := 0, ""
	// Метка неполноты и строка «Не уточнено» в теле считаются одной функцией по
	// ядру: расходиться им не с чего.
	incomplete := p.rules.Unclear(cs.Kind, cs.Filled) != ""
	// Ищем всегда, а не со второй попытки: «Публикую» после исчерпанных повторов
	// ставит новую работу с нулевым счётом, а issue прошлой уже мог создаться.
	if number, url, err = p.gh.FindIssue(ctx, project, marker); err != nil {
		return err
	}
	if number == 0 {
		labels := append(typeLabels(cs.Kind), labelNew, "author:"+author.Slug)
		if incomplete {
			labels = append(labels, "incomplete")
		}
		body := p.body(cs, author, collectLinks(items), marker)
		number, url, err = p.gh.CreateIssue(ctx, project, cs.Title, body, labels)
		if err != nil {
			return err
		}
	}

	// Доска - производное от тикета: её сбой публикацию не останавливает,
	// иначе автор получил бы отказ на уже созданный тикет. Карточку ставит
	// разбор по строке в уведомлении владельцу.
	onBoard := true
	if p.board != "" {
		if _, err := p.gh.AddToBoard(ctx, project, number, p.board); err != nil {
			onBoard = false
			p.log.Error("board_add_failed", "case_id", cs.ID, "project", project.Slug,
				"issue", number, "error", err)
		}
	}

	published := false
	err = p.cases.inTx(ctx, func(tx pgx.Tx) error {
		// told_status ставится здесь, а не первым тиком слежения: иначе метка,
		// смененная в первые пять минут, была бы принята за первое наблюдение и
		// автора не разбудила бы.
		tag, err := tx.Exec(ctx, `
			UPDATE cases SET status = 'published', issue_number = $2, issue_url = $3,
			                 told_status = $4, updated_at = now()
			WHERE id = $1 AND status = 'publishing'`, cs.ID, number, url, labelNew)
		if err != nil {
			return fmt.Errorf("publish case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		published = true

		if err := addEvent(ctx, tx, cs.ID, "published", map[string]any{
			"issue": number, "incomplete": incomplete,
		}); err != nil {
			return err
		}
		// Панель возвращается в исходное состояние тем же сообщением: обращение
		// доиграно, «Готово» больше не по чему нажимать.
		if err := putNotifyKey(ctx, tx, cs.ID, strconv.FormatInt(job.ID, 10),
			publishedMessage(number, url, incomplete), keysHome); err != nil {
			return err
		}
		if p.alertChat == 0 {
			return nil
		}
		return putAlert(ctx, tx, cs.ID, "alert",
			alertPublished(project, cs, author, number, url, incomplete, onBoard), p.alertChat)
	})
	if err != nil {
		return err
	}
	if !published {
		return nil
	}

	p.log.Info("issue_created", "case_id", cs.ID, "project", project.Slug,
		"issue", number, "incomplete", incomplete)

	// Медиа не переживает обращение; удаление здесь, а не в нормализации: до
	// подтверждения саммари файл ловит неверно прочитанный скриншот. Сбой не
	// ошибка: повтор вышел бы на issue_number, подчистит почасовой сборщик.
	if err := p.cases.DropFiles(ctx, cs.ID); err != nil {
		p.log.Error("drop_files_failed", "case_id", cs.ID, "error", err)
	}
	return nil
}

// typeLabels - метки типа тикета. Смесь - один тикет с метками обоих типов:
// делить обращение на два - решение разработчика, а не бота (Р-2 ticket-form).
func typeLabels(kind string) []string {
	if kind == "mixed" {
		return []string{"type:bug", "type:feature"}
	}
	return []string{"type:" + kind}
}

// caseMarker - скрытая метка обращения в теле тикета. По ней повтор работы
// узнаёт свой issue, если ответ на успешный запрос потерялся.
func caseMarker(caseID string) string { return "<!-- intake:case:" + caseID + " -->" }

func authorName(u User) string {
	name := strings.TrimSpace(u.First + " " + u.Last)
	if u.Username != "" {
		return name + " (@" + u.Username + ")"
	}
	return name
}

// isDenied - отказ в правах: GitHub отвечает и 403, и 404, потому что
// fine-grained PAT не показывает разницы между «права нет» и «репозитория для
// этого токена не существует». Для ответа автору это одно и то же.
func isDenied(err error) bool {
	var apiErr *githubError
	return errors.As(err, &apiErr) &&
		(apiErr.status == http.StatusForbidden || apiErr.status == http.StatusNotFound)
}

// publishFailedText отделяет отказ в правах от временного сбоя. Советовать
// «нажмите ещё раз» там, где токену не хватает прав, значит гонять автора по
// кругу: повтор не поможет, пока владелец не выдаст право заводить тикеты.

// Repo - репозиторий в том виде, в каком его читает заведение проекта.
type Repo struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	DefaultBranch string `json:"default_branch"`
}

// GetRepo читает репозиторий быстрым клиентом: зовут из хендлера, автор ждёт.
// 404 означает «нет репозитория либо токен его не видит»; для fine-grained PAT
// это одно и то же, и различать их сервису незачем.
func (g *GitHub) GetRepo(ctx context.Context, owner, repo string) (Repo, error) {
	raw, err := g.get(ctx, fmt.Sprintf("/repos/%s/%s", owner, repo), true)
	if err != nil {
		return Repo{}, err
	}

	var out Repo
	if err := json.Unmarshal(raw, &out); err != nil {
		return Repo{}, fmt.Errorf("decode repo %s/%s: %w", owner, repo, err)
	}
	return out, nil
}

// GetReadme отдаёт текст README. Пустая строка означает, что его нет: контекст
// проекта тогда соберётся из описания репозитория.
//
// Содержимое приходит в base64 внутри JSON - сырой текст потребовал бы своего
// заголовка Accept, а клиент шлёт один на все запросы.
func (g *GitHub) GetReadme(ctx context.Context, owner, repo string) (string, error) {
	raw, err := g.get(ctx, fmt.Sprintf("/repos/%s/%s/readme", owner, repo), true)
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}

	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode readme of %s/%s: %w", owner, repo, err)
	}
	if out.Encoding != "base64" {
		return out.Content, nil
	}

	// Переносы строк внутри base64 - обычное дело для этого ответа, декодер их
	// не принимает.
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("decode readme body of %s/%s: %w", owner, repo, err)
	}
	return string(decoded), nil
}

// DocFile - md-файл дерева репозитория: путь и размер, дальше их читает
// Lookup, решая, какие скачивать.
type DocFile struct {
	Path string
	Size int
}

// TreeDocs читает дерево репозитория по ref и оставляет только md-файлы:
// каталоги и файлы прочих расширений отбору Lookup не нужны. Рабочий клиент -
// зовут из работы очереди, не из обработчика сообщения.
func (g *GitHub) TreeDocs(ctx context.Context, p Project, ref string) ([]DocFile, error) {
	path := fmt.Sprintf("/repos/%s/%s/git/trees/%s?recursive=1", p.Owner, p.Repo, url.PathEscape(ref))
	raw, err := g.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var out struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int    `json:"size"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode tree of %s/%s: %w", p.Owner, p.Repo, err)
	}
	if out.Truncated {
		// GitHub режет большие деревья молча: без предупреждения отбор Lookup
		// решил бы, что видел все md-файлы репозитория, хотя часть отсутствует.
		g.log.Warn("tree_truncated", "project", p.Slug)
	}

	docs := make([]DocFile, 0, len(out.Tree))
	for _, item := range out.Tree {
		if item.Type != "blob" || !strings.HasSuffix(strings.ToLower(item.Path), ".md") {
			continue
		}
		docs = append(docs, DocFile{Path: item.Path, Size: item.Size})
	}
	return docs, nil
}

// File читает содержимое одного файла по ref. Рабочий клиент - зовут из
// работы очереди, не из обработчика сообщения.
//
// Содержимое приходит в base64 внутри JSON - тем же приёмом, что и в
// GetReadme: сырой текст потребовал бы своего заголовка Accept, а клиент шлёт
// один на все запросы.
func (g *GitHub) File(ctx context.Context, p Project, path, ref string) (string, error) {
	if err := validateDocPath(path); err != nil {
		return "", err
	}

	reqPath := fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s",
		p.Owner, p.Repo, escapeDocPath(path), url.QueryEscape(ref))
	raw, err := g.call(ctx, http.MethodGet, reqPath, nil)
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("doc file not found: %s@%s", path, ref)
		}
		return "", err
	}

	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode file %s: %w", path, err)
	}
	// Файлы больше 1 МБ Contents API отдаёт без содержимого: пустая строка
	// читалась бы дальше по потоку как «файл пустой», а не как отказ.
	if out.Encoding == "none" {
		return "", fmt.Errorf("doc file too large for contents api: %s", path)
	}
	if out.Encoding != "base64" {
		return out.Content, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("decode file body %s: %w", path, err)
	}
	if !utf8.Valid(decoded) {
		return "", fmt.Errorf("doc file is not valid utf-8: %s", path)
	}
	return string(decoded), nil
}

// validateDocPath отвергает путь до похода в сеть: имя называет модель, а не
// человек, и путь с ".." или ведущим "/" мог бы уйти за пределы репозитория.
func validateDocPath(path string) error {
	if path == "" {
		return errors.New("doc path is empty")
	}
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("doc path is absolute: %s", path)
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("doc path contains ..: %s", path)
	}
	if !utf8.ValidString(path) {
		return fmt.Errorf("doc path is not valid utf-8: %q", path)
	}
	for _, r := range path {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("doc path contains non-printable characters: %q", path)
		}
	}
	return nil
}

// escapeDocPath экранирует каждый сегмент пути отдельно: PathEscape целиком
// закодировал бы разделитель "/" и сломал бы адрес.
func escapeDocPath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}
