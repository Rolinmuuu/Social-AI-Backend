package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/feedplan"
	"socialai/shared/model"

	elastic "github.com/olivere/elastic/v7"
)

type FeedWorker struct {
	es     backend.ElasticsearchBackendInterface
	redis  backend.RedisBackendInterface
	Policy feedplan.Policy
}

func NewFeedWorker(es backend.ElasticsearchBackendInterface, redis backend.RedisBackendInterface) *FeedWorker {
	return &FeedWorker{es: es, redis: redis}
}

// HandlePostCreated consumes "post.created" and delivers the post to followers' home feeds.
//
//   - Authors with at most Policy.CelebrityThreshold followers: push. Followers are written in
//     pipelined batches (one Redis round trip per BatchSize followers, a few batches in
//     parallel) with ZADD keyed by post id, so a redelivered event is a no-op.
//   - Authors above the threshold: pull. The author is added to the celebrity set and nothing
//     is pushed; readers merge that author's posts at read time (PostService.GetHomeFeed).
//     This also removes the old silent cap, where only the first 10,000 followers got the post.
//
// Returning an error makes the consumer retry the event with backoff (and dead-letter it
// after the retry budget), so a partial Redis failure is completed on redelivery.
func (w *FeedWorker) HandlePostCreated(key string, value []byte) error {
	var event model.PostCreatedEvent
	if err := json.Unmarshal(value, &event); err != nil {
		return fmt.Errorf("unmarshal PostCreatedEvent: %w", err)
	}
	ctx := context.Background()
	policy := w.Policy.Defaults()

	// Ask for one more follower than the threshold: enough to decide push vs pull without
	// reading a celebrity's whole follower list.
	query := elastic.NewTermQuery("followee_id", event.UserId)
	result, err := w.es.ReadFromESWithSize(query, constants.FOLLOW_INDEX, policy.CelebrityThreshold+1)
	if err != nil {
		return fmt.Errorf("fetch followers for user %s: %w", event.UserId, err)
	}
	total := int(result.TotalHits())
	if total == 0 {
		return nil
	}

	if policy.Decide(total) == feedplan.Pull {
		if err := w.redis.SAdd(ctx, backend.CelebritySetKey, event.UserId); err != nil {
			return fmt.Errorf("mark %s as pull-mode author: %w", event.UserId, err)
		}
		log.Printf("feed: post %s by %s (%d+ followers) served by pull", event.PostId, event.UserId, total)
		return nil
	}

	followers := make([]string, 0, len(result.Hits.Hits))
	for _, hit := range result.Hits.Hits {
		var follow model.Follow
		if err := json.Unmarshal(hit.Source, &follow); err != nil || follow.FollowerId == "" {
			continue
		}
		followers = append(followers, follow.FollowerId)
	}

	createdAt := event.CreatedAt
	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}
	item := feedplan.Item{PostID: event.PostId, AuthorID: event.UserId, CreatedAt: createdAt}
	start := time.Now()
	trips, err := feedplan.FanOut(ctx, w.redis, followers, item, policy)
	if err != nil {
		return fmt.Errorf("fan-out post %s: %w", event.PostId, err)
	}
	log.Printf("feed: post %s pushed to %d followers in %d round trips (%s)", event.PostId, len(followers), trips, time.Since(start))
	return nil
}
