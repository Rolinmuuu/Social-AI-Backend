package feedplan

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recWriter struct {
	mu       sync.Mutex
	got      map[string]int
	calls    int32
	inFlight int32
	maxIn    int32
	rtt      time.Duration
	failOn   int32 // fail the n-th call (1-based), 0 = never
}

func (w *recWriter) AddToFeeds(_ context.Context, ids []string, item Item, _ int, _ time.Duration) error {
	n := atomic.AddInt32(&w.calls, 1)
	cur := atomic.AddInt32(&w.inFlight, 1)
	defer atomic.AddInt32(&w.inFlight, -1)
	for {
		m := atomic.LoadInt32(&w.maxIn)
		if cur <= m || atomic.CompareAndSwapInt32(&w.maxIn, m, cur) {
			break
		}
	}
	if w.rtt > 0 {
		time.Sleep(w.rtt)
	}
	if w.failOn != 0 && n == w.failOn {
		return errors.New("redis timeout")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, id := range ids {
		w.got[id]++
	}
	return nil
}

func followers(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("u%05d", i)
	}
	return out
}

func TestDecideSwitchesToPullAboveThreshold(t *testing.T) {
	p := Policy{CelebrityThreshold: 1000}
	if p.Decide(1000) != Push || p.Decide(1001) != Pull {
		t.Fatal("threshold boundary wrong")
	}
}

func TestFanOutBatchesAndBoundsConcurrency(t *testing.T) {
	w := &recWriter{got: map[string]int{}, rtt: 2 * time.Millisecond}
	fs := followers(4321)
	trips, err := FanOut(context.Background(), w, fs, Item{PostID: "p1", CreatedAt: 1}, Policy{BatchSize: 500, Parallelism: 3})
	if err != nil {
		t.Fatal(err)
	}
	if trips != 9 { // ceil(4321/500); a per-follower LPUSH+LTRIM+EXPIRE loop makes 12963
		t.Fatalf("round trips = %d, want 9", trips)
	}
	if len(w.got) != len(fs) {
		t.Fatalf("%d followers received the post, want %d", len(w.got), len(fs))
	}
	if w.maxIn > 3 {
		t.Fatalf("%d batches in flight, limit is 3", w.maxIn)
	}
}

func TestFanOutReportsPartialFailure(t *testing.T) {
	w := &recWriter{got: map[string]int{}, failOn: 2}
	_, err := FanOut(context.Background(), w, followers(1500), Item{PostID: "p1"}, Policy{BatchSize: 500, Parallelism: 1})
	if err == nil {
		t.Fatal("a failed batch must surface so the consumer retries the event")
	}
}

func TestMergeDedupesAndOrdersNewestFirst(t *testing.T) {
	pushed := []Item{{PostID: "a", CreatedAt: 50}, {PostID: "b", CreatedAt: 30}, {PostID: "c", CreatedAt: 10}}
	pulled := []Item{{PostID: "x", CreatedAt: 40}, {PostID: "b", CreatedAt: 30}, {PostID: "y", CreatedAt: 20}}
	got := Merge(4, pushed, pulled)
	want := []string{"a", "x", "b", "y"}
	for i, id := range want {
		if got[i].PostID != id {
			t.Fatalf("pos %d: got %s want %s (%v)", i, got[i].PostID, id, got)
		}
	}
}

func TestBeforeCursorPagesWithoutGapsOrRepeats(t *testing.T) {
	items := Merge(0, []Item{{PostID: "a", CreatedAt: 5}, {PostID: "b", CreatedAt: 5}, {PostID: "c", CreatedAt: 4}})
	page1 := items[:2]
	last := page1[len(page1)-1]
	page2 := Before(items, CursorOf(last))
	if len(page2) != 1 || page2[0].PostID != "c" {
		t.Fatalf("page2 = %v", page2)
	}
}
