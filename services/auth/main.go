package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"socialai/services/auth/handler"
	"socialai/shared/constants"
	"socialai/shared/db"
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

	pool, err := db.Open(context.Background(), constants.DATABASE_URL, 60*time.Second)
	if err != nil {
		log.Fatalf("Failed to connect to PostgreSQL: %v", err)
	}
	defer pool.Close()

	addr := ":8081"
	logger.Logger.Info("auth-service starting", zap.String("addr", addr))
	if err := http.ListenAndServe(addr, handler.InitRouter(pool, jwtSecret)); err != nil {
		logger.Logger.Fatal("auth-service stopped", zap.Error(err))
	}
}
