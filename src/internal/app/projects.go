package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// readmeLimit - сколько текста README уходит в модель. Дальше начинается
// установка и лицензия, а токены платные.
const readmeLimit = 8000

// askTimeout - бюджет хода модели. Команда идёт синхронно, и апдейты всех
// авторов ждут её в очереди: полный бюджет клиента с повторами держал бы бота
// до двух минут. Отказ по таймауту не роняет команду - сработает фолбэк на
// поля репозитория.
const askTimeout = 20 * time.Second

var (
	// ErrBadProjectRef - в команде нет разбираемой ссылки на репозиторий.
	ErrBadProjectRef = errors.New("project reference is not a github repository")
	// ErrSlugTaken - имя репозитория уже занято другим проектом.
	ErrSlugTaken = errors.New("project slug belongs to another repository")
)

// Projects - заведение проекта из бота. Владелец таблицы по-прежнему db.go,
// здесь сценарий: прочитать репозиторий, собрать описание, завести метки.
type Projects struct {
	cases *Cases
	gh    *GitHub
	llm   Completer
	model DialogModel
	log   *slog.Logger
}

func NewProjects(cases *Cases, gh *GitHub, llm Completer, model DialogModel, log *slog.Logger) *Projects {
	return &Projects{cases: cases, gh: gh, llm: llm, model: model, log: log}
}

// projectRef - разобранная команда: адрес репозитория и то, что автор задал сам.
type projectRef struct {
	Owner   string
	Repo    string
	Title   string
	Context string
}

// Add заводит проект по ссылке. Возвращает сохранённую строку - её же бот
// показывает автору карточкой.
func (p *Projects) Add(ctx context.Context, userID int64, args string) (ProjectConfig, string, error) {
	ref, err := parseProjectRef(args)
	if err != nil {
		return ProjectConfig{}, "", err
	}

	repo, err := p.gh.GetRepo(ctx, ref.Owner, ref.Repo)
	if err != nil {
		return ProjectConfig{}, "", err
	}
	// GitHub принимает owner/repo в любом регистре, у нас же это и ключ поиска,
	// и строка карточки: берём каноническое написание из ответа API.
	if owner, name, ok := strings.Cut(repo.FullName, "/"); ok && owner != "" && name != "" {
		ref.Owner, ref.Repo = owner, name
	}

	source := projectSourceAuthor
	title, context := ref.Title, ref.Context
	if title == "" || context == "" {
		title, context, source = p.describe(ctx, ref, repo, title, context)
	}

	slug, err := p.slugFor(ctx, ref)
	if err != nil {
		return ProjectConfig{}, "", err
	}
	project := ProjectConfig{
		Slug:    slug,
		Title:   title,
		Owner:   ref.Owner,
		Repo:    ref.Repo,
		Context: context,
	}

	// Право на запись проверяется одной меткой и коротким бюджетом: строка в
	// меню, ведущая в репозиторий без доступа, хуже её отсутствия - автор
	// потратит на интервью пять минут и упрётся в отказ на публикации. Полный
	// набор меток заведёт старт или первая публикация: десять меток рабочим
	// клиентом держали бы очередь апдейтов всех авторов.
	if err := p.gh.CheckWrite(ctx, Project{
		Slug: project.Slug, Owner: project.Owner, Repo: project.Repo,
	}); err != nil {
		return ProjectConfig{}, "", err
	}

	_, err = p.cases.pool.Exec(ctx, `
		INSERT INTO projects (slug, title, github_owner, github_repo, context, active)
		VALUES ($1, $2, $3, $4, $5, true)
		ON CONFLICT (slug) DO UPDATE SET
			title = excluded.title, github_owner = excluded.github_owner,
			github_repo = excluded.github_repo, context = excluded.context,
			active = true, updated_at = now()`,
		project.Slug, project.Title, project.Owner, project.Repo, project.Context)
	if err != nil {
		return ProjectConfig{}, "", fmt.Errorf("save project %s: %w", project.Slug, err)
	}

	p.log.Info("project_saved", "user_id", userID, "project", project.Slug,
		"repo", project.Owner+"/"+project.Repo, "source", source)
	return project, source, nil
}

// slugFor выбирает ключ проекта. Репозиторий, уже заведённый под другим ключом,
// обновляется по нему: ключ считается один раз и дальше живёт в callback_data и
// в тикетах. Занятый чужим репозиторием ключ - отказ, а не перезапись: иначе
// одной командой можно было бы перенацелить чужой проект, а обращения остались
// бы привязаны к нему по project_id.
func (p *Projects) slugFor(ctx context.Context, ref projectRef) (string, error) {
	// Регистр не различается: GitHub считает Owner/Repo и owner/repo одним
	// репозиторием, а точное сравнение давало бы ложный «занят» на строках,
	// заведённых до канонизации написания.
	var existing string
	err := p.cases.pool.QueryRow(ctx, `
		SELECT slug FROM projects
		WHERE LOWER(github_owner) = LOWER($1) AND LOWER(github_repo) = LOWER($2)`,
		ref.Owner, ref.Repo).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("look up project of %s/%s: %w", ref.Owner, ref.Repo, err)
	}

	slug := projectSlugFrom(ref.Repo)
	if slug == "" {
		return "", ErrBadProjectRef
	}

	var owner, repo string
	err = p.cases.pool.QueryRow(ctx,
		`SELECT github_owner, github_repo FROM projects WHERE slug = $1`, slug).Scan(&owner, &repo)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return slug, nil
	case err != nil:
		return "", fmt.Errorf("look up slug %s: %w", slug, err)
	}
	return "", errSlugTaken(slug, owner, repo)
}

// describe собирает недостающие название и контекст. Отказ модели не роняет
// команду: имя и описание репозитория есть всегда, а бот честно скажет, что
// собрал их сам.
func (p *Projects) describe(ctx context.Context, ref projectRef, repo Repo, title, context string) (string, string, string) {
	readme, err := p.gh.GetReadme(ctx, ref.Owner, ref.Repo)
	if err != nil {
		p.log.Warn("readme_unavailable", "repo", ref.Owner+"/"+ref.Repo, "error", err)
	}

	guess, err := p.ask(ctx, repo, readme)
	if err != nil {
		p.log.Warn("project_describe_failed", "repo", ref.Owner+"/"+ref.Repo, "error", err)
		return valueOr(title, repo.Name), valueOr(context, valueOr(repo.Description, repo.Name)), projectSourceRepo
	}
	return valueOr(title, guess.Title), valueOr(context, guess.Context), projectSourceModel
}

type projectGuess struct {
	Title   string `json:"title"`
	Context string `json:"context"`
}

var projectSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["title", "context"],
  "properties": {
    "title": {"type": "string"},
    "context": {"type": "string"}
  }
}`)

func (p *Projects) ask(ctx context.Context, repo Repo, readme string) (projectGuess, error) {
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()

	if len(readme) > readmeLimit {
		readme = readme[:readmeLimit]
	}

	var material strings.Builder
	material.WriteString(modelRepoLabel + repo.FullName + "\n")
	if repo.Description != "" {
		material.WriteString(modelDescriptionLabel + repo.Description + "\n")
	}
	if readme != "" {
		material.WriteString("\nREADME:\n" + readme)
	}

	raw, err := p.llm.Complete(ctx, Request{
		Step:      "project",
		Model:     p.model.Name,
		Reasoning: p.model.Reasoning,
		MaxTokens: llmMaxTokens,
		Messages: []Message{
			{Role: "system", Parts: []Part{TextPart(mustPrompt("project.md"))}},
			{Role: "user", Parts: []Part{TextPart(material.String())}},
		},
		SchemaName: "project_card",
		Schema:     projectSchema,
	})
	if err != nil {
		return projectGuess{}, err
	}

	var guess projectGuess
	if err := json.Unmarshal(raw, &guess); err != nil {
		return projectGuess{}, fmt.Errorf("decode project card: %w", err)
	}
	if strings.TrimSpace(guess.Title) == "" || strings.TrimSpace(guess.Context) == "" {
		return projectGuess{}, errors.New("model returned empty project card")
	}
	return guess, nil
}

// parseProjectRef разбирает аргумент команды: ссылку или owner/repo, следом
// необязательные название и контекст через вертикальную черту.
func parseProjectRef(args string) (projectRef, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return projectRef{}, ErrBadProjectRef
	}

	address, rest, _ := strings.Cut(args, " ")
	address = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(address), "/"), ".git")
	if i := strings.Index(address, "github.com"); i >= 0 {
		address = strings.TrimPrefix(address[i+len("github.com"):], "/")
		address = strings.TrimPrefix(address, ":")
	}
	// Хвост вида /issues или /tree/main отбрасываем: ссылку копируют со страницы.
	parts := strings.Split(strings.Trim(address, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return projectRef{}, ErrBadProjectRef
	}

	ref := projectRef{Owner: parts[0], Repo: strings.TrimSuffix(parts[1], ".git")}
	title, context, _ := strings.Cut(rest, "|")
	ref.Title = strings.TrimSpace(title)
	ref.Context = strings.TrimSpace(context)
	return ref, nil
}

// projectSlugFrom приводит имя репозитория к маске схемы тем же способом, что и
// метка автора: маска и предел callback_data у них общие.
func projectSlugFrom(name string) string {
	slug := clean(strings.ToLower(name))
	if len(slug) > 32 {
		slug = strings.Trim(slug[:32], "-")
	}
	return slug
}
