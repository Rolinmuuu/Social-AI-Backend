package service

import (
	"bytes"
	"context"
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
	"socialai/shared/constants"
	"socialai/shared/db/dbtest"
	"socialai/shared/feedplan"
	"socialai/shared/model"
	"socialai/shared/testutil"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// env is a PostService on a fresh PostgreSQL schema, with in-memory doubles for the
// services that are not the system of record (search, cache, media, AI, broker).
type env struct {
	svc    *PostService
	db     *pgxpool.Pool
	es     *testutil.MockESBackend
	redis  *testutil.MockRedisBackend
	gcs    *testutil.MockGCSBackend
	openai *testutil.MockOpenAIBackend
	kafka  *testutil.MockKafkaProducer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		db:     dbtest.New(t),
		es:     testutil.NewMockESBackend(),
		redis:  testutil.NewMockRedisBackend(),
		gcs:    testutil.NewMockGCSBackend(),
		openai: testutil.NewMockOpenAIBackend(),
		kafka:  testutil.NewMockKafkaProducer(),
	}
	e.svc = NewPostService(e.db, e.es, e.redis, e.gcs, e.openai, e.kafka)
	return e
}

var ctx = context.Background()

// post inserts a post row directly.
func (e *env) post(t *testing.T, id, author string, createdAt int64) {
	t.Helper()
	_, err := e.db.Exec(ctx, `INSERT INTO posts (post_id, user_id, message, created_at) VALUES ($1, $2, $3, to_timestamp($4))`,
		id, author, "caption of "+id, createdAt)
	require.NoError(t, err)
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.db.QueryRow(ctx, sql, args...).Scan(&n))
	return n
}

// indexed puts a live document into the search index double.
func (e *env) indexed(id, author string) {
	e.es.SetDoc(constants.SEARCH_POST_ALIAS, id, backend.PostSearchDoc{PostId: id, UserId: author, Message: "caption of " + id})
}

func fakeMultipartFile(content string) multipart.File {
	return &fakeFile{Reader: bytes.NewReader([]byte(content))}
}

type fakeFile struct{ *bytes.Reader }

func (f *fakeFile) Close() error { return nil }

var _ io.Seeker = &fakeFile{}

// ──────────────────────── Create ────────────────────────

func TestSavePost_Success(t *testing.T) {
	e := newEnv(t)
	post := &model.Post{UserId: "user1", Message: "hello world", Type: "image"}
	require.NoError(t, e.svc.SavePost(ctx, post, fakeMultipartFile("image-bytes")))

	assert.NotEmpty(t, post.PostId)
	assert.NotEmpty(t, post.Url)
	assert.Equal(t, 1, e.gcs.Count(), "file saved to GCS")
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM posts WHERE post_id = $1 AND user_id = 'user1'`, post.PostId))
	assert.Equal(t, 1, e.kafka.Count(model.TopicPostCreated), "published inline after commit")
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM outbox WHERE aggregate_id = $1 AND status = 'published'`, post.PostId))
	assert.Zero(t, atomic.LoadInt64(&e.openai.EmbeddingCalls), "embeddings are computed by the indexer, not on the request path")
}

func TestSavePost_GCSFails(t *testing.T) {
	e := newEnv(t)
	e.gcs.SaveErr = errors.New("gcs down")
	err := e.svc.SavePost(ctx, &model.Post{UserId: "user1"}, fakeMultipartFile("data"))
	assert.ErrorContains(t, err, "failed to save to GCS")
	assert.Zero(t, e.count(t, `SELECT count(*) FROM posts`))
}

func TestSavePost_DatabaseDown_CompensatesGCS(t *testing.T) {
	e := newEnv(t)
	e.db.Close()
	err := e.svc.SavePost(ctx, &model.Post{UserId: "user1"}, fakeMultipartFile("data"))
	assert.ErrorContains(t, err, "failed to save post")
	assert.Zero(t, e.gcs.Count(), "the uploaded object is deleted as compensation")
}

// The heart of the outbox: if the event cannot be recorded, the post is not created either.
func TestSavePost_OutboxFailureRollsBackThePost(t *testing.T) {
	e := newEnv(t)
	_, err := e.db.Exec(ctx, `ALTER TABLE outbox ADD CONSTRAINT refuse_all CHECK (false) NOT VALID`)
	require.NoError(t, err)

	err = e.svc.SavePost(ctx, &model.Post{UserId: "user1"}, fakeMultipartFile("data"))
	require.Error(t, err)
	assert.Zero(t, e.count(t, `SELECT count(*) FROM posts`), "no post without its event")
	assert.Zero(t, e.gcs.Count())
	assert.Zero(t, e.kafka.Count(model.TopicPostCreated))
}

func TestSavePost_KafkaDown_PostSavedAndEventPublishedLater(t *testing.T) {
	e := newEnv(t)
	clock := time.Now()
	e.svc.now = func() time.Time { return clock }
	e.svc.Relay.Now = func() time.Time { return clock }
	e.kafka.PublishErr = errors.New("broker unavailable")

	post := &model.Post{UserId: "user1", Message: "hello"}
	require.NoError(t, e.svc.SavePost(ctx, post, fakeMultipartFile("img")), "a Kafka outage must not fail the upload")
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM posts`))
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM outbox WHERE status = 'pending' AND attempts = 1`))

	// Broker back; after the backoff the relay publishes the stored event.
	e.kafka.PublishErr = nil
	clock = clock.Add(time.Minute)
	res, err := e.svc.Relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Published)
	assert.Equal(t, 1, e.kafka.Count(model.TopicPostCreated))

	res, err = e.svc.Relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Published, "nothing left to publish")
}

func TestGenerateImage_PublishesPostCreated(t *testing.T) {
	e := newEnv(t)
	e.svc.download = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte("png"))), nil }

	post, err := e.svc.GenerateImageFromOpenAIAndSavePost(ctx, "u1", "a cat")
	require.NoError(t, err)
	assert.Equal(t, 1, e.kafka.Count(model.TopicPostCreated), "generated posts must reach followers too")
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM posts WHERE post_id = $1 AND message = 'a cat'`, post.PostId))
}

// ──────────────────────── Search ────────────────────────

// The index ranks, the database decides: a post deleted in PostgreSQL but still live in a
// lagging index is not returned, and counts come from the row, not the index.
func TestSearchPostByKeywords_HydratesFromTheDatabase(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "u1", 100)
	e.post(t, "p2", "u1", 200)
	e.post(t, "p3", "u1", 300)
	for _, id := range []string{"p1", "p2", "p3"} {
		e.indexed(id, "u1")
	}
	_, err := e.db.Exec(ctx, `UPDATE posts SET deleted_at = now() WHERE post_id = 'p2'`)
	require.NoError(t, err)
	_, err = e.db.Exec(ctx, `UPDATE posts SET like_count = 42 WHERE post_id = 'p3'`)
	require.NoError(t, err)

	posts, err := e.svc.SearchPostByKeywords(ctx, "caption")
	require.NoError(t, err)
	require.Len(t, posts, 2)
	assert.Equal(t, "p1", posts[0].PostId, "index order is kept")
	assert.Equal(t, "p3", posts[1].PostId)
	assert.EqualValues(t, 42, posts[1].LikeCount)
}

func TestSearchPostByKeywords_TombstonesAreNotMatched(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "u1", 100)
	e.es.SetDoc(constants.SEARCH_POST_ALIAS, "p1", backend.PostSearchDoc{PostId: "p1", Deleted: true})
	posts, err := e.svc.SearchPostByKeywords(ctx, "caption")
	require.NoError(t, err)
	assert.Empty(t, posts)
}

func TestSearchPostByKeywords_EmptyListsNewestFromTheDatabase(t *testing.T) {
	e := newEnv(t)
	e.post(t, "old", "u1", 100)
	e.post(t, "new", "u2", 200)
	e.post(t, "gone", "u2", 300)
	_, err := e.db.Exec(ctx, `UPDATE posts SET deleted_at = now() WHERE post_id = 'gone'`)
	require.NoError(t, err)

	posts, err := e.svc.SearchPostByKeywords(ctx, "")
	require.NoError(t, err)
	require.Len(t, posts, 2)
	assert.Equal(t, "new", posts[0].PostId)
	assert.Zero(t, atomic.LoadInt64(&e.es.Queries), "no search index needed to list recent posts")
}

func TestSearchPostByUserId(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "u1", 100)
	e.post(t, "p2", "u1", 200)
	e.post(t, "other", "u2", 300)

	posts, err := e.svc.SearchPostByUserId(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, posts, 2)
	assert.Equal(t, "p2", posts[0].PostId, "newest first")
}

func TestSearchPostByUserId_ConcurrentMissesShareOneQuery(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "u1", 100)
	var queries int64
	load := e.svc.userPosts
	e.svc.userPosts = func(ctx context.Context, id string) ([]model.Post, error) {
		atomic.AddInt64(&queries, 1)
		time.Sleep(50 * time.Millisecond)
		return load(ctx, id)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			posts, err := e.svc.SearchPostByUserId(ctx, "u1")
			assert.NoError(t, err)
			assert.Len(t, posts, 1)
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, atomic.LoadInt64(&queries), int64(2), "50 concurrent misses should cost ~1 query")
}

func TestSemanticSearch(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "u1", 100)
	e.indexed("p1", "u1")

	posts, err := e.svc.SemanticSearch(ctx, "artificial intelligence", 10)
	require.NoError(t, err)
	require.Len(t, posts, 1)
	assert.Equal(t, "p1", posts[0].PostId)

	e.openai.EmbeddingErr = errors.New("openai down")
	_, err = e.svc.SemanticSearch(ctx, "test", 10)
	assert.ErrorContains(t, err, "failed to generate query embedding")
}

// ──────────────────────── Likes ────────────────────────

func TestLikePost_Success(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "owner1", 100)

	liked, err := e.svc.LikePost(ctx, "p1", "liker1")
	require.NoError(t, err)
	assert.True(t, liked)
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM post_likes WHERE post_id = 'p1' AND user_id = 'liker1'`))
	assert.Equal(t, 1, e.kafka.Count(model.TopicPostLiked))

	_, err = e.svc.Counters.Flush(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, e.count(t, `SELECT like_count FROM posts WHERE post_id = 'p1'`))
}

func TestLikePost_PostNotFoundOrDeleted(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.LikePost(ctx, "nonexistent", "user1")
	assert.ErrorIs(t, err, ErrPostNotFound)

	e.post(t, "p1", "owner1", 100)
	_, err = e.db.Exec(ctx, `UPDATE posts SET deleted_at = now()`)
	require.NoError(t, err)
	_, err = e.svc.LikePost(ctx, "p1", "user1")
	assert.ErrorIs(t, err, ErrPostNotFound)

	liked, err := e.svc.LikePost(ctx, "", "user1")
	assert.NoError(t, err)
	assert.False(t, liked)
}

func TestLikePost_ConcurrentDoubleTapCountsOnce(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "owner1", 100)

	var wg sync.WaitGroup
	var ok, dup int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			liked, err := e.svc.LikePost(ctx, "p1", "liker1")
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
	assert.Equal(t, 1, e.kafka.Count(model.TopicPostLiked), "and exactly one notification event")

	n, err := e.svc.Counters.Flush(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, e.count(t, `SELECT like_count FROM posts WHERE post_id = 'p1'`))
}

// Before the move to PostgreSQL the post.liked notification was published after the write
// and dropped when Kafka was down. Now it is in the outbox, committed with the like.
func TestLikePost_KafkaDownNotificationIsDelayedNotLost(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "owner1", 100)
	clock := time.Now()
	e.svc.now = func() time.Time { return clock }
	e.svc.Relay.Now = func() time.Time { return clock }
	e.kafka.PublishErr = errors.New("broker unavailable")

	liked, err := e.svc.LikePost(ctx, "p1", "liker1")
	require.NoError(t, err)
	assert.True(t, liked)
	assert.Zero(t, e.kafka.Count(model.TopicPostLiked))

	e.kafka.PublishErr = nil
	clock = clock.Add(time.Minute)
	_, err = e.svc.Relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, e.kafka.Count(model.TopicPostLiked))
}

func TestUnlikePost(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "owner1", 100)
	_, err := e.svc.LikePost(ctx, "p1", "liker1")
	require.NoError(t, err)

	require.NoError(t, e.svc.UnlikePost(ctx, "p1", "liker1"))
	assert.ErrorIs(t, e.svc.UnlikePost(ctx, "p1", "liker1"), ErrNotLiked)
	_, err = e.svc.Counters.Flush(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 0, e.count(t, `SELECT like_count FROM posts WHERE post_id = 'p1'`), "+1 and -1 flushed as one net change")

	_, err = e.svc.LikePost(ctx, "p1", "liker1")
	assert.NoError(t, err, "can like again after unliking")
}

func TestSharePost(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "owner1", 100)
	shared, err := e.svc.SharePost(ctx, "p1", "u2", "twitter")
	require.NoError(t, err)
	assert.True(t, shared)
	_, err = e.svc.SharePost(ctx, "missing", "u2", "twitter")
	assert.ErrorIs(t, err, ErrPostNotFound)

	_, err = e.svc.Counters.Flush(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, e.count(t, `SELECT share_count FROM posts WHERE post_id = 'p1'`))
}

// ──────────────────────── Delete and cleanup ────────────────────────

func TestDeletePost_Success(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "u1", 100)

	deleted, err := e.svc.DeletePost(ctx, "p1", "u1")
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM posts WHERE deleted_at IS NOT NULL AND cleanup_status = 'pending' AND version = 2`))
	assert.Equal(t, 1, e.kafka.Count(model.TopicPostDeleted), "the search index is told in the same transaction")

	_, err = e.svc.DeletePost(ctx, "p1", "u1")
	assert.ErrorIs(t, err, ErrPostNotFound, "deleting twice is not found, and records no second event")
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM outbox WHERE topic = 'post.deleted'`))
}

func TestDeletePost_RejectsNonOwner(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "u1", 100)

	deleted, err := e.svc.DeletePost(ctx, "p1", "u2")
	assert.ErrorIs(t, err, ErrNotPostOwner)
	assert.False(t, deleted)
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM posts WHERE deleted_at IS NULL`))
	assert.Zero(t, e.count(t, `SELECT count(*) FROM outbox`))
}

func TestDeletePost_NotFound(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.DeletePost(ctx, "nonexistent", "u1")
	assert.ErrorIs(t, err, ErrPostNotFound)
}

func TestDeletePost_DoesNotOverwriteCounters(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "u1", 100)
	_, err := e.db.Exec(ctx, `UPDATE posts SET like_count = 7`)
	require.NoError(t, err)

	_, err = e.svc.DeletePost(ctx, "p1", "u1")
	require.NoError(t, err)
	assert.Equal(t, 7, e.count(t, `SELECT like_count FROM posts`))
}

func TestCleanupDeletedPosts(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"a", "b", "c"} {
		e.post(t, id, "u1", 100)
		e.gcs.Files[id] = []byte("x")
		_, err := e.svc.DeletePost(ctx, id, "u1")
		require.NoError(t, err)
	}

	n, err := e.svc.CleanupDeletedPosts(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "bounded by limit")
	n, err = e.svc.CleanupDeletedPosts(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Zero(t, e.gcs.Count())
	assert.Equal(t, 3, e.count(t, `SELECT count(*) FROM posts WHERE cleanup_status = 'completed'`))
}

func TestCleanupGivesUpAfterFiveFailures(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "u1", 100)
	_, err := e.svc.DeletePost(ctx, "p1", "u1")
	require.NoError(t, err)
	e.gcs.DeleteErr = errors.New("permission denied")

	for i := 0; i < 7; i++ {
		_, err := e.svc.CleanupDeletedPosts(ctx, 1)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM posts WHERE cleanup_status = 'failed' AND cleanup_attempts = 5 AND cleanup_error <> ''`))
}

// Several post-service instances clean up at once: every post is processed exactly once.
func TestConcurrentCleanupClaimsEachPostOnce(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("p%02d", i)
		e.post(t, id, "u1", 100)
		_, err := e.svc.DeletePost(ctx, id, "u1")
		require.NoError(t, err)
	}
	var total int64
	var wg sync.WaitGroup
	for w := 0; w < 5; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := e.svc.CleanupDeletedPosts(ctx, 100)
			assert.NoError(t, err)
			atomic.AddInt64(&total, int64(n))
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 30, total)
}

// ──────────────────────── Comments ────────────────────────

func TestComments(t *testing.T) {
	e := newEnv(t)
	e.post(t, "p1", "u1", 100)
	e.post(t, "p2", "u1", 100)

	top, err := e.svc.AddComment(ctx, "p1", "", "u2", "great post!")
	require.NoError(t, err)
	reply, err := e.svc.AddComment(ctx, "p1", top, "u3", "agreed")
	require.NoError(t, err)
	nested, err := e.svc.AddComment(ctx, "p1", reply, "u2", "thanks")
	require.NoError(t, err)

	var root string
	var depth int
	require.NoError(t, e.db.QueryRow(ctx, `SELECT root_comment_id, depth FROM comments WHERE comment_id = $1`, nested).Scan(&root, &depth))
	assert.Equal(t, top, root)
	assert.Equal(t, 2, depth)

	_, err = e.svc.AddComment(ctx, "p2", top, "u2", "wrong thread")
	assert.ErrorIs(t, err, ErrCommentNotFound, "a parent on another post is refused")
	_, err = e.svc.AddComment(ctx, "missing", "", "u2", "hi")
	assert.ErrorIs(t, err, ErrPostNotFound)
	_, err = e.svc.AddComment(ctx, "p1", "", "u2", "")
	assert.ErrorIs(t, err, ErrInvalidComment)

	page, err := e.svc.ListComments(ctx, "p1", 2, "")
	require.NoError(t, err)
	require.Len(t, page.Comments, 2)
	assert.Equal(t, top, page.Comments[0].CommentId, "oldest first")
	require.NotEmpty(t, page.NextCursor)
	next, err := e.svc.ListComments(ctx, "p1", 2, page.NextCursor)
	require.NoError(t, err)
	require.Len(t, next.Comments, 1)
	assert.Equal(t, nested, next.Comments[0].CommentId)
	assert.Equal(t, reply, next.Comments[0].ParentCommentId)
	assert.Empty(t, next.NextCursor)

	_, err = e.svc.ListComments(ctx, "missing", 10, "")
	assert.ErrorIs(t, err, ErrPostNotFound)
	_, err = e.svc.ListComments(ctx, "p1", 10, "%%%")
	assert.ErrorIs(t, err, ErrBadCursor)
}

// ──────────────────────── Home feed (push + pull) ────────────────────────

func follow(t *testing.T, e *env, follower, followee string) {
	_, err := e.db.Exec(ctx, `INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2)`, follower, followee)
	require.NoError(t, err)
}

func TestGetHomeFeed_MergesPushedAndCelebrityPosts(t *testing.T) {
	e := newEnv(t)

	// Pushed by feed-worker: p1 (t=100), p3 (t=300).
	e.post(t, "p1", "friend", 100)
	e.post(t, "p3", "friend", 300)
	require.NoError(t, e.redis.AddToFeeds(ctx, []string{"me"}, feedplan.Item{PostID: "p1", CreatedAt: 100}, 500, time.Hour))
	require.NoError(t, e.redis.AddToFeeds(ctx, []string{"me"}, feedplan.Item{PostID: "p3", CreatedAt: 300}, 500, time.Hour))

	// A celebrity I follow; their post p2 (t=200) is pulled at read time.
	e.post(t, "p2", "star", 200)
	follow(t, e, "me", "star")
	require.NoError(t, e.redis.SAdd(ctx, backend.CelebritySetKey, "star", "unfollowed-star"))
	e.post(t, "x", "unfollowed-star", 250)

	page, err := e.svc.GetHomeFeed(ctx, "me", 2, "")
	require.NoError(t, err)
	require.Len(t, page.Posts, 2)
	assert.Equal(t, "p3", page.Posts[0].PostId)
	assert.Equal(t, "p2", page.Posts[1].PostId)
	assert.NotEmpty(t, page.NextCursor)

	next, err := e.svc.GetHomeFeed(ctx, "me", 2, page.NextCursor)
	require.NoError(t, err)
	require.Len(t, next.Posts, 1)
	assert.Equal(t, "p1", next.Posts[0].PostId)
	assert.Empty(t, next.NextCursor)
}

// Paging through the whole feed must return every post exactly once, in order, even when
// many posts share a timestamp across the pushed and pulled parts, some posts were migrated
// without a creation time (epoch), and some pushed posts were deleted.
func TestGetHomeFeed_PagingNeverRepeatsOrSkips(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.redis.SAdd(ctx, backend.CelebritySetKey, "star"))
	follow(t, e, "me", "star")

	type row struct {
		id string
		t  int64
		ok bool
	}
	var all []row
	// 20 pushed posts over 5 timestamps (4 per second); every 7th is deleted after the push.
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("push-%02d", i)
		ts := int64(1000 + i/4)
		e.post(t, id, "friend", ts)
		require.NoError(t, e.redis.AddToFeeds(ctx, []string{"me"}, feedplan.Item{PostID: id, CreatedAt: ts}, 500, time.Hour))
		if i%7 == 3 {
			_, err := e.db.Exec(ctx, `UPDATE posts SET deleted_at = now() WHERE post_id = $1`, id)
			require.NoError(t, err)
		}
		all = append(all, row{id, ts, i%7 != 3})
	}
	// 12 pulled posts sharing the same 5 timestamps.
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("star-%02d", i)
		e.post(t, id, "star", int64(1000+i%5))
		all = append(all, row{id, int64(1000 + i%5), true})
	}
	// 3 celebrity posts migrated from Elasticsearch documents that had no created_at.
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("legacy-%d", i)
		e.post(t, id, "star", 0)
		all = append(all, row{id, 0, true})
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].t != all[j].t {
			return all[i].t > all[j].t
		}
		return all[i].id > all[j].id
	})
	var want []string
	for _, r := range all {
		if r.ok {
			want = append(want, r.id)
		}
	}

	var got []string
	cursor := ""
	for pages := 0; pages < 50; pages++ {
		page, err := e.svc.GetHomeFeed(ctx, "me", 4, cursor)
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
	e := newEnv(t)
	_, err := e.svc.GetHomeFeed(ctx, "me", 10, "%%%not-a-cursor")
	assert.ErrorIs(t, err, ErrBadCursor)
}

// ──────────────────────── Count reconciliation ────────────────────────

func TestReconcilerRepairsDriftedQuietPosts(t *testing.T) {
	e := newEnv(t)
	e.post(t, "drifted", "u1", 100)
	e.post(t, "busy", "u1", 100)
	e.post(t, "pending", "u1", 100)
	e.post(t, "fine", "u1", 100)
	// 3 likes on each, recorded long ago, except "busy" which was liked just now.
	for _, p := range []string{"drifted", "busy", "pending", "fine"} {
		for i := 0; i < 3; i++ {
			at := "now() - interval '1 hour'"
			if p == "busy" {
				at = "now()"
			}
			_, err := e.db.Exec(ctx, `INSERT INTO post_likes (post_id, user_id, created_at) VALUES ($1, $2, `+at+`)`, p, fmt.Sprintf("u%d", i))
			require.NoError(t, err)
		}
	}
	// A crash lost two flushed deltas of "drifted" and "busy"; "pending" has a delta in Redis.
	_, err := e.db.Exec(ctx, `UPDATE posts SET like_count = CASE post_id WHEN 'fine' THEN 3 WHEN 'pending' THEN 2 ELSE 1 END`)
	require.NoError(t, err)
	require.NoError(t, e.svc.Counters.Incr(ctx, "pending", "like_count", 1))

	r := e.svc.NewReconciler()
	r.Batch = 2 // forces the keyset walk over two passes
	var total ReconcileResult
	for i := 0; i < 2; i++ {
		res, err := r.RunOnce(ctx)
		require.NoError(t, err)
		total.Checked += res.Checked
		total.Fixed += res.Fixed
		total.Skipped += res.Skipped
	}
	assert.Equal(t, 4, total.Checked)
	assert.Equal(t, 1, total.Fixed)
	assert.Equal(t, 2, total.Skipped, "the busy post and the one with an unflushed delta")

	counts := map[string]int{}
	rows, err := e.db.Query(ctx, `SELECT post_id, like_count FROM posts`)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		var n int
		require.NoError(t, rows.Scan(&id, &n))
		counts[id] = n
	}
	assert.Equal(t, map[string]int{"drifted": 3, "busy": 1, "pending": 2, "fine": 3}, counts)

	// Once the delta is flushed, the pending post adds up without any repair.
	_, err = e.svc.Counters.Flush(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 3, e.count(t, `SELECT like_count FROM posts WHERE post_id = 'pending'`))
}
