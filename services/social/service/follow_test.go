package service

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"socialai/shared/db/dbtest"
	"socialai/shared/pagecursor"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSocialService(t *testing.T, users ...string) (*SocialService, *pgxpool.Pool) {
	pool := dbtest.New(t)
	for _, u := range users {
		_, err := pool.Exec(context.Background(), `INSERT INTO users (user_id, password_hash) VALUES ($1, 'x')`, u)
		require.NoError(t, err)
	}
	return NewSocialService(pool), pool
}

func TestAddFollow(t *testing.T) {
	svc, _ := newTestSocialService(t, "alice", "bob")
	ctx := context.Background()

	id, err := svc.AddFollow(ctx, "alice", "bob")
	require.NoError(t, err)
	assert.Equal(t, "alice:bob", id)

	_, err = svc.AddFollow(ctx, "alice", "bob")
	assert.ErrorIs(t, err, ErrAlreadyFollowing)
	_, err = svc.AddFollow(ctx, "alice", "alice")
	assert.ErrorIs(t, err, ErrCannotFollowSelf)
	_, err = svc.AddFollow(ctx, "alice", "ghost")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

// A double click fires two follows at once: one relationship, one success.
func TestAddFollow_ConcurrentDoubleClickStoresOneRow(t *testing.T) {
	svc, pool := newTestSocialService(t, "alice", "bob")
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, errs[i] = svc.AddFollow(ctx, "alice", "bob") }(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else {
			assert.ErrorIs(t, err, ErrAlreadyFollowing)
		}
	}
	assert.Equal(t, 1, ok)
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM follows`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestRemoveFollow(t *testing.T) {
	svc, _ := newTestSocialService(t, "alice", "bob")
	ctx := context.Background()
	_, err := svc.AddFollow(ctx, "alice", "bob")
	require.NoError(t, err)

	require.NoError(t, svc.RemoveFollow(ctx, "alice", "bob"))
	assert.ErrorIs(t, svc.RemoveFollow(ctx, "alice", "bob"), ErrNotFollowing)
}

// 45 followers, many sharing a created_at: paging returns each exactly once, newest first.
// (The Elasticsearch version returned the first 10 and stopped.)
func TestFollowers_PagesThroughEveryone(t *testing.T) {
	svc, pool := newTestSocialService(t, "star")
	ctx := context.Background()
	for i := 0; i < 45; i++ {
		fan := fmt.Sprintf("fan%02d", i)
		// Three followers per timestamp, to exercise the (created_at, id) tie-break.
		_, err := pool.Exec(ctx, `INSERT INTO follows (follower_id, followee_id, created_at)
			VALUES ($1, 'star', timestamptz '2026-01-01' + make_interval(secs => $2))`, fan, i/3)
		require.NoError(t, err)
	}

	var got []string
	cursor := ""
	for pages := 0; pages < 20; pages++ {
		p, err := svc.Followers(ctx, "star", 7, cursor)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(p.IDs), 7)
		got = append(got, p.IDs...)
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	require.Len(t, got, 45)
	seen := map[string]bool{}
	for _, id := range got {
		assert.False(t, seen[id], "repeated %s", id)
		seen[id] = true
	}
	assert.Equal(t, "fan44", got[0], "newest first")
	assert.Equal(t, "fan00", got[44])

	following, err := svc.Following(ctx, "fan07", 10, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"star"}, following.IDs)
	assert.Empty(t, following.NextCursor)
}

func TestFollowers_BadCursor(t *testing.T) {
	svc, _ := newTestSocialService(t)
	_, err := svc.Followers(context.Background(), "star", 10, "%%%")
	assert.ErrorIs(t, err, pagecursor.ErrBad)
}
