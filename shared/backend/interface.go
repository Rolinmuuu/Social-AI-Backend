package backend

import (
	"context"
	"io"
	"time"

	"github.com/olivere/elastic/v7"
)

type ElasticsearchBackendInterface interface {
	ReadFromES(query elastic.Query, index string) (*elastic.SearchResult, error)
	SaveToES(i interface{}, index string, id string) error
	DeleteFromES(index string, id string) (bool, error)
	IncrementFieldInES(index string, id string, field string, value int) error
}

type GoogleCloudStorageBackendInterface interface {
	SaveToGCS(r io.Reader, objectName string) (string, error)
	DeleteFromGCS(objectName string) error
}

// RedisBackendInterface mirrors the go-redis client signature with context.
type RedisBackendInterface interface {
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error
	Get(ctx context.Context, key string) (string, error)
	Delete(ctx context.Context, key ...string) error
	SAdd(ctx context.Context, key string, members ...interface{}) error
	SIsMember(ctx context.Context, key string, member interface{}) (bool, error)
}
