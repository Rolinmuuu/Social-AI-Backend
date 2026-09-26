package worker

import (
	"context"
	"encoding/json"
	"testing"

	"socialai/shared/db/dbtest"
	"socialai/shared/model"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestNotificationWorker(t *testing.T) (*NotificationWorker, *pgxpool.Pool) {
	pool := dbtest.New(t)
	return NewNotificationWorker(pool), pool
}

func count(t *testing.T, pool *pgxpool.Pool) int {
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications`).Scan(&n))
	return n
}

func TestHandlePostLiked_CreatesNotification(t *testing.T) {
	w, pool := newTestNotificationWorker(t)
	payload, _ := json.Marshal(model.PostLikedEvent{PostId: "p1", LikerId: "alice", OwnerId: "bob", CreatedAt: 12345})

	require.NoError(t, w.HandlePostLiked(context.Background(), payload))
	assert.Equal(t, 1, count(t, pool))

	var userID, actor string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT user_id, actor_id FROM notifications`).Scan(&userID, &actor))
	assert.Equal(t, "bob", userID)
	assert.Equal(t, "alice", actor)
}

// Redelivery must not notify twice, and must not flip a read notification back to unread.
func TestHandlePostLiked_RedeliveryIsANoOp(t *testing.T) {
	w, pool := newTestNotificationWorker(t)
	ctx := context.Background()
	payload, _ := json.Marshal(model.PostLikedEvent{PostId: "p1", LikerId: "alice", OwnerId: "bob"})
	require.NoError(t, w.HandlePostLiked(ctx, payload))
	_, err := pool.Exec(ctx, `UPDATE notifications SET read = true`)
	require.NoError(t, err)

	require.NoError(t, w.HandlePostLiked(ctx, payload))
	assert.Equal(t, 1, count(t, pool))
	var read bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT read FROM notifications`).Scan(&read))
	assert.True(t, read)
}

func TestHandlePostLiked_SelfLike_NoNotification(t *testing.T) {
	w, pool := newTestNotificationWorker(t)
	payload, _ := json.Marshal(model.PostLikedEvent{PostId: "p1", LikerId: "alice", OwnerId: "alice"})

	require.NoError(t, w.HandlePostLiked(context.Background(), payload))
	assert.Zero(t, count(t, pool), "self-like should not create notification")
}

func TestHandlePostLiked_InvalidJSON(t *testing.T) {
	w, _ := newTestNotificationWorker(t)
	err := w.HandlePostLiked(context.Background(), []byte("{invalid"))
	assert.ErrorContains(t, err, "unmarshal")
}

func TestHandlePostLiked_DBFails(t *testing.T) {
	w, pool := newTestNotificationWorker(t)
	pool.Close() // every query now fails
	payload, _ := json.Marshal(model.PostLikedEvent{PostId: "p1", LikerId: "alice", OwnerId: "bob"})
	assert.ErrorContains(t, w.HandlePostLiked(context.Background(), payload), "save notification",
		"the error goes back to the consumer, which retries and then dead-letters")
}
