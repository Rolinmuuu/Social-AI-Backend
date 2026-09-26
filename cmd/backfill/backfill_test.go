package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"socialai/shared/constants"
	"socialai/shared/db/dbtest"
	"socialai/shared/testutil"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ctx = context.Background()

// legacyStore reproduces the old Elasticsearch data, including the anomalies the old
// check-then-write code could produce.
func legacyStore() *testutil.MockESBackend {
	es := testutil.NewMockESBackend()
	t0 := time.Date(2025, 12, 1, 10, 0, 0, 0, time.UTC)

	es.SetDoc(constants.USER_INDEX, "alice", map[string]any{"user_id": "alice", "password": "$2a$10$hash", "age": 30})
	es.SetDoc(constants.USER_INDEX, "bob", map[string]any{"user_id": "bob", "password": "$2a$10$hash"})
	es.SetDoc(constants.USER_INDEX, "broken", map[string]any{"user_id": "broken"}) // no password

	// The same follow stored twice under random ids (double click), plus a self-follow.
	es.SetDoc(constants.FOLLOW_INDEX, "f1", map[string]any{"follow_id": "f1", "follower_id": "bob", "followee_id": "alice", "created_at": t0})
	es.SetDoc(constants.FOLLOW_INDEX, "f2", map[string]any{"follow_id": "f2", "follower_id": "bob", "followee_id": "alice", "created_at": t0})
	es.SetDoc(constants.FOLLOW_INDEX, "f3", map[string]any{"follow_id": "f3", "follower_id": "bob", "followee_id": "bob", "created_at": t0})

	vec := make([]float32, 1536)
	es.SetDoc(constants.POST_INDEX, "p1", map[string]any{"post_id": "p1", "user_id": "alice", "message": "sunset", "created_at": 1700000000,
		"like_count": 9, "embedding": vec, "outbox_status": "published"})
	// Written before created_at existed; its post.created never reached Kafka.
	es.SetDoc(constants.POST_INDEX, "p2", map[string]any{"post_id": "p2", "user_id": "alice", "message": "old", "outbox_status": "pending"})
	es.SetDoc(constants.POST_INDEX, "p3", map[string]any{"post_id": "p3", "user_id": "bob", "deleted": true, "deleted_at": 1700000500,
		"cleanup_status": "pending", "created_at": 1700000100})

	es.SetDoc(constants.LIKE_INDEX, "p1_bob", map[string]any{"post_like_id": "p1_bob", "post_id": "p1", "user_id": "bob", "created_at": 1700000200})
	es.SetDoc(constants.LIKE_INDEX, "gone_bob", map[string]any{"post_like_id": "gone_bob", "post_id": "gone", "user_id": "bob"}) // orphan
	es.SetDoc(constants.SHARE_INDEX, "s1", map[string]any{"post_share_id": "s1", "post_id": "p1", "user_id": "bob", "platform": "x"})

	// A reply stored before its parent in scan order ("a" < "z"), and an empty comment.
	es.SetDoc(constants.COMMENT_INDEX, "a-reply", map[string]any{"comment_id": "a-reply", "post_id": "p1", "parent_comment_id": "z-top",
		"root_comment_id": "z-top", "depth": 1, "user_id": "alice", "content": "thanks"})
	es.SetDoc(constants.COMMENT_INDEX, "z-top", map[string]any{"comment_id": "z-top", "post_id": "p1", "depth": 0, "user_id": "bob", "content": "nice"})
	es.SetDoc(constants.COMMENT_INDEX, "empty", map[string]any{"comment_id": "empty", "post_id": "p1", "user_id": "bob", "content": ""})

	for i, m := range []struct{ id, from, to, text string }{
		{"m-c", "alice", "bob", "third"}, {"m-a", "bob", "alice", "first"}, {"m-b", "alice", "bob", "second"},
	} {
		es.SetDoc(constants.MESSAGE_INDEX, m.id, map[string]any{"message_id": m.id, "sender_id": m.from, "receiver_id": m.to,
			"content": m.text, "created_at": t0.Add(time.Duration([]int{3, 1, 2}[i]) * time.Minute)})
	}
	es.SetDoc(constants.NOTIFICATION_INDEX, "like:p1:bob", map[string]any{"notification_id": "like:p1:bob", "user_id": "alice",
		"type": "like", "actor_id": "bob", "post_id": "p1", "created_at": 1700000200})
	return es
}

func one(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, sql, args...).Scan(&n))
	return n
}

func TestBackfillCopiesAndCleansLegacyData(t *testing.T) {
	pool := dbtest.New(t)
	b := &Backfill{ES: legacyStore(), DB: pool, Logf: t.Logf}

	rep, err := b.Run(ctx)
	require.NoError(t, err)

	assert.Equal(t, 2, rep["users"].Inserted)
	assert.Equal(t, 1, rep["users"].Rejected, "a user without a password hash")
	assert.Equal(t, 1, rep["follows"].Inserted, "duplicate follow documents collapse into one row")
	assert.Equal(t, 1, rep["follows"].Existing)
	assert.Equal(t, 1, rep["follows"].Rejected, "self-follow violates the CHECK")
	assert.Equal(t, 3, rep["posts"].Inserted)
	assert.Equal(t, 1, rep["likes"].Inserted)
	assert.Equal(t, 1, rep["likes"].Rejected, "like on a post that does not exist")
	assert.Equal(t, 2, rep["comments"].Inserted, "the reply is inserted after its parent")
	assert.Equal(t, 1, rep["comments"].Rejected)
	assert.Equal(t, 3, rep["messages"].Inserted)
	assert.Equal(t, 1, rep["notifications"].Inserted)

	// Counters are recomputed from the rows: the legacy like_count of 9 was wrong.
	assert.Equal(t, 1, one(t, pool, `SELECT like_count FROM posts WHERE post_id = 'p1'`))
	assert.Equal(t, 1, one(t, pool, `SELECT share_count FROM posts WHERE post_id = 'p1'`))
	assert.Equal(t, 1, one(t, pool, `SELECT count(*) FROM posts WHERE post_id = 'p1' AND array_length(embedding, 1) = 1536`),
		"existing embeddings are kept, not paid for again")
	assert.Equal(t, 1, one(t, pool, `SELECT count(*) FROM posts WHERE post_id = 'p2' AND created_at = to_timestamp(0)`),
		"undated posts get the epoch, and sort last in feeds")
	assert.Equal(t, 1, one(t, pool, `SELECT count(*) FROM posts WHERE post_id = 'p3' AND deleted_at IS NOT NULL AND cleanup_status = 'pending'`))

	// The post whose event was stuck in the old outbox is carried over into the new one.
	assert.Equal(t, 1, one(t, pool, `SELECT count(*) FROM outbox WHERE aggregate_id = 'p2' AND status = 'pending'`))
	assert.Equal(t, 1, one(t, pool, `SELECT count(*) FROM outbox`))

	// Flat messages become one conversation numbered by time, fully read.
	rows, err := pool.Query(ctx, `SELECT content FROM messages WHERE conversation_id = 'dm:alice:bob' ORDER BY seq`)
	require.NoError(t, err)
	var got []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		got = append(got, c)
	}
	assert.Equal(t, []string{"first", "second", "third"}, got)
	assert.Equal(t, 3, one(t, pool, `SELECT last_seq FROM conversations`))
	assert.Equal(t, 6, one(t, pool, `SELECT sum(last_read_seq) FROM conversation_members`), "history imported as read")

	// Re-running inserts nothing and changes nothing.
	rep2, err := b.Run(ctx)
	require.NoError(t, err)
	for name, s := range rep2 {
		assert.Zero(t, s.Inserted, "%s re-inserted rows", name)
	}
	assert.Equal(t, 3, rep2["messages"].Existing)
	assert.Equal(t, 3, one(t, pool, `SELECT count(*) FROM messages`))
	assert.Equal(t, 1, one(t, pool, `SELECT count(*) FROM outbox`), "no second event for p2")
}

func TestBackfillMessagesContinueAfterNewTraffic(t *testing.T) {
	pool := dbtest.New(t)
	es := legacyStore()
	b := &Backfill{ES: es, DB: pool}
	_, err := b.Run(ctx)
	require.NoError(t, err)

	// A message that reached the old store late is appended after the copied history.
	es.SetDoc(constants.MESSAGE_INDEX, "m-d", map[string]any{"message_id": "m-d", "sender_id": "bob", "receiver_id": "alice",
		"content": strings.Repeat("x", 10), "created_at": time.Now()})
	rep, err := b.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, rep["messages"].Inserted)
	assert.Equal(t, 4, one(t, pool, `SELECT max(seq) FROM messages`))
}
