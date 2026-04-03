package main

import (
	"log"
	"net/http"
	"os"

	"socialai/services/message/handler"
	sharedBackend "socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/logger"

	"go.uber.org/zap"
)

func main() {
	logger.InitLogger(constants.LOGSTASH_ADDRESS)
	defer logger.Logger.Sync()

	if os.Getenv("JWT_SECRET") == "" {
		log.Fatal("JWT_SECRET environment variable is required")
	}

	esBackend, err := sharedBackend.InitElasticsearchBackend()
	if err != nil {
		log.Fatalf("Failed to initialize Elasticsearch: %v", err)
	}

	addr := ":8084"
	logger.Logger.Info("message-service starting", zap.String("addr", addr))
	if err := http.ListenAndServe(addr, handler.InitRouter(esBackend)); err != nil {
		logger.Logger.Fatal("message-service stopped", zap.Error(err))
	}
}
