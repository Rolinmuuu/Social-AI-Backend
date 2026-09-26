package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"socialai/services/message/handler"
	"socialai/shared/constants"
	"socialai/shared/db"
	"socialai/shared/logger"

	"go.uber.org/zap"
)

func main() {
	logger.InitLogger(constants.LOGSTASH_ADDRESS)
	defer logger.Logger.Sync()

	if os.Getenv("JWT_SECRET") == "" {
		log.Fatal("JWT_SECRET environment variable is required")
	}

	pool, err := db.Open(context.Background(), constants.DATABASE_URL, 60*time.Second)
	if err != nil {
		log.Fatalf("Failed to connect to PostgreSQL: %v", err)
	}
	defer pool.Close()

	addr := ":8084"
	logger.Logger.Info("message-service starting", zap.String("addr", addr))
	if err := http.ListenAndServe(addr, handler.InitRouter(pool)); err != nil {
		logger.Logger.Fatal("message-service stopped", zap.Error(err))
	}
}
