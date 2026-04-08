package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"socialai/services/post/handler"
	"socialai/services/post/service"
	sharedBackend "socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/logger"
	"socialai/shared/kafka"

	"go.uber.org/zap"
)

func main() {
	logger.InitLogger(constants.LOGSTASH_ADDRESS)
	defer logger.Logger.Sync()

	jwtSecret := []byte(os.Getenv("JWT_SECRET"))
	if len(jwtSecret) == 0 {
		log.Fatal("JWT_SECRET environment variable is required")
	}

	esBackend, err := sharedBackend.InitElasticsearchBackend()
	if err != nil {
		log.Fatalf("Failed to initialize Elasticsearch: %v", err)
	}

	redisBackend, err := sharedBackend.InitRedisBackend()
	if err != nil {
		log.Fatalf("Failed to initialize Redis: %v", err)
	}

	gcsBackend, err := sharedBackend.InitGCSBackend()
	if err != nil {
		log.Fatalf("Failed to initialize GCS: %v", err)
	}

	openaiBackend, err := sharedBackend.InitOpenAIBackend()
	if err != nil {
		log.Fatalf("Failed to initialize OpenAI: %v", err)
	}

	kafkaProducer := kafka.NewKafkaProducer(constants.KAFKA_BROKERS)
	defer kafkaProducer.Close()

	postSvc := service.NewPostService(esBackend, redisBackend, gcsBackend, openaiBackend, kafkaProducer)

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := postSvc.CleanupDeletedPosts(10); err != nil {
				log.Printf("cleanup error: %v", err)
			}
		}
	}()

	addr := ":8082"
	logger.Logger.Info("post-service starting", zap.String("addr", addr))
	if err := http.ListenAndServe(addr, handler.InitRouter(postSvc, jwtSecret)); err != nil {
		logger.Logger.Fatal("post-service stopped", zap.Error(err))
	}
}
