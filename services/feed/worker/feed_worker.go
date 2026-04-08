package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/model"

	elastic "github.com/olivere/elastic/v7"
)

const homeFeedMaxLen = 100 // 每个用户最多缓存 100 条 Feed

type FeedWorker struct {
	es    backend.ElasticsearchBackendInterface
	redis backend.RedisBackendInterface
}

func NewFeedWorker(es backend.ElasticsearchBackendInterface, redis backend.RedisBackendInterface) *FeedWorker {
	return &FeedWorker{es: es, redis: redis}
}

// HandlePostCreated 消费 "post.created" 事件，将新帖 fan-out 到所有粉丝的 Redis Feed 列表。
// key = 发帖者 userId（用于 Kafka partition routing）
// value = PostCreatedEvent JSON
func (w *FeedWorker) HandlePostCreated(key string, value []byte) error {
	var event model.PostCreatedEvent
	if err := json.Unmarshal(value, &event); err != nil {
		return fmt.Errorf("unmarshal PostCreatedEvent: %w", err)
	}

	ctx := context.Background()

	// 查询发帖者的所有粉丝（follow 索引中 followee_id == event.UserId）
	query := elastic.NewTermQuery("followee_id", event.UserId)
	result, err := w.es.ReadFromESWithSize(query, constants.FOLLOW_INDEX, 10000)
	if err != nil {
		return fmt.Errorf("fetch followers for user %s: %w", event.UserId, err)
	}
	if result.TotalHits() == 0 {
		return nil // 没有粉丝，无需 fan-out
	}

	// 把新帖序列化为 Feed 条目，存入每个粉丝的 Redis List
	feedItem := model.Post{
		PostId:  event.PostId,
		UserId:  event.UserId,
		Message: event.Message,
		Url:     event.Url,
		Type:    event.Type,
	}
	feedData, err := json.Marshal(feedItem)
	if err != nil {
		return fmt.Errorf("marshal feed item: %w", err)
	}

	for _, hit := range result.Hits.Hits {
		var follow model.Follow
		if err := json.Unmarshal(hit.Source, &follow); err != nil {
			continue
		}
		feedKey := fmt.Sprintf("home_feed:%s", follow.FollowerId)

		// LPUSH 插入头部 → LTRIM 保留最新 100 条 → EXPIRE 24 小时 TTL
		_ = w.redis.LPush(ctx, feedKey, feedData)
		_ = w.redis.LTrim(ctx, feedKey, 0, homeFeedMaxLen-1)
		_ = w.redis.Expire(ctx, feedKey, 24*time.Hour)
	}

	fmt.Printf("fan-out complete: post_id=%s followers=%d\n", event.PostId, result.TotalHits())
	return nil
}
