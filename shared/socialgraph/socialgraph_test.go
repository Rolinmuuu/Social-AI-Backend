package socialgraph

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"socialai/shared/db/dbtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGraphReads(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	g := Graph{DB: pool}
	_, err := pool.Exec(ctx, `INSERT INTO users (user_id, password_hash) VALUES ('star', 'x')`)
	require.NoError(t, err)
	for i := 0; i < 2500; i++ {
		_, err := pool.Exec(ctx, `INSERT INTO follows (follower_id, followee_id) VALUES ($1, 'star')`, fmt.Sprintf("fan%04d", i))
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO follows (follower_id, followee_id) VALUES ('fan0001', 'other'), ('fan0001', 'third')`)
	require.NoError(t, err)

	ok, err := g.UserExists(ctx, "star")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = g.UserExists(ctx, "ghost")
	require.NoError(t, err)
	assert.False(t, ok)

	n, err := g.FollowerCountUpTo(ctx, "star", 1001)
	require.NoError(t, err)
	assert.Equal(t, 1001, n, "stops counting at the cap")
	n, err = g.FollowerCountUpTo(ctx, "other", 1001)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	all, err := g.AllFollowers(ctx, "star", 10000)
	require.NoError(t, err)
	assert.Len(t, all, 2500, "reads across several 1000-row pages")
	uniq := map[string]bool{}
	for _, f := range all {
		uniq[f] = true
	}
	assert.Len(t, uniq, 2500)
	capped, err := g.AllFollowers(ctx, "star", 1200)
	require.NoError(t, err)
	assert.Len(t, capped, 1200)

	among, err := g.FollowedAmong(ctx, "fan0001", []string{"star", "third", "nobody"})
	require.NoError(t, err)
	sort.Strings(among)
	assert.Equal(t, []string{"star", "third"}, among)
	none, err := g.FollowedAmong(ctx, "fan0001", nil)
	require.NoError(t, err)
	assert.Empty(t, none)
}
