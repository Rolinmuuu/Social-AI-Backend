//go:build e2e

// End-to-end test of the whole system as docker compose runs it: nginx, the four API
// services, the three Kafka workers, PostgreSQL, Redis, Elasticsearch, Kafka and the GCS
// emulator. It talks to the gateway over HTTP like a client would, and checks the
// asynchronous paths (outbox -> Kafka -> workers) by waiting for their effects.
//
//	docker compose up -d --build
//	go test -tags=e2e -count=1 -v ./e2e/
//
// BASE_URL (default http://localhost), DATABASE_URL (default the compose database on
// localhost:5432) and ES_URL (default http://localhost:9200) point it elsewhere. From a
// container on the compose network, also set MEDIA_DIAL_ADDR=gcs:4443.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	baseURL = env("BASE_URL", "http://localhost")
	dbURL   = env("DATABASE_URL", "postgres://socialai:socialai@localhost:5432/socialai?sslmode=disable")
	esURL   = env("ES_URL", "http://localhost:9200")
	client  = &http.Client{Timeout: 30 * time.Second}
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// A 1x1 PNG.
var pixelPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0xf0,
	0x1f, 0x00, 0x05, 0x00, 0x01, 0xff, 0x89, 0x99, 0x3d, 0x1d, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
	0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

type response struct {
	status int
	body   []byte
}

func (r response) json(t *testing.T, v interface{}) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.body, v), "body: %s", r.body)
}

func do(t *testing.T, method, path, token string, body io.Reader, header map[string]string) response {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, body)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	require.NoError(t, err, "%s %s", method, path)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return response{res.StatusCode, b}
}

func doJSON(t *testing.T, method, path, token string, v interface{}, header map[string]string) response {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range header {
		h[k] = v
	}
	return do(t, method, path, token, bytes.NewReader(b), h)
}

// eventually polls check every second until it returns true or timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", timeout, what)
		}
		time.Sleep(time.Second)
	}
}

func signUpAndIn(t *testing.T, userID string) string {
	t.Helper()
	cred := map[string]string{"user_id": userID, "password": "e2e-password"}
	res := doJSON(t, http.MethodPost, "/signup", "", cred, nil)
	require.Equal(t, http.StatusCreated, res.status, "signup %s: %s", userID, res.body)
	res = doJSON(t, http.MethodPost, "/signin", "", cred, nil)
	require.Equal(t, http.StatusOK, res.status, "signin %s: %s", userID, res.body)
	var out struct {
		Token string `json:"token"`
	}
	res.json(t, &out)
	require.NotEmpty(t, out.Token)
	return out.Token
}

type post struct {
	PostId    string `json:"post_id"`
	UserId    string `json:"user_id"`
	Message   string `json:"message"`
	Url       string `json:"url"`
	LikeCount int64  `json:"like_count"`
}

func findPost(posts []post, id string) (post, bool) {
	for _, p := range posts {
		if p.PostId == id {
			return p, true
		}
	}
	return post{}, false
}

func upload(t *testing.T, token, message, idemKey string) response {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	require.NoError(t, w.WriteField("message", message))
	fw, err := w.CreateFormFile("media_file", "pixel.png")
	require.NoError(t, err)
	_, err = fw.Write(pixelPNG)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return do(t, http.MethodPost, "/upload", token, &buf, map[string]string{
		"Content-Type":    w.FormDataContentType(),
		"Idempotency-Key": idemKey,
	})
}

// waitForServices waits until every API service answers through the gateway. nginx returns
// 502 while an upstream is still starting (or has crashed); an unauthenticated request that
// reaches the service gets 400/401 instead.
func waitForServices(t *testing.T) {
	t.Helper()
	probes := []struct{ name, method, path string }{
		{"auth", http.MethodPost, "/signin"},
		{"post", http.MethodGet, "/feed"},
		{"social", http.MethodGet, "/follow/followers"},
		{"message", http.MethodGet, "/conversations"},
	}
	for _, p := range probes {
		eventually(t, 5*time.Minute, p.name+" service reachable through the gateway", func() bool {
			req, _ := http.NewRequest(p.method, baseURL+p.path, bytes.NewReader([]byte("{}")))
			res, err := client.Do(req)
			if err != nil {
				return false
			}
			res.Body.Close()
			return res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusUnauthorized
		})
	}
}

func TestSystemEndToEnd(t *testing.T) {
	waitForServices(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	defer pool.Close()

	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	alice, bob := "alice_"+run, "bob_"+run
	word := "lighthouse" + run // unique search term for this run

	// 1. Auth service (PostgreSQL users table) behind the gateway.
	aliceToken := signUpAndIn(t, alice)
	bobToken := signUpAndIn(t, bob)

	// 2. Social service: alice follows bob.
	res := doJSON(t, http.MethodPost, "/follow", aliceToken, map[string]string{"followee_id": bob}, nil)
	require.Equal(t, http.StatusCreated, res.status, "follow: %s", res.body)

	// 3. Post service: upload with an Idempotency-Key; the media goes to the GCS emulator.
	idem := "e2e-upload-" + run
	res = upload(t, bobToken, "sunrise over the "+word, idem)
	require.Equal(t, http.StatusCreated, res.status, "upload: %s", res.body)
	var created struct {
		PostId string `json:"post_id"`
	}
	res.json(t, &created)
	require.NotEmpty(t, created.PostId)
	postID := created.PostId

	// A retry with the same key is answered from the idempotency store: same post, no second one.
	res = upload(t, bobToken, "sunrise over the "+word, idem)
	require.Equal(t, http.StatusCreated, res.status, "retried upload: %s", res.body)
	var retried struct {
		PostId string `json:"post_id"`
	}
	res.json(t, &retried)
	assert.Equal(t, postID, retried.PostId, "retry returns the first post")
	var bobPosts int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM posts WHERE user_id = $1`, bob).Scan(&bobPosts))
	assert.Equal(t, 1, bobPosts, "exactly one post row")

	// 4. outbox relay -> Kafka post.created -> feed-worker -> Redis: the post reaches alice's feed.
	var inFeed post
	eventually(t, 2*time.Minute, "post in the follower's home feed (feed-worker fan-out)", func() bool {
		res := do(t, http.MethodGet, "/feed?limit=20", aliceToken, nil, nil)
		if res.status != http.StatusOK {
			return false
		}
		var page struct {
			Posts []post `json:"posts"`
		}
		res.json(t, &page)
		var ok bool
		inFeed, ok = findPost(page.Posts, postID)
		return ok
	})
	assert.Equal(t, bob, inFeed.UserId)

	// The media URL points at the object that was stored.
	require.NotEmpty(t, inFeed.Url)
	mediaReq, err := http.NewRequest(http.MethodGet, inFeed.Url, nil)
	require.NoError(t, err)
	if addr := os.Getenv("MEDIA_DIAL_ADDR"); addr != "" {
		// Running inside the compose network: the URL says localhost:4443, the emulator is
		// gcs:4443. Connect there but keep the Host header, which the emulator matches on.
		mediaReq.Host = mediaReq.URL.Host
		mediaReq.URL.Host = addr
	}
	media, err := client.Do(mediaReq)
	require.NoError(t, err, "GET %s", inFeed.Url)
	mediaBody, _ := io.ReadAll(media.Body)
	media.Body.Close()
	assert.Equal(t, http.StatusOK, media.StatusCode, "media %s", inFeed.Url)
	assert.Equal(t, pixelPNG, mediaBody, "stored bytes come back")

	// 5. Kafka post.created -> search-indexer -> Elasticsearch: keyword search finds the post.
	eventually(t, 2*time.Minute, "post searchable by keyword (search-indexer)", func() bool {
		res := do(t, http.MethodGet, "/search?keywords="+word, aliceToken, nil, nil)
		if res.status != http.StatusOK {
			return false
		}
		var out struct {
			Posts []post `json:"posts"`
		}
		res.json(t, &out)
		_, ok := findPost(out.Posts, postID)
		return ok
	})

	// 6. Like: outbox -> Kafka post.liked -> notification-worker writes bob's notification;
	// the write-behind counter reaches PostgreSQL.
	res = do(t, http.MethodPost, "/post/"+postID+"/like", aliceToken, nil, nil)
	require.Equal(t, http.StatusOK, res.status, "like: %s", res.body)
	res = do(t, http.MethodPost, "/post/"+postID+"/like", aliceToken, nil, nil)
	require.Less(t, res.status, 500, "second like: %s", res.body)
	eventually(t, 2*time.Minute, "like notification for the author (notification-worker)", func() bool {
		var n int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM notifications WHERE user_id = $1 AND actor_id = $2 AND post_id = $3`,
			bob, alice, postID).Scan(&n)
		return err == nil && n == 1
	})
	eventually(t, time.Minute, "like count flushed to PostgreSQL", func() bool {
		var likes int64
		err := pool.QueryRow(ctx, `SELECT like_count FROM posts WHERE post_id = $1`, postID).Scan(&likes)
		return err == nil && likes == 1
	})

	// 7. Comments.
	res = doJSON(t, http.MethodPost, "/post/"+postID+"/comment", aliceToken, map[string]string{"content": "what a view"}, nil)
	require.Equal(t, http.StatusCreated, res.status, "comment: %s", res.body)
	res = do(t, http.MethodGet, "/post/"+postID+"/comments", bobToken, nil, nil)
	require.Equal(t, http.StatusOK, res.status, "comments: %s", res.body)
	assert.Contains(t, string(res.body), "what a view")

	// 8. Message service: send (idempotent) and read the inbox.
	msg := map[string]string{"receiver_id": bob, "content": "hi bob"}
	key := map[string]string{"Idempotency-Key": "e2e-msg-" + run}
	res = doJSON(t, http.MethodPost, "/message", aliceToken, msg, key)
	require.Less(t, res.status, 300, "send message: %s", res.body)
	res = doJSON(t, http.MethodPost, "/message", aliceToken, msg, key)
	require.Less(t, res.status, 300, "resend message: %s", res.body)
	res = do(t, http.MethodGet, "/conversations", bobToken, nil, nil)
	require.Equal(t, http.StatusOK, res.status, "conversations: %s", res.body)
	var inbox struct {
		Conversations []struct {
			WithUserId  string `json:"with_user_id"`
			LastMessage string `json:"last_message"`
			Unread      int64  `json:"unread"`
		} `json:"conversations"`
	}
	res.json(t, &inbox)
	require.Len(t, inbox.Conversations, 1)
	assert.Equal(t, alice, inbox.Conversations[0].WithUserId)
	assert.Equal(t, "hi bob", inbox.Conversations[0].LastMessage)
	assert.Equal(t, int64(1), inbox.Conversations[0].Unread, "the retried send was not stored twice")

	// 9. Delete: outbox -> Kafka post.deleted -> search-indexer turns the document into a
	// tombstone. The indexer subscribed to post.deleted before anything was ever published to
	// it, which only works because it created the topic at startup.
	res = do(t, http.MethodDelete, "/post/"+postID, bobToken, nil, nil)
	require.Equal(t, http.StatusOK, res.status, "delete: %s", res.body)
	eventually(t, 2*time.Minute, "search document tombstoned (search-indexer on post.deleted)", func() bool {
		r, err := client.Get(fmt.Sprintf("%s/posts/_doc/%s", esURL, postID))
		if err != nil {
			return false
		}
		defer r.Body.Close()
		var doc struct {
			Source struct {
				Deleted bool `json:"deleted"`
			} `json:"_source"`
		}
		return r.StatusCode == http.StatusOK && json.NewDecoder(r.Body).Decode(&doc) == nil && doc.Source.Deleted
	})
	res = do(t, http.MethodGet, "/search?keywords="+word, aliceToken, nil, nil)
	require.Equal(t, http.StatusOK, res.status)
	assert.NotContains(t, string(res.body), postID, "deleted post is not searchable")

	// 10. Nothing is stuck: every event of this run was published.
	eventually(t, 30*time.Second, "every outbox row of this post marked published", func() bool {
		var total, published int
		err := pool.QueryRow(ctx, `
			SELECT count(*), count(*) FILTER (WHERE status = 'published')
			FROM outbox WHERE aggregate_id = $1`, postID).Scan(&total, &published)
		return err == nil && total == 3 && published == total // created, liked, deleted
	})
}
