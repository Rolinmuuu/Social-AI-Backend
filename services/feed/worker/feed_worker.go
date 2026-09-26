package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"socialai/shared/backend"
	"socialai/shared/feedplan"
	"socialai/shared/model"
)

// Followers is the part of the social graph fan-out needs (socialgraph.Graph in production).
type Followers interface {
	FollowerCountUpTo(ctx context.Context, userID string, max int) (int, error)
	AllFollowers(ctx context.Context, userID string, max int) ([]string, error)
}

type FeedWorker struct {
	graph  Followers
	redis  backend.RedisBackendInterface
	Policy feedplan.Policy
}

func NewFeedWorker(graph Followers, redis backend.RedisBackendInterface) *FeedWorker {
	return &FeedWorker{graph: graph, redis: redis}
}

// HandlePostCreated consumes "post.created" and delivers the post to followers' home feeds.
//
//   - Authors with at most Policy.CelebrityThreshold followers: push. Followers are written in
//     pipelined batches (one Redis round trip per BatchSize followers, a few batches in
//     parallel) with ZADD keyed by post id, so a redelivered event is a no-op.
//   - Authors above the threshold: pull. The author is added to the celebrity set and nothing
//     is pushed; readers merge that author's posts at read time (PostService.GetHomeFeed).
//
// Followers come from the follows table in PostgreSQL. Counting stops at threshold+1, so
// deciding push vs pull costs a bounded index scan even for an account with millions of
// followers.
//
// Returning an error makes the consumer retry the event with backoff (and dead-letter it
// after the retry budget), so a partial Redis failure is completed on redelivery.
func (w *FeedWorker) HandlePostCreated(ctx context.Context, value []byte) error {
	var event model.PostCreatedEvent
	if err := json.Unmarshal(value, &event); err != nil {
		return fmt.Errorf("unmarshal PostCreatedEvent: %w", err)
	}
	policy := w.Policy.Defaults()

	total, err := w.graph.FollowerCountUpTo(ctx, event.UserId, policy.CelebrityThreshold+1)
	if err != nil {
		return fmt.Errorf("count followers of %s: %w", event.UserId, err)
	}
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

	followers, err := w.graph.AllFollowers(ctx, event.UserId, policy.CelebrityThreshold)
	if err != nil {
		return fmt.Errorf("read followers of %s: %w", event.UserId, err)
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
