// Package counter implements write-behind counters for like_count / shared_count.
//
// Bottleneck it removes: a like that also increments posts.like_count in the same transaction
// makes every like on a popular post queue on that one row's lock (and, when the counts lived
// in Elasticsearch, on its per-document version, where concurrent updates conflicted and
// failed). Here each like is an atomic Redis INCRBY on a delta key plus SADD into a "dirty"
// set; a flusher periodically takes the dirty posts, reads-and-clears each delta (GETDEL) and
// applies it to the database as one UPDATE. N likes in a flush window become one row write.
//
// Trade-offs, stated honestly:
//   - Stored counts lag by up to one flush interval (seconds). Readers can add the pending
//     delta if they need exact numbers.
//   - If the process crashes after SPOP (the post leaves the dirty set) or after GETDEL but
//     before the UPDATE, that delta is not applied. The post_likes / post_shares rows stay the
//     source of truth and the reconciler in post-service recounts from them. A failed database
//     or Redis call, the common case, loses nothing: the delta or the dirty marker is put back
//     and the next flush retries.
//   - Cancelling Flush's context (shutdown) stops it from taking new work, but entries it
//     already popped are finished with a detached context, so a SIGTERM mid-flush does not
//     strand them.
package counter

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// ErrNotRecorded means Incr left no trace in Redis, so the caller may safely fall back to
// writing the count directly. Any other error from Incr means the delta IS in Redis and will
// be flushed; falling back as well would count it twice.
var ErrNotRecorded = errors.New("counter: increment not recorded")

// Store is the Redis subset the counter needs.
type Store interface {
	IncrBy(ctx context.Context, key string, n int64) (int64, error)
	SAdd(ctx context.Context, key string, members ...interface{}) error
	SPopN(ctx context.Context, key string, n int64) ([]string, error)
	GetDel(ctx context.Context, key string) (string, error)
}

// Sink applies an aggregated delta to the durable store.
type Sink interface {
	Add(ctx context.Context, id, field string, delta int64) error
}

// Counter aggregates increments for one index.
type Counter struct {
	Store Store
	Sink  Sink
	// Prefix namespaces the Redis keys (default "cnt").
	Prefix string
}

func (c *Counter) prefix() string {
	if c.Prefix == "" {
		return "cnt"
	}
	return c.Prefix
}

func (c *Counter) dirtyKey() string { return c.prefix() + ":dirty" }

func (c *Counter) deltaKey(id, field string) string {
	return fmt.Sprintf("%s:%s:%s", c.prefix(), field, id)
}

func member(id, field string) string { return field + "|" + id }

func split(m string) (id, field string, ok bool) {
	for i := 0; i < len(m); i++ {
		if m[i] == '|' {
			return m[i+1:], m[:i], true
		}
	}
	return "", "", false
}

// Incr records n increments of field on document id. It never touches the database.
func (c *Counter) Incr(ctx context.Context, id, field string, n int64) error {
	if _, err := c.Store.IncrBy(ctx, c.deltaKey(id, field), n); err != nil {
		return fmt.Errorf("%w: %v", ErrNotRecorded, err)
	}
	if err := c.Store.SAdd(ctx, c.dirtyKey(), member(id, field)); err != nil {
		// The delta is stored but nothing will flush it. Undo it so the caller can fall back;
		// if the undo fails too, the delta stays and is flushed with the next increment.
		if _, uerr := c.Store.IncrBy(ctx, c.deltaKey(id, field), -n); uerr == nil {
			return fmt.Errorf("%w: %v", ErrNotRecorded, err)
		}
		return fmt.Errorf("counter: delta for %s/%s stored but not scheduled: %v", id, field, err)
	}
	return nil
}

// Flush applies up to max pending deltas to the Sink and returns how many rows were written.
func (c *Counter) Flush(ctx context.Context, max int64) (int, error) {
	dirty, err := c.Store.SPopN(ctx, c.dirtyKey(), max)
	if err != nil {
		return 0, err
	}
	// These entries are no longer in the dirty set: finish them even if ctx is cancelled now.
	ctx = context.WithoutCancel(ctx)
	written := 0
	var firstErr error
	for _, m := range dirty {
		id, field, ok := split(m)
		if !ok {
			continue
		}
		raw, err := c.Store.GetDel(ctx, c.deltaKey(id, field))
		if err != nil {
			// Delta untouched: mark it dirty again so the next flush picks it up.
			_ = c.Store.SAdd(ctx, c.dirtyKey(), m)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if raw == "" {
			continue // nothing pending (already flushed)
		}
		delta, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || delta == 0 {
			continue
		}
		if err := c.Sink.Add(ctx, id, field, delta); err != nil {
			// Put the delta back so the next flush retries it.
			if rerr := c.Incr(ctx, id, field, delta); rerr != nil && firstErr == nil {
				firstErr = fmt.Errorf("counter: lost %d on %s/%s: db=%v redis=%v", delta, id, field, err, rerr)
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		written++
	}
	return written, firstErr
}

// PendingKey is the Redis key holding the not-yet-flushed delta (add it for exact reads).
func (c *Counter) PendingKey(id, field string) string { return c.deltaKey(id, field) }
