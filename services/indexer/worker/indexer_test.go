package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"socialai/shared/backend"
	"socialai/shared/db/dbtest"
	"socialai/shared/model"
	"socialai/shared/testutil"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const idx = "posts"

var ctx = context.Background()

func newIndexer(t *testing.T) (*Indexer, *pgxpool.Pool, *testutil.MockESBackend, *testutil.MockOpenAIBackend) {
	pool := dbtest.New(t)
	es := testutil.NewMockESBackend()
	ai := testutil.NewMockOpenAIBackend()
	return &Indexer{DB: pool, ES: es, OpenAI: ai, Index: idx, Logf: t.Logf}, pool, es, ai
}

func insertPost(t *testing.T, pool *pgxpool.Pool, id, msg string) {
	_, err := pool.Exec(ctx, `INSERT INTO posts (post_id, user_id, message, type) VALUES ($1, 'alice', $2, 'image')`, id, msg)
	require.NoError(t, err)
}

func deletePost(t *testing.T, pool *pgxpool.Pool, id string) {
	_, err := pool.Exec(ctx, `UPDATE posts SET deleted_at = now(), version = version + 1 WHERE post_id = $1`, id)
	require.NoError(t, err)
}

func created(id string) []byte {
	b, _ := json.Marshal(model.PostCreatedEvent{PostId: id, UserId: "alice"})
	return b
}

func deleted(id string) []byte {
	b, _ := json.Marshal(model.PostDeletedEvent{PostId: id, UserId: "alice"})
	return b
}

func doc(t *testing.T, es *testutil.MockESBackend, id string) backend.PostSearchDoc {
	var d backend.PostSearchDoc
	require.True(t, es.Doc(idx, id, &d), "document %s not indexed", id)
	return d
}

func TestCreatedPostIsIndexedWithItsEmbedding(t *testing.T) {
	x, pool, es, _ := newIndexer(t)
	insertPost(t, pool, "p1", "a red fox")

	require.NoError(t, x.Handle(ctx, created("p1")))
	d := doc(t, es, "p1")
	assert.Equal(t, "a red fox", d.Message)
	assert.False(t, d.Deleted)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, d.Embedding)
	assert.EqualValues(t, 2, es.Version(idx, "p1"), "storing the embedding bumped the row version")

	var stored []float32
	require.NoError(t, pool.QueryRow(ctx, `SELECT embedding FROM posts WHERE post_id = 'p1'`).Scan(&stored))
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, stored, "kept in PostgreSQL so a rebuild never pays for it again")
}

func TestRedeliveryIsANoOp(t *testing.T) {
	x, pool, es, ai := newIndexer(t)
	insertPost(t, pool, "p1", "a red fox")
	require.NoError(t, x.Handle(ctx, created("p1")))
	require.NoError(t, x.Handle(ctx, created("p1")))
	assert.EqualValues(t, 2, es.Version(idx, "p1"))
	assert.EqualValues(t, 1, ai.EmbeddingCalls, "the embedding is computed once")
}

func TestDeleteLeavesATombstone(t *testing.T) {
	x, pool, es, _ := newIndexer(t)
	insertPost(t, pool, "p1", "a red fox")
	require.NoError(t, x.Handle(ctx, created("p1")))
	deletePost(t, pool, "p1")

	require.NoError(t, x.Handle(ctx, deleted("p1")))
	d := doc(t, es, "p1")
	assert.True(t, d.Deleted)
	assert.Empty(t, d.Message)
	assert.Nil(t, d.Embedding)
}

// post.created arrives after post.deleted (relay retry, partition rebalance): the post must
// not come back into search.
func TestLateCreatedEventCannotResurrectADeletedPost(t *testing.T) {
	x, pool, es, _ := newIndexer(t)
	insertPost(t, pool, "p1", "a red fox")
	deletePost(t, pool, "p1")

	require.NoError(t, x.Handle(ctx, deleted("p1")))
	require.NoError(t, x.Handle(ctx, created("p1")))
	assert.True(t, doc(t, es, "p1").Deleted)
}

// Even an index write carrying an old state is refused once a newer version is in.
func TestStaleWriteIsRefusedByVersion(t *testing.T) {
	x, pool, es, _ := newIndexer(t)
	insertPost(t, pool, "p1", "a red fox")
	deletePost(t, pool, "p1")
	require.NoError(t, x.Sync(ctx, "p1")) // tombstone at v2

	applied, err := es.IndexVersioned(idx, "p1", backend.PostSearchDoc{PostId: "p1", Message: "a red fox"}, 1)
	require.NoError(t, err)
	assert.False(t, applied)
	assert.True(t, doc(t, es, "p1").Deleted)
}

func TestOpenAIOutageIndexesWithoutVectorAndRepairsLater(t *testing.T) {
	x, pool, es, ai := newIndexer(t)
	insertPost(t, pool, "p1", "a red fox")
	ai.EmbeddingErr = errors.New("openai: 503")

	require.NoError(t, x.Handle(ctx, created("p1")), "an OpenAI outage must not dead-letter the event")
	d := doc(t, es, "p1")
	assert.Equal(t, "a red fox", d.Message, "keyword search works meanwhile")
	assert.Nil(t, d.Embedding)

	ai.EmbeddingErr = nil
	n, err := x.RepairMissingEmbeddings(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.NotNil(t, doc(t, es, "p1").Embedding)

	n, err = x.RepairMissingEmbeddings(ctx, 100)
	require.NoError(t, err)
	assert.Zero(t, n, "nothing left to repair")
}

func TestSearchIndexFailureIsRetried(t *testing.T) {
	x, pool, es, _ := newIndexer(t)
	insertPost(t, pool, "p1", "a red fox")
	es.SaveErr = errors.New("es unavailable")
	assert.Error(t, x.Handle(ctx, created("p1")), "returned to the consumer, which retries")
}

func TestMissingPostAndBadEvent(t *testing.T) {
	x, _, _, _ := newIndexer(t)
	assert.NoError(t, x.Handle(ctx, created("ghost")))
	assert.Error(t, x.Handle(ctx, []byte("not json")))
	assert.Error(t, x.Handle(ctx, []byte(`{}`)))
}

func TestReindexRebuildsEverything(t *testing.T) {
	x, pool, es, _ := newIndexer(t)
	for i := 0; i < 23; i++ {
		insertPost(t, pool, fmt.Sprintf("p%02d", i), "caption")
	}
	deletePost(t, pool, "p05")
	x.Index = "posts_v2"

	var pages int
	n, err := x.Reindex(ctx, 10, func(int) { pages++ })
	require.NoError(t, err)
	assert.Equal(t, 23, n)
	assert.Equal(t, 3, pages)
	assert.Len(t, es.Docs["posts_v2"], 23)
	var d backend.PostSearchDoc
	require.True(t, es.Doc("posts_v2", "p05", &d))
	assert.True(t, d.Deleted)

	n, err = x.Reindex(ctx, 10, nil)
	require.NoError(t, err)
	assert.Equal(t, 23, n, "a second run is harmless: every write is refused as already indexed")
}
