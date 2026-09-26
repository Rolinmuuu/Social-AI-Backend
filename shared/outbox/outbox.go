// Package outbox implements the transactional outbox used by post-service.
//
// Problem it solves (the "dual write"): creating a post writes to the database and then
// publishes post.created to Kafka. Two independent systems cannot share a transaction, so
// either write can fail after the other succeeded:
//
//   - DB ok, Kafka down  -> the post exists but followers never get it in their feed;
//   - returning an error for that case makes the client retry and create a duplicate post.
//
// The fix: the event is inserted into the outbox table in the *same PostgreSQL transaction*
// as the change it describes (Enqueue), so both commit or neither does. Publishing then
// happens asynchronously: right after commit (PublishNow, the fast path) and, for anything
// still pending, by a relay that keeps retrying until the broker accepts it. Delivery is
// at-least-once, so every consumer must be idempotent (the feed worker writes with ZADD keyed
// by post id, the search indexer writes with external versions, notifications have
// deterministic ids).
//
// Relay and Store are storage- and broker-agnostic; PGStore (pgstore.go) and the Kafka
// producer are the adapters used in production.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// Record is one event waiting to be published.
type Record struct {
	ID       string      // outbox row id
	Topic    string      // e.g. "post.created"
	Key      string      // partition key; events with the same key keep their order
	Payload  interface{} // marshalled by the Publisher
	Attempts int         // failed publish attempts so far
	// NextAttemptAt is the earliest time the relay may retry (zero = now).
	NextAttemptAt time.Time
	// CreatedAt is when the event was recorded (used for the publish-lag metric).
	CreatedAt time.Time
}

// Store reads pending records and records the outcome of a publish.
type Store interface {
	// Pending returns up to limit records whose NextAttemptAt is <= now, oldest first.
	Pending(ctx context.Context, now time.Time, limit int) ([]Record, error)
	MarkPublished(ctx context.Context, id string) error
	// MarkFailed stores the attempt count and when to try again. dead=true means the
	// record exceeded MaxAttempts and needs an operator (it stays visible, never dropped).
	MarkFailed(ctx context.Context, id string, attempts int, next time.Time, lastErr string, dead bool) error
}

// Publisher is satisfied by kafka.KafkaProducerInterface.
type Publisher interface {
	Publish(ctx context.Context, topic, key string, message interface{}) error
}

// Metrics is optional instrumentation; nil means no metrics.
type Metrics interface {
	Published(topic string)
	Failed(topic string, dead bool)
	Lag(topic string, seconds float64)
}

// Relay moves pending records from the Store to the Publisher.
type Relay struct {
	Store     Store
	Publisher Publisher
	Metrics   Metrics
	BatchSize int // records per poll (default 100)
	// MaxAttempts parks a record as dead after this many failures. 0 (the default) never
	// parks: a broker outage of any length only delays events, it never strands them. Set it
	// only if you alert on dead records and have a way to re-drive them.
	MaxAttempts int
	BaseBackoff time.Duration // first retry delay, doubled per attempt (default 1s)
	MaxBackoff  time.Duration // cap (default 5m)
	// FastPathTimeout bounds the inline publish in PublishNow so a slow or unreachable broker
	// cannot hold the user's request (default 1s); the relay retries later.
	FastPathTimeout time.Duration
	// PublishTimeout bounds each publish made by the relay (default 5s).
	PublishTimeout time.Duration
	Now            func() time.Time
}

func (r *Relay) defaults() {
	if r.BatchSize <= 0 {
		r.BatchSize = 100
	}
	if r.FastPathTimeout <= 0 {
		r.FastPathTimeout = time.Second
	}
	if r.PublishTimeout <= 0 {
		r.PublishTimeout = 5 * time.Second
	}
	if r.BaseBackoff <= 0 {
		r.BaseBackoff = time.Second
	}
	if r.MaxBackoff <= 0 {
		r.MaxBackoff = 5 * time.Minute
	}
	if r.Now == nil {
		r.Now = time.Now
	}
}

// Backoff returns the delay before retry number `attempts` (1-based): base * 2^(attempts-1), capped.
func (r *Relay) Backoff(attempts int) time.Duration {
	r.defaults()
	if attempts < 1 {
		attempts = 1
	}
	d := float64(r.BaseBackoff) * math.Pow(2, float64(attempts-1))
	if d > float64(r.MaxBackoff) {
		return r.MaxBackoff
	}
	return time.Duration(d)
}

// Result summarises one relay pass.
type Result struct {
	Published int
	Failed    int
	Dead      int
}

// PublishNow is the fast path: called right after the business write so the event usually
// goes out immediately. A failure is not returned to the caller's client: the record stays
// pending and the relay retries it.
func (r *Relay) PublishNow(ctx context.Context, rec Record) error {
	r.defaults()
	pctx, cancel := context.WithTimeout(ctx, r.FastPathTimeout)
	err := r.Publisher.Publish(pctx, rec.Topic, rec.Key, rec.Payload)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err() // shutting down: leave the record pending for the relay
		}
		return r.fail(ctx, rec, err)
	}
	if r.Metrics != nil {
		r.Metrics.Published(rec.Topic)
	}
	return r.Store.MarkPublished(ctx, rec.ID)
}

// RunOnce publishes one batch. Records are handled in order; a failure only affects that
// record (it is rescheduled with backoff), so one bad event cannot block the rest.
func (r *Relay) RunOnce(ctx context.Context) (Result, error) {
	r.defaults()
	var res Result
	now := r.Now()
	recs, err := r.Store.Pending(ctx, now, r.BatchSize)
	if err != nil {
		return res, fmt.Errorf("outbox: load pending: %w", err)
	}
	for _, rec := range recs {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		pctx, cancel := context.WithTimeout(ctx, r.PublishTimeout)
		pubErr := r.Publisher.Publish(pctx, rec.Topic, rec.Key, rec.Payload)
		cancel()
		if pubErr != nil && ctx.Err() != nil {
			// Shutting down, not a broker failure: don't spend one of the record's attempts.
			return res, ctx.Err()
		}
		if pubErr == nil {
			if err := r.Store.MarkPublished(ctx, rec.ID); err != nil {
				// Published but not marked: the next pass publishes it again. Consumers are
				// idempotent, so a duplicate is harmless; losing the event would not be.
				return res, fmt.Errorf("outbox: mark published %s: %w", rec.ID, err)
			}
			res.Published++
			if r.Metrics != nil {
				r.Metrics.Published(rec.Topic)
				if lag, ok := ageSeconds(rec, now); ok {
					r.Metrics.Lag(rec.Topic, lag)
				}
			}
			continue
		}
		if err := r.fail(ctx, rec, pubErr); err != nil && !errors.Is(err, errPublishFailed) {
			return res, err
		}
		if r.MaxAttempts > 0 && rec.Attempts+1 >= r.MaxAttempts {
			res.Dead++
		} else {
			res.Failed++
		}
	}
	return res, nil
}

var errPublishFailed = errors.New("outbox: publish failed")

func (r *Relay) fail(ctx context.Context, rec Record, pubErr error) error {
	attempts := rec.Attempts + 1
	dead := r.MaxAttempts > 0 && attempts >= r.MaxAttempts
	next := r.Now().Add(r.Backoff(attempts))
	if err := r.Store.MarkFailed(ctx, rec.ID, attempts, next, pubErr.Error(), dead); err != nil {
		return fmt.Errorf("outbox: mark failed %s: %w", rec.ID, err)
	}
	if r.Metrics != nil {
		r.Metrics.Failed(rec.Topic, dead)
	}
	return fmt.Errorf("%w: %s: %v", errPublishFailed, rec.ID, pubErr)
}

// Run polls every interval until ctx is cancelled. Pass errors to onErr (may be nil).
func (r *Relay) Run(ctx context.Context, interval time.Duration, onErr func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := r.RunOnce(ctx); err != nil && onErr != nil && ctx.Err() == nil {
				onErr(err)
			}
		}
	}
}

// CreatedAt lets a payload report its creation time so the relay can measure lag.
type CreatedAt interface{ CreatedAtUnix() int64 }

func ageSeconds(rec Record, now time.Time) (float64, bool) {
	if !rec.CreatedAt.IsZero() {
		return now.Sub(rec.CreatedAt).Seconds(), true
	}
	c, ok := rec.Payload.(CreatedAt)
	if !ok || c.CreatedAtUnix() == 0 {
		return 0, false
	}
	return now.Sub(time.Unix(c.CreatedAtUnix(), 0)).Seconds(), true
}
