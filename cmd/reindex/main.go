// Command reindex rebuilds the post search index from PostgreSQL.
//
//	reindex                        re-sync every post into the live index (repair, backfill)
//	reindex -new-index posts_v2    build a new physical index, fill it, then point the
//	                               "posts" alias at it in one atomic call (mapping changes)
//
// Both are safe while the search-indexer runs: every write carries the row's version.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"socialai/services/indexer/worker"
	sharedBackend "socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/db"
)

func main() {
	newIndex := flag.String("new-index", "", "build this new index and move the alias to it")
	pageSize := flag.Int("page", 500, "posts read per query")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, constants.DATABASE_URL, 30*time.Second)
	if err != nil {
		log.Fatalf("reindex: %v", err)
	}
	defer pool.Close()
	es, err := sharedBackend.InitElasticsearchBackend()
	if err != nil {
		log.Fatalf("reindex: %v", err)
	}

	x := &worker.Indexer{DB: pool, ES: es, Index: constants.SEARCH_POST_ALIAS}
	if constants.OPENAI_API_KEY != "" {
		if x.OpenAI, err = sharedBackend.InitOpenAIBackend(); err != nil {
			log.Fatalf("reindex: %v", err)
		}
	}
	if *newIndex != "" {
		if err := es.CreateIndex(ctx, *newIndex, sharedBackend.PostSearchMapping); err != nil {
			log.Fatalf("reindex: %v", err)
		}
		x.Index = *newIndex
	}

	start := time.Now()
	n, err := x.Reindex(ctx, *pageSize, func(done int) { log.Printf("reindex: %d posts", done) })
	if err != nil {
		log.Fatalf("reindex: stopped after %d posts: %v", n, err)
	}
	if *newIndex != "" {
		// Moving the alias switches readers and the event-driven indexer at once.
		if err := es.PointAlias(ctx, constants.SEARCH_POST_ALIAS, *newIndex); err != nil {
			log.Fatalf("reindex: %v", err)
		}
		log.Printf("reindex: alias %s now points at %s", constants.SEARCH_POST_ALIAS, *newIndex)
		// A post changed after the build read it but before the alias moved had its event
		// applied to the old index. One more pass catches those up; everything unchanged is
		// refused by version and costs only the write attempt.
		if _, err := x.Reindex(ctx, *pageSize, nil); err != nil {
			log.Fatalf("reindex: catch-up pass: %v", err)
		}
	}
	log.Printf("reindex: %d posts in %s", n, time.Since(start).Round(time.Millisecond))
}
