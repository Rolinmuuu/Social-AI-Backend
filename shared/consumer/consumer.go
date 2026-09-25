// Package consumer is the reliable event-processing loop shared by feed-worker and
// notification-worker. It is broker-agnostic; shared/kafka adapts a kafka-go Reader to Source.
//
// Guarantees:
//
//   - At-least-once: an offset is committed only after its message was handled or parked in
//     the dead-letter topic. A crash re-delivers, it never drops. (The previous loop committed
//     after three instant retries even when the handler failed, silently losing the event.)
//   - Per-key ordering with parallelism: messages are routed to Workers goroutines by a hash of
//     the key (the author id for post events), so one author's events stay in order while
//     different authors are processed in parallel.
//   - In-order commits: workers finish out of order, so a per-partition tracker commits only the
//     highest offset below which everything is done.
//   - Backpressure: each worker has a bounded queue; when workers fall behind, Fetch blocks
//     instead of buffering without limit.
//   - Poison messages: after MaxAttempts with exponential backoff the message goes to the
//     dead-letter topic with the error and its origin, then is committed.
package consumer

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

// Message is a broker-neutral record.
type Message struct {
	Topic     string
	Partition int
	Offset    int64
	Key       []byte
	Value     []byte
}

// Source fetches messages and commits processed offsets. Commit(m) means "everything in m's
// partition up to and including m.Offset is processed".
type Source interface {
	Fetch(ctx context.Context) (Message, error)
	Commit(ctx context.Context, m Message) error
}

// Publisher is satisfied by kafka.KafkaProducerInterface; used for the dead-letter topic.
type Publisher interface {
	Publish(ctx context.Context, topic, key string, message interface{}) error
}

// Handler processes one message. It must be idempotent: after a crash a message can be
// delivered again.
type Handler func(ctx context.Context, m Message) error

// DeadLetter is what lands on the DLQ topic.
type DeadLetter struct {
	OriginalTopic string `json:"original_topic"`
	Partition     int    `json:"partition"`
	Offset        int64  `json:"offset"`
	Key           string `json:"key"`
	Value         string `json:"value"`
	Error         string `json:"error"`
	Attempts      int    `json:"attempts"`
	FailedAt      int64  `json:"failed_at"`
}

// Metrics is optional.
type Metrics interface {
	Processed(topic string, d time.Duration)
	Retried(topic string)
	DeadLettered(topic string)
}

// Config tunes a Consumer. Zero values get defaults.
type Config struct {
	Workers        int           // parallel workers (default 8)
	QueueSize      int           // per-worker queue (default 64)
	MaxAttempts    int           // handler attempts before dead-lettering (default 5)
	BaseBackoff    time.Duration // first retry delay, doubled per attempt (default 200ms)
	MaxBackoff     time.Duration // default 10s
	DLQTopic       string        // required for dead-lettering; empty = retry forever
	CommitInterval time.Duration // default 1s
}

func (c Config) withDefaults() Config {
	if c.Workers <= 0 {
		c.Workers = 8
	}
	if c.QueueSize <= 0 {
		c.QueueSize = 64
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 200 * time.Millisecond
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 10 * time.Second
	}
	if c.CommitInterval <= 0 {
		c.CommitInterval = time.Second
	}
	return c
}

// Consumer runs a Handler over a Source.
type Consumer struct {
	Source  Source
	DLQ     Publisher
	Config  Config
	Metrics Metrics
	Logf    func(format string, args ...interface{})
	// Sleep is replaceable in tests; it must return early when ctx is done.
	Sleep func(ctx context.Context, d time.Duration)
}

func (c *Consumer) logf(format string, args ...interface{}) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (c *Consumer) backoff(cfg Config, attempt int) time.Duration {
	d := cfg.BaseBackoff << uint(attempt-1)
	if d <= 0 || d > cfg.MaxBackoff {
		return cfg.MaxBackoff
	}
	return d
}

// Run consumes until ctx is cancelled (returns nil) or Fetch fails (returns the error).
// On return every worker has stopped and all finished offsets have been committed.
func (c *Consumer) Run(ctx context.Context, handle Handler) error {
	cfg := c.Config.withDefaults()
	if c.Sleep == nil {
		c.Sleep = sleepCtx
	}
	tracker := NewTracker()
	queues := make([]chan Message, cfg.Workers)
	var wg sync.WaitGroup
	for i := range queues {
		queues[i] = make(chan Message, cfg.QueueSize)
		wg.Add(1)
		go func(q chan Message) {
			defer wg.Done()
			for m := range q {
				if c.process(ctx, cfg, handle, m) {
					tracker.Done(m.Topic, m.Partition, m.Offset)
				}
			}
		}(queues[i])
	}

	commitCtx, stopCommitter := context.WithCancel(context.Background())
	committerDone := make(chan struct{})
	go func() {
		defer close(committerDone)
		t := time.NewTicker(cfg.CommitInterval)
		defer t.Stop()
		for {
			select {
			case <-commitCtx.Done():
				return
			case <-t.C:
				c.commit(tracker)
			}
		}
	}()

	var fetchErr error
	for {
		m, err := c.Source.Fetch(ctx)
		if err != nil {
			if ctx.Err() == nil {
				fetchErr = fmt.Errorf("consumer: fetch: %w", err)
			}
			break
		}
		tracker.Track(m.Topic, m.Partition, m.Offset)
		q := queues[route(m.Key, cfg.Workers)]
		select {
		case q <- m: // blocks when the worker is behind: backpressure
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}

	for _, q := range queues {
		close(q)
	}
	wg.Wait()
	stopCommitter()
	<-committerDone
	c.commit(tracker) // final commit of whatever finished
	return fetchErr
}

func (c *Consumer) commit(t *Tracker) {
	for _, pos := range t.Committable() {
		m := Message{Topic: pos.Topic, Partition: pos.Partition, Offset: pos.Offset}
		if err := c.Source.Commit(context.Background(), m); err != nil {
			c.logf("consumer: commit %s/%d@%d failed: %v", pos.Topic, pos.Partition, pos.Offset, err)
			t.Rewind(pos) // try again next tick
		}
	}
}

// process returns true when the message is finished (handled or dead-lettered) and its
// offset may be committed; false when shutdown interrupted it (it will be redelivered).
func (c *Consumer) process(ctx context.Context, cfg Config, handle Handler, m Message) bool {
	start := time.Now()
	var err error
	for attempt := 1; attempt <= cfg.MaxAttempts || cfg.DLQTopic == ""; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		if err = handle(ctx, m); err == nil {
			if c.Metrics != nil {
				c.Metrics.Processed(m.Topic, time.Since(start))
			}
			return true
		}
		c.logf("consumer: %s/%d@%d attempt %d failed: %v", m.Topic, m.Partition, m.Offset, attempt, err)
		if c.Metrics != nil {
			c.Metrics.Retried(m.Topic)
		}
		if attempt < cfg.MaxAttempts || cfg.DLQTopic == "" {
			c.Sleep(ctx, c.backoff(cfg, attempt))
		}
	}
	dl := DeadLetter{
		OriginalTopic: m.Topic, Partition: m.Partition, Offset: m.Offset,
		Key: string(m.Key), Value: string(m.Value), Error: err.Error(),
		Attempts: cfg.MaxAttempts, FailedAt: time.Now().Unix(),
	}
	// The DLQ write itself must succeed before the offset is committed; keep trying.
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		perr := c.DLQ.Publish(ctx, cfg.DLQTopic, string(m.Key), dl)
		if perr == nil {
			if c.Metrics != nil {
				c.Metrics.DeadLettered(m.Topic)
			}
			c.logf("consumer: %s/%d@%d dead-lettered to %s after %d attempts: %v", m.Topic, m.Partition, m.Offset, cfg.DLQTopic, cfg.MaxAttempts, err)
			return true
		}
		c.logf("consumer: dead-letter publish failed (attempt %d): %v", attempt, perr)
		c.Sleep(ctx, c.backoff(cfg, attempt))
	}
}

func route(key []byte, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write(key)
	return int(h.Sum32() % uint32(n))
}

// ErrStopped can be returned by Source.Fetch implementations on close.
var ErrStopped = errors.New("consumer: source stopped")
