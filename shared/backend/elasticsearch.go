package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"socialai/shared/constants"

	"github.com/olivere/elastic/v7"
)

// PostSearchMapping is the mapping of the post search index. The index holds only what
// search needs; everything shown to a client is read from PostgreSQL.
//
// A mapping change means a new physical index: create posts_vN+1 with the new mapping, fill
// it from PostgreSQL (cmd/reindex -new-index), then move the "posts" alias in one atomic
// call. Readers and the indexer only ever use the alias.
const PostSearchMapping = `{
	"mappings": {
		"dynamic": "strict",
		"properties": {
			"post_id":    { "type": "keyword" },
			"user_id":    { "type": "keyword" },
			"message":    { "type": "text" },
			"type":       { "type": "keyword" },
			"created_at": { "type": "long" },
			"deleted":    { "type": "boolean" },
			"embedding":  { "type": "dense_vector", "dims": 1536, "index": true, "similarity": "cosine" }
		}
	}
}`

type ElasticsearchBackend struct {
	client *elastic.Client
}

// InitElasticsearchBackend connects and makes sure the post search index and its alias
// exist. It no longer creates the legacy per-entity indices (user, follow, like, ...): that
// data lives in PostgreSQL. cmd/backfill still reads them once, during migration.
func InitElasticsearchBackend() (*ElasticsearchBackend, error) {
	client, err := elastic.NewClient(
		elastic.SetURL(constants.ES_URL),
		elastic.SetBasicAuth(constants.ES_USERNAME, constants.ES_PASSWORD),
		elastic.SetSniff(false),
		// In docker compose the cluster takes a while to accept requests; without this the
		// client gives up after 5 s and the service exits at startup.
		elastic.SetHealthcheckTimeoutStartup(90*time.Second),
	)
	if err != nil {
		return nil, err
	}
	b := &ElasticsearchBackend{client: client}
	if err := b.EnsureSearchIndex(context.Background(), constants.SEARCH_POST_ALIAS, constants.SEARCH_POST_INDEX); err != nil {
		return nil, err
	}
	return b, nil
}

// EnsureSearchIndex creates index with PostSearchMapping and points alias at it, unless the
// alias already exists.
//
// post-service and search-indexer both call it at startup, at the same moment in docker
// compose. Both can see "no alias" and try to create the index; the loser gets
// resource_already_exists_exception, which is success for it (the index it wanted is there),
// and adding the alias again is idempotent.
func (b *ElasticsearchBackend) EnsureSearchIndex(ctx context.Context, alias, index string) error {
	exists, err := b.client.IndexExists(alias).Do(ctx)
	if err != nil {
		return fmt.Errorf("check alias %s: %w", alias, err)
	}
	if exists {
		return nil
	}
	if err := b.CreateIndex(ctx, index, PostSearchMapping); err != nil && !isAlreadyExists(err) {
		return err
	}
	return b.PointAlias(ctx, alias, index)
}

func isAlreadyExists(err error) bool {
	var e *elastic.Error
	return errors.As(err, &e) && e.Details != nil && e.Details.Type == "resource_already_exists_exception"
}

// withoutVectors keeps the 1536-float embedding out of search responses: it is only needed
// inside Elasticsearch for kNN, and shipping it back added roughly 15-20 KB of JSON per hit.
func withoutVectors() *elastic.FetchSourceContext {
	return elastic.NewFetchSourceContext(true).Exclude("embedding")
}

func (b *ElasticsearchBackend) ReadFromESWithSize(query elastic.Query, index string, size int) (*elastic.SearchResult, error) {
	return b.client.Search().
		Index(index).
		Query(query).
		Size(size).
		FetchSourceContext(withoutVectors()).
		Do(context.Background())
}

// KNNSearchFromES runs an approximate nearest-neighbour search (top-level "knn" section of
// the search API) restricted by filter.
func (b *ElasticsearchBackend) KNNSearchFromES(index, field string, vector []float32, k int, filter elastic.Query) (*elastic.SearchResult, error) {
	knn := map[string]interface{}{
		"field":          field,
		"query_vector":   vector,
		"k":              k,
		"num_candidates": k * 10,
	}
	if filter != nil {
		src, err := filter.Source()
		if err != nil {
			return nil, err
		}
		knn["filter"] = src
	}
	body := map[string]interface{}{
		"knn":     knn,
		"size":    k,
		"_source": map[string]interface{}{"excludes": []string{"embedding"}},
	}
	return b.client.Search().Index(index).Source(body).Do(context.Background())
}

// IndexVersioned writes doc under id with version_type=external. Elasticsearch keeps the
// document only if version is higher than the one it has, so replays, duplicates and
// out-of-order events can never move the index backwards. applied=false with a nil error
// means the index already had this version or a newer one (not an error for the caller).
func (b *ElasticsearchBackend) IndexVersioned(index, id string, doc interface{}, version int64) (bool, error) {
	_, err := b.client.Index().
		Index(index).
		Id(id).
		VersionType("external").
		Version(version).
		BodyJson(doc).
		Do(context.Background())
	if elastic.IsConflict(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Scan calls fn with the id and source of every document in index (scroll API).
func (b *ElasticsearchBackend) Scan(ctx context.Context, index string, fn func(id string, source json.RawMessage) error) error {
	scroll := b.client.Scroll(index).Size(500).FetchSourceContext(elastic.NewFetchSourceContext(true))
	defer scroll.Clear(context.Background())
	for {
		res, err := scroll.Do(ctx)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if elastic.IsNotFound(err) {
				return nil // index does not exist: nothing to scan
			}
			return err
		}
		for _, hit := range res.Hits.Hits {
			if err := fn(hit.Id, hit.Source); err != nil {
				return err
			}
		}
	}
}

// CreateIndex creates index with the given settings/mappings body.
func (b *ElasticsearchBackend) CreateIndex(ctx context.Context, index, body string) error {
	if _, err := b.client.CreateIndex(index).Body(body).Do(ctx); err != nil {
		return fmt.Errorf("create index %s: %w", index, err)
	}
	return nil
}

// PointAlias makes alias point at index only, removing it from any other index in the same
// atomic request, so readers switch from the old index to the new one at once.
func (b *ElasticsearchBackend) PointAlias(ctx context.Context, alias, index string) error {
	svc := b.client.Alias().Add(index, alias)
	current, err := b.client.Aliases().Alias(alias).Do(ctx)
	if err == nil {
		for _, old := range current.IndicesByAlias(alias) {
			if old != index {
				svc = svc.Remove(old, alias)
			}
		}
	} else if !elastic.IsNotFound(err) {
		return err
	}
	if _, err := svc.Do(ctx); err != nil {
		return fmt.Errorf("point alias %s at %s: %w", alias, index, err)
	}
	return nil
}
