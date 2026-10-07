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
| G0 | команда владельца «катим intake 0.2.4», 07.10 ~08:10 МСК |
| G1 | dev-контура нет; замещение - тесты 3b среза и живая проба доски токеном прода 07.10 |
| G2 | `make ci-check` с `TEST_DATABASE_URL` на чистой базе: см. «Порядок» |
| G3 | после сборки: `docker manifest inspect` |
| G4 | DDL нет; база под ночным дампом хоста |
| G5 | Compose менялся, `sync-compose` первым |
| G6 | 05:16 UTC: живых интервью нет, `job-errors` - известные (801 публикация 06.10, до этапа 1) |
| G7 | тег `v0.2.4` на мердж-коммите |

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

## G8. Приёмка

Заполняется после выката.

## G9. Анонс

Не публикуется: служебный релиз без видимых клубу изменений, раздел 0.2.4 помечен «анонса нет».

## Остаток риска

- Шаг доски в самом боте вживую проверит первое новое обращение; до него доказательство -
  тесты и проба мутации токеном прода.
