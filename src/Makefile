ifneq ($(NO_DOTENV),1)
    ENV_FILE := $(firstword $(wildcard .env ../.env))
    ifneq (,$(ENV_FILE))
        -include $(ENV_FILE)
        export
    endif
endif

GOOSE := go run -tags='no_sqlite3' github.com/pressly/goose/v3/cmd/goose@v3.27.3
MIGRATIONS_DIR := ./migrations

.PHONY: run build test test-fast eval eval-compare vet fmt-check lint security commit-check ci-check check-db migrate-up migrate-down migrate-reset live check-live-db

run:
	go run ./cmd/intake

build:
	go build ./...

# RUN фильтрует тесты при разборе: make test RUN=TestAuthorSlug.
# TestMigrations дропает схему и потому смотрит в TEST_DATABASE_URL, а не в
# DATABASE_URL: иначе make test на .env с dev-контуром стёр бы dev-базу.
test:
	go test ./... $(if $(RUN),-run $(RUN) -count=1,)

# Быстрый ярус без БД: петля внутри среза и кольцо коммита.
test-fast:
	go test -short ./... $(if $(RUN),-run $(RUN) -count=1,)

# Прогон набора eval/cases.jsonl через текущие промты: реальный OpenRouter и
# TRUNCATE базы TEST_DATABASE_URL. EVAL_OUT - имя файла в eval/results/.
eval:
	go test -tags eval -count=1 -timeout 8h -v -run '^TestEvalRun$$' ./internal/app

# Тот же прогон со сравнением с базой (Р-9): EVAL_BASE и EVAL_OUT по умолчанию
# base/after, переопределяются как обычно - make eval-compare EVAL_OUT=iterN.
eval-compare: export EVAL_BASE ?= base
eval-compare: export EVAL_OUT ?= after
eval-compare: eval

# Тег eval: иначе vet и линтер не видят прогон, и он ломается молча.
vet:
	go vet -tags eval,live ./...

# Срез 8: сквозной прогон через настоящие хендлеры бота, реальный OpenRouter и
# GitHub-песочницу daniil4545/intake-sandbox. Telegram - фейковый, база -
# LIVE_DATABASE_URL (только intake_live на localhost:5434, тест сам это
# проверяет и делает TRUNCATE). GITHUB_TOKEN - обязательно make-переменной
# командной строки, не через `VAR=... make`: .env этого дерева тоже может
# держать GITHUB_TOKEN (для контура, не для песочницы), а make отдаёт
# приоритет файлу (-include .env) над окружением процесса - переменная той же
# командной строки перекрыла бы правильный токен молча. Верно:
#   make live GITHUB_TOKEN=$$(gh auth token) LIVE_DATABASE_URL=...
# Неверно (может тихо взять токен из .env):
#   GITHUB_TOKEN=$$(gh auth token) make live LIVE_DATABASE_URL=...
# 3h: liveStepDeadline даёт каждому шагу до двух попыток работы воркера
# (2*jobTimeout+запас), а сценариев и шагов в них достаточно, чтобы худший
# случай подряд превысил прежние 2h без единой настоящей поломки.
live: check-live-db
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(LIVE_DATABASE_URL)" up
	go test -tags live -count=1 -timeout 3h -v -run '^TestLiveRun$$' ./internal/app

check-live-db:
	@test -n "$(LIVE_DATABASE_URL)" || { echo "LIVE_DATABASE_URL is not set"; exit 1; }
	@host=$$(printf '%s' "$(LIVE_DATABASE_URL)" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||' -e 's|^.*@||'); \
	case "$(LIVE_DATABASE_URL)" in \
	postgres://*@localhost:5434/intake_live*|postgres://*@127.0.0.1:5434/intake_live*) ;; \
	*) echo "LIVE_DATABASE_URL must point to intake_live on localhost:5434, got host/db: $${host}"; exit 1 ;; \
	esac
	@test -n "$(GITHUB_TOKEN)" || { echo "GITHUB_TOKEN is not set"; exit 1; }
	@if [ "$(origin GITHUB_TOKEN)" = "file" ]; then \
		echo "GITHUB_TOKEN came from .env (origin=file), not from your command - .env wins over"; \
		echo "a plain env-prefix, so it may have silently replaced the real token. Pass it as a"; \
		echo "make variable instead: make live GITHUB_TOKEN=\$$(gh auth token) LIVE_DATABASE_URL=..."; \
		exit 1; \
	fi

fmt-check:
	@out=$$(gofmt -l ./cmd ./internal); \
	test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }

lint:
	golangci-lint run --timeout 120s

# Сканеры закреплены версией через go run, как goose: не зависят от PATH машины и CI.
GOVULNCHECK := go run golang.org/x/vuln/cmd/govulncheck@v1.8.0
GITLEAKS := go run github.com/zricethezav/gitleaks/v8@v8.30.1

# gitleaks не читает .gitignore, поэтому сканируется снимок того, что видит git
# (отслеживаемое и новое неигнорируемое), а не локальные .env и secrets/.
# История репозитория сюда не входит: только рабочее дерево.
security:
	$(GOVULNCHECK) ./...
	@set -e; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	root=$$(git rev-parse --show-toplevel); \
	cp "$$(git rev-parse --git-path index)" "$$tmp/index"; \
	GIT_INDEX_FILE="$$tmp/index" git -C "$$root" add -A; \
	tree=$$(GIT_INDEX_FILE="$$tmp/index" git -C "$$root" write-tree); \
	mkdir "$$tmp/tree"; git -C "$$root" archive "$$tree" | tar -x -C "$$tmp/tree"; \
	cd "$$tmp/tree" && $(GITLEAKS) dir --no-banner --redact .

# Кольцо коммита: секунды, без БД. Полный набор закрывает срез (ci-check).
commit-check: fmt-check vet lint test-fast

ci-check: migrate-up test vet fmt-check lint security build

# DATABASE_URL молча подхватывается из .env, поэтому migrate-* может уехать в
# чужую базу. Печатаем цель без пароля и требуем ALLOW_REMOTE=1 для всего, что
# не localhost.
check-db:
	@test -n "$(DATABASE_URL)" || { echo "DATABASE_URL is not set"; exit 1; }
	@rest=$$(printf '%s' "$(DATABASE_URL)" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||' -e 's|^.*@||'); \
	host=$${rest%%[:/?]*}; \
	echo "migration target: host=$${host:-local}"; \
	case "$$host" in \
	localhost|127.0.0.1|::1|"") ;; \
	*) \
		if [ "$(ALLOW_REMOTE)" = "1" ]; then \
			echo "WARNING: remote target $$host, running because ALLOW_REMOTE=1"; \
		else \
			echo "refusing to migrate non-local host $$host; re-run with ALLOW_REMOTE=1 if this is intended"; \
			exit 1; \
		fi ;; \
	esac

# Команды не эхом: в них целиком лежит DATABASE_URL с паролем, а цель миграции
# уже напечатал check-db.
migrate-up: check-db
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

migrate-down: check-db
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down

migrate-reset: check-db
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down-to 0
