#!/usr/bin/env bash
# Именованные read-only команды к серверу вместо произвольного ssh.
# Все действия только читают: ни одно из них не меняет состояние контура.
set -euo pipefail

# Адреса узлов в публичный репозиторий не попадают: хост задаётся окружением,
# значение - карточка intake-prod базы знаний.
host="${INTAKE_SSH_HOST:?INTAKE_SSH_HOST не задан: адрес узла - карточка intake-prod базы знаний}"
resource="${INTAKE_RESOURCE:-intake}"
action="${1:-}"
arg="${2:-}"

usage() {
    cat <<'USAGE'
Usage: deploy/safe-ssh.sh <action> [args...]

Read-only actions:
  inventory        контейнеры и образы контура
  health           ревизия образа, health контейнеров, код выхода миграций
  app-logs [N]     логи приложения, по умолчанию 200 строк, максимум 1000
  db-logs [N]      логи PostgreSQL, по умолчанию 200 строк, максимум 1000
  app-env          переменные окружения приложения, значения скрыты
  db-counts        число строк по таблицам, без содержимого
  case-trace [N]   ход разговора: события обращений, статус и раунд, без текстов
  cases [N]        последние обращения: id, статус, проект, автор, тикет
  case <id>        обращение целиком с текстами: протокол, сырьё, события, работы
  job-errors [N]   упавшие и повторяющиеся работы очереди с ошибками
  eval-export      набор для make eval: JSON-строка на обращение, только в файл
  db-dump          pg_dump -Fc базы в stdout, только в файл: переезд узла
  deploy-config    имена переменных сервиса в Coolify и версия его compose
  deploy-state     образ кандидата на хосте и незавершённые деплои Coolify

INTAKE_SSH_HOST - обязательный адрес узла, user@host.
INTAKE_RESOURCE переопределяет ресурс Coolify intake.
USAGE
}

tail_lines() {
    local value="${1:-200}"
    if [[ ! "$value" =~ ^[0-9]+$ ]]; then
        echo "tail-lines must be a number" >&2
        exit 2
    fi
    if (( value > 1000 )); then
        value=1000
    fi
    echo "$value"
}

ssh_opts=(
    -o BatchMode=yes
    -o StrictHostKeyChecking=accept-new
    -o ConnectTimeout=10
)

filter="--filter label=coolify.resourceName=$resource"
lines=""
remote=""

case "$action" in
    inventory)
        remote="docker ps -a $filter --format 'service={{.Label \"com.docker.compose.service\"}} name={{.Names}} image={{.Image}} status={{.Status}}' | sort"
        ;;
    health)
        # migrate - one-shot: отсутствие контейнера после очистки не ошибка,
        # ошибка - существующий контейнер с ненулевым кодом выхода.
        remote="app=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=app' | head -1); db=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=postgres' | head -1); mig=\$(docker ps -aq $filter --filter 'label=com.docker.compose.service=migrate' | head -1); test -n \"\$app\" || { echo 'app=not_running'; exit 1; }; test -n \"\$db\" || { echo 'postgres=not_running'; exit 1; }; docker inspect -f 'app_revision={{index .Config.Labels \"org.opencontainers.image.revision\"}} app_health={{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} app_restart={{.HostConfig.RestartPolicy.Name}}' \"\$app\"; docker inspect -f 'postgres_health={{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' \"\$db\"; if test -n \"\$mig\"; then echo \"migrate_state=\$(docker inspect -f '{{.State.Status}}:{{.State.ExitCode}}' \"\$mig\")\"; else echo 'migrate_state=absent'; fi; test \"\$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' \"\$app\")\" = healthy"
        ;;
    app-logs)
        lines="$(tail_lines "$arg")"
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=app' | head -1); test -n \"\$c\" || { echo 'app=not_running'; exit 1; }; docker logs --tail $lines \"\$c\" 2>&1"
        ;;
    db-logs)
        lines="$(tail_lines "$arg")"
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$c\" || { echo 'postgres=not_running'; exit 1; }; docker logs --tail $lines \"\$c\" 2>&1"
        ;;
    db-counts)
        # Только счётчики: строки таблиц содержат ФИО автора и текст обращения,
        # им место в базе, а не в терминале.
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$c\" || { echo 'postgres=not_running'; exit 1; }; docker exec \"\$c\" psql -U intake -d intake -tAc \"select 'projects='||(select count(*) from projects)||' users='||(select count(*) from users)||' cases='||(select count(*) from cases)||' case_items='||(select count(*) from case_items)||' jobs='||(select count(*) from jobs)\""
        ;;
    cases)
        # Содержимое обращений (тексты, транскрипты) выводится в терминал для
        # дебага - решение владельца 2026-08-09. В git и логи сервиса оно
        # по-прежнему не попадает.
        lines="$(tail_lines "$arg")"
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$c\" || { echo 'postgres=not_running'; exit 1; }; docker exec \"\$c\" psql -U intake -d intake -tAc \"select left(c.id::text,8)||' status='||c.status||' kind='||coalesce(c.kind,'-')||' project='||coalesce(p.slug,'-')||' user='||c.user_id||' issue='||coalesce(c.issue_number::text,'-')||' updated='||to_char(c.updated_at,'MM-DD HH24:MI') from cases c left join projects p on p.id = c.project_id order by c.updated_at desc limit $lines\""
        ;;
    case)
        # Префикс id из вывода cases. Маска обязательна: аргумент уходит и в
        # shell, и в SQL, произвольная строка тут - инъекция.
        if [[ ! "$arg" =~ ^[0-9a-f][0-9a-f-]{3,35}$ ]]; then
            echo "case <id>: нужен префикс uuid из вывода cases (минимум 4 символа)" >&2
            exit 2
        fi
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$c\" || { echo 'postgres=not_running'; exit 1; }; docker exec \"\$c\" psql -U intake -d intake -tAc \"
            select '=== case ==='||chr(10)||'id='||id||' status='||status||' kind='||coalesce(kind,'-')||' round='||round||' incomplete='||incomplete||' issue='||coalesce(issue_number::text,'-')||coalesce(chr(10)||'url='||issue_url,'')||chr(10)||'=== title ==='||chr(10)||coalesce(title,'-')||chr(10)||'=== protocol ==='||chr(10)||coalesce(protocol,'-')||chr(10)||'=== contract ==='||chr(10)||coalesce(contract::text,'-')||chr(10)||'=== gaps ==='||chr(10)||coalesce(gaps::text,'-')||chr(10)||'=== summary ==='||chr(10)||coalesce(summary,'-') from cases where id::text like '$arg%';
            select '=== item '||id||' ==='||chr(10)||'kind='||kind||' status='||status||coalesce(' error='||nullif(error,''),'')||' file_id='||coalesce(tg_file_id,'-')||chr(10)||'source: '||coalesce(source_text,'-')||chr(10)||'normalized: '||coalesce(normalized,'-') from case_items where case_id in (select id from cases where id::text like '$arg%') order by id;
            select '=== event '||id||' '||to_char(created_at,'MM-DD HH24:MI:SS')||' '||kind||' ==='||chr(10)||coalesce(payload::text,'') from case_events where case_id in (select id from cases where id::text like '$arg%') order by id;
            select '=== job '||id||' ==='||chr(10)||kind||' status='||status||' attempts='||attempts||coalesce(chr(10)||'error='||nullif(last_error,''),'') from jobs where payload->>'case_id' in (select id::text from cases where id::text like '$arg%') order by id;\""
        ;;
    job-errors)
        lines="$(tail_lines "$arg")"
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$c\" || { echo 'postgres=not_running'; exit 1; }; docker exec \"\$c\" psql -U intake -d intake -tAc \"select id||' '||kind||' status='||status||' attempts='||attempts||' case='||coalesce(left(payload->>'case_id',8),'-')||' updated='||to_char(updated_at,'MM-DD HH24:MI')||coalesce(chr(10)||'  error: '||nullif(last_error,''),'') from jobs where status = 'failed' or attempts > 1 order by updated_at desc limit $lines\""
        ;;
    eval-export)
        # Набор прогона eval (спека ticket-form, Р-10): тексты обращений идут
        # сразу в файл вне git, а не в терминал. Автор не выгружается: прогону
        # он не нужен, а набор лежит на машине разработчика.
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$c\" || { echo 'postgres=not_running' >&2; exit 1; }; docker exec \"\$c\" psql -U intake -d intake -tAc \"select json_build_object('id', c.id, 'project', p.slug, 'context', p.context, 'protocol', c.protocol, 'kind', c.kind, 'events', coalesce((select json_agg(json_build_object('kind', e.kind, 'payload', e.payload) order by e.id) from case_events e where e.case_id = c.id and e.kind in ('round_asked', 'answer_given', 'interview_done', 'summary_ready', 'switched_to_ticket')), '[]'::json)) from cases c join projects p on p.id = c.project_id where c.mode = 'ticket' and c.protocol <> '' order by c.created_at\""
        ;;
    db-dump)
        # Окно переезда: база уходит потоком на машину оператора, на сервере
        # ничего не пишется. Вывод двоичный и с текстами обращений - только в файл.
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$c\" || { echo 'postgres=not_running' >&2; exit 1; }; docker exec \"\$c\" pg_dump -U intake -Fc intake"
        ;;
    case-trace)
        # Ход разговора без единого слова автора: вид события, время, статус и
        # раунд. Тексты вопросов, ответов и саммари лежат в payload и наружу не
        # выходят - разбирать надо последовательность, а не содержание.
        lines="$(tail_lines "$arg")"
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$c\" || { echo 'postgres=not_running'; exit 1; }; docker exec \"\$c\" psql -U intake -d intake -tAc \"select e.created_at||' case='||left(e.case_id::text,8)||' event='||e.kind||' status='||c.status||' round='||c.round from case_events e join cases c on c.id = e.case_id order by e.id desc limit $lines\" | tac"
        ;;
    deploy-state)
        # Зелёный workflow означает «Coolify принял запрос». Выкат мог не
        # начаться: образ не стянут, помощник деплоя висит, очередь стоит.
        remote="echo '--- образы кандидата ---'; docker images ghcr.io/galera-club/tg-intake --format '{{.Tag}} {{.CreatedSince}}' | head -5; echo '--- сам coolify ---'; docker ps -a --format '{{.Names}} {{.Status}}' | grep -i coolify | head -8; echo '--- ошибки coolify ---'; docker logs coolify --since 30m 2>&1 | grep -iE 'error|exception|failed|intake' | tail -12 || echo 'logs=empty'"
        ;;
    deploy-config)
        # Окружение контейнера отвечает на вопрос «что получил работающий
        # процесс», а не «что заведено в контуре»: до выката нового compose
        # свежая переменная в контейнер не попадает и выглядит отсутствующей.
        # Значения скрыты: там токен бота и ключ OpenRouter.
        remote="name=\$(docker ps -a $filter --filter 'label=com.docker.compose.service=app' --format '{{.Names}}' | head -1); test -n \"\$name\" || { echo 'app=not_found'; exit 1; }; uuid=\${name#app-}; dir=/data/coolify/services/\$uuid; test -d \"\$dir\" || { echo \"service_dir=not_found uuid=\$uuid\"; exit 1; }; echo \"service_uuid=\$uuid\"; echo '--- env keys ---'; sed 's/=.*/=<set>/' \"\$dir/.env\" 2>/dev/null | grep -v '^#' | grep -v '^\$' | sort || echo 'env=absent'; echo '--- APP_IMAGE ---'; grep '^APP_IMAGE=' \"\$dir/.env\" 2>/dev/null || echo 'APP_IMAGE=absent'; echo '--- compose ---'; grep -cE 'OPENROUTER_API_KEY|MEDIA_DIR' \"\$dir/docker-compose.yml\" 2>/dev/null | sed 's/^/m1_markers=/'"
        ;;
    app-env)
        # Значения скрыты намеренно: в окружении лежат токен бота и пароль базы.
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=app' | head -1); test -n \"\$c\" || { echo 'app=not_running'; exit 1; }; docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' \"\$c\" | sed 's/=.*/=<set>/' | sort"
        ;;
    *)
        usage
        exit 2
        ;;
esac

exec ssh "${ssh_opts[@]}" "$host" "$remote"
