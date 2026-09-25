package backend

import (
	"context"
	"encoding/json"
	"fmt"

	"socialai/shared/constants"

	"github.com/olivere/elastic/v7"
)

var ESBackend ElasticsearchBackendInterface

type ElasticsearchBackend struct {
	client *elastic.Client
}

func InitElasticsearchBackend() (ElasticsearchBackendInterface, error) {
	client, err := elastic.NewClient(
		elastic.SetURL(constants.ES_URL),
		elastic.SetBasicAuth(constants.ES_USERNAME, constants.ES_PASSWORD),
	)
	if err != nil {
		return nil, err
	}

	indices := map[string]string{
		constants.POST_INDEX: `{
			"mappings": { "properties": {
				"post_id":       { "type": "keyword" },
				"user_id":       { "type": "keyword" },
				"user":          { "type": "keyword" },
				"message":       { "type": "text" },
				"url":           { "type": "keyword", "index": false },
				"type":          { "type": "keyword", "index": false },
				"deleted":       { "type": "boolean" },
				"deleted_at":    { "type": "long" },
				"cleanup_status":{ "type": "keyword" },
				"retry_count":   { "type": "integer" },
				"last_error":    { "type": "text" },
				"like_count":    { "type": "integer" },
				"shared_count":  { "type": "integer" },
				"embedding":     { "type": "dense_vector", "dims": 1536, "index": true, "similarity": "cosine" },
				"created_at":    { "type": "long" },
				"outbox_status": { "type": "keyword" },
				"outbox_attempts": { "type": "integer" },
				"outbox_next_at":  { "type": "long" },
				"outbox_error":    { "type": "text", "index": false }
			}}}`,
		constants.USER_INDEX: `{
			"mappings": { "properties": {
				"user_id":  { "type": "keyword" },
				"username": { "type": "keyword" },
				"password": { "type": "keyword" },
				"age":      { "type": "long", "index": false },
				"gender":   { "type": "keyword", "index": false }
			}}}`,
		constants.FOLLOW_INDEX: `{
			"mappings": { "properties": {
				"follow_id":   { "type": "keyword" },
				"follower_id": { "type": "keyword" },
				"followee_id": { "type": "keyword" },
				"created_at":  { "type": "date" }
			}}}`,
		constants.MESSAGE_INDEX: `{
			"mappings": { "properties": {
				"message_id":  { "type": "keyword" },
				"sender_id":   { "type": "keyword" },
				"receiver_id": { "type": "keyword" },
				"content":     { "type": "text" },
				"created_at":  { "type": "date" }
			}}}`,
		constants.LIKE_INDEX: `{
			"mappings": { "properties": {
				"post_like_id": { "type": "keyword" },
				"user_id":      { "type": "keyword" },
				"post_id":      { "type": "keyword" },
				"created_at":   { "type": "long" }
			}}}`,
		constants.SHARE_INDEX: `{
			"mappings": { "properties": {
				"post_share_id": { "type": "keyword" },
				"user_id":       { "type": "keyword" },
				"post_id":       { "type": "keyword" },
				"created_at":    { "type": "long" },
				"platform":      { "type": "keyword" }
			}}}`,
		constants.COMMENT_INDEX: `{
			"mappings": { "properties": {
				"comment_id":        { "type": "keyword" },
				"parent_comment_id": { "type": "keyword" },
				"root_comment_id":   { "type": "keyword" },
				"user_id":           { "type": "keyword" },
				"post_id":           { "type": "keyword" },
				"depth":             { "type": "integer" },
				"content":           { "type": "text" },
				"created_at":        { "type": "long" },
				"deleted":           { "type": "boolean" },
				"deleted_at":        { "type": "long" }
			}}}`,
		constants.NOTIFICATION_INDEX: `{
			"mappings": { "properties": {
				"notification_id": { "type": "keyword" },
				"user_id":         { "type": "keyword" },
				"type":            { "type": "keyword" },
				"actor_id":        { "type": "keyword" },
				"post_id":         { "type": "keyword" },
				"read":            { "type": "boolean" },
				"created_at":      { "type": "long" }
			}}}`,
	}

	ctx := context.Background()
	for index, mapping := range indices {
		exists, err := client.IndexExists(index).Do(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to check index %s: %v", index, err)
		}
		if !exists {
			if _, err := client.CreateIndex(index).Body(mapping).Do(ctx); err != nil {
				return nil, fmt.Errorf("failed to create index %s: %v", index, err)
			}
		}
	}

	fmt.Println("All Elasticsearch indices are ready.")
	return &ElasticsearchBackend{client: client}, nil
}

// withoutVectors keeps the 1536-float embedding out of search responses: it is only needed
// inside Elasticsearch for kNN, and shipping it back added roughly 15-20 KB of JSON per post.
func withoutVectors() *elastic.FetchSourceContext {
	return elastic.NewFetchSourceContext(true).Exclude("embedding")
}

func (b *ElasticsearchBackend) ReadFromES(query elastic.Query, index string) (*elastic.SearchResult, error) {
	return b.client.Search().
		Index(index).
		Query(query).
		FetchSourceContext(withoutVectors()).
		Do(context.Background())
}

func (b *ElasticsearchBackend) ReadFromESWithSize(query elastic.Query, index string, size int) (*elastic.SearchResult, error) {
	return b.client.Search().
		Index(index).
		Query(query).
		Size(size).
		FetchSourceContext(withoutVectors()).
		Do(context.Background())
}

// SearchSorted returns up to size hits ordered by sortField, ties broken by post_id in the
// same direction so the order is total (paging by (sortField, post_id) depends on that).
// UnmappedType lets the sort run on an older index where the field does not exist yet.
func (b *ElasticsearchBackend) SearchSorted(query elastic.Query, index, sortField string, ascending bool, size int) (*elastic.SearchResult, error) {
	return b.client.Search().
		Index(index).
		Query(query).
		SortBy(
			elastic.NewFieldSort(sortField).Order(ascending).UnmappedType("long"),
			elastic.NewFieldSort("post_id").Order(ascending).UnmappedType("keyword"),
		).
		Size(size).
		FetchSourceContext(withoutVectors()).
		Do(context.Background())
}

// CreateInES indexes the document only if the id does not exist yet (op_type=create).
// created=false with a nil error means the document already existed. This makes the
// like document the atomic, durable "has this user liked this post" check.
func (b *ElasticsearchBackend) CreateInES(i interface{}, index string, id string) (bool, error) {
	_, err := b.client.Index().
		Index(index).
		Id(id).
		OpType("create").
		BodyJson(i).
		Do(context.Background())
	if err != nil {
		if elastic.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// UpdateFieldsInES merges fields into an existing document (partial update). Unlike a full
// SaveToES of a document read earlier, it cannot overwrite counters that changed meanwhile.
func (b *ElasticsearchBackend) UpdateFieldsInES(index string, id string, fields map[string]interface{}) error {
	_, err := b.client.Update().
		Index(index).
		Id(id).
		Doc(fields).
		RetryOnConflict(3).
		Do(context.Background())
	return err
}

func (b *ElasticsearchBackend) SaveToES(i interface{}, index string, id string) error {
	_, err := b.client.Index().
		Index(index).
		Id(id).
		BodyJson(i).
		Do(context.Background())
	return err
}

func (b *ElasticsearchBackend) DeleteFromES(index string, id string) (bool, error) {
	resp, err := b.client.Delete().
		Index(index).
		Id(id).
		Do(context.Background())
	if err != nil {
		return false, err
	}
	return resp.Result == "deleted", nil
}

func (b *ElasticsearchBackend) IncrementFieldInES(index, id, field string, value int) error {
	scriptSource := "ctx._source[params.field] = (ctx._source[params.field] == null ? 0 : ctx._source[params.field]) + params.value"
	script := elastic.NewScript(scriptSource).Params(map[string]interface{}{"field": field, "value": value})
	result, err := b.client.Update().
		Index(index).
		Id(id).
		Script(script).
		RetryOnConflict(3).
		Do(context.Background())
	if err != nil {
		return err
	}
	if result.Result != "updated" && result.Result != "noop" {
		return fmt.Errorf("increment failed: result=%s", result.Result)
	}
	return nil
}

func (b *ElasticsearchBackend) KNNSearchFromES(index, field string, vector []float32, k int) (*elastic.SearchResult, error) {
	query := map[string]interface{}{
		"knnQuery": map[string]interface{}{
			"field":          field,
			"vector":         vector,
			"k":              k,
			"num_candidates": k * 2,
		},
	}
	jsonQuery, err := json.Marshal(query)
	if err != nil {
		return nil, err
	}
	searchResult, err := b.client.Search().
		Index(index).
		Source(string(jsonQuery)).
		Do(context.Background())
	if err != nil {
		return nil, err
	}
	return searchResult, nil
}
