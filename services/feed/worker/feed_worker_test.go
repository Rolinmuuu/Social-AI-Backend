package worker

import (
	"encoding/json"
	"testing"

	"socialai/shared/model"
	"socialai/shared/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestFeedWorker() (*FeedWorker, *testutil.MockESBackend, *testutil.MockRedisBackend) {
	es := testutil.NewMockESBackend()
	redis := testutil.NewMockRedisBackend()
	w := NewFeedWorker(es, redis)
	return w, es, redis
}

func TestHandlePostCreated_FanOutToFollowers(t *testing.T) {
	w, es, redis := newTestFeedWorker()

	es.SetDoc("follow", "f1", model.Follow{FollowId: "f1", FollowerId: "follower_a", FolloweeId: "author1"})
	es.SetDoc("follow", "f2", model.Follow{FollowId: "f2", FollowerId: "follower_b", FolloweeId: "author1"})

	event := model.PostCreatedEvent{
		PostId: "p1", UserId: "author1", Message: "hello", Url: "http://img.png", Type: "image",
	}
	payload, _ := json.Marshal(event)

	err := w.HandlePostCreated("author1", payload)
	require.NoError(t, err)

	feedA := redis.GetList("home_feed:follower_a")
	feedB := redis.GetList("home_feed:follower_b")
	assert.Len(t, feedA, 1, "follower_a should have 1 feed item")
	assert.Len(t, feedB, 1, "follower_b should have 1 feed item")
}

func TestHandlePostCreated_NoFollowers(t *testing.T) {
	w, _, redis := newTestFeedWorker()

	event := model.PostCreatedEvent{PostId: "p1", UserId: "loner"}
	payload, _ := json.Marshal(event)

	err := w.HandlePostCreated("loner", payload)
	require.NoError(t, err)
	assert.Empty(t, redis.GetList("home_feed:loner"))
}

func TestHandlePostCreated_InvalidJSON(t *testing.T) {
	w, _, _ := newTestFeedWorker()

	err := w.HandlePostCreated("key", []byte("not-json"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal")
}
