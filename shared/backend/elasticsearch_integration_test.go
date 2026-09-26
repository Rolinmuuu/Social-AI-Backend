//go:build integration

// Run against a real cluster: ES_URL=http://localhost:9200 go test -tags=integration ./shared/backend/
//
// These check the behaviour the in-memory double (testutil.MockESBackend) imitates, so a
// passing unit test means the same thing against Elasticsearch: external versions refuse
// stale writes, tombstones are filtered, the kNN request is accepted, the alias moves.
package backend

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"socialai/shared/constants"

	"github.com/olivere/elastic/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testES *ElasticsearchBackend

func TestMain(m *testing.M) {
	var err error
	testES, err = InitElasticsearchBackend()
	if err != nil {
		panic("failed to init ES for integration test: " + err.Error())
	}
	os.Exit(m.Run())
}

// tempIndex creates a post index with the production mapping, deleted after the test.
func tempIndex(t *testing.T) string {
	name := "it-posts-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	require.NoError(t, testES.CreateIndex(context.Background(), name, PostSearchMapping))
	t.Cleanup(func() { _, _ = testES.client.DeleteIndex(name).Do(context.Background()) })
	return name
}

func refresh(t *testing.T, index string) {
	_, err := testES.client.Refresh(index).Do(context.Background())
	require.NoError(t, err)
}

func TestExternalVersionRefusesStaleWrites(t *testing.T) {
	idx := tempIndex(t)
	applied, err := testES.IndexVersioned(idx, "p1", PostSearchDoc{PostId: "p1", Message: "v2"}, 2)
	require.NoError(t, err)
	assert.True(t, applied)

	applied, err = testES.IndexVersioned(idx, "p1", PostSearchDoc{PostId: "p1", Message: "v1"}, 1)
	require.NoError(t, err)
	assert.False(t, applied, "older version refused")
	applied, err = testES.IndexVersioned(idx, "p1", PostSearchDoc{PostId: "p1", Message: "v2 again"}, 2)
	require.NoError(t, err)
	assert.False(t, applied, "same version refused (redelivery)")

	res, err := testES.client.Get().Index(idx).Id("p1").Do(context.Background())
	require.NoError(t, err)
	assert.Contains(t, string(res.Source), `"v2"`)
}

func TestTombstonesAreNotSearchable(t *testing.T) {
	idx := tempIndex(t)
	_, err := testES.IndexVersioned(idx, "live", PostSearchDoc{PostId: "live", Message: "red fox"}, 1)
	require.NoError(t, err)
	_, err = testES.IndexVersioned(idx, "gone", PostSearchDoc{PostId: "gone", Message: "red fox"}, 1)
	require.NoError(t, err)
	_, err = testES.IndexVersioned(idx, "gone", PostSearchDoc{PostId: "gone", Deleted: true}, 2)
	require.NoError(t, err)
	refresh(t, idx)

	q := elastic.NewBoolQuery().Must(elastic.NewMatchQuery("message", "fox")).Filter(liveOnly())
	res, err := testES.ReadFromESWithSize(q, idx, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"live"}, hitIDs(res))
}

func TestKNNSearchRequestIsAccepted(t *testing.T) {
	idx := tempIndex(t)
	vec := make([]float32, 1536)
	vec[0] = 1
	_, err := testES.IndexVersioned(idx, "p1", PostSearchDoc{PostId: "p1", Embedding: vec}, 1)
	require.NoError(t, err)
	refresh(t, idx)

	res, err := testES.KNNSearchFromES(idx, "embedding", vec, 5, liveOnly())
	require.NoError(t, err)
	assert.Equal(t, []string{"p1"}, hitIDs(res))
}

func TestPointAliasMovesAtomically(t *testing.T) {
	a, b := tempIndex(t), tempIndex(t)
	alias := "it-alias-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ctx := context.Background()
	require.NoError(t, testES.PointAlias(ctx, alias, a))
	require.NoError(t, testES.PointAlias(ctx, alias, b))

	res, err := testES.client.Aliases().Alias(alias).Do(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{b}, res.IndicesByAlias(alias))
}

// post-service and search-indexer both run EnsureSearchIndex when they start, at the same
// moment in docker compose. Every caller must succeed and the alias must end up on the index.
func TestEnsureSearchIndexConcurrentStartup(t *testing.T) {
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	alias, index := "it-ensure-alias-"+suffix, "it-ensure-index-"+suffix
	ctx := context.Background()
	t.Cleanup(func() { _, _ = testES.client.DeleteIndex(index).Do(ctx) })

	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = testES.EnsureSearchIndex(ctx, alias, index)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		assert.NoError(t, err, "caller %d", i)
	}

	res, err := testES.client.Aliases().Alias(alias).Do(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{index}, res.IndicesByAlias(alias))
	require.NoError(t, testES.EnsureSearchIndex(ctx, alias, index), "second start is a no-op")
}

// The production read path: SearchPostIDs and NearestPostIDs query the "posts" alias that
// InitElasticsearchBackend created with the strict production mapping.
func TestSearchThroughProductionAlias(t *testing.T) {
	ctx := context.Background()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	live, gone := "it-live-"+suffix, "it-gone-"+suffix
	word := "zebra" + suffix // unique, so other tests' documents cannot match
	vec := make([]float32, 1536)
	vec[1] = 1
	t.Cleanup(func() {
		for _, id := range []string{live, gone} {
			_, _ = testES.client.Delete().Index(constants.SEARCH_POST_INDEX).Id(id).Do(ctx)
		}
	})

	for _, id := range []string{live, gone} {
		_, err := testES.IndexVersioned(constants.SEARCH_POST_ALIAS, id,
			PostSearchDoc{PostId: id, UserId: "u1", Message: "a " + word + " at noon", Type: "image", CreatedAt: time.Now().Unix(), Embedding: vec}, 1)
		require.NoError(t, err)
	}
	_, err := testES.IndexVersioned(constants.SEARCH_POST_ALIAS, gone, PostSearchDoc{PostId: gone, Deleted: true}, 2)
	require.NoError(t, err)
	refresh(t, constants.SEARCH_POST_ALIAS)

	ids, err := SearchPostIDs(testES, word+" noon", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{live}, ids)

	ids, err = NearestPostIDs(testES, vec, 50)
	require.NoError(t, err)
	assert.Contains(t, ids, live)
	assert.NotContains(t, ids, gone)
}
