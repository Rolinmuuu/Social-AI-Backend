package worker

import (
	"encoding/json"
	"fmt"
	"time"

	"socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/model"

	"github.com/google/uuid"
)

type NotificationWorker struct {
	es backend.ElasticsearchBackendInterface
}

func NewNotificationWorker(es backend.ElasticsearchBackendInterface) *NotificationWorker {
	return &NotificationWorker{es: es}
}

// HandlePostLiked consumes "post.liked" events and creates a notification
// for the post owner (unless the liker is the owner themselves).
func (w *NotificationWorker) HandlePostLiked(key string, value []byte) error {
	var event model.PostLikedEvent
	if err := json.Unmarshal(value, &event); err != nil {
		return fmt.Errorf("unmarshal PostLikedEvent: %w", err)
	}

	if event.LikerId == event.OwnerId {
		return nil
	}

	notification := model.Notification{
		NotificationId: uuid.New().String(),
		UserId:         event.OwnerId,
		Type:           "like",
		ActorId:        event.LikerId,
		PostId:         event.PostId,
		Read:           false,
		CreatedAt:      time.Now().Unix(),
	}

	if err := w.es.SaveToES(notification, constants.NOTIFICATION_INDEX, notification.NotificationId); err != nil {
		return fmt.Errorf("save notification: %w", err)
	}

	fmt.Printf("notification created: %s liked post %s (notify %s)\n",
		event.LikerId, event.PostId, event.OwnerId)
	return nil
}
