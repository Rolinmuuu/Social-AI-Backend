package backend

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"socialai/shared/feedplan"

	"github.com/olivere/elastic/v7"
)

// ElasticsearchBackendInterface is the search side. Elasticsearch holds a projection of
// posts built by the search indexer; it is never the source of truth for anything.
type ElasticsearchBackendInterface interface {
	ReadFromESWithSize(query elastic.Query, index string, size int) (*elastic.SearchResult, error)
	KNNSearchFromES(index string, field string, vector []float32, k int, filter elastic.Query) (*elastic.SearchResult, error)
	// IndexVersioned writes with version_type=external; applied=false means the index
	// already holds this version or a newer one.
	IndexVersioned(index, id string, doc interface{}, version int64) (applied bool, err error)
	// Scan visits every document of an index (used once, to migrate the legacy indices).
	Scan(ctx context.Context, index string, fn func(id string, source json.RawMessage) error) error
}

type GoogleCloudStorageBackendInterface interface {
	SaveToGCS(r io.Reader, objectName string) (string, error)
	DeleteFromGCS(objectName string) error
	GenerateSignedURL(objectName string) (string, error)
}

// RedisBackendInterface mirrors the go-redis client signature with context.
type RedisBackendInterface interface {
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error
	Get(ctx context.Context, key string) (string, error)
	Delete(ctx context.Context, key ...string) error
	SAdd(ctx context.Context, key string, members ...interface{}) error
	SIsMember(ctx context.Context, key string, member interface{}) (bool, error)
	// List operations used by feed fan-out worker
	LPush(ctx context.Context, key string, values ...interface{}) error
	LRange(ctx context.Context, key string, start, stop int64) ([]string, error)
	LTrim(ctx context.Context, key string, start, stop int64) error
	Expire(ctx context.Context, key string, expiration time.Duration) error

	// Atomic primitives used for idempotency keys, like de-duplication and counters.
	SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) (bool, error)
	SAddCount(ctx context.Context, key string, members ...interface{}) (int64, error)
	SRem(ctx context.Context, key string, members ...interface{}) error
	SMembers(ctx context.Context, key string) ([]string, error)
	SPopN(ctx context.Context, key string, count int64) ([]string, error)
	IncrBy(ctx context.Context, key string, value int64) (int64, error)
	GetDel(ctx context.Context, key string) (string, error)

	// Home feeds: sorted sets of post ids scored by creation time.
	ZRevRange(ctx context.Context, key string, start, stop int64) ([]string, error)
	// FeedItems returns up to count items of a feed that come after the cursor, newest first,
	// reading only that window of the sorted set.
	FeedItems(ctx context.Context, key string, after feedplan.Cursor, count int) ([]feedplan.Item, error)
	// AddToFeeds writes one post into many followers' feeds in a single pipeline.
	AddToFeeds(ctx context.Context, followerIDs []string, item feedplan.Item, maxLen int, ttl time.Duration) error
}
