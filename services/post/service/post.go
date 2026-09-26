package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"time"

	"socialai/shared/backend"
	"socialai/shared/cache"
	"socialai/shared/counter"
	"socialai/shared/db"
	"socialai/shared/feedplan"
	"socialai/shared/idempotency"
	"socialai/shared/kafka"
	"socialai/shared/model"
	"socialai/shared/outbox"
	"socialai/shared/socialgraph"
	"socialai/shared/utils"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostService owns the posts, post_likes, post_shares and comments tables and the outbox.
//
// PostgreSQL is the system of record. Elasticsearch is only read, for keyword and vector
// search, and every search hit is re-read from the database (livePostsByID), so a lagging
// index can rank results but never show a deleted post or a stale count.
type PostService struct {
	db     *pgxpool.Pool
	es     backend.ElasticsearchBackendInterface
	redis  backend.RedisBackendInterface
	gcs    backend.GoogleCloudStorageBackendInterface
	openai backend.OpenAIBackendInterface
	graph  socialgraph.Graph

	// Outbox relay: publishes rows the request path wrote to the outbox table.
	Relay  *outbox.Relay
	Outbox *outbox.PGStore
	// Write-behind like/share counters (see shared/counter).
	Counters *counter.Counter
	// Idempotency-Key store for POST /upload and image generation.
	Idempotency *idempotency.Store
	// Home feed policy (push/pull threshold, feed length).
	FeedPolicy feedplan.Policy

	loads     cache.Group // single-flight for cache misses
	userPosts func(ctx context.Context, userID string) ([]model.Post, error)
	now       func() time.Time
	download  func(url string) (io.ReadCloser, error)
}

func NewPostService(
	pool *pgxpool.Pool,
	es backend.ElasticsearchBackendInterface,
	redis backend.RedisBackendInterface,
	gcs backend.GoogleCloudStorageBackendInterface,
	openai backend.OpenAIBackendInterface,
	kafka kafka.KafkaProducerInterface,
) *PostService {
	s := &PostService{
		db: pool, es: es, redis: redis, gcs: gcs, openai: openai,
		graph: socialgraph.Graph{DB: pool},
		now:   time.Now, download: backend.DownloadImage,
	}
	s.Outbox = &outbox.PGStore{Pool: pool}
	s.Relay = &outbox.Relay{Store: s.Outbox, Publisher: kafka}
	s.Counters = &counter.Counter{Store: redis, Sink: pgCounterSink{db: pool}}
	s.Idempotency = &idempotency.Store{KV: redis}
	s.userPosts = s.loadUserPosts
	return s
}

// fastPathGrace is how long the relay leaves a new outbox row alone. The request publishes
// it inline right after commit (normally within milliseconds); the relay only steps in for
// rows still pending after that, so the two rarely publish the same event.
const fastPathGrace = 10 * time.Second

// publishAfterCommit is the outbox fast path. A failure is only logged: the row is committed
// and the relay will publish it.
func (s *PostService) publishAfterCommit(ctx context.Context, rec outbox.Record) {
	if err := s.Relay.PublishNow(ctx, rec); err != nil {
		log.Printf("outbox: %s %s deferred to the relay: %v", rec.Topic, rec.ID, err)
	}
}

// ──────────────────────────── reads ────────────────────────────

const userPostsTTL = 10 * time.Second
const userPostsLimit = 100

// SearchPostByUserId returns a user's newest posts through a read-through cache. Misses for
// the same user are single-flighted (one query however many requests miss at once) and the
// TTL is jittered so entries written together do not expire together.
func (s *PostService) SearchPostByUserId(ctx context.Context, userId string) ([]model.Post, error) {
	cacheKey := utils.UserFeedCacheKey(userId)

	if cached, err := s.redis.Get(ctx, cacheKey); err == nil {
		var posts []model.Post
		if err := json.Unmarshal([]byte(cached), &posts); err == nil {
			return posts, nil
		}
	}

	v, err, _ := s.loads.Do(cacheKey, func() (interface{}, error) {
		// Detached: one caller giving up must not fail the query for everyone sharing it.
		lctx := context.WithoutCancel(ctx)
		posts, err := s.userPosts(lctx, userId)
		if err != nil {
			return nil, err
		}
		if data, err := json.Marshal(posts); err == nil {
			_ = s.redis.Set(lctx, cacheKey, data, cache.JitterTTL(userPostsTTL, 0.2))
		}
		return posts, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]model.Post), nil
}

func (s *PostService) loadUserPosts(ctx context.Context, userID string) ([]model.Post, error) {
	return queryPosts(ctx, s.db, `
		SELECT `+postColumns+` FROM posts
		WHERE user_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC, post_id DESC
		LIMIT $2`, userID, userPostsLimit)
}

// recentLimit bounds search results and the "all posts" listing.
const recentLimit = 50

// SearchPostByKeywords ranks posts in Elasticsearch and loads them from PostgreSQL. With no
// keywords it lists the newest posts straight from the database.
func (s *PostService) SearchPostByKeywords(ctx context.Context, keywords string) ([]model.Post, error) {
	if keywords == "" {
		return queryPosts(ctx, s.db, `
			SELECT `+postColumns+` FROM posts WHERE deleted_at IS NULL
			ORDER BY created_at DESC, post_id DESC LIMIT $1`, recentLimit)
	}
	ids, err := backend.SearchPostIDs(s.es, keywords, recentLimit)
	if err != nil {
		return nil, err
	}
	return livePostsByID(ctx, s.db, ids)
}

// SemanticSearch embeds the query, asks Elasticsearch for the nearest post vectors, and loads
// those posts from PostgreSQL.
func (s *PostService) SemanticSearch(ctx context.Context, queryText string, topK int) ([]model.Post, error) {
	queryVector, err := s.openai.GetEmbedding(ctx, queryText)
	if err != nil {
		return nil, fmt.Errorf("failed to generate query embedding: %w", err)
	}
	ids, err := backend.NearestPostIDs(s.es, queryVector, topK)
	if err != nil {
		return nil, err
	}
	return livePostsByID(ctx, s.db, ids)
}

// ──────────────────────────── create ────────────────────────────

// SavePost stores an uploaded post: media to GCS, then one database transaction that inserts
// the post and its post.created event. If the transaction fails, the GCS object is deleted
// (compensation). The event is published right after commit; if Kafka is unavailable the
// request still succeeds and the outbox relay publishes it later.
//
// The embedding for semantic search is no longer computed here: it was a second OpenAI call
// (hundreds of ms) on every upload. The search indexer computes it asynchronously.
func (s *PostService) SavePost(ctx context.Context, post *model.Post, file multipart.File) error {
	post.PostId = uuid.New().String()

	medialink, err := s.gcs.SaveToGCS(file, post.PostId)
	if err != nil {
		return fmt.Errorf("failed to save to GCS: %w", err)
	}
	post.Url = medialink
	return s.persistNewPost(ctx, post)
}

// persistNewPost is the shared tail of upload and image generation.
func (s *PostService) persistNewPost(ctx context.Context, post *model.Post) error {
	now := s.now()
	post.CreatedAt = now.Unix()
	post.Deleted, post.DeletedAt = false, 0

	var rec outbox.Record
	err := db.InTx(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO posts (post_id, user_id, message, url, type, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			post.PostId, post.UserId, post.Message, post.Url, post.Type, time.Unix(post.CreatedAt, 0)); err != nil {
			return err
		}
		var err error
		rec, err = outbox.Enqueue(ctx, tx, outbox.Event{
			AggregateType: "post",
			AggregateID:   post.PostId,
			Topic:         model.TopicPostCreated,
			Key:           post.UserId, // one author's events stay in order on one partition
			Payload: model.PostCreatedEvent{
				PostId: post.PostId, UserId: post.UserId, Message: post.Message,
				Url: post.Url, Type: post.Type, CreatedAt: post.CreatedAt,
			},
		}, now.Add(fastPathGrace))
		return err
	})
	if err != nil {
		// Compensating action: remove the orphan GCS file.
		if deleteErr := s.gcs.DeleteFromGCS(post.PostId); deleteErr != nil {
			log.Printf("CRITICAL: GCS orphan file, manual cleanup needed. post_id=%s db_err=%v gcs_err=%v",
				post.PostId, err, deleteErr)
		}
		return fmt.Errorf("failed to save post: %w", err)
	}

	_ = s.redis.Delete(ctx, utils.UserFeedCacheKey(post.UserId))
	s.publishAfterCommit(ctx, rec)
	return nil
}

func (s *PostService) GenerateImageFromOpenAIAndSavePost(ctx context.Context, userId, prompt string) (*model.Post, error) {
	imageUrl, err := s.openai.GenerateImage(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("failed to generate image: %w", err)
	}

	body, err := s.download(imageUrl)
	if err != nil {
		return nil, fmt.Errorf("failed to download image: %w", err)
	}
	defer body.Close()

	post := &model.Post{
		PostId:  uuid.New().String(),
		UserId:  userId,
		Message: prompt,
		Type:    "image",
	}

	mediaLink, err := s.gcs.SaveToGCS(body, post.PostId)
	if err != nil {
		return nil, fmt.Errorf("failed to upload image to GCS: %w", err)
	}
	post.Url = mediaLink

	if err := s.persistNewPost(ctx, post); err != nil {
		return nil, err
	}
	return post, nil
}

// ──────────────────────────── delete ────────────────────────────

// DeletePost soft-deletes a post (only its author may) and records post.deleted in the same
// transaction, so the search index is told exactly when the delete commits. The media file
// is removed later by CleanupDeletedPosts.
func (s *PostService) DeletePost(ctx context.Context, postId, userId string) (bool, error) {
	if postId == "" || userId == "" {
		return false, nil
	}
	var rec outbox.Record
	err := db.InTx(ctx, s.db, func(tx pgx.Tx) error {
		var owner string
		var deletedAt *time.Time
		// FOR UPDATE: two concurrent deletes of one post serialise here, and the second
		// sees it already deleted instead of recording a second event.
		err := tx.QueryRow(ctx, `SELECT user_id, deleted_at FROM posts WHERE post_id = $1 FOR UPDATE`, postId).Scan(&owner, &deletedAt)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && deletedAt != nil) {
			return ErrPostNotFound
		}
		if err != nil {
			return err
		}
		if owner != userId {
			return ErrNotPostOwner
		}
		now := s.now()
		// Only the columns that change: counters are not rewritten (no lost update).
		if _, err := tx.Exec(ctx, `
			UPDATE posts SET deleted_at = $2, cleanup_status = 'pending', cleanup_attempts = 0,
			                 cleanup_error = '', version = version + 1
			WHERE post_id = $1`, postId, now); err != nil {
			return err
		}
		rec, err = outbox.Enqueue(ctx, tx, outbox.Event{
			AggregateType: "post", AggregateID: postId,
			Topic: model.TopicPostDeleted, Key: owner,
			Payload: model.PostDeletedEvent{PostId: postId, UserId: owner, DeletedAt: now.Unix()},
		}, now.Add(fastPathGrace))
		return err
	})
	if err != nil {
		return false, err
	}
	_ = s.redis.Delete(ctx, utils.UserFeedCacheKey(userId))
	s.publishAfterCommit(ctx, rec)
	return true, nil
}

// CleanupDeletedPosts removes the media of up to limit deleted posts. Each post is claimed
// with FOR UPDATE SKIP LOCKED, so every post-service instance can run this loop without two
// of them working on the same post. Returns how many posts were processed.
func (s *PostService) CleanupDeletedPosts(ctx context.Context, limit int) (int, error) {
	done := 0
	for done < limit {
		claimed := false
		err := db.InTx(ctx, s.db, func(tx pgx.Tx) error {
			var id string
			var attempts int
			err := tx.QueryRow(ctx, `
				SELECT post_id, cleanup_attempts FROM posts
				WHERE cleanup_status = 'pending'
				ORDER BY deleted_at
				LIMIT 1
				FOR UPDATE SKIP LOCKED`).Scan(&id, &attempts)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			claimed = true
			if gcsErr := s.gcs.DeleteFromGCS(id); gcsErr != nil {
				status := "pending"
				if attempts+1 >= 5 {
					status = "failed" // needs an operator; stays visible in the table
				}
				_, err = tx.Exec(ctx, `
					UPDATE posts SET cleanup_status = $2, cleanup_attempts = cleanup_attempts + 1, cleanup_error = $3
					WHERE post_id = $1`, id, status, gcsErr.Error())
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE posts SET cleanup_status = 'completed', cleanup_error = '' WHERE post_id = $1`, id)
			return err
		})
		if err != nil {
			return done, err
		}
		if !claimed {
			break
		}
		done++
	}
	return done, nil
}

// ──────────────────────────── likes and shares ────────────────────────────

// LikePost records one like per user and post.
//
// The (post_id, user_id) primary key is the de-duplication: concurrent double taps race on
// one INSERT ... ON CONFLICT DO NOTHING and exactly one inserts a row. The post.liked event
// is written to the outbox in the same transaction, so a notification is no longer dropped
// when Kafka is down. like_count goes through the write-behind counter, so a viral post costs
// one row update per flush instead of a row lock per like.
func (s *PostService) LikePost(ctx context.Context, postId, userId string) (bool, error) {
	if postId == "" || userId == "" {
		return false, nil
	}
	var rec outbox.Record
	err := db.InTx(ctx, s.db, func(tx pgx.Tx) error {
		owner, err := postOwner(ctx, tx, postId)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO post_likes (post_id, user_id) VALUES ($1, $2)
			ON CONFLICT (post_id, user_id) DO NOTHING`, postId, userId)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrAlreadyLiked
		}
		now := s.now()
		rec, err = outbox.Enqueue(ctx, tx, outbox.Event{
			AggregateType: "post", AggregateID: postId,
			Topic: model.TopicPostLiked, Key: postId,
			Payload: model.PostLikedEvent{PostId: postId, LikerId: userId, OwnerId: owner, CreatedAt: now.Unix()},
		}, now.Add(fastPathGrace))
		return err
	})
	if err != nil {
		return false, err
	}
	if err := s.bumpCounter(ctx, postId, "like_count", 1); err != nil {
		// The like is committed; the reconciler repairs the count from post_likes.
		log.Printf("like %s/%s stored, count not updated: %v", postId, userId, err)
	}
	s.publishAfterCommit(ctx, rec)
	return true, nil
}

// UnlikePost removes the caller's like. Returns ErrNotLiked if there was none.
func (s *PostService) UnlikePost(ctx context.Context, postId, userId string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM post_likes WHERE post_id = $1 AND user_id = $2`, postId, userId)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotLiked
	}
	if err := s.bumpCounter(ctx, postId, "like_count", -1); err != nil {
		log.Printf("unlike %s/%s stored, count not updated: %v", postId, userId, err)
	}
	return nil
}

func (s *PostService) SharePost(ctx context.Context, postId, userId, platform string) (bool, error) {
	if postId == "" || userId == "" {
		return false, nil
	}
	tag, err := s.db.Exec(ctx, `
		INSERT INTO post_shares (share_id, post_id, user_id, platform)
		SELECT $1, $2, $3, $4
		WHERE EXISTS (SELECT 1 FROM posts WHERE post_id = $2 AND deleted_at IS NULL)`,
		uuid.New().String(), postId, userId, platform)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, ErrPostNotFound
	}
	if err := s.bumpCounter(ctx, postId, "shared_count", 1); err != nil {
		log.Printf("share of %s stored, count not updated: %v", postId, err)
	}
	return true, nil
}

// bumpCounter records a counter change in Redis (flushed in batches); if Redis did not take
// it, it writes the row directly. Never both: that would count the change twice.
func (s *PostService) bumpCounter(ctx context.Context, postId, field string, n int64) error {
	err := s.Counters.Incr(ctx, postId, field, n)
	if errors.Is(err, counter.ErrNotRecorded) {
		return pgCounterSink{db: s.db}.Add(ctx, postId, field, n)
	}
	if err != nil {
		// Recorded in Redis but not scheduled; the next increment of this post flushes it.
		log.Printf("counter %s/%s: %v", postId, field, err)
	}
	return nil
}
