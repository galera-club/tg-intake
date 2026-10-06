package app

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Обе стороны миграции: down-to 0, а не down - down откатывает ровно одну.
// DSN из TEST_DATABASE_URL, не из DATABASE_URL: тест дропает все таблицы, а
// DATABASE_URL молча подхватывается из .env и может смотреть в dev-контур.
func TestMigrations(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}

	const dir = "../../migrations"
	if err := goose.Up(db, dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := goose.DownTo(db, dir, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if err := goose.Up(db, dir); err != nil {
		t.Fatalf("up after down: %v", err)
	}
}

// TestMigration0011: обращения в полёте переживают смену контракта на ядро
// (Р-14). Каждый старый набор ключей переводится, закрытые обращения и те, где
// контракта ещё нет, не трогаются, updated_at остаётся прежним.
func TestMigration0011(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	const dir = "../../migrations"
	// Остальные тесты пакета ждут схему последней версии.
	t.Cleanup(func() {
		if err := goose.Up(db, dir); err != nil {
			t.Errorf("restore schema: %v", err)
		}
	})
	// UpTo не откатывает: база может стоять выше, поэтому сначала вверх, потом
	// вниз до версии перед 0011.
	if err := goose.Up(db, dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := goose.DownTo(db, dir, 10); err != nil {
		t.Fatalf("down to 10: %v", err)
	}
	if _, err := db.Exec(`TRUNCATE cases, case_items, case_events, jobs, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	type row struct {
		name, status, kind, contract, gaps string
		incomplete                         bool
		wantContract, wantGaps             string
		wantIncomplete                     bool
	}
	rows := []row{
		{"баг закрыт", "interview", "bug", `{"case":"c","expected":"e","actual":"a","where":"w"}`, `["when"]`, false,
			`{"case":"c","wrong":"e\na"}`, `[]`, false},
		{"баг без факта", "summary", "bug", `{"case":"c","expected":"e"}`, `["actual","when"]`, true,
			`{"case":"c"}`, `["wrong"]`, true},
		{"баг без случая", "publishing", "bug", `{"expected":"e","actual":"a"}`, `["case"]`, false,
			`{"wrong":"e\na"}`, `["case"]`, true},
		{"пожелание закрыто", "summary", "feature", `{"problem":"p","today":"t","result":"r"}`, `["done","who"]`, true,
			`{"why":"p","need":"r"}`, `[]`, false},
		{"пожелание без результата", "interview", "feature", `{"problem":"p","today":"t"}`, `["result","done"]`, false,
			`{"why":"p"}`, `["need"]`, false},
		{"вопрос", "interview", "question", `{"question":"q","context":"x"}`, `[]`, false,
			`{"question":"q"}`, `[]`, false},
		{"до первого хода", "interview", "", `{}`, `[]`, false, `{}`, `[]`, false},
		{"коллизия ключей", "summary", "feature", `{"result":"old","need":"new","why":"w"}`, `[]`, false,
			`{"need":"new","why":"w"}`, `[]`, false},
		{"уже ядро", "summary", "bug", `{"case":"c","wrong":"w"}`, `[]`, false,
			`{"case":"c","wrong":"w"}`, `[]`, false},
		{"опубликовано", "published", "bug", `{"expected":"e"}`, `["actual"]`, true,
			`{"expected":"e"}`, `["actual"]`, true},
		{"сбор", "collecting", "bug", `{"expected":"e"}`, `["actual"]`, false,
			`{"expected":"e"}`, `["actual"]`, false},
		{"спросить", "answering", "bug", `{"expected":"e"}`, `["actual"]`, false,
			`{"expected":"e"}`, `["actual"]`, false},
	}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ids := make([]string, len(rows))
	for n, r := range rows {
		userID := 9100 + n
		if _, err := db.Exec(`INSERT INTO users (telegram_id, first_name, slug) VALUES ($1, 'Тест', $2)`,
			userID, fmt.Sprintf("test-%d", n)); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		err := db.QueryRow(`
			INSERT INTO cases (user_id, project_id, status, kind, contract, gaps, incomplete, updated_at)
			VALUES ($1, (SELECT id FROM projects WHERE slug = 'tg-intake'), $2, NULLIF($3, ''), $4, $5, $6, $7)
			RETURNING id`, userID, r.status, r.kind, r.contract, r.gaps, r.incomplete, stamp).Scan(&ids[n])
		if err != nil {
			t.Fatalf("insert case %q: %v", r.name, err)
		}
	}

	if err := goose.UpTo(db, dir, 11); err != nil {
		t.Fatalf("up to 11: %v", err)
	}

	for n, r := range rows {
		var sameContract, sameGaps, incomplete bool
		var status string
		var updated time.Time
		err := db.QueryRow(`
			SELECT contract = $2::jsonb, gaps = $3::jsonb, incomplete, status, updated_at
			FROM cases WHERE id = $1`, ids[n], r.wantContract, r.wantGaps).
			Scan(&sameContract, &sameGaps, &incomplete, &status, &updated)
		if err != nil {
			t.Fatalf("read case %q: %v", r.name, err)
		}
		if !sameContract || !sameGaps || incomplete != r.wantIncomplete {
			var contract, gaps string
			_ = db.QueryRow(`SELECT contract::text, gaps::text FROM cases WHERE id = $1`, ids[n]).Scan(&contract, &gaps)
			t.Errorf("%s: contract %s, gaps %s, incomplete %v; ожидалось %s, %s, %v",
				r.name, contract, gaps, incomplete, r.wantContract, r.wantGaps, r.wantIncomplete)
		}
		if status != r.status || !updated.Equal(stamp) {
			t.Errorf("%s: status %s, updated_at %v; ожидалось %s, %v", r.name, status, updated, r.status, stamp)
		}
	}
	var jobs int
	if err := db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 0 {
		t.Errorf("миграция поставила работ: %d", jobs)
	}

	// Откат: смесь становится багом, новых ключей в живых обращениях нет, и
	// CHECK снова не принимает mixed.
	mixed := ids[4]
	if _, err := db.Exec(`UPDATE cases SET kind = 'mixed', gaps = '["wrong", "need"]' WHERE id = $1`, mixed); err != nil {
		t.Fatalf("mark mixed: %v", err)
	}
	if err := goose.DownTo(db, dir, 10); err != nil {
		t.Fatalf("down to 10: %v", err)
	}
	var kind, contract, gaps string
	var updated time.Time
	err = db.QueryRow(`SELECT kind, contract::text, gaps::text, updated_at FROM cases WHERE id = $1`, mixed).
		Scan(&kind, &contract, &gaps, &updated)
	if err != nil {
		t.Fatalf("read after down: %v", err)
	}
	if kind != "bug" || contract != "{}" || gaps != "[]" || !updated.Equal(stamp) {
		t.Errorf("после отката: kind %s, contract %s, gaps %s, updated_at %v; ожидалось bug, {}, [], %v",
			kind, contract, gaps, updated, stamp)
	}
	if _, err := db.Exec(`UPDATE cases SET kind = 'mixed' WHERE id = $1`, mixed); err == nil {
		t.Error("после отката CHECK принял mixed")
	}
}

// TestMigration0012: строка, заведённая до миграции, получает нулевой живой
// экран (screen_msg = 0 - «экрана нет», screen_round = 0 - «не раунд»), а
// откат убирает обе колонки без следа (строка 1 §3b плана live-screen).
func TestMigration0012(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	const dir = "../../migrations"
	// Остальные тесты пакета ждут схему последней версии.
	t.Cleanup(func() {
		if err := goose.Up(db, dir); err != nil {
			t.Errorf("restore schema: %v", err)
		}
	})

	if err := goose.UpTo(db, dir, 11); err != nil {
		t.Fatalf("up to 11: %v", err)
	}
	if _, err := db.Exec(`TRUNCATE cases, case_items, case_events, jobs, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (telegram_id, first_name, slug) VALUES (9400, 'Тест', 'test-0012')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	var id string
	err = db.QueryRow(`
		INSERT INTO cases (user_id, status) VALUES (9400, 'collecting') RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("insert case: %v", err)
	}

	if err := goose.UpTo(db, dir, 12); err != nil {
		t.Fatalf("up to 12: %v", err)
	}
	var screenMsg, screenRound int
	if err := db.QueryRow(`SELECT screen_msg, screen_round FROM cases WHERE id = $1`, id).
		Scan(&screenMsg, &screenRound); err != nil {
		t.Fatalf("read screen columns: %v", err)
	}
	if screenMsg != 0 || screenRound != 0 {
		t.Errorf("после миграции: screen_msg=%d screen_round=%d, ожидалось 0 и 0", screenMsg, screenRound)
	}

	if err := goose.DownTo(db, dir, 11); err != nil {
		t.Fatalf("down to 11: %v", err)
	}
	var columns int
	if err := db.QueryRow(`
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'cases' AND column_name IN ('screen_msg', 'screen_round')`).Scan(&columns); err != nil {
		t.Fatalf("check columns: %v", err)
	}
	if columns != 0 {
		t.Errorf("после отката колонки живого экрана остались: %d", columns)
	}
}
