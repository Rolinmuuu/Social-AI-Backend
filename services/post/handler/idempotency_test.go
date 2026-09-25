package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"socialai/services/post/service"
	"socialai/shared/idempotency"
	"socialai/shared/testutil"

	jwt "github.com/form3tech-oss/jwt-go"
)

func uploadRequest(t *testing.T, key, caption string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("message", caption)
	fw, err := mw.CreateFormFile("media_file", "cat.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("png-bytes"))
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/upload", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	token := &jwt.Token{Claims: jwt.MapClaims{"user_id": "alice"}}
	return r.WithContext(context.WithValue(r.Context(), "user", token))
}

func TestUploadRetryWithSameKeyCreatesOnePost(t *testing.T) {
	es := testutil.NewMockESBackend()
	svc := service.NewPostService(es, testutil.NewMockRedisBackend(), testutil.NewMockGCSBackend(), testutil.NewMockOpenAIBackend(), testutil.NewMockKafkaProducer())
	h := NewPostHandler(svc)

	first := httptest.NewRecorder()
	h.uploadPostHandler(first, uploadRequest(t, "key-1", "sunset"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first upload: %d %s", first.Code, first.Body.String())
	}

	retry := httptest.NewRecorder()
	h.uploadPostHandler(retry, uploadRequest(t, "key-1", "sunset"))
	if retry.Code != http.StatusCreated || retry.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("retry should replay the stored response: %d %v", retry.Code, retry.Header())
	}

	var a, b map[string]string
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(retry.Body.Bytes(), &b)
	if a["post_id"] == "" || a["post_id"] != b["post_id"] {
		t.Fatalf("retry returned a different post: %v vs %v", a, b)
	}
	if n := len(es.Docs["post"]); n != 1 {
		t.Fatalf("%d posts stored, want 1", n)
	}

	reused := httptest.NewRecorder()
	h.uploadPostHandler(reused, uploadRequest(t, "key-1", "a different caption"))
	if reused.Code != http.StatusUnprocessableEntity {
		t.Fatalf("key reuse with another body: %d, want 422", reused.Code)
	}
}

func TestUploadWithoutKeyStillWorks(t *testing.T) {
	svc := service.NewPostService(testutil.NewMockESBackend(), testutil.NewMockRedisBackend(), testutil.NewMockGCSBackend(), testutil.NewMockOpenAIBackend(), testutil.NewMockKafkaProducer())
	rec := httptest.NewRecorder()
	NewPostHandler(svc).uploadPostHandler(rec, uploadRequest(t, "", "sunset"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d", rec.Code)
	}
}

// ctxRedis behaves like the real client: a call with a cancelled context fails.
type ctxRedis struct{ *testutil.MockRedisBackend }

func (c ctxRedis) SetNX(ctx context.Context, k string, v interface{}, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return c.MockRedisBackend.SetNX(ctx, k, v, ttl)
}
func (c ctxRedis) Get(ctx context.Context, k string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return c.MockRedisBackend.Get(ctx, k)
}
func (c ctxRedis) Set(ctx context.Context, k string, v interface{}, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.MockRedisBackend.Set(ctx, k, v, ttl)
}
func (c ctxRedis) Delete(ctx context.Context, keys ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.MockRedisBackend.Delete(ctx, keys...)
}

// The case idempotency keys exist for: the client gives up (its connection closes, so the
// request context is cancelled) after the post was saved, then retries. The retry must get
// the stored response, not 409 and not a second post.
func TestUploadClientDisconnectThenRetryReplays(t *testing.T) {
	es := testutil.NewMockESBackend()
	redis := testutil.NewMockRedisBackend()
	svc := service.NewPostService(es, redis, testutil.NewMockGCSBackend(), testutil.NewMockOpenAIBackend(), testutil.NewMockKafkaProducer())
	svc.Idempotency = &idempotency.Store{KV: ctxRedis{redis}}
	h := NewPostHandler(svc)

	req := uploadRequest(t, "key-dc", "sunset")
	ctx, cancel := context.WithCancel(req.Context())
	cancel() // the client is already gone when the handler records the result
	h.uploadPostHandler(httptest.NewRecorder(), req.WithContext(ctx))

	retry := httptest.NewRecorder()
	h.uploadPostHandler(retry, uploadRequest(t, "key-dc", "sunset"))
	if retry.Code != http.StatusCreated || retry.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("retry after disconnect: %d %s, want replayed 201", retry.Code, retry.Body.String())
	}
	if n := len(es.Docs["post"]); n != 1 {
		t.Fatalf("%d posts stored, want 1", n)
	}
}
