-- +goose Up
-- Репозитории проектов переехали из личного аккаунта в организацию galera-club.
-- SyncProjects существующие строки не трогает, поэтому владелец меняется здесь.
UPDATE projects SET github_owner = 'galera-club', updated_at = now()
WHERE github_owner = 'daniil4545';

-- +goose Down
-- Отката нет: GitHub перенаправляет старые адреса, бот прошлой версии работает
-- и с новым владельцем, а строки, заведённые после миграции, не отличить.
SELECT 1;
