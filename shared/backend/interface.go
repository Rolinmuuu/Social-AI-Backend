package backend

import (
	"context"
	"io"
	"time"

	"socialai/shared/feedplan"

	"github.com/olivere/elastic/v7"
)

type ElasticsearchBackendInterface interface {
	ReadFromES(query elastic.Query, index string) (*elastic.SearchResult, error)
	ReadFromESWithSize(query elastic.Query, index string, size int) (*elastic.SearchResult, error)
	SaveToES(i interface{}, index string, id string) error
	DeleteFromES(index string, id string) (bool, error)
	IncrementFieldInES(index string, id string, field string, value int) error
	KNNSearchFromES(index string, field string, vector []float32, k int) (*elastic.SearchResult, error)
	SearchSorted(query elastic.Query, index, sortField string, ascending bool, size int) (*elastic.SearchResult, error)
	CreateInES(i interface{}, index string, id string) (bool, error)
	UpdateFieldsInES(index string, id string, fields map[string]interface{}) error
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
