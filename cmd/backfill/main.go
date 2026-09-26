// Command backfill is the one-time data migration from the legacy Elasticsearch indices
// (user, follow, post, like, share, comment, message, notification) into PostgreSQL.
//
// It is idempotent and resumable: run it, check the report, run it again after fixing
// anything. See docs/MIGRATION.md for the whole cut-over procedure.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"socialai/migrations"
	sharedBackend "socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/db"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, constants.DATABASE_URL, 60*time.Second)
	if err != nil {
		log.Fatalf("backfill: %v", err)
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		log.Fatalf("backfill: %v", err)
	}
	es, err := sharedBackend.InitElasticsearchBackend()
	if err != nil {
		log.Fatalf("backfill: %v", err)
	}

	start := time.Now()
	rep, err := (&Backfill{ES: es, DB: pool, Logf: log.Printf}).Run(ctx)
	out, _ := json.MarshalIndent(rep, "", "  ")
	os.Stdout.Write(append(out, '\n'))
	if err != nil {
		log.Fatalf("backfill: %v", err)
	}
	log.Printf("backfill: done in %s; now run cmd/reindex to build the search index", time.Since(start).Round(time.Second))
}
