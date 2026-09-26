// Command indexer (search-indexer) keeps the Elasticsearch post index in step with
// PostgreSQL by consuming post.created and post.deleted. See worker/indexer.go.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"socialai/services/indexer/worker"
	sharedBackend "socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/consumer"
	"socialai/shared/db"
	"socialai/shared/kafka"
	"socialai/shared/logger"
	"socialai/shared/metrics"
	"socialai/shared/model"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

func main() {
	logger.InitLogger(constants.LOGSTASH_ADDRESS)
	defer logger.Logger.Sync()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := db.Open(ctx, constants.DATABASE_URL, 60*time.Second)
	if err != nil {
		log.Fatalf("PostgreSQL init failed: %v", err)
	}
	defer pool.Close()

	esBackend, err := sharedBackend.InitElasticsearchBackend()
	if err != nil {
		log.Fatalf("ES init failed: %v", err)
	}

	x := &worker.Indexer{DB: pool, ES: esBackend, Index: constants.SEARCH_POST_ALIAS}
	if constants.OPENAI_API_KEY != "" {
		ai, err := sharedBackend.InitOpenAIBackend()
		if err != nil {
			log.Fatalf("OpenAI init failed: %v", err)
		}
		x.OpenAI = ai
	} else {
		log.Printf("OPENAI_API_KEY not set: indexing without embeddings (keyword search only)")
	}

	topics := []string{model.TopicPostCreated, model.TopicPostDeleted}
	tctx, tcancel := context.WithTimeout(ctx, 2*time.Minute)
	if err := kafka.EnsureTopics(tctx, constants.KAFKA_BROKERS, topics...); err != nil {
		log.Printf("%v (continuing; the consumer retries)", err)
	}
	tcancel()

	source := kafka.NewKafkaGroupConsumer(constants.KAFKA_BROKERS, topics, "search-indexer-group")
	defer source.Close()
	dlq := kafka.NewKafkaProducer(constants.KAFKA_BROKERS)
	defer dlq.Close()

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(":9103", mux); err != nil {
			log.Printf("metrics server: %v", err)
		}
	}()

	// Posts indexed while OpenAI was failing get their vectors here.
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := x.RepairMissingEmbeddings(ctx, 100); err != nil {
					log.Printf("embedding repair: %v", err)
				}
			}
		}
	}()

	c := &consumer.Consumer{
		Source:  source,
		DLQ:     dlq,
		Metrics: metrics.Consumer{},
		Config:  consumer.Config{Workers: 8, DLQTopic: "search-indexer.dlq"},
		Logf:    log.Printf,
	}
	logger.Logger.Info("search-indexer starting", zap.Strings("brokers", constants.KAFKA_BROKERS))
	if err := c.Run(ctx, func(ctx context.Context, m consumer.Message) error {
		return x.Handle(ctx, m.Value)
	}); err != nil {
		logger.Logger.Error("search-indexer stopped", zap.Error(err))
	}
}
