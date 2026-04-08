package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"socialai/services/feed/worker"
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

	redisBackend, err := sharedBackend.InitRedisBackend()
	if err != nil {
		log.Fatalf("Redis init failed: %v", err)
	}

	feedWorker := worker.NewFeedWorker(esBackend, redisBackend)

	consumer := kafka.NewKafkaConsumer(constants.KAFKA_BROKERS, "post.created", "feed-worker-group")
	defer consumer.Close()

	// 监听系统信号，支持优雅退出
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Logger.Info("feed-worker starting", zap.Strings("brokers", constants.KAFKA_BROKERS))

	if err := consumer.Consume(ctx, feedWorker.HandlePostCreated); err != nil {
		logger.Logger.Error("feed-worker stopped", zap.Error(err))
	}
}
