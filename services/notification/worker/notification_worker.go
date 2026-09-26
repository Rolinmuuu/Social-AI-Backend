package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"socialai/shared/db"
	"socialai/shared/model"
)

// NotificationWorker owns the notifications table.
type NotificationWorker struct {
	db db.Querier
}

func NewNotificationWorker(q db.Querier) *NotificationWorker {
	return &NotificationWorker{db: q}
}

// HandlePostLiked consumes "post.liked" events and creates a notification
// for the post owner (unless the liker is the owner themselves).
//
// The id is deterministic and the insert is ON CONFLICT DO NOTHING: a redelivered event
// (delivery is at-least-once) neither notifies twice nor marks a read notification unread.
func (w *NotificationWorker) HandlePostLiked(ctx context.Context, value []byte) error {
	var event model.PostLikedEvent
	if err := json.Unmarshal(value, &event); err != nil {
		return fmt.Errorf("unmarshal PostLikedEvent: %w", err)
	}

	if event.LikerId == event.OwnerId {
		return nil
	}

	createdAt := time.Now()
	if event.CreatedAt > 0 {
		createdAt = time.Unix(event.CreatedAt, 0)
	}
	tag, err := w.db.Exec(ctx, `
		INSERT INTO notifications (notification_id, user_id, type, actor_id, post_id, created_at)
		VALUES ($1, $2, 'like', $3, $4, $5)
		ON CONFLICT (notification_id) DO NOTHING`,
		"like:"+event.PostId+":"+event.LikerId, event.OwnerId, event.LikerId, event.PostId, createdAt)
	if err != nil {
		return fmt.Errorf("save notification: %w", err)
	}
	if tag.RowsAffected() == 1 {
		log.Printf("notification created: %s liked post %s (notify %s)", event.LikerId, event.PostId, event.OwnerId)
	}
	return nil
}
