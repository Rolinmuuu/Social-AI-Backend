package backend

import (
	"context"
	"time"

	"socialai/shared/constants"

	"github.com/redis/go-redis/v9"
)

var RedisBackend RedisBackendInterface

type RedisBackendImpl struct {
	client *redis.Client
}

func InitRedisBackend() (RedisBackendInterface, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     constants.REDIS_ADDRESS,
		Password: constants.REDIS_PASSWORD,
		DB:       constants.REDIS_DB,
	})
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}
	return &RedisBackendImpl{client: client}, nil
}

func (r *RedisBackendImpl) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	return r.client.Set(ctx, key, value, expiration).Err()
}

func (r *RedisBackendImpl) Get(ctx context.Context, key string) (string, error) {
	return r.client.Get(ctx, key).Result()
}

func (r *RedisBackendImpl) Delete(ctx context.Context, key ...string) error {
	return r.client.Del(ctx, key...).Err()
}

func (r *RedisBackendImpl) SAdd(ctx context.Context, key string, members ...interface{}) error {
	return r.client.SAdd(ctx, key, members...).Err()
}

func (r *RedisBackendImpl) SIsMember(ctx context.Context, key string, member interface{}) (bool, error) {
	return r.client.SIsMember(ctx, key, member).Result()
}
