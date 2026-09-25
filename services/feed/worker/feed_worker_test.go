package worker

import (
	"encoding/json"
	"fmt"
	"testing"

	"socialai/shared/backend"
	"socialai/shared/feedplan"
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

	assert.Equal(t, []string{"p1"}, redis.Feed("follower_a"))
	assert.Equal(t, []string{"p1"}, redis.Feed("follower_b"))
}

func TestHandlePostCreated_RedeliveryIsIdempotent(t *testing.T) {
	w, es, redis := newTestFeedWorker()
	es.SetDoc("follow", "f1", model.Follow{FollowId: "f1", FollowerId: "fan", FolloweeId: "author1"})
	payload, _ := json.Marshal(model.PostCreatedEvent{PostId: "p1", UserId: "author1", CreatedAt: 100})

	// At-least-once delivery: the same event can arrive twice (outbox retry, consumer restart).
	require.NoError(t, w.HandlePostCreated("author1", payload))
	require.NoError(t, w.HandlePostCreated("author1", payload))

	assert.Equal(t, []string{"p1"}, redis.Feed("fan"), "a duplicate event must not duplicate the feed entry")
}

func TestHandlePostCreated_BatchesFollowerWrites(t *testing.T) {
	w, es, redis := newTestFeedWorker()
	w.Policy = feedplan.Policy{BatchSize: 100, CelebrityThreshold: 10000}
	for i := 0; i < 250; i++ {
		id := fmt.Sprintf("f%d", i)
		es.SetDoc("follow", id, model.Follow{FollowId: id, FollowerId: "fan" + id, FolloweeId: "author1"})
	}
	payload, _ := json.Marshal(model.PostCreatedEvent{PostId: "p1", UserId: "author1", CreatedAt: 100})
	require.NoError(t, w.HandlePostCreated("author1", payload))

	assert.Equal(t, 3, redis.PipelineCalls, "250 followers in batches of 100 = 3 round trips")
	assert.Equal(t, []string{"p1"}, redis.Feed("fanf249"))
}

func TestHandlePostCreated_CelebrityIsPulledNotPushed(t *testing.T) {
	w, es, redis := newTestFeedWorker()
	w.Policy = feedplan.Policy{CelebrityThreshold: 3}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("f%d", i)
		es.SetDoc("follow", id, model.Follow{FollowId: id, FollowerId: "fan" + id, FolloweeId: "star"})
	}
	payload, _ := json.Marshal(model.PostCreatedEvent{PostId: "p1", UserId: "star", CreatedAt: 100})
	require.NoError(t, w.HandlePostCreated("star", payload))

	assert.Equal(t, 0, redis.PipelineCalls, "no fan-out writes for a pull-mode author")
	assert.True(t, redis.IsMember(backend.CelebritySetKey, "star"))
}

func TestHandlePostCreated_NoFollowers(t *testing.T) {
	w, _, redis := newTestFeedWorker()

	event := model.PostCreatedEvent{PostId: "p1", UserId: "loner"}
	payload, _ := json.Marshal(event)

	err := w.HandlePostCreated("loner", payload)
	require.NoError(t, err)
	assert.Empty(t, redis.Feed("loner"))
	assert.Equal(t, 0, redis.PipelineCalls)
}

func TestHandlePostCreated_InvalidJSON(t *testing.T) {
	w, _, _ := newTestFeedWorker()

	err := w.HandlePostCreated("key", []byte("not-json"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal")
}
