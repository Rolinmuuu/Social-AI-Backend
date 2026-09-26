// Package db holds the PostgreSQL plumbing shared by the services: the connection pool,
// transactions and schema migrations.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is satisfied by *pgxpool.Pool and pgx.Tx, so a query helper can run inside or
// outside a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Open connects to url and waits up to wait for the database to accept connections (in
// docker compose the services can start before Postgres is ready).
func Open(ctx context.Context, url string, wait time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse url: %w", err)
	}
	return OpenConfig(ctx, cfg, wait)
}

// OpenConfig is Open with a parsed config.
func OpenConfig(ctx context.Context, cfg *pgxpool.Config, wait time.Duration) (*pgxpool.Pool, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = pool.Ping(pctx)
		cancel()
		if err == nil {
			return pool, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			pool.Close()
			return nil, fmt.Errorf("db: ping: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// InTx runs fn in a transaction: committed if fn returns nil, rolled back otherwise.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) // no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// IsUniqueViolation reports whether err is a unique/primary-key violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool { return sqlState(err) == "23505" }

// IsForeignKeyViolation reports whether err is a foreign-key violation (SQLSTATE 23503).
func IsForeignKeyViolation(err error) bool { return sqlState(err) == "23503" }

// IsCheckViolation reports whether err is a CHECK constraint violation (SQLSTATE 23514).
func IsCheckViolation(err error) bool { return sqlState(err) == "23514" }

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
