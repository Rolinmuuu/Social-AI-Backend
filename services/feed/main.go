package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"socialai/services/feed/worker"
	sharedBackend "socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/consumer"
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

	esBackend, err := sharedBackend.InitElasticsearchBackend()
	if err != nil {
		log.Fatalf("ES init failed: %v", err)
	}

	redisBackend, err := sharedBackend.InitRedisBackend()
	if err != nil {
		log.Fatalf("Redis init failed: %v", err)
	}

	feedWorker := worker.NewFeedWorker(esBackend, redisBackend)

	source := kafka.NewKafkaConsumer(constants.KAFKA_BROKERS, model.TopicPostCreated, "feed-worker-group")
	defer source.Close()
	dlq := kafka.NewKafkaProducer(constants.KAFKA_BROKERS)
	defer dlq.Close()

	// Metrics endpoint for the worker (Prometheus scrapes :9101).
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(":9101", mux); err != nil {
			log.Printf("metrics server: %v", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	c := &consumer.Consumer{
		Source:  source,
		DLQ:     dlq,
		Metrics: metrics.Consumer{},
		Config: consumer.Config{
			Workers:  8, // authors are spread over 8 goroutines; one author's posts stay in order
			DLQTopic: model.TopicPostCreated + ".dlq",
		},
		Logf: log.Printf,
	}

	logger.Logger.Info("feed-worker starting", zap.Strings("brokers", constants.KAFKA_BROKERS))
	err = c.Run(ctx, func(_ context.Context, m consumer.Message) error {
		return feedWorker.HandlePostCreated(string(m.Key), m.Value)
	})
	if err != nil {
		logger.Logger.Error("feed-worker stopped", zap.Error(err))
	}
}
