//go:build integration

// Run against a real Redis: REDIS_ADDRESS=localhost:6379 go test -tags=integration ./shared/backend/
package backend

import (
	"context"
	"strconv"
	"testing"
	"time"

	"socialai/shared/feedplan"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The home feed read path against a real Redis sorted set: newest first, and paging with the
// (created_at, post_id) cursor neither repeats nor skips items, including items that share a
// timestamp. The in-memory double missed that the ZRANGE arguments were reversed.
func TestHomeFeedPagingOnRealRedis(t *testing.T) {
	ctx := context.Background()
	rb, err := InitRedisBackend()
	require.NoError(t, err)
	r := rb.(*RedisBackendImpl)

	user := "it-user-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	key := HomeFeedKey(user)
	t.Cleanup(func() { r.client.Del(ctx, key) })

	items := []feedplan.Item{
		{PostID: "p1", CreatedAt: 100},
		{PostID: "p2", CreatedAt: 200},
		{PostID: "p3a", CreatedAt: 300},
		{PostID: "p3b", CreatedAt: 300}, // same second as p3a
		{PostID: "p4", CreatedAt: 400},
	}
	for _, it := range items {
		require.NoError(t, r.AddToFeeds(ctx, []string{user}, it, 100, time.Hour))
	}

	var got []string
	var cur feedplan.Cursor
	for page := 0; page < 10; page++ {
		batch, err := r.FeedItems(ctx, key, cur, 2)
		require.NoError(t, err)
		if len(batch) == 0 {
			break
		}
		for _, it := range batch {
			got = append(got, it.PostID)
		}
		cur = feedplan.CursorOf(batch[len(batch)-1])
	}
	assert.Equal(t, []string{"p4", "p3b", "p3a", "p2", "p1"}, got)

	empty, err := r.FeedItems(ctx, HomeFeedKey(user+"-nobody"), feedplan.Cursor{}, 20)
	require.NoError(t, err)
	assert.Empty(t, empty)
}
