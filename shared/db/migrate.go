package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockKey is the pg_advisory_lock key held while migrating, so two migrators
// started at the same time (two deploys, a retried job) apply each file once.
const migrationLockKey = 0x50c1a1

// Migration is one applied or pending schema change.
type Migration struct {
	Version  string // file name without .sql, e.g. "0001_init"
	SQL      string
	Checksum string
}

// LoadMigrations reads *.sql from fsys in name order.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([]Migration, 0, len(names))
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out = append(out, Migration{
			Version:  strings.TrimSuffix(n, ".sql"),
			SQL:      string(b),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	return out, nil
}

// Migrate applies the migrations in fsys that are not applied yet, each in its own
// transaction together with its schema_migrations row. It refuses to run if an applied
// migration's file was edited afterwards: the database would no longer match the code, and
// that must be fixed with a new migration, not by rewriting history.
//
// Returns the versions applied by this call.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) ([]string, error) {
	migrations, err := LoadMigrations(fsys)
	if err != nil {
		return nil, fmt.Errorf("migrate: load: %w", err)
	}

	// Session-level advisory lock on one dedicated connection for the whole run.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return nil, fmt.Errorf("migrate: lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockKey)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		checksum   text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	applied := map[string]string{}
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v, c string
		if err := rows.Scan(&v, &c); err != nil {
			rows.Close()
			return nil, err
		}
		applied[v] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var done []string
	for _, m := range migrations {
		if sum, ok := applied[m.Version]; ok {
			if sum != m.Checksum {
				return done, fmt.Errorf("migrate: %s was changed after it was applied (checksum %s, file %s)", m.Version, sum[:12], m.Checksum[:12])
			}
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)`, m.Version, m.Checksum)
			return err
		})
		if err != nil {
			return done, fmt.Errorf("migrate: apply %s: %w", m.Version, err)
		}
		done = append(done, m.Version)
	}
	return done, nil
}
