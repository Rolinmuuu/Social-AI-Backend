package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"socialai/services/notification/worker"
	sharedBackend "socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/kafka"
	"socialai/shared/logger"

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

	consumer := kafka.NewKafkaConsumer(constants.KAFKA_BROKERS, "post.liked", "notification-worker-group")
	defer consumer.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Logger.Info("notification-worker starting", zap.Strings("brokers", constants.KAFKA_BROKERS))

	if err := consumer.Consume(ctx, nWorker.HandlePostLiked); err != nil {
		logger.Logger.Error("notification-worker stopped", zap.Error(err))
	}
}
