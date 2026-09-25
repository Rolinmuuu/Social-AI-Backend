package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"socialai/shared/backend"
	"socialai/shared/feedplan"
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

func (f *fakeFile) Close() error                            { return nil }
func (f *fakeFile) ReadAt(p []byte, off int64) (int, error) { return f.Reader.ReadAt(p, off) }
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

func TestDeletePost_RejectsNonOwner(t *testing.T) {
	svc, es, _, _, _, _ := newTestPostService()
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "u1"})

	deleted, err := svc.DeletePost("p1", "u2")
	assert.ErrorIs(t, err, ErrNotPostOwner)
	assert.False(t, deleted)

	var stored model.Post
	require.NoError(t, json.Unmarshal(es.Docs["post"]["p1"], &stored))
	assert.False(t, stored.Deleted, "a non-owner must not be able to mark the post deleted")
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

// ──────────────────────── Outbox (dual-write) ────────────────────────

func TestSavePost_KafkaDown_PostSavedAndEventPublishedLater(t *testing.T) {
	svc, es, _, _, _, kafka := newTestPostService()
	clock := time.Unix(1_700_000_000, 0)
	svc.now = func() time.Time { return clock }
	svc.Relay.Now = func() time.Time { return clock }
	kafka.PublishErr = errors.New("broker unavailable")

	post := &model.Post{UserId: "user1", Message: "hello"}
	require.NoError(t, svc.SavePost(post, fakeMultipartFile("img")), "a Kafka outage must not fail the upload")

	var stored model.Post
	require.True(t, es.Doc("post", post.PostId, &stored))
	assert.Equal(t, model.OutboxPending, stored.OutboxStatus)
	assert.Equal(t, 1, stored.OutboxAttempts)
	assert.Equal(t, 0, kafka.Count("post.created"))

	// Broker back; after the backoff the relay publishes the stored event.
	kafka.PublishErr = nil
	clock = clock.Add(time.Minute)
	res, err := svc.Relay.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Published)
	assert.Equal(t, 1, kafka.Count("post.created"))
	require.True(t, es.Doc("post", post.PostId, &stored))
	assert.Equal(t, model.OutboxPublished, stored.OutboxStatus)

	// Nothing left to publish.
	res, err = svc.Relay.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, res.Published)
}

func TestGenerateImage_PublishesPostCreated(t *testing.T) {
	svc, es, _, _, _, kafka := newTestPostService()
	svc.download = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte("png"))), nil }

	post, err := svc.GenerateImageFromOpenAIAndSavePost(context.Background(), "u1", "a cat")
	require.NoError(t, err)
	assert.Equal(t, 1, kafka.Count("post.created"), "generated posts must reach followers too")
	var stored model.Post
	require.True(t, es.Doc("post", post.PostId, &stored))
	assert.Equal(t, model.OutboxPublished, stored.OutboxStatus)
}

// ──────────────────────── Likes: concurrency + write-behind ────────────────────────

func TestLikePost_ConcurrentDoubleTapCountsOnce(t *testing.T) {
	svc, es, _, _, _, _ := newTestPostService()
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "owner1"})

	var wg sync.WaitGroup
	var ok, dup int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			liked, err := svc.LikePost("p1", "liker1")
			switch {
			case err == nil && liked:
				atomic.AddInt32(&ok, 1)
			case errors.Is(err, ErrAlreadyLiked):
				atomic.AddInt32(&dup, 1)
			default:
				t.Errorf("unexpected: liked=%v err=%v", liked, err)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), ok, "exactly one like may succeed")
	assert.Equal(t, int32(19), dup)

	n, err := svc.Counters.Flush(context.Background(), 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, es.Increments["post/p1/like_count"], "like_count +1, applied in one flush")
}

func TestLikePost_DurableCheckWhenRedisWasFlushed(t *testing.T) {
	svc, es, redis, _, _, _ := newTestPostService()
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "owner1"})
	_, err := svc.LikePost("p1", "liker1")
	require.NoError(t, err)

	require.NoError(t, redis.SRem(context.Background(), "like_set:p1", "liker1")) // cache lost
	_, err = svc.LikePost("p1", "liker1")
	assert.ErrorIs(t, err, ErrAlreadyLiked, "op_type=create on the like document is the source of truth")
}

func TestLikePost_KafkaDownStillLikes(t *testing.T) {
	svc, es, _, _, _, kafka := newTestPostService()
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "owner1"})
	kafka.PublishErr = errors.New("broker unavailable")
	liked, err := svc.LikePost("p1", "liker1")
	require.NoError(t, err)
	assert.True(t, liked)
}

// ──────────────────────── Cache stampede ────────────────────────

func TestSearchPostByUserId_ConcurrentMissesShareOneQuery(t *testing.T) {
	svc, es, redis, _, _, _ := newTestPostService()
	redis.GetErr = errors.New("cache miss")
	es.ReadDelay = 50 * time.Millisecond
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "u1"})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			posts, err := svc.SearchPostByUserId("u1")
			assert.NoError(t, err)
			assert.Len(t, posts, 1)
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, atomic.LoadInt64(&es.Queries), int64(2), "50 concurrent misses should cost ~1 ES query")
}

// ──────────────────────── Home feed (push + pull) ────────────────────────

func TestGetHomeFeed_MergesPushedAndCelebrityPosts(t *testing.T) {
	svc, es, redis, _, _, _ := newTestPostService()
	ctx := context.Background()

	// Pushed by feed-worker: p1 (t=100), p3 (t=300).
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "friend", CreatedAt: 100})
	es.SetDoc("post", "p3", model.Post{PostId: "p3", UserId: "friend", CreatedAt: 300})
	require.NoError(t, redis.AddToFeeds(ctx, []string{"me"}, feedplan.Item{PostID: "p1", CreatedAt: 100}, 500, time.Hour))
	require.NoError(t, redis.AddToFeeds(ctx, []string{"me"}, feedplan.Item{PostID: "p3", CreatedAt: 300}, 500, time.Hour))

	// A celebrity I follow; their post p2 (t=200) is pulled at read time.
	es.SetDoc("post", "p2", model.Post{PostId: "p2", UserId: "star", CreatedAt: 200})
	es.SetDoc("follow", "f1", model.Follow{FollowId: "f1", FollowerId: "me", FolloweeId: "star"})
	require.NoError(t, redis.SAdd(ctx, backend.CelebritySetKey, "star"))

	page, err := svc.GetHomeFeed(ctx, "me", 2, "")
	require.NoError(t, err)
	require.Len(t, page.Posts, 2)
	assert.Equal(t, "p3", page.Posts[0].PostId)
	assert.Equal(t, "p2", page.Posts[1].PostId)
	assert.NotEmpty(t, page.NextCursor)

	next, err := svc.GetHomeFeed(ctx, "me", 2, page.NextCursor)
	require.NoError(t, err)
	require.Len(t, next.Posts, 1)
	assert.Equal(t, "p1", next.Posts[0].PostId)
	assert.Empty(t, next.NextCursor)
}

// Paging through the whole feed must return every post exactly once, in order, even when
// many posts share a timestamp across the pushed and pulled parts, some posts predate the
// created_at field, and some pushed posts were deleted.
func TestGetHomeFeed_PagingNeverRepeatsOrSkips(t *testing.T) {
	svc, es, redis, _, _, _ := newTestPostService()
	ctx := context.Background()
	require.NoError(t, redis.SAdd(ctx, backend.CelebritySetKey, "star"))
	es.SetDoc("follow", "f1", model.Follow{FollowId: "f1", FollowerId: "me", FolloweeId: "star"})

	var want []string
	// 20 pushed posts over 5 timestamps (4 per second).
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("push-%02d", i)
		ts := int64(1000 + i/4)
		es.SetDoc("post", id, model.Post{PostId: id, UserId: "friend", CreatedAt: ts})
		require.NoError(t, redis.AddToFeeds(ctx, []string{"me"}, feedplan.Item{PostID: id, CreatedAt: ts}, 500, time.Hour))
		if i%7 == 3 {
			es.SetDoc("post", id, model.Post{PostId: id, UserId: "friend", CreatedAt: ts, Deleted: true})
		}
	}
	// 12 pulled posts sharing the same 5 timestamps.
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("star-%02d", i)
		es.SetDoc("post", id, model.Post{PostId: id, UserId: "star", CreatedAt: int64(1000 + i%5)})
	}
	// 3 old celebrity posts written before created_at existed (no such field at all).
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("legacy-%d", i)
		es.SetDoc("post", id, map[string]interface{}{"post_id": id, "user_id": "star", "deleted": false})
	}

	// Expected order: (created_at desc, post_id desc), legacy last, deleted posts absent.
	type row struct {
		id string
		t  int64
		ok bool
	}
	var all []row
	for i := 0; i < 20; i++ {
		all = append(all, row{fmt.Sprintf("push-%02d", i), int64(1000 + i/4), i%7 != 3})
	}
	for i := 0; i < 12; i++ {
		all = append(all, row{fmt.Sprintf("star-%02d", i), int64(1000 + i%5), true})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].t != all[j].t {
			return all[i].t > all[j].t
		}
		return all[i].id > all[j].id
	})
	for _, r := range all {
		if r.ok {
			want = append(want, r.id)
		}
	}
	want = append(want, "legacy-2", "legacy-1", "legacy-0")

	var got []string
	cursor := ""
	for pages := 0; pages < 50; pages++ {
		page, err := svc.GetHomeFeed(ctx, "me", 4, cursor)
		require.NoError(t, err)
		for _, p := range page.Posts {
			got = append(got, p.PostId)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	assert.Equal(t, want, got)
}

func TestGetHomeFeed_BadCursorIsAClientError(t *testing.T) {
	svc, _, _, _, _, _ := newTestPostService()
	_, err := svc.GetHomeFeed(context.Background(), "me", 10, "%%%not-a-cursor")
	assert.ErrorIs(t, err, ErrBadCursor)
}

// ──────────────────────── Partial updates ────────────────────────

func TestDeletePost_DoesNotOverwriteConcurrentCounterUpdates(t *testing.T) {
	svc, es, _, _, _, _ := newTestPostService()
	es.SetDoc("post", "p1", model.Post{PostId: "p1", UserId: "u1", LikeCount: 7})

	_, err := svc.DeletePost("p1", "u1")
	require.NoError(t, err)

	var stored model.Post
	require.True(t, es.Doc("post", "p1", &stored))
	assert.True(t, stored.Deleted)
	assert.Equal(t, 7, stored.LikeCount, "delete is a partial update; it must not rewrite like_count")
}
