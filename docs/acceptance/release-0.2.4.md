# Выкат 0.2.4: паспорт релиза

## Состояние на 07.10.2026

- Кандидат: PR #48 `feature/org-board` в `prod` (galera-tasks#102), плюс влитый ранее и не
  выкаченный PR #47 (единые проверки, galera-tasks#137).
- Чужая работа: prunable worktree `fix/prod-compose-no-proxy` (старая сессия), ветки
  `chore/checks-unify`, `feature/tickets-paging`, `slice-*` без открытых PR; релиз не задевают.
  Открытых PR в `prod`, кроме #48, нет. Неотслеживаемый `docs/research/publish-forbidden-*`
  оставила утренняя сессия этапа 1, в релиз не идёт.
- Версия `v0.2.4`, названа владельцем.
- Прод до выката: `9d08cd603821435848c3b21a8da3e5c4b0fff9ed` (`v0.2.3`), схема 12.
- Что едет: тикет бота сразу встаёт на доску Galera; адреса galera-club; автор изменений не
  заметит.

## Развилки

| Развилка | Значение |
| --- | --- |
| Миграции | 0013: `projects.github_owner` `daniil4545` в `galera-club`, без DDL; в проде затронет 4 строки (прочитано 07.10), down пустой |
| Числа наружу | нет |
| Compose | одна строка `GITHUB_BOARD_ID: ${GITHUB_BOARD_ID:-}` |
| Новые переменные | `GITHUB_BOARD_ID`: заводится пустой при `sync-compose`, значение - `set-env` до выката |
| Образ | первый в пакете `ghcr.io/galera-club/tg-intake`; проверить, что узел A его скачивает |
| Пользовательские изменения | для клуба нет: релиз без анонса (G9 не проводится) |
| Откат | `GALERA_IMAGE_OWNER=daniil4545 coolify-deploy.sh intake release 9d08cd6...`; миграция 0013 не мешает, редирект GitHub держит старые адреса |

## Ворота до выката

| Ворота | Состояние |
| --- | --- |
| G0 | команда владельца «катим intake 0.2.4», 07.10 08:10 МСК |
| G1 | dev-контура нет; замещение - тесты 3b среза и живая проба доски токеном прода 07.10 |
| G2 | `make ci-check RUN='.'` на `ac82aac` с чистой базой (миграции 1-13, тесты без кеша, 0 skip, lint 0 issues, govulncheck, gitleaks, build) - ok; мердж-коммит `6086d62` того же дерева |
| G3 | `sha-6086d62...` собран, `docker manifest inspect` - index amd64; пакет новый, приватный, узел A его скачал |
| G4 | DDL нет; база под ночным дампом хоста |
| G5 | Compose менялся, `sync-compose` первым |
| G6 | 05:16 UTC: живых интервью нет, `job-errors` - известные (801 публикация 06.10, до этапа 1) |
| G7 | тег `v0.2.4` на `6086d62`, push сделан |

Завершение среза закрыто 07.10: хвостов кода нет; живой прогон во внешних системах - только
проба мутации на тикете qualifier#121, уже стоявшем на доске (след не оставлен); спека и дифф
сверены ревью; `docs/contracts.md`, `docs/prd.md`, `AGENTS.md` обновлены; спека среза удалена
релизным коммитом, решения - в `docs/contracts.md`.

## Окно выката

Расписаний отправок нет, кроме `watch_tick` раз в 5 минут. Обращение 5c925aac со 06.10 в
`summary` ждёт «Публикую» автора, выкат его не трогает. Выкат до 09:00 МСК.

## Порядок

```sh
gh pr merge 48 --merge && git pull --ff-only
git tag -a v0.2.4 -F <тело> <sha> && git push origin v0.2.4
CONFIRM=yes coolify-deploy.sh intake build <sha>
docker manifest inspect ghcr.io/galera-club/tg-intake:sha-<sha>
CONFIRM=yes coolify-deploy.sh intake sync-compose
CONFIRM=yes coolify-deploy.sh intake set-env GITHUB_BOARD_ID <id доски>
CONFIRM=yes coolify-deploy.sh intake release <sha>
```

## G8. Приёмка - PASS, 07.10 08:36 МСК

1. `app_revision` = `6086d6267d74954e3a87d0f4e099a29efee029de`.
2. `app` и `postgres` healthy, `migrate` Exited (0), в его логе `OK 0013_org_owner.sql`.
3. Логи старта без ERROR и WARN: `db_connected`, `openrouter_ready`, `github_write_ok` и
   `github_read_ok` по 4 проекта уже по адресам galera-club, `alerts_enabled`, `bot_started`.
4. Stop signals нет: `job-errors` без новых записей, незавершённых деплоев нет.
5. Функциональная - замещающая: в базе прода 4 проекта на владельце `galera-club`;
   `GITHUB_BOARD_ID` задана в окружении `app` (значение не печаталось); мутация доски
   токеном прода проверена 07.10 до выката. Живое обращение не заводилось: интервью зовёт
   платную модель, а публиковать за сотрудника агент не может.

## Закрытие

- GitHub Release `v0.2.4`, `Latest`.
- galera-tasks#148 и #102: `status:prod`, закрыты; отзыв старого токена - владельцу.

## G9. Анонс

Не публикуется: служебный релиз без видимых клубу изменений, раздел 0.2.4 помечен «анонса нет».

## Остаток риска

- Шаг доски в самом боте вживую проверит первое обращение: 5c925aac (06.10, `summary`)
  публикуется повторным «Публикую» автора и должен встать на доску.
