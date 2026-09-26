package model

// Topic: "post.created"
// Written to the outbox in the transaction that inserts the post.
// Consumers: feed-worker (fan-out to followers), search-indexer (search projection).
type PostCreatedEvent struct {
	PostId    string `json:"post_id"`
	UserId    string `json:"user_id"`
	Message   string `json:"message"`
	Url       string `json:"url"`
	Type      string `json:"type"`
	CreatedAt int64  `json:"created_at"`
}

// CreatedAtUnix lets the outbox relay measure publish lag.
func (e PostCreatedEvent) CreatedAtUnix() int64 { return e.CreatedAt }

// Topic names.
const (
	TopicPostCreated = "post.created"
	TopicPostDeleted = "post.deleted"
	TopicPostLiked   = "post.liked"
)

// Topic: "post.deleted"
// Written to the outbox in the transaction that soft-deletes the post.
// Consumers: search-indexer (drops the post from search results).
type PostDeletedEvent struct {
	PostId    string `json:"post_id"`
	UserId    string `json:"user_id"`
	DeletedAt int64  `json:"deleted_at"`
}

// Topic: "post.liked"
// Written to the outbox in the transaction that inserts the like, so a notification is no
// longer lost when Kafka is down (it used to be a best-effort publish after the write).
// Consumers: notification-worker (notifies the post's author).
type PostLikedEvent struct {
	PostId    string `json:"post_id"`
	LikerId   string `json:"liker_id"`
	OwnerId   string `json:"owner_id"`
	CreatedAt int64  `json:"created_at"`
}
