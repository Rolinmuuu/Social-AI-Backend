package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"socialai/services/post/handler"
	"socialai/services/post/service"
	sharedBackend "socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/kafka"
	"socialai/shared/logger"
	"socialai/shared/metrics"

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
	postSvc.Relay.Metrics = metrics.Outbox{}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Background loops. Each instance runs them; the work is safe to share: the relay
	// re-publishes at most a few duplicates (consumers are idempotent), SPOP hands each
	// dirty counter to exactly one flusher, and cleanup updates are idempotent.
	var loops sync.WaitGroup
	loop := func(fn func()) {
		loops.Add(1)
		go func() { defer loops.Done(); fn() }()
	}
	loop(func() {
		every(ctx, 10*time.Second, func() {
			if _, err := postSvc.CleanupDeletedPosts(10); err != nil {
				log.Printf("cleanup error: %v", err)
			}
		})
	})
	loop(func() {
		postSvc.Relay.Run(ctx, 2*time.Second, func(err error) { log.Printf("outbox relay: %v", err) })
	})
	loop(func() {
		every(ctx, 2*time.Second, func() {
			if _, err := postSvc.Counters.Flush(ctx, 500); err != nil {
				log.Printf("counter flush: %v", err)
			}
		})
	})

	srv := &http.Server{
		Addr:              ":8082",
		Handler:           handler.InitRouter(postSvc, jwtSecret),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Logger.Info("post-service starting", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Logger.Fatal("post-service stopped", zap.Error(err))
		}
	}()

	// Graceful shutdown: on SIGTERM (container stop, rolling deploy) stop accepting requests,
	// let in-flight ones finish, then flush pending counters once more.
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	loops.Wait() // an in-flight flush or relay pass finishes before the final flush
	if _, err := postSvc.Counters.Flush(shutdownCtx, 10000); err != nil {
		log.Printf("final counter flush: %v", err)
	}
	logger.Logger.Info("post-service stopped")
}

func every(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}
