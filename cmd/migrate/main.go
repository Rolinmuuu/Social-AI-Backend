// Command migrate applies the SQL migrations in migrations/ to DATABASE_URL.
//
// It runs as a one-shot job before the services start (a compose service with
// `service_completed_successfully`, a Kubernetes Job or a Cloud Run job), not inside every
// service at startup: N replicas racing to migrate on boot is how schema changes go wrong,
// and a failed migration should stop a deploy, not crash-loop the API.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"socialai/migrations"
	"socialai/shared/constants"
	"socialai/shared/db"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, constants.DATABASE_URL, 60*time.Second)
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	defer pool.Close()

	applied, err := db.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if len(applied) == 0 {
		log.Printf("migrate: schema is up to date")
		return
	}
	log.Printf("migrate: applied %v", applied)
}
