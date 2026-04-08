package service

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"testing"

	"socialai/shared/model"
	"socialai/shared/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestPostService() (*PostService, *testutil.MockESBackend, *testutil.MockRedisBackend, *testutil.MockGCSBackend, *testutil.MockOpenAIBackend, *testutil.MockKafkaProducer) {
	es := testutil.NewMockESBackend()
	redis := testutil.NewMockRedisBackend()
	gcs := testutil.NewMockGCSBackend()
	openai := testutil.NewMockOpenAIBackend()
	kafka := testutil.NewMockKafkaProducer()
	svc := NewPostService(es, redis, gcs, openai, kafka)
	return svc, es, redis, gcs, openai, kafka
}

func fakeMultipartFile(content string) multipart.File {
	return &fakeFile{Reader: bytes.NewReader([]byte(content))}
}

type fakeFile struct {
	*bytes.Reader
}

func (f *fakeFile) Close() error                              { return nil }
func (f *fakeFile) ReadAt(p []byte, off int64) (int, error)   { return f.Reader.ReadAt(p, off) }
func (f *fakeFile) Seek(offset int64, whence int) (int64, error) {
	return f.Reader.Seek(offset, whence)
}

// ──────────────────────── SavePost ────────────────────────

func TestSavePost_Success(t *testing.T) {
	svc, es, _, gcs, openai, kafka := newTestPostService()
	openai.Embedding = []float32{0.1, 0.2, 0.3}

	post := &model.Post{UserId: "user1", Message: "hello world", Type: "image"}
	err := svc.SavePost(post, fakeMultipartFile("image-bytes"))

	require.NoError(t, err)
	assert.NotEmpty(t, post.PostId)
	assert.NotEmpty(t, post.Url)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, post.Embedding, "should auto-generate embedding")

	assert.Len(t, gcs.Files, 1, "file should be saved to GCS")
	assert.NotNil(t, es.Docs["post"][post.PostId], "post should be saved to ES")
	assert.Equal(t, 1, kafka.Count("post.created"), "should publish to Kafka")
}

func TestSavePost_GCSFails(t *testing.T) {
	svc, _, _, gcs, _, _ := newTestPostService()
	gcs.SaveErr = errors.New("gcs down")

	post := &model.Post{UserId: "user1", Message: "test"}
	err := svc.SavePost(post, fakeMultipartFile("data"))

	assert.ErrorContains(t, err, "failed to save to GCS")
}

func TestSavePost_ESFails_CompensatesGCS(t *testing.T) {
	svc, es, _, gcs, _, _ := newTestPostService()
	es.SaveErr = errors.New("es down")

	post := &model.Post{UserId: "user1", Message: "test"}
	err := svc.SavePost(post, fakeMultipartFile("data"))

	assert.ErrorContains(t, err, "failed to save to ES")
	assert.Empty(t, gcs.Files, "GCS file should be deleted as compensation")
}

func TestSavePost_EmbeddingFailure_StillSaves(t *testing.T) {
	svc, es, _, _, openai, _ := newTestPostService()
	openai.EmbeddingErr = errors.New("openai rate limit")

	post := &model.Post{UserId: "user1", Message: "test"}
	err := svc.SavePost(post, fakeMultipartFile("data"))

	require.NoError(t, err)
	assert.Nil(t, post.Embedding, "embedding should be nil on failure")
	assert.NotNil(t, es.Docs["post"][post.PostId], "post should still be saved")
}

// ──────────────────────── SearchPostByKeywords ────────────────────────

func TestSearchPostByKeywords_ReturnsResults(t *testing.T) {
	svc, es, _, _, _, _ := newTestPostService()
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "u1", Message: "hello"})

	posts, err := svc.SearchPostByKeywords("hello")
	require.NoError(t, err)
	assert.Len(t, posts, 1)
	assert.Equal(t, "p1", posts[0].PostId)
}

func TestSearchPostByKeywords_ExcludesDeleted(t *testing.T) {
	svc, es, _, _, _, _ := newTestPostService()
	es.SetDoc("post", "p1", model.Post{PostId: "p1", Deleted: true})

	posts, err := svc.SearchPostByKeywords("")
	require.NoError(t, err)
	assert.Empty(t, posts, "deleted posts should be filtered out")
}

// ──────────────────────── SearchPostByUserId ────────────────────────

func TestSearchPostByUserId_CacheMiss(t *testing.T) {
	svc, es, redis, _, _, _ := newTestPostService()
	redis.GetErr = errors.New("cache miss")
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "u1"})

	posts, err := svc.SearchPostByUserId("u1")
	require.NoError(t, err)
	assert.Len(t, posts, 1)
}

// ──────────────────────── LikePost ────────────────────────

func TestLikePost_Success(t *testing.T) {
	svc, es, _, _, _, kafka := newTestPostService()
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "owner1"})

	liked, err := svc.LikePost("p1", "liker1")
	require.NoError(t, err)
	assert.True(t, liked)
	assert.NotNil(t, es.Docs["like"]["p1_liker1"], "like should be saved")
	assert.Equal(t, 1, kafka.Count("post.liked"), "should publish liked event")
}

func TestLikePost_PostNotFound(t *testing.T) {
	svc, _, _, _, _, _ := newTestPostService()

	_, err := svc.LikePost("nonexistent", "user1")
	assert.ErrorIs(t, err, ErrPostNotFound)
}

func TestLikePost_AlreadyLiked_RedisPath(t *testing.T) {
	svc, es, redis, _, _, _ := newTestPostService()
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "owner1"})
	_ = redis.SAdd(nil, "like_set:p1", "liker1")

	_, err := svc.LikePost("p1", "liker1")
	assert.ErrorIs(t, err, ErrAlreadyLiked)
}

func TestLikePost_EmptyParams(t *testing.T) {
	svc, _, _, _, _, _ := newTestPostService()

	liked, err := svc.LikePost("", "user1")
	assert.NoError(t, err)
	assert.False(t, liked)
}

// ──────────────────────── DeletePost ────────────────────────

func TestDeletePost_Success(t *testing.T) {
	svc, es, _, _, _, _ := newTestPostService()
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "u1"})

	deleted, err := svc.DeletePost("p1", "u1")
	require.NoError(t, err)
	assert.True(t, deleted)
}

func TestDeletePost_NotFound(t *testing.T) {
	svc, _, _, _, _, _ := newTestPostService()

	_, err := svc.DeletePost("nonexistent", "u1")
	assert.ErrorIs(t, err, ErrPostNotFound)
}

// ──────────────────────── SemanticSearch ────────────────────────

func TestSemanticSearch_Success(t *testing.T) {
	svc, es, _, _, _, _ := newTestPostService()
	es.SetDoc("post", "p1", model.Post{PostId: "p1", Message: "AI news"})

	posts, err := svc.SemanticSearch(nil, "artificial intelligence", 10)
	require.NoError(t, err)
	assert.Len(t, posts, 1)
}

func TestSemanticSearch_EmbeddingFails(t *testing.T) {
	svc, _, _, _, openai, _ := newTestPostService()
	openai.EmbeddingErr = errors.New("openai down")

	_, err := svc.SemanticSearch(nil, "test", 10)
	assert.ErrorContains(t, err, "failed to generate query embedding")
}

// ──────────────────────── AddComment ────────────────────────

func TestAddComment_Success(t *testing.T) {
	svc, es, _, _, _, _ := newTestPostService()
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "u1"})

	commentId, err := svc.AddComment("p1", "", "u2", "great post!")
	require.NoError(t, err)
	assert.NotEmpty(t, commentId)
	assert.NotNil(t, es.Docs["comment"][commentId])
}

func TestAddComment_PostNotFound(t *testing.T) {
	svc, _, _, _, _, _ := newTestPostService()

	_, err := svc.AddComment("nonexistent", "", "u1", "comment")
	assert.ErrorIs(t, err, ErrPostNotFound)
}

func TestAddComment_MissingFields(t *testing.T) {
	svc, _, _, _, _, _ := newTestPostService()

	_, err := svc.AddComment("", "", "u1", "comment")
	assert.Error(t, err)
}

// ──────────────────────── helpers ────────────────────────

// Verify io.Reader interface compliance
var _ io.Reader = &fakeFile{}
var _ io.Seeker = &fakeFile{}
