// Package idempotency makes non-idempotent POST endpoints safe to retry.
//
// A client that times out on POST /upload or POST /post/generate-image-from-openai does not
// know whether the post was created. Retrying blindly creates a duplicate post — and for
// image generation, a second paid model call. Clients send an Idempotency-Key header (a
// UUID per logical action); the server remembers the first outcome for that key:
//
//	first request          -> claim the key atomically (SET NX), run the handler, store the response
//	retry while running    -> 409 "request in progress" (the handler is not run twice)
//	retry after success    -> replay the stored response, handler not run
//	retry after a failure  -> the claim was released, so the retry runs normally
//	same key, other body   -> 422: a key must not be reused for a different request
//
// The claim and the stored response live in Redis with a TTL, so keys expire on their own.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// KV is the subset of Redis the store needs. SetNX must be atomic.
type KV interface {
	SetNX(ctx context.Context, key string, value interface{}, ttl time.Duration) (bool, error)
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error
	Delete(ctx context.Context, keys ...string) error
}

var (
	// ErrInProgress: another request with this key is still running.
	ErrInProgress = errors.New("idempotency: request with this key is in progress")
	// ErrKeyReused: the key was first used with a different request body.
	ErrKeyReused = errors.New("idempotency: key reused with a different request")
)

// Response is what gets replayed to a retried request.
type Response struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type entry struct {
	State       string    `json:"state"` // "running" | "done"
	Fingerprint string    `json:"fp"`
	Response    *Response `json:"resp,omitempty"`
}

// Store claims keys and records outcomes.
type Store struct {
	KV KV
	// LockTTL bounds how long a crashed request can block its key (default 2 minutes; image
	// generation can take ~30s).
	LockTTL time.Duration
	// ResultTTL is how long a finished response is replayed (default 24h).
	ResultTTL time.Duration
}

func (s *Store) ttl() (time.Duration, time.Duration) {
	lock, result := s.LockTTL, s.ResultTTL
	if lock <= 0 {
		lock = 2 * time.Minute
	}
	if result <= 0 {
		result = 24 * time.Hour
	}
	return lock, result
}

// Fingerprint hashes the parts of a request that define "the same request".
func Fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func redisKey(scope, userID, key string) string {
	// Scoped per user so one client cannot read another user's replayed response.
	return fmt.Sprintf("idem:%s:%s:%s", scope, userID, key)
}

// Begin claims the key. It returns (nil, nil) when the caller should run the handler and
// then call Complete or Release; (resp, nil) when a stored response must be replayed; or
// ErrInProgress / ErrKeyReused.
func (s *Store) Begin(ctx context.Context, scope, userID, key, fingerprint string) (*Response, error) {
	lockTTL, _ := s.ttl()
	k := redisKey(scope, userID, key)
	claim, _ := json.Marshal(entry{State: "running", Fingerprint: fingerprint})
	ok, err := s.KV.SetNX(ctx, k, string(claim), lockTTL)
	if err != nil {
		return nil, fmt.Errorf("idempotency: claim: %w", err)
	}
	if ok {
		return nil, nil
	}
	raw, err := s.KV.Get(ctx, k)
	if err != nil {
		// The claim expired between SETNX and GET; treat it as in progress, the client retries.
		return nil, ErrInProgress
	}
	var e entry
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return nil, fmt.Errorf("idempotency: corrupt entry: %w", err)
	}
	if e.Fingerprint != fingerprint {
		return nil, ErrKeyReused
	}
	if e.State == "done" && e.Response != nil {
		return e.Response, nil
	}
	return nil, ErrInProgress
}

// Complete stores the response so retries replay it.
func (s *Store) Complete(ctx context.Context, scope, userID, key, fingerprint string, resp Response) error {
	_, resultTTL := s.ttl()
	data, err := json.Marshal(entry{State: "done", Fingerprint: fingerprint, Response: &resp})
	if err != nil {
		return err
	}
	return s.KV.Set(ctx, redisKey(scope, userID, key), string(data), resultTTL)
}

// Release drops the claim after a failure so the client can retry with the same key.
func (s *Store) Release(ctx context.Context, scope, userID, key string) error {
	return s.KV.Delete(ctx, redisKey(scope, userID, key))
}
