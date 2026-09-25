package model

import "time"

type Post struct {
	PostId        string    `json:"post_id"`
	UserId        string    `json:"user_id"`
	User          string    `json:"user"`
	Message       string    `json:"message"`
	Url           string    `json:"url"`
	Type          string    `json:"type"`
	Deleted       bool      `json:"deleted"`
	DeletedAt     int64     `json:"deleted_at"`
	CleanupStatus string    `json:"cleanup_status"`
	RetryCount    int       `json:"retry_count"`
	LastError     string    `json:"last_error"`
	LikeCount     int       `json:"like_count"`
	SharedCount   int       `json:"shared_count"`
	Embedding     []float32 `json:"embedding,omitempty"`
	CreatedAt     int64     `json:"created_at"`

	// Transactional outbox: the post.created event is stored in the same document write as
	// the post, and a relay publishes it (see shared/outbox). Not exposed to clients.
	OutboxStatus   string `json:"outbox_status,omitempty"` // "pending" | "published" | "dead"
	OutboxAttempts int    `json:"outbox_attempts,omitempty"`
	OutboxNextAt   int64  `json:"outbox_next_at,omitempty"`
	OutboxError    string `json:"outbox_error,omitempty"`
}

// Outbox states.
const (
	OutboxPending   = "pending"
	OutboxPublished = "published"
	OutboxDead      = "dead"
)

// Public returns the post without internal bookkeeping fields, for API responses.
func (p Post) Public() Post {
	p.OutboxStatus, p.OutboxAttempts, p.OutboxNextAt, p.OutboxError = "", 0, 0, ""
	p.Embedding = nil
	return p
}

type User struct {
	UserId   string `json:"user_id"`
	Username string `json:"username"`
	Password string `json:"password"`
	Age      int64  `json:"age"`
	Gender   string `json:"gender"`
}

type PostLike struct {
	PostLikeId string `json:"post_like_id"`
	UserId     string `json:"user_id"`
	PostId     string `json:"post_id"`
	CreatedAt  int64  `json:"created_at"`
}

type PostShare struct {
	PostShareId string `json:"post_share_id"`
	UserId      string `json:"user_id"`
	PostId      string `json:"post_id"`
	CreatedAt   int64  `json:"created_at"`
	Platform    string `json:"platform"`
}

type Comment struct {
	CommentId       string `json:"comment_id"`
	ParentCommentId string `json:"parent_comment_id"`
	RootCommentId   string `json:"root_comment_id"`
	UserId          string `json:"user_id"`
	PostId          string `json:"post_id"`
	Depth           int    `json:"depth"`
	Content         string `json:"content"`
	CreatedAt       int64  `json:"created_at"`
	Deleted         bool   `json:"deleted"`
	DeletedAt       int64  `json:"deleted_at"`
}

type Follow struct {
	FollowId   string    `json:"follow_id"`
	FollowerId string    `json:"follower_id"`
	FolloweeId string    `json:"followee_id"`
	CreatedAt  time.Time `json:"created_at"`
}

type Message struct {
	MessageId  string    `json:"message_id"`
	SenderId   string    `json:"sender_id"`
	ReceiverId string    `json:"receiver_id"`
	Content    string    `json:"content"`
	CreatedAt  time.Time `json:"created_at"`
}

type Notification struct {
	NotificationId string `json:"notification_id"`
	UserId         string `json:"user_id"`
	Type           string `json:"type"`
	ActorId        string `json:"actor_id"`
	PostId         string `json:"post_id"`
	Read           bool   `json:"read"`
	CreatedAt      int64  `json:"created_at"`
}
