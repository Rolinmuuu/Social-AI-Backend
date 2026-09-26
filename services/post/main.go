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
	"socialai/shared/db"
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

	pool, err := db.Open(context.Background(), constants.DATABASE_URL, 60*time.Second)
	if err != nil {
		log.Fatalf("Failed to connect to PostgreSQL: %v", err)
	}
	defer pool.Close()

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

	postSvc := service.NewPostService(pool, esBackend, redisBackend, gcsBackend, openaiBackend, kafkaProducer)
	postSvc.Relay.Metrics = metrics.Outbox{}
	// Keep a relay pass (BatchSize × PublishTimeout) shorter than the claim lease, so a slow
	// pass does not let another instance re-claim rows it is still publishing.
	postSvc.Relay.BatchSize = 20
	reconciler := postSvc.NewReconciler()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Background loops. Every instance runs them and they are safe to share: the relay and
	// media cleanup claim rows with FOR UPDATE SKIP LOCKED, SPOP hands each dirty counter to
	// exactly one flusher, and the reconciler's updates are conditional on what it read.
	var loops sync.WaitGroup
	loop := func(fn func()) {
		loops.Add(1)
		go func() { defer loops.Done(); fn() }()
	}
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
	loop(func() {
		every(ctx, 10*time.Second, func() {
			if _, err := postSvc.CleanupDeletedPosts(ctx, 10); err != nil {
				log.Printf("media cleanup: %v", err)
			}
		})
	})
	loop(func() {
		every(ctx, time.Minute, func() {
			res, err := reconciler.RunOnce(ctx)
			if err != nil {
				log.Printf("count reconcile: %v", err)
				return
			}
			metrics.CountersRepaired(res.Fixed)
		})
	})
	loop(func() {
		every(ctx, 15*time.Second, func() {
			if pending, dead, oldest, err := postSvc.Outbox.Backlog(ctx); err == nil {
				metrics.OutboxBacklog(pending, dead, oldest)
			}
		})
	})
	loop(func() {
		every(ctx, time.Hour, func() {
			if _, err := postSvc.Outbox.Prune(ctx, 7*24*time.Hour, 10000); err != nil {
				log.Printf("outbox prune: %v", err)
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
