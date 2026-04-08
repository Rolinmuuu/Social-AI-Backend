package worker

import (
	"encoding/json"
	"testing"

	"socialai/shared/model"
	"socialai/shared/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestNotificationWorker() (*NotificationWorker, *testutil.MockESBackend) {
	es := testutil.NewMockESBackend()
	w := NewNotificationWorker(es)
	return w, es
}

func TestHandlePostLiked_CreatesNotification(t *testing.T) {
	w, es := newTestNotificationWorker()

	event := model.PostLikedEvent{PostId: "p1", LikerId: "alice", OwnerId: "bob", CreatedAt: 12345}
	payload, _ := json.Marshal(event)

	err := w.HandlePostLiked([]byte("p1"), payload)
	require.NoError(t, err)
	assert.Len(t, es.Docs["notification"], 1, "should create one notification")
}

func TestHandlePostLiked_SelfLike_NoNotification(t *testing.T) {
	w, es := newTestNotificationWorker()

	event := model.PostLikedEvent{PostId: "p1", LikerId: "alice", OwnerId: "alice"}
	payload, _ := json.Marshal(event)

	err := w.HandlePostLiked([]byte("p1"), payload)
	require.NoError(t, err)
	assert.Empty(t, es.Docs["notification"], "self-like should not create notification")
}

func TestHandlePostLiked_InvalidJSON(t *testing.T) {
	w, _ := newTestNotificationWorker()

	err := w.HandlePostLiked([]byte("key"), []byte("{invalid"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal")
}

func TestHandlePostLiked_ESFails(t *testing.T) {
	w, es := newTestNotificationWorker()
	es.SaveErr = assert.AnError

	event := model.PostLikedEvent{PostId: "p1", LikerId: "alice", OwnerId: "bob"}
	payload, _ := json.Marshal(event)

	err := w.HandlePostLiked([]byte("p1"), payload)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "save notification")
}
