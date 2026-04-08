package service

import (
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"time"

	"socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/kafka"
	"socialai/shared/model"
	"socialai/shared/utils"

	"github.com/google/uuid"
	"github.com/olivere/elastic/v7"
)

// PostService encapsulates all post-related business logic.
type PostService struct {
	es    backend.ElasticsearchBackendInterface
	redis backend.RedisBackendInterface
	gcs   backend.GoogleCloudStorageBackendInterface
	openai backend.OpenAIBackendInterface
	kafka kafka.KafkaProducerInterface
}

func NewPostService(
	es backend.ElasticsearchBackendInterface,
	redis backend.RedisBackendInterface,
	gcs backend.GoogleCloudStorageBackendInterface,
	openai backend.OpenAIBackendInterface,
	kafka kafka.KafkaProducerInterface
) *PostService {
	return &PostService{es: es, redis: redis, gcs: gcs, openai: openai, kafka: kafka}
}

func (s *PostService) SearchPostByUserId(userId string) ([]model.Post, error) {
	ctx := context.Background()
	cacheKey := utils.UserFeedCacheKey(userId)

	// Check Redis cache first.
	if cached, err := s.redis.Get(ctx, cacheKey); err == nil {
		var posts []model.Post
		if err := json.Unmarshal([]byte(cached), &posts); err == nil {
			fmt.Printf("cache hit for user feed: %s\n", userId)
			return posts, nil
		}
	}

	query := elastic.NewBoolQuery().
		Must(elastic.NewTermQuery("user_id", userId)).
		MustNot(elastic.NewTermQuery("deleted", true))

	searchResult, err := s.es.ReadFromES(query, constants.POST_INDEX)
	if err != nil {
		return nil, err
	}
	posts := getPostFromSearchResult(searchResult)

	if data, err := json.Marshal(posts); err == nil {
		_ = s.redis.Set(ctx, cacheKey, data, 10*time.Second)
	}
	return posts, nil
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

// SavePost persists a new post via Saga: GCS → ES → invalidate cache.
// If ES save fails, the GCS file is deleted as compensation.
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

	if err := s.es.SaveToES(post, constants.POST_INDEX, post.PostId); err != nil {
		// Compensating transaction: remove the orphan GCS file.
		if deleteErr := s.gcs.DeleteFromGCS(post.PostId); deleteErr != nil {
			fmt.Printf("CRITICAL: GCS orphan file, manual cleanup needed. post_id=%s es_err=%v gcs_err=%v\n",
				post.PostId, err, deleteErr)
		}
		return fmt.Errorf("failed to save to ES: %w", err)
	}

	ctx := context.Background()
	_ = s.redis.Delete(ctx, utils.UserFeedCacheKey(post.UserId))

	// Publish event to Kafka
	event := model.PostCreatedEvent{
		PostId: post.PostId,
		UserId: post.UserId,
		Message: post.Message,
		Url: post.Url,
		Type: post.Type,
		CreatedAt: time.Now().Unix(),
	}
	if err := s.kafka.Publish(ctx, "post.created", post.PostId, event); err != nil {
		return fmt.Errorf("WARN: failed to publish event to Kafka: %w", err)
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
	post.Deleted = true
	post.DeletedAt = time.Now().Unix()
	post.CleanupStatus = "pending"
	post.RetryCount = 0
	post.LastError = ""

	if err := s.es.SaveToES(&post, constants.POST_INDEX, post.PostId); err != nil {
		return false, err
	}

	ctx := context.Background()
	_ = s.redis.Delete(ctx, utils.UserFeedCacheKey(userId))
	return true, nil
}

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

	// Check Redis set for duplicate like (fast path).
	likeSetKey := fmt.Sprintf("like_set:%s", postId)
	if alreadyLiked, err := s.redis.SIsMember(ctx, likeSetKey, userId); err == nil && alreadyLiked {
		return false, ErrAlreadyLiked
	}

	// Check ES for duplicate (slow path, source of truth).
	likeId := postId + "_" + userId
	likeResult, err := s.es.ReadFromES(elastic.NewTermQuery("post_like_id", likeId), constants.LIKE_INDEX)
	if err != nil {
		return false, err
	}
	if likeResult.TotalHits() > 0 {
		_ = s.redis.SAdd(ctx, likeSetKey, userId)
		return false, ErrAlreadyLiked
	}

	like := model.PostLike{
		PostLikeId: likeId,
		UserId:     userId,
		PostId:     postId,
		CreatedAt:  time.Now().Unix(),
	}
	if err := s.es.SaveToES(&like, constants.LIKE_INDEX, like.PostLikeId); err != nil {
		return false, err
	}
	_ = post // post variable used to confirm existence; count updated via script
	if err := s.es.IncrementFieldInES(constants.POST_INDEX, postId, "like_count", 1); err != nil {
		return false, err
	}
	_ = s.redis.SAdd(ctx, likeSetKey, userId)

	// Publish event to Kafka
	event := model.PostLikedEvent{
		PostId: postId,
		LikerId: userId,
		OwnerId: post.UserId,
		CreatedAt: time.Now().Unix(),
	}
	if err := s.kafka.Publish(ctx, "post.liked", postId, event); err != nil {
		return false, fmt.Errorf("WARN: failed to publish event to Kafka: %w", err)
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
	if err := s.es.IncrementFieldInES(constants.POST_INDEX, postId, "shared_count", 1); err != nil {
		return false, err
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
		if err := s.gcs.DeleteFromGCS(post.PostId); err != nil {
			post.RetryCount++
			post.LastError = err.Error()
			if post.RetryCount >= 5 {
				post.CleanupStatus = "failed"
			} else {
				post.CleanupStatus = "pending"
			}
		} else {
			post.CleanupStatus = "completed"
			post.LastError = ""
		}
		if err := s.es.SaveToES(&post, constants.POST_INDEX, post.PostId); err != nil {
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

	body, err := backend.DownloadImage(imageUrl)
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

	if err := s.es.SaveToES(post, constants.POST_INDEX, post.PostId); err != nil {
		_ = s.gcs.DeleteFromGCS(post.PostId)
		return nil, fmt.Errorf("failed to save to ES: %w", err)
	}

	_ = s.redis.Delete(ctx, utils.UserFeedCacheKey(userId))
	return post, nil
}