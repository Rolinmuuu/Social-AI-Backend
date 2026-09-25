package model

// Topic: "post.created"
// 触发时机: SavePost 成功后
// 消费方: feed-worker (fan-out 给所有粉丝)
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
	TopicPostLiked   = "post.liked"
)

// Topic: "post.liked"
// 触发时机: LikePost 成功后
// 消费方: notification-worker (通知帖子作者)
type PostLikedEvent struct {
	PostId    string `json:"post_id"`
	LikerId   string `json:"liker_id"`
	OwnerId   string `json:"owner_id"`
	CreatedAt int64  `json:"created_at"`
}
