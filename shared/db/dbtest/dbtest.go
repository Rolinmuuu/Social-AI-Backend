// Package dbtest gives tests a real, migrated PostgreSQL schema of their own.
//
// Tests that touch SQL run against Postgres, not a fake: constraints, ON CONFLICT, row locks
// and SKIP LOCKED are the behaviour under test, and a fake would only mirror our assumptions.
//
// Set TEST_DATABASE_URL (e.g. postgres://postgres@localhost:5432/postgres?sslmode=disable).
// Without it these tests are skipped, unless REQUIRE_DB=1 (as in CI), which turns a missing
// database into a failure so the suite cannot silently pass without them.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"socialai/migrations"
	"socialai/shared/db"

	"github.com/jackc/pgx/v5/pgxpool"
)

// New creates a fresh schema, applies all migrations to it and returns a pool whose
// connections use it (search_path). The schema is dropped when the test ends, so tests can
// run in parallel without seeing each other's rows.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_DB") != "" {
			t.Fatal("TEST_DATABASE_URL is not set but REQUIRE_DB is")
		}
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL test")
	}
	ctx := context.Background()

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)

	admin, err := db.Open(ctx, url, 5*time.Second)
	if err != nil {
		t.Fatalf("dbtest: connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("dbtest: create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("dbtest: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 16
	pool, err := db.OpenConfig(ctx, cfg, 5*time.Second)
	if err != nil {
		t.Fatalf("dbtest: connect: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	if _, err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("dbtest: migrate: %v", err)
	}
	return pool
}
