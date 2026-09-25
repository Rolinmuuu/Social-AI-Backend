package consumer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	ch        chan Message
	mu        sync.Mutex
	committed map[int]int64
	commits   int
}

func newSource(msgs []Message) *fakeSource {
	s := &fakeSource{ch: make(chan Message, len(msgs)), committed: map[int]int64{}}
	for _, m := range msgs {
		s.ch <- m
	}
	return s
}

func (s *fakeSource) Fetch(ctx context.Context) (Message, error) {
	select {
	case m := <-s.ch:
		return m, nil
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

func (s *fakeSource) Commit(_ context.Context, m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Offset < s.committed[m.Partition] {
		return fmt.Errorf("commit went backwards on partition %d: %d < %d", m.Partition, m.Offset, s.committed[m.Partition])
	}
	s.committed[m.Partition] = m.Offset
	s.commits++
	return nil
}

func (s *fakeSource) committedAt(p int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.committed[p]; ok {
		return v
	}
	return -1
}

type fakeDLQ struct {
	mu   sync.Mutex
	down bool
	got  []DeadLetter
}

func (d *fakeDLQ) Publish(_ context.Context, topic, _ string, msg interface{}) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down {
		return errors.New("broker down")
	}
	d.got = append(d.got, msg.(DeadLetter))
	return nil
}

func noSleep(context.Context, time.Duration) {}

// msgs builds n messages per partition, keys cycling through `keys`.
func msgs(partitions, n int, keys ...string) []Message {
	var out []Message
	for i := 0; i < n; i++ {
		for p := 0; p < partitions; p++ {
			k := keys[(i+p)%len(keys)]
			out = append(out, Message{Topic: "post.created", Partition: p, Offset: int64(i), Key: []byte(k), Value: []byte(fmt.Sprintf("%s-%d-%d", k, p, i))})
		}
	}
	return out
}

// runUntil runs the consumer until cond holds, then stops it.
func runUntil(t *testing.T, c *Consumer, h Handler, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, h) }()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTrackerCommitsOnlyContiguousOffsets(t *testing.T) {
	tr := NewTracker()
	for o := int64(0); o < 5; o++ {
		tr.Track("t", 0, o)
	}
	tr.Done("t", 0, 1)
	tr.Done("t", 0, 2)
	if got := tr.Committable(); len(got) != 0 {
		t.Fatalf("offset 0 not done yet, nothing to commit: %v", got)
	}
	tr.Done("t", 0, 0)
	if got := tr.Committable(); len(got) != 1 || got[0].Offset != 2 {
		t.Fatalf("want commit at 2, got %v", got)
	}
	tr.Done("t", 0, 4)
	if got := tr.Committable(); len(got) != 0 {
		t.Fatalf("3 still pending: %v", got)
	}
}

func TestPerKeyOrderIsKeptWhileKeysRunInParallel(t *testing.T) {
	in := msgs(3, 200, "alice", "bob", "carol", "dave", "erin")
	src := newSource(in)
	var mu sync.Mutex
	seen := map[string][]string{}
	h := func(_ context.Context, m Message) error {
		time.Sleep(time.Duration(len(m.Value)%3) * 100 * time.Microsecond) // uneven work
		mu.Lock()
		seen[string(m.Key)] = append(seen[string(m.Key)], string(m.Value))
		mu.Unlock()
		return nil
	}
	c := &Consumer{Source: src, Config: Config{Workers: 4, CommitInterval: 5 * time.Millisecond}, Sleep: noSleep}
	runUntil(t, c, h, func() bool {
		return src.committedAt(0) == 199 && src.committedAt(1) == 199 && src.committedAt(2) == 199
	})

	// Expected per-key order = order of appearance in the input.
	want := map[string][]string{}
	for _, m := range in {
		want[string(m.Key)] = append(want[string(m.Key)], string(m.Value))
	}
	for k, w := range want {
		got := seen[k]
		if len(got) != len(w) {
			t.Fatalf("key %s: %d handled, want %d", k, len(got), len(w))
		}
		for i := range w {
			if got[i] != w[i] {
				t.Fatalf("key %s out of order at %d: got %s want %s", k, i, got[i], w[i])
			}
		}
	}
}

func TestPoisonMessageGoesToDLQAndDoesNotBlockThePartition(t *testing.T) {
	in := []Message{
		{Topic: "post.created", Partition: 0, Offset: 0, Key: []byte("a"), Value: []byte("ok")},
		{Topic: "post.created", Partition: 0, Offset: 1, Key: []byte("b"), Value: []byte("poison")},
		{Topic: "post.created", Partition: 0, Offset: 2, Key: []byte("c"), Value: []byte("ok")},
	}
	src := newSource(in)
	dlq := &fakeDLQ{}
	attempts := 0
	var mu sync.Mutex
	h := func(_ context.Context, m Message) error {
		if string(m.Value) == "poison" {
			mu.Lock()
			attempts++
			mu.Unlock()
			return errors.New("cannot parse")
		}
		return nil
	}
	c := &Consumer{Source: src, DLQ: dlq, Config: Config{Workers: 2, MaxAttempts: 4, DLQTopic: "post.created.dlq", CommitInterval: 5 * time.Millisecond}, Sleep: noSleep}
	runUntil(t, c, h, func() bool { return src.committedAt(0) == 2 })

	if attempts != 4 {
		t.Fatalf("handler attempts = %d, want 4", attempts)
	}
	if len(dlq.got) != 1 || dlq.got[0].Offset != 1 || dlq.got[0].Error != "cannot parse" || dlq.got[0].Value != "poison" {
		t.Fatalf("dead letter = %+v", dlq.got)
	}
}

func TestNothingIsCommittedPastAMessageThatCouldNotBeParked(t *testing.T) {
	in := []Message{
		{Topic: "post.created", Partition: 0, Offset: 0, Key: []byte("a"), Value: []byte("ok")},
		{Topic: "post.created", Partition: 0, Offset: 1, Key: []byte("b"), Value: []byte("poison")},
		{Topic: "post.created", Partition: 0, Offset: 2, Key: []byte("c"), Value: []byte("ok")},
	}
	src := newSource(in)
	dlq := &fakeDLQ{down: true}
	h := func(_ context.Context, m Message) error {
		if string(m.Value) == "poison" {
			return errors.New("cannot parse")
		}
		return nil
	}
	c := &Consumer{Source: src, DLQ: dlq, Config: Config{Workers: 3, MaxAttempts: 2, DLQTopic: "dlq", CommitInterval: 2 * time.Millisecond}, Sleep: func(ctx context.Context, _ time.Duration) {
		select {
		case <-ctx.Done():
		case <-time.After(time.Millisecond):
		}
	}}
	start := time.Now()
	runUntil(t, c, h, func() bool { return time.Since(start) > 80*time.Millisecond })
	if got := src.committedAt(0); got != 0 {
		t.Fatalf("committed offset %d; must stop at 0 so offset 1 is redelivered after restart", got)
	}
}
