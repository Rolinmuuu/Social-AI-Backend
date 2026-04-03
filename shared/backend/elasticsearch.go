package backend

import (
	"context"
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
				"shared_count":  { "type": "integer" }
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

func (b *ElasticsearchBackend) ReadFromES(query elastic.Query, index string) (*elastic.SearchResult, error) {
	return b.client.Search().
		Index(index).
		Query(query).
		Pretty(true).
		Do(context.Background())
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
