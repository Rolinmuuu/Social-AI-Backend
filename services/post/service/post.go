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
	"socialai/shared/constants"
	"socialai/shared/counter"
	"socialai/shared/feedplan"
	"socialai/shared/idempotency"
	"socialai/shared/kafka"
	"socialai/shared/model"
	"socialai/shared/outbox"
	"socialai/shared/utils"

	"github.com/google/uuid"
	"github.com/olivere/elastic/v7"
)

// PostService encapsulates all post-related business logic.
type PostService struct {
	es     backend.ElasticsearchBackendInterface
	redis  backend.RedisBackendInterface
	gcs    backend.GoogleCloudStorageBackendInterface
	openai backend.OpenAIBackendInterface
	kafka  kafka.KafkaProducerInterface

	// Outbox relay for post.created (see shared/outbox and outbox_store.go).
	Relay *outbox.Relay
	// Write-behind like/share counters (see shared/counter).
	Counters *counter.Counter
	// Idempotency-Key store for POST /upload and image generation.
	Idempotency *idempotency.Store
	// Home feed policy (push/pull threshold, feed length).
	FeedPolicy feedplan.Policy

	loads    cache.Group // single-flight for cache misses
	now      func() time.Time
	download func(url string) (io.ReadCloser, error)
}

func NewPostService(
	es backend.ElasticsearchBackendInterface,
	redis backend.RedisBackendInterface,
	gcs backend.GoogleCloudStorageBackendInterface,
	openai backend.OpenAIBackendInterface,
	kafka kafka.KafkaProducerInterface,
) *PostService {
	s := &PostService{es: es, redis: redis, gcs: gcs, openai: openai, kafka: kafka, now: time.Now, download: backend.DownloadImage}
	s.Relay = &outbox.Relay{Store: &esOutboxStore{es: es}, Publisher: kafka}
	s.Counters = &counter.Counter{Store: redis, Sink: es, Index: constants.POST_INDEX}
	s.Idempotency = &idempotency.Store{KV: redis}
	return s
}

const userPostsTTL = 10 * time.Second

// SearchPostByUserId is a read-through cache over ES. Misses for the same user are
// single-flighted (one ES query however many requests miss at once) and the TTL is
// jittered so entries written together do not expire together.
func (s *PostService) SearchPostByUserId(userId string) ([]model.Post, error) {
	ctx := context.Background()
	cacheKey := utils.UserFeedCacheKey(userId)

	if cached, err := s.redis.Get(ctx, cacheKey); err == nil {
		var posts []model.Post
		if err := json.Unmarshal([]byte(cached), &posts); err == nil {
			return posts, nil
		}
	}

	v, err, _ := s.loads.Do(cacheKey, func() (interface{}, error) {
		query := elastic.NewBoolQuery().
			Must(elastic.NewTermQuery("user_id", userId)).
			MustNot(elastic.NewTermQuery("deleted", true))
		searchResult, err := s.es.ReadFromES(query, constants.POST_INDEX)
		if err != nil {
			return nil, err
		}
		posts := getPostFromSearchResult(searchResult)
		if data, err := json.Marshal(posts); err == nil {
			_ = s.redis.Set(ctx, cacheKey, data, cache.JitterTTL(userPostsTTL, 0.2))
		}
		return posts, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]model.Post), nil
}

func (s *PostService) SearchPostByKeywords(keywords string) ([]model.Post, error) {
	baseQuery := elastic.NewMatchQuery("message", keywords).Operator("AND")
	if keywords == "" {
		baseQuery.ZeroTermsQuery("all")
	}
	query := elastic.NewBoolQuery().
		Must(baseQuery).
		MustNot(elastic.NewTermQuery("deleted", true))

	searchResult, err := s.es.ReadFromES(query, constants.POST_INDEX)
	if err != nil {
		return nil, err
	}
	return getPostFromSearchResult(searchResult), nil
}

func (s *PostService) SemanticSearch(ctx context.Context, queryText string, topK int) ([]model.Post, error) {
	queryVector, err := s.openai.GetEmbedding(ctx, queryText)
	if err != nil {
		return nil, fmt.Errorf("failed to generate query embedding: %w", err)
	}

	searchResult, err := s.es.KNNSearchFromES(constants.POST_INDEX, "embedding", queryVector, topK)
	if err != nil {
		return nil, err
	}
	return getPostFromSearchResult(searchResult), nil
}

// SavePost persists a new post: GCS upload, then one ES write that stores the post *and*
// its post.created event (outbox). If the ES write fails, the GCS object is deleted
// (compensation). Publishing to Kafka is attempted right away; if Kafka is unavailable the
// request still succeeds and the outbox relay publishes later.
func (s *PostService) SavePost(post *model.Post, file multipart.File) error {
	post.PostId = uuid.New().String()

	medialink, err := s.gcs.SaveToGCS(file, post.PostId)
	if err != nil {
		return fmt.Errorf("failed to save to GCS: %w", err)
	}

	post.Url = medialink
	post.Deleted = false
	post.DeletedAt = 0
	post.CleanupStatus = ""
	post.RetryCount = 0
	post.LastError = ""

	if post.Message != "" && s.openai != nil {
		if emb, err := s.openai.GetEmbedding(context.Background(), post.Message); err == nil {
			post.Embedding = emb
		} else {
			fmt.Printf("WARNING: embedding generation failed for post %s: %v\n", post.PostId, err)
		}
	}

	return s.persistNewPost(context.Background(), post)
}

// persistNewPost is the shared tail of upload and image generation.
func (s *PostService) persistNewPost(ctx context.Context, post *model.Post) error {
	post.CreatedAt = s.now().Unix()
	post.OutboxStatus = model.OutboxPending
	post.OutboxAttempts = 0
	// The inline publish below normally succeeds within milliseconds; the relay only picks
	// the record up if it is still pending 10s later, so the two rarely publish the same post.
	post.OutboxNextAt = post.CreatedAt + 10

	if err := s.es.SaveToES(post, constants.POST_INDEX, post.PostId); err != nil {
		// Compensating action: remove the orphan GCS file.
		if deleteErr := s.gcs.DeleteFromGCS(post.PostId); deleteErr != nil {
			fmt.Printf("CRITICAL: GCS orphan file, manual cleanup needed. post_id=%s es_err=%v gcs_err=%v\n",
				post.PostId, err, deleteErr)
		}
		return fmt.Errorf("failed to save to ES: %w", err)
	}

	_ = s.redis.Delete(ctx, utils.UserFeedCacheKey(post.UserId))

	// Fast path; on failure the event stays pending and the relay retries it.
	if err := s.Relay.PublishNow(ctx, recordFor(*post)); err != nil {
		log.Printf("post %s saved; post.created deferred to the outbox relay: %v", post.PostId, err)
	} else {
		post.OutboxStatus = model.OutboxPublished
	}
	return nil
}

func (s *PostService) DeletePost(postId, userId string) (bool, error) {
	if postId == "" || userId == "" {
		return false, nil
	}

	query := elastic.NewBoolQuery().
		Must(elastic.NewTermQuery("post_id", postId)).
		MustNot(elastic.NewTermQuery("deleted", true))
	searchResult, err := s.es.ReadFromES(query, constants.POST_INDEX)
	if err != nil {
		return false, err
	}
	posts := getPostFromSearchResult(searchResult)
	if len(posts) == 0 {
		return false, ErrPostNotFound
	}

	post := posts[0]
	// Only the author may delete a post. Previously any authenticated user
	// could delete any post by id.
	if post.UserId != userId {
		return false, ErrNotPostOwner
	}
	// Partial update: rewriting the whole document read above could overwrite a like or
	// share count that changed in between (lost update).
	if err := s.es.UpdateFieldsInES(constants.POST_INDEX, post.PostId, map[string]interface{}{
		"deleted":        true,
		"deleted_at":     time.Now().Unix(),
		"cleanup_status": "pending",
		"retry_count":    0,
		"last_error":     "",
	}); err != nil {
		return false, err
	}

	ctx := context.Background()
	_ = s.redis.Delete(ctx, utils.UserFeedCacheKey(post.UserId))
	return true, nil
}

// LikePost records one like per user and post.
//
// Concurrency: two requests from the same user (double tap, client retry) used to both pass
// the "already liked?" check and both increment like_count. Now the check and the write are
// one atomic step: SADD returns 1 only for the first caller, and the like document is created
// with op_type=create, which Elasticsearch rejects if it already exists (the durable check
// when Redis has been flushed). The count goes through the write-behind counter, so a hot
// post costs one ES update per flush instead of one per like.
func (s *PostService) LikePost(postId, userId string) (bool, error) {
	if postId == "" || userId == "" {
		return false, nil
	}
	ctx := context.Background()

	query := elastic.NewBoolQuery().
		Must(elastic.NewTermQuery("post_id", postId)).
		MustNot(elastic.NewTermQuery("deleted", true))
	searchResult, err := s.es.ReadFromES(query, constants.POST_INDEX)
	if err != nil {
		return false, err
	}
	posts := getPostFromSearchResult(searchResult)
	if len(posts) == 0 {
		return false, ErrPostNotFound
	}
	post := posts[0]

	likeSetKey := fmt.Sprintf("like_set:%s", postId)
	added, redisErr := s.redis.SAddCount(ctx, likeSetKey, userId)
	if redisErr == nil && added == 0 {
		return false, ErrAlreadyLiked
	}

	likeId := postId + "_" + userId
	like := model.PostLike{
		PostLikeId: likeId,
		UserId:     userId,
		PostId:     postId,
		CreatedAt:  time.Now().Unix(),
	}
	created, err := s.es.CreateInES(&like, constants.LIKE_INDEX, like.PostLikeId)
	if err != nil {
		if redisErr == nil {
			_ = s.redis.SRem(ctx, likeSetKey, userId) // let the user retry
		}
		return false, err
	}
	if !created {
		return false, ErrAlreadyLiked
	}

	if err := s.Counters.Incr(ctx, postId, "like_count", 1); err != nil {
		if !errors.Is(err, counter.ErrNotRecorded) {
			log.Printf("like counter: %v", err) // recorded; it will still be flushed
		} else if err := s.es.IncrementFieldInES(constants.POST_INDEX, postId, "like_count", 1); err != nil {
			// Redis unavailable: fall back to the direct (slower) ES increment.
			return false, err
		}
	}

	// Notifications are best-effort: the like is already durable, so a Kafka hiccup must not
	// turn it into an error the client would retry.
	event := model.PostLikedEvent{
		PostId:    postId,
		LikerId:   userId,
		OwnerId:   post.UserId,
		CreatedAt: time.Now().Unix(),
	}
	if err := s.kafka.Publish(ctx, model.TopicPostLiked, postId, event); err != nil {
		log.Printf("like %s stored; post.liked notification dropped: %v", likeId, err)
	}
	return true, nil
}

func (s *PostService) SharePost(postId, userId, platform string) (bool, error) {
	if postId == "" || userId == "" {
		return false, nil
	}

	query := elastic.NewBoolQuery().
		Must(elastic.NewTermQuery("post_id", postId)).
		MustNot(elastic.NewTermQuery("deleted", true))
	searchResult, err := s.es.ReadFromES(query, constants.POST_INDEX)
	if err != nil {
		return false, err
	}
	if len(getPostFromSearchResult(searchResult)) == 0 {
		return false, ErrPostNotFound
	}

	shareId := fmt.Sprintf("%s_%s_%s_%d", postId, userId, platform, time.Now().Unix())
	share := model.PostShare{
		PostShareId: shareId,
		UserId:      userId,
		PostId:      postId,
		CreatedAt:   time.Now().Unix(),
		Platform:    platform,
	}
	if err := s.es.SaveToES(&share, constants.SHARE_INDEX, share.PostShareId); err != nil {
		return false, err
	}
	if err := s.Counters.Incr(context.Background(), postId, "shared_count", 1); err != nil {
		if !errors.Is(err, counter.ErrNotRecorded) {
			log.Printf("share counter: %v", err)
		} else if err := s.es.IncrementFieldInES(constants.POST_INDEX, postId, "shared_count", 1); err != nil {
			return false, err
		}
	}
	return true, nil
}

// CleanupDeletedPosts processes up to `limit` posts marked for cleanup.
func (s *PostService) CleanupDeletedPosts(limit int) (bool, error) {
	query := elastic.NewBoolQuery().Must(
		elastic.NewTermQuery("deleted", true),
		elastic.NewTermQuery("cleanup_status", "pending"),
	)
	searchResult, err := s.es.ReadFromES(query, constants.POST_INDEX)
	if err != nil {
		return false, err
	}
	posts := getDeletedPostFromSearchResult(searchResult)
	if len(posts) == 0 {
		return false, nil
	}
	if limit <= 0 || limit > len(posts) {
		limit = len(posts)
	}
	for i := 0; i < limit; i++ {
		post := posts[i]
		fields := map[string]interface{}{"cleanup_status": "completed", "last_error": ""}
		if err := s.gcs.DeleteFromGCS(post.PostId); err != nil {
			retries := post.RetryCount + 1
			status := "pending"
			if retries >= 5 {
				status = "failed"
			}
			fields = map[string]interface{}{"cleanup_status": status, "retry_count": retries, "last_error": err.Error()}
		}
		if err := s.es.UpdateFieldsInES(constants.POST_INDEX, post.PostId, fields); err != nil {
			return false, err
		}
	}
	return true, nil
}

// AddComment adds a comment (or reply) to a post.
func (s *PostService) AddComment(postId, parentCommentId, userId, content string) (string, error) {
	if postId == "" || userId == "" || content == "" {
		return "", fmt.Errorf("postId, userId, and content are required")
	}

	// Verify the post exists.
	postQuery := elastic.NewBoolQuery().
		Must(elastic.NewTermQuery("post_id", postId)).
		MustNot(elastic.NewTermQuery("deleted", true))
	postResult, err := s.es.ReadFromES(postQuery, constants.POST_INDEX)
	if err != nil {
		return "", err
	}
	if len(getPostFromSearchResult(postResult)) == 0 {
		return "", ErrPostNotFound
	}

	commentId := uuid.New().String()
	now := time.Now().Unix()
	rootCommentId := commentId
	depth := 0

	if parentCommentId != "" {
		parentQuery := elastic.NewBoolQuery().
			Must(elastic.NewTermQuery("comment_id", parentCommentId)).
			MustNot(elastic.NewTermQuery("deleted", true))
		parentResult, err := s.es.ReadFromES(parentQuery, constants.COMMENT_INDEX)
		if err != nil {
			return "", err
		}
		parents := getCommentFromSearchResult(parentResult)
		if len(parents) == 0 {
			return "", ErrCommentNotFound
		}
		parent := parents[0]
		if parent.PostId != postId {
			return "", fmt.Errorf("parent comment does not belong to this post")
		}
		rootCommentId = parent.RootCommentId
		if rootCommentId == "" {
			rootCommentId = parent.CommentId
		}
		depth = parent.Depth + 1
	}

	comment := model.Comment{
		CommentId:       commentId,
		ParentCommentId: parentCommentId,
		RootCommentId:   rootCommentId,
		UserId:          userId,
		PostId:          postId,
		Depth:           depth,
		Content:         content,
		CreatedAt:       now,
		Deleted:         false,
		DeletedAt:       0,
	}
	if err := s.es.SaveToES(comment, constants.COMMENT_INDEX, comment.CommentId); err != nil {
		return "", err
	}
	return commentId, nil
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

	if embedding, err := s.openai.GetEmbedding(ctx, prompt); err == nil {
		post.Embedding = embedding
	} else {
		fmt.Printf("WARNING: embedding generation failed, post saved without vector: %v\n", err)
	}

	// Same path as uploads, so generated posts also reach followers' feeds (they were never
	// published to Kafka before).
	if err := s.persistNewPost(ctx, post); err != nil {
		return nil, err
	}
	return post, nil
}
