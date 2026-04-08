package main

import (
	"log"
	"net/http"
	"os"

	"socialai/services/auth/handler"
	sharedBackend "socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/logger"

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

	addr := ":8081"
	logger.Logger.Info("auth-service starting", zap.String("addr", addr))
	if err := http.ListenAndServe(addr, handler.InitRouter(esBackend, jwtSecret)); err != nil {
		logger.Logger.Fatal("auth-service stopped", zap.Error(err))
	}
}
