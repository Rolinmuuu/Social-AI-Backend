package counter

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
)

type memRedis struct {
	mu   sync.Mutex
	kv   map[string]int64
	sets map[string]map[string]bool
}

func newRedis() *memRedis {
	return &memRedis{kv: map[string]int64{}, sets: map[string]map[string]bool{}}
}

func (r *memRedis) IncrBy(_ context.Context, k string, n int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kv[k] += n
	return r.kv[k], nil
}
func (r *memRedis) SAdd(_ context.Context, k string, ms ...interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sets[k] == nil {
		r.sets[k] = map[string]bool{}
	}
	for _, m := range ms {
		r.sets[k][m.(string)] = true
	}
	return nil
}
func (r *memRedis) SPopN(_ context.Context, k string, n int64) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for m := range r.sets[k] {
		if int64(len(out)) == n {
			break
		}
		out = append(out, m)
		delete(r.sets[k], m)
	}
	return out, nil
}
func (r *memRedis) GetDel(_ context.Context, k string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.kv[k]
	if !ok {
		return "", nil
	}
	delete(r.kv, k)
	return strconv.FormatInt(v, 10), nil
}

type esSink struct {
	mu     sync.Mutex
	writes int
	counts map[string]int
	fail   bool
}

func (s *esSink) IncrementFieldInES(_ string, id, field string, v int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("version_conflict_engine_exception")
	}
	s.writes++
	s.counts[id+"/"+field] += v
	return nil
}

func TestThousandConcurrentLikesBecomeOneWrite(t *testing.T) {
	r, es := newRedis(), &esSink{counts: map[string]int{}}
	c := &Counter{Store: r, Sink: es, Index: "post"}
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Incr(context.Background(), "hot-post", "like_count", 1); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	n, err := c.Flush(context.Background(), 100)
	if err != nil || n != 1 {
		t.Fatalf("flush wrote %d docs, err %v", n, err)
	}
	if es.writes != 1 || es.counts["hot-post/like_count"] != 1000 {
		t.Fatalf("ES writes=%d count=%d, want 1 write of +1000", es.writes, es.counts["hot-post/like_count"])
	}
}

func TestFailedFlushKeepsTheDelta(t *testing.T) {
	r, es := newRedis(), &esSink{counts: map[string]int{}, fail: true}
	c := &Counter{Store: r, Sink: es, Index: "post"}
	for i := 0; i < 5; i++ {
		_ = c.Incr(context.Background(), "p", "like_count", 1)
	}
	if _, err := c.Flush(context.Background(), 10); err == nil {
		t.Fatal("want the ES error")
	}
	_ = c.Incr(context.Background(), "p", "like_count", 1) // a like during the outage
	es.fail = false
	if _, err := c.Flush(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if got := es.counts["p/like_count"]; got != 6 {
		t.Fatalf("count = %d, want 6 (nothing lost)", got)
	}
}

// ctxRedis fails calls made with a cancelled context, like the real client.
type ctxRedis struct {
	*memRedis
	failSAdd bool
}

func (r *ctxRedis) IncrBy(ctx context.Context, k string, n int64) (int64, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	return r.memRedis.IncrBy(ctx, k, n)
}
func (r *ctxRedis) SAdd(ctx context.Context, k string, ms ...interface{}) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.failSAdd {
		return errors.New("connection reset")
	}
	return r.memRedis.SAdd(ctx, k, ms...)
}
func (r *ctxRedis) GetDel(ctx context.Context, k string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return r.memRedis.GetDel(ctx, k)
}

// SIGTERM arrives while a flush is running: the entries it already took out of the dirty set
// must still reach ES.
type cancelOnWrite struct {
	*esSink
	cancel context.CancelFunc
}

func (s *cancelOnWrite) IncrementFieldInES(i, id, f string, v int) error {
	s.cancel()
	return s.esSink.IncrementFieldInES(i, id, f, v)
}

func TestShutdownDuringFlushLosesNothing(t *testing.T) {
	r := &ctxRedis{memRedis: newRedis()}
	ctx, cancel := context.WithCancel(context.Background())
	es := &cancelOnWrite{esSink: &esSink{counts: map[string]int{}}, cancel: cancel}
	c := &Counter{Store: r, Sink: es, Index: "post"}
	for _, id := range []string{"a", "b", "c"} {
		_ = c.Incr(context.Background(), id, "like_count", 2)
	}
	if _, err := c.Flush(ctx, 10); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if got := es.counts[id+"/like_count"]; got != 2 {
			t.Fatalf("%s: count %d, want 2", id, got)
		}
	}
}

// If the delta was stored but could not be scheduled, Incr undoes it and says so, so the
// caller's fallback write is the only one: no double count.
func TestIncrThatCannotScheduleIsUndone(t *testing.T) {
	r := &ctxRedis{memRedis: newRedis(), failSAdd: true}
	c := &Counter{Store: r, Sink: &esSink{counts: map[string]int{}}, Index: "post"}
	err := c.Incr(context.Background(), "p", "like_count", 1)
	if !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("err = %v, want ErrNotRecorded", err)
	}
	if v := r.kv[c.PendingKey("p", "like_count")]; v != 0 {
		t.Fatalf("pending delta = %d, want 0 after undo", v)
	}
}
