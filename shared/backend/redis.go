package backend

import (
	"context"
	"strconv"
	"time"

	"socialai/shared/constants"
	"socialai/shared/feedplan"

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

func (r *RedisBackendImpl) LPush(ctx context.Context, key string, values ...interface{}) error {
	return r.client.LPush(ctx, key, values...).Err()
}

func (r *RedisBackendImpl) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return r.client.LRange(ctx, key, start, stop).Result()
}

func (r *RedisBackendImpl) LTrim(ctx context.Context, key string, start, stop int64) error {
	return r.client.LTrim(ctx, key, start, stop).Err()
}

func (r *RedisBackendImpl) Expire(ctx context.Context, key string, expiration time.Duration) error {
	return r.client.Expire(ctx, key, expiration).Err()
}

func (r *RedisBackendImpl) SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) (bool, error) {
	return r.client.SetNX(ctx, key, value, expiration).Result()
}

func (r *RedisBackendImpl) SAddCount(ctx context.Context, key string, members ...interface{}) (int64, error) {
	return r.client.SAdd(ctx, key, members...).Result()
}

func (r *RedisBackendImpl) SRem(ctx context.Context, key string, members ...interface{}) error {
	return r.client.SRem(ctx, key, members...).Err()
}

func (r *RedisBackendImpl) SMembers(ctx context.Context, key string) ([]string, error) {
	return r.client.SMembers(ctx, key).Result()
}

func (r *RedisBackendImpl) SPopN(ctx context.Context, key string, count int64) ([]string, error) {
	return r.client.SPopN(ctx, key, count).Result()
}

func (r *RedisBackendImpl) IncrBy(ctx context.Context, key string, value int64) (int64, error) {
	return r.client.IncrBy(ctx, key, value).Result()
}

// GetDel returns ("", nil) when the key does not exist, so callers can tell "nothing there"
// from a real error.
func (r *RedisBackendImpl) GetDel(ctx context.Context, key string) (string, error) {
	v, err := r.client.GetDel(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (r *RedisBackendImpl) ZRevRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return r.client.ZRevRange(ctx, key, start, stop).Result()
}

// FeedItems reads one page window: ZRANGE key <cursor score> -inf BYSCORE REV LIMIT 0 n.
// Members that share the cursor's score but sort at or before the cursor id are skipped by
// fetching that many extra (ZCOUNT of the tie) and filtering, so paging never repeats or
// skips an item even when several posts have the same timestamp.
func (r *RedisBackendImpl) FeedItems(ctx context.Context, key string, after feedplan.Cursor, count int) ([]feedplan.Item, error) {
	max, extra := "+inf", int64(0)
	if after.IsSet() {
		max = strconv.FormatInt(after.T, 10)
		ties, err := r.client.ZCount(ctx, key, max, max).Result()
		if err != nil {
			return nil, err
		}
		extra = ties
	}
	// go-redis takes Start/Stop as min/max and swaps them itself for Rev+ByScore (ZRANGE key
	// max min BYSCORE REV). Passing them already reversed sent "-inf +inf", an empty range, so
	// every home feed read came back empty; the in-memory test double did not notice.
	zs, err := r.client.ZRangeArgsWithScores(ctx, redis.ZRangeArgs{
		Key: key, Start: "-inf", Stop: max, ByScore: true, Rev: true, Count: int64(count) + extra,
	}).Result()
	if err != nil {
		return nil, err
	}
	items := make([]feedplan.Item, 0, len(zs))
	for _, z := range zs {
		id, _ := z.Member.(string)
		items = append(items, feedplan.Item{PostID: id, CreatedAt: int64(z.Score)})
	}
	items = feedplan.Before(items, after)
	if len(items) > count {
		items = items[:count]
	}
	return items, nil
}

// AddToFeeds: for every follower, ZADD the post (idempotent: same member, same score),
// trim to the newest maxLen and refresh the TTL. All commands go out in one pipeline, so a
// batch of 500 followers costs one network round trip instead of 1,500.
func (r *RedisBackendImpl) AddToFeeds(ctx context.Context, followerIDs []string, item feedplan.Item, maxLen int, ttl time.Duration) error {
	_, err := r.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, id := range followerIDs {
			key := HomeFeedKey(id)
			pipe.ZAdd(ctx, key, redis.Z{Score: float64(item.CreatedAt), Member: item.PostID})
			pipe.ZRemRangeByRank(ctx, key, 0, int64(-maxLen-1))
			pipe.Expire(ctx, key, ttl)
		}
		return nil
	})
	return err
}

// HomeFeedKey is the sorted set holding a user's materialised home feed (the old
// "home_feed:" lists are left to expire; a new prefix avoids WRONGTYPE errors).
func HomeFeedKey(userID string) string { return "feed:home:" + userID }

// CelebritySetKey holds the ids of authors whose posts are pulled at read time.
const CelebritySetKey = "feed:celebrities"

// IsNil reports whether err is go-redis's "key does not exist".
func IsNil(err error) bool { return err == redis.Nil }
