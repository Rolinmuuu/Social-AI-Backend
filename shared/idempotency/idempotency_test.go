package idempotency

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memKV struct {
	mu sync.Mutex
	m  map[string]string
}

func newKV() *memKV { return &memKV{m: map[string]string{}} }

func (k *memKV) SetNX(_ context.Context, key string, v interface{}, _ time.Duration) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.m[key]; ok {
		return false, nil
	}
	k.m[key] = v.(string)
	return true, nil
}
func (k *memKV) Get(_ context.Context, key string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	if !ok {
		return "", errors.New("nil")
	}
	return v, nil
}
func (k *memKV) Set(_ context.Context, key string, v interface{}, _ time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = v.(string)
	return nil
}
func (k *memKV) Delete(_ context.Context, keys ...string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, key := range keys {
		delete(k.m, key)
	}
	return nil
}

// Simulates the HTTP handler flow used by post-service.
func run(ctx context.Context, s *Store, key, fp string, handler func() (Response, error)) (Response, error) {
	if resp, err := s.Begin(ctx, "upload", "alice", key, fp); err != nil {
		return Response{}, err
	} else if resp != nil {
		return *resp, nil
	}
	resp, err := handler()
	if err != nil {
		_ = s.Release(ctx, "upload", "alice", key)
		return Response{}, err
	}
	return resp, s.Complete(ctx, "upload", "alice", key, fp, resp)
}

func TestConcurrentRetriesRunTheHandlerOnce(t *testing.T) {
	s := &Store{KV: newKV()}
	var calls int32
	release := make(chan struct{})
	handler := func() (Response, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return Response{Status: 201, Body: []byte(`{"post_id":"p1"}`)}, nil
	}
	fp := Fingerprint("caption", "file-sha")

	var wg sync.WaitGroup
	var inProgress int32
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := run(context.Background(), s, "k1", fp, handler); errors.Is(err, ErrInProgress) {
				atomic.AddInt32(&inProgress, 1)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1", calls)
	}
	if inProgress != 49 {
		t.Fatalf("%d concurrent duplicates got 409, want 49", inProgress)
	}
	// A later retry replays the stored response without running the handler.
	resp, err := run(context.Background(), s, "k1", fp, handler)
	if err != nil || resp.Status != 201 || string(resp.Body) != `{"post_id":"p1"}` || calls != 1 {
		t.Fatalf("replay: resp=%+v err=%v calls=%d", resp, err, calls)
	}
}

func TestFailureReleasesTheKey(t *testing.T) {
	s := &Store{KV: newKV()}
	fp := Fingerprint("x")
	_, err := run(context.Background(), s, "k2", fp, func() (Response, error) { return Response{}, errors.New("model timeout") })
	if err == nil {
		t.Fatal("expected handler error")
	}
	resp, err := run(context.Background(), s, "k2", fp, func() (Response, error) { return Response{Status: 201}, nil })
	if err != nil || resp.Status != 201 {
		t.Fatalf("retry after failure should run: %+v %v", resp, err)
	}
}

func TestKeyReuseWithDifferentBodyIsRejected(t *testing.T) {
	s := &Store{KV: newKV()}
	ok := func() (Response, error) { return Response{Status: 201}, nil }
	if _, err := run(context.Background(), s, "k3", Fingerprint("a cat"), ok); err != nil {
		t.Fatal(err)
	}
	if _, err := run(context.Background(), s, "k3", Fingerprint("a dog"), ok); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("got %v, want ErrKeyReused", err)
	}
}

func TestFingerprintIsUnambiguous(t *testing.T) {
	if Fingerprint("ab", "c") == Fingerprint("a", "bc") {
		t.Fatal("length-prefixing should keep part boundaries distinct")
	}
}
