package backend

import (
	"socialai/shared/constants"

	"github.com/olivere/elastic/v7"
)

// PostSearchDoc is the document the indexer writes for a post. Deleted posts are kept as
// tombstones (deleted=true, filtered out of every query) rather than removed: an external
// version only guards a document that still exists, and Elasticsearch forgets the version of
// a deleted one after index.gc_deletes (60 s by default). A late post.created redelivered
// after that would otherwise bring a deleted post back.
type PostSearchDoc struct {
	PostId    string    `json:"post_id"`
	UserId    string    `json:"user_id"`
	Message   string    `json:"message"`
	Type      string    `json:"type"`
	CreatedAt int64     `json:"created_at"`
	Deleted   bool      `json:"deleted"`
	Embedding []float32 `json:"embedding,omitempty"`
}

func liveOnly() elastic.Query { return elastic.NewTermQuery("deleted", false) }

// SearchPostIDs returns the ids of the best keyword matches, most relevant first.
func SearchPostIDs(es ElasticsearchBackendInterface, keywords string, size int) ([]string, error) {
	q := elastic.NewBoolQuery().
		Must(elastic.NewMatchQuery("message", keywords).Operator("AND")).
		Filter(liveOnly())
	res, err := es.ReadFromESWithSize(q, constants.SEARCH_POST_ALIAS, size)
	if err != nil {
		return nil, err
	}
	return hitIDs(res), nil
}

// NearestPostIDs returns the ids of the k posts whose embeddings are closest to vector.
func NearestPostIDs(es ElasticsearchBackendInterface, vector []float32, k int) ([]string, error) {
	res, err := es.KNNSearchFromES(constants.SEARCH_POST_ALIAS, "embedding", vector, k, liveOnly())
	if err != nil {
		return nil, err
	}
	return hitIDs(res), nil
}

func hitIDs(res *elastic.SearchResult) []string {
	if res == nil || res.Hits == nil {
		return nil
	}
	ids := make([]string, 0, len(res.Hits.Hits))
	for _, h := range res.Hits.Hits {
		ids = append(ids, h.Id)
	}
	return ids
}
