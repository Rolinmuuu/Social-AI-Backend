package cache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentMissesQueryOnce(t *testing.T) {
	var g Group
	var queries int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]interface{}, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			v, err, _ := g.Do("user_feed:alice", func() (interface{}, error) {
				atomic.AddInt32(&queries, 1)
				time.Sleep(20 * time.Millisecond) // an ES query
				return "posts", nil
			})
			if err != nil {
				t.Error(err)
			}
			results[i] = v
		}(i)
	}
	close(start)
	wg.Wait()
	if queries != 1 {
		t.Fatalf("%d backend queries for 100 concurrent misses, want 1", queries)
	}
	for _, r := range results {
		if r != "posts" {
			t.Fatal("every caller should get the shared result")
		}
	}
}

func TestErrorsAreSharedButNotCached(t *testing.T) {
	var g Group
	_, err, _ := g.Do("k", func() (interface{}, error) { return nil, errors.New("es down") })
	if err == nil {
		t.Fatal("want error")
	}
	v, err, _ := g.Do("k", func() (interface{}, error) { return 1, nil })
	if err != nil || v != 1 {
		t.Fatal("a later call must run again")
	}
}

func TestPanicInLoaderReleasesWaiters(t *testing.T) {
	var g Group
	_, err, _ := g.Do("k", func() (interface{}, error) { panic("boom") })
	if err == nil {
		t.Fatal("panic should surface as an error")
	}
}

func TestJitterStaysInRange(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := JitterTTL(10*time.Second, 0.2)
		if d < 8*time.Second || d > 12*time.Second {
			t.Fatalf("out of range: %v", d)
		}
	}
}
