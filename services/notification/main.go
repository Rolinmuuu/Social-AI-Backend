package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"socialai/services/notification/worker"
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

	nWorker := worker.NewNotificationWorker(esBackend)

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
	err = c.Run(ctx, func(_ context.Context, m consumer.Message) error {
		return nWorker.HandlePostLiked(string(m.Key), m.Value)
	})
	if err != nil {
		logger.Logger.Error("notification-worker stopped", zap.Error(err))
	}
}
