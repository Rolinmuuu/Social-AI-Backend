package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"socialai/shared/backend"
	"socialai/shared/db/dbtest"
	"socialai/shared/feedplan"
	"socialai/shared/model"
	"socialai/shared/socialgraph"
	"socialai/shared/testutil"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ctx = context.Background()

// The worker runs on the real follow graph in PostgreSQL.
func newTestFeedWorker(t *testing.T) (*FeedWorker, *pgxpool.Pool, *testutil.MockRedisBackend) {
	pool := dbtest.New(t)
	redis := testutil.NewMockRedisBackend()
	return NewFeedWorker(socialgraph.Graph{DB: pool}, redis), pool, redis
}

func followers(t *testing.T, pool *pgxpool.Pool, author string, n int) {
	_, err := pool.Exec(ctx, `
		INSERT INTO follows (follower_id, followee_id)
		SELECT 'fan' || i, $1 FROM generate_series(0, $2 - 1) i`, author, n)
	require.NoError(t, err)
}

func event(post, author string) []byte {
	b, _ := json.Marshal(model.PostCreatedEvent{PostId: post, UserId: author, CreatedAt: 100})
	return b
}

func TestHandlePostCreated_FanOutToFollowers(t *testing.T) {
	w, pool, redis := newTestFeedWorker(t)
	followers(t, pool, "author1", 2)

	require.NoError(t, w.HandlePostCreated(ctx, event("p1", "author1")))
	assert.Equal(t, []string{"p1"}, redis.Feed("fan0"))
	assert.Equal(t, []string{"p1"}, redis.Feed("fan1"))
}

func TestHandlePostCreated_RedeliveryIsIdempotent(t *testing.T) {
	w, pool, redis := newTestFeedWorker(t)
	followers(t, pool, "author1", 1)

	// At-least-once delivery: the same event can arrive twice (outbox retry, consumer restart).
	require.NoError(t, w.HandlePostCreated(ctx, event("p1", "author1")))
	require.NoError(t, w.HandlePostCreated(ctx, event("p1", "author1")))
	assert.Equal(t, []string{"p1"}, redis.Feed("fan0"), "a duplicate event must not duplicate the feed entry")
}

// 2,500 followers: read from PostgreSQL in pages, written in batches of 100.
func TestHandlePostCreated_BatchesFollowerWrites(t *testing.T) {
	w, pool, redis := newTestFeedWorker(t)
	w.Policy = feedplan.Policy{BatchSize: 100, CelebrityThreshold: 10000}
	followers(t, pool, "author1", 2500)

	require.NoError(t, w.HandlePostCreated(ctx, event("p1", "author1")))
	assert.Equal(t, 25, redis.PipelineCalls, "2,500 followers in batches of 100 = 25 round trips")
	assert.Equal(t, []string{"p1"}, redis.Feed("fan2499"))
	assert.Equal(t, []string{"p1"}, redis.Feed("fan0"))
}

func TestHandlePostCreated_CelebrityIsPulledNotPushed(t *testing.T) {
	w, pool, redis := newTestFeedWorker(t)
	w.Policy = feedplan.Policy{CelebrityThreshold: 3}
	followers(t, pool, "star", 5)

	require.NoError(t, w.HandlePostCreated(ctx, event("p1", "star")))
	assert.Equal(t, 0, redis.PipelineCalls, "no fan-out writes for a pull-mode author")
	assert.True(t, redis.IsMember(backend.CelebritySetKey, "star"))
}

func TestHandlePostCreated_NoFollowers(t *testing.T) {
	w, _, redis := newTestFeedWorker(t)
	require.NoError(t, w.HandlePostCreated(ctx, event("p1", "loner")))
	assert.Equal(t, 0, redis.PipelineCalls)
}

func TestHandlePostCreated_InvalidJSON(t *testing.T) {
	w, _, _ := newTestFeedWorker(t)
	err := w.HandlePostCreated(ctx, []byte("not-json"))
	assert.ErrorContains(t, err, "unmarshal")
}

type brokenGraph struct{}

func (brokenGraph) FollowerCountUpTo(context.Context, string, int) (int, error) {
	return 0, errors.New("db down")
}
func (brokenGraph) AllFollowers(context.Context, string, int) ([]string, error) {
	return nil, fmt.Errorf("unreachable")
}

func TestHandlePostCreated_DatabaseErrorIsRetried(t *testing.T) {
	w := NewFeedWorker(brokenGraph{}, testutil.NewMockRedisBackend())
	assert.ErrorContains(t, w.HandlePostCreated(ctx, event("p1", "a")), "count followers", "returned so the consumer retries")
}
