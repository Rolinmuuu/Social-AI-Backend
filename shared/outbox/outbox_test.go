package outbox

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu   sync.Mutex
	recs map[string]*Record
	pub  map[string]bool
	dead map[string]bool
	errs map[string]string
}

func newMemStore(recs ...Record) *memStore {
	s := &memStore{recs: map[string]*Record{}, pub: map[string]bool{}, dead: map[string]bool{}, errs: map[string]string{}}
	for i := range recs {
		r := recs[i]
		s.recs[r.ID] = &r
	}
	return s
}

func (s *memStore) Pending(_ context.Context, now time.Time, limit int) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, r := range s.recs {
		if s.pub[r.ID] || s.dead[r.ID] || r.NextAttemptAt.After(now) {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memStore) MarkPublished(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pub[id] = true
	return nil
}

func (s *memStore) MarkFailed(_ context.Context, id string, attempts int, next time.Time, lastErr string, dead bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[id]
	r.Attempts, r.NextAttemptAt = attempts, next
	s.errs[id] = lastErr
	if dead {
		s.dead[id] = true
	}
	return nil
}

type flakyPublisher struct {
	mu      sync.Mutex
	down    bool
	failKey map[string]bool
	sent    []string
}

func (p *flakyPublisher) Publish(_ context.Context, topic, key string, _ interface{}) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down || p.failKey[key] {
		return errors.New("broker unavailable")
	}
	p.sent = append(p.sent, topic+"/"+key)
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func rec(id string) Record {
	return Record{ID: id, Topic: "post.created", Key: id, Payload: map[string]string{"id": id}}
}

func TestBrokerOutageDoesNotLoseEvents(t *testing.T) {
	store := newMemStore(rec("p1"), rec("p2"), rec("p3"))
	pub := &flakyPublisher{down: true}
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	r := &Relay{Store: store, Publisher: pub, Now: c.now, BaseBackoff: time.Second}

	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 3 || res.Published != 0 {
		t.Fatalf("outage pass: %+v", res)
	}
	// Still inside the backoff window: nothing is retried yet.
	if res, _ := r.RunOnce(context.Background()); res.Published+res.Failed != 0 {
		t.Fatalf("retried before backoff elapsed: %+v", res)
	}

	pub.down = false
	c.t = c.t.Add(2 * time.Second)
	res, err = r.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Published != 3 || len(pub.sent) != 3 {
		t.Fatalf("recovery pass: %+v sent=%v", res, pub.sent)
	}
}

func TestPoisonRecordIsParkedNotDropped(t *testing.T) {
	store := newMemStore(rec("bad"), rec("ok"))
	pub := &flakyPublisher{failKey: map[string]bool{"bad": true}}
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	r := &Relay{Store: store, Publisher: pub, Now: c.now, MaxAttempts: 3, BaseBackoff: time.Millisecond}

	for i := 0; i < 5; i++ {
		if _, err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.t = c.t.Add(time.Hour)
	}
	if !store.pub["ok"] {
		t.Fatal("healthy record was blocked by the poison one")
	}
	if !store.dead["bad"] || store.recs["bad"].Attempts != 3 {
		t.Fatalf("poison record should be parked after 3 attempts, got attempts=%d dead=%v", store.recs["bad"].Attempts, store.dead["bad"])
	}
	if store.errs["bad"] == "" {
		t.Fatal("last error should be kept for the operator")
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	r := &Relay{BaseBackoff: time.Second, MaxBackoff: 10 * time.Second}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i, w := range want {
		if got := r.Backoff(i + 1); got != w {
			t.Fatalf("attempt %d: got %v want %v", i+1, got, w)
		}
	}
}

func TestPublishNowFailureLeavesRecordPending(t *testing.T) {
	store := newMemStore(rec("p1"))
	pub := &flakyPublisher{down: true}
	r := &Relay{Store: store, Publisher: pub}
	if err := r.PublishNow(context.Background(), rec("p1")); err == nil {
		t.Fatal("expected an error to log")
	}
	if store.pub["p1"] || store.dead["p1"] || store.recs["p1"].Attempts != 1 {
		t.Fatalf("record should stay pending with one attempt: %+v", store.recs["p1"])
	}
}

// A long outage must only delay events: with the default settings nothing is ever parked.
func TestLongOutageNeverParksByDefault(t *testing.T) {
	store := newMemStore(rec("p1"))
	pub := &flakyPublisher{down: true}
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	r := &Relay{Store: store, Publisher: pub, Now: c.now}
	for i := 0; i < 200; i++ { // 200 passes, 5 minutes apart: over 16 hours of outage
		if _, err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.t = c.t.Add(5 * time.Minute)
	}
	if store.dead["p1"] {
		t.Fatal("record was parked during an outage; it should keep retrying")
	}
	pub.down = false
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.pub["p1"] {
		t.Fatal("record not published after the broker came back")
	}
}

// Shutting down is not a publish failure and must not use up one of the record's attempts.
func TestCancelledContextDoesNotSpendAnAttempt(t *testing.T) {
	store := newMemStore(rec("p1"))
	ctx, cancel := context.WithCancel(context.Background())
	pub := &cancelOnPublish{cancel: cancel}
	r := &Relay{Store: store, Publisher: pub}
	if _, err := r.RunOnce(ctx); err == nil {
		t.Fatal("expected the context error")
	}
	if got := store.recs["p1"].Attempts; got != 0 {
		t.Fatalf("attempts = %d, want 0", got)
	}
}

type cancelOnPublish struct{ cancel context.CancelFunc }

func (p *cancelOnPublish) Publish(ctx context.Context, _, _ string, _ interface{}) error {
	p.cancel()
	return ctx.Err()
}
