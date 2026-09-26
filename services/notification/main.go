package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"socialai/services/notification/worker"
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

	pool, err := db.Open(context.Background(), constants.DATABASE_URL, 60*time.Second)
	if err != nil {
		log.Fatalf("PostgreSQL init failed: %v", err)
	}
	defer pool.Close()

	nWorker := worker.NewNotificationWorker(pool)

	tctx, tcancel := context.WithTimeout(context.Background(), 2*time.Minute)
	if err := kafka.EnsureTopics(tctx, constants.KAFKA_BROKERS, model.TopicPostLiked); err != nil {
		log.Printf("%v (continuing; the consumer retries)", err)
	}
	tcancel()

	source := kafka.NewKafkaConsumer(constants.KAFKA_BROKERS, model.TopicPostLiked, "notification-worker-group")
	defer source.Close()
	dlq := kafka.NewKafkaProducer(constants.KAFKA_BROKERS)
	defer dlq.Close()

	// Metrics endpoint (Prometheus scrapes notification-worker:9102).
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(":9102", mux); err != nil {
			log.Printf("metrics server: %v", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	c := &consumer.Consumer{
		Source:  source,
		DLQ:     dlq,
		Metrics: metrics.Consumer{},
		Config:  consumer.Config{Workers: 4, DLQTopic: model.TopicPostLiked + ".dlq"},
		Logf:    log.Printf,
	}

	logger.Logger.Info("notification-worker starting", zap.Strings("brokers", constants.KAFKA_BROKERS))
	err = c.Run(ctx, func(ctx context.Context, m consumer.Message) error {
		return nWorker.HandlePostLiked(ctx, m.Value)
	})
	if err != nil {
		logger.Logger.Error("notification-worker stopped", zap.Error(err))
	}
}
