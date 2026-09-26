package constants

import (
	"os"
	"strings"
)

const (
	// Post search index. Readers and the indexer use the alias; the physical index behind it
	// is versioned so a mapping change can be rebuilt next to it and swapped in atomically.
	SEARCH_POST_ALIAS = "posts"
	SEARCH_POST_INDEX = "posts_v1"

	// Legacy indices from when Elasticsearch was the system of record. Only cmd/backfill
	// reads them, to copy their data into PostgreSQL.
	USER_INDEX         = "user"
	POST_INDEX         = "post"
	LIKE_INDEX         = "like"
	SHARE_INDEX        = "share"
	COMMENT_INDEX      = "comment"
	FOLLOW_INDEX       = "follow"
	MESSAGE_INDEX      = "message"
	NOTIFICATION_INDEX = "notification"

	REDIS_PASSWORD = ""
	REDIS_DB       = 0

	LOGSTASH_ADDRESS = "logstash:5000"
)

var (
	// PostgreSQL: the system of record for users, follows, posts, likes, comments, messages
	// and the outbox. Elasticsearch only holds the post search index.
	DATABASE_URL = getEnvOrDefault("DATABASE_URL", "postgres://socialai:socialai@postgres:5432/socialai?sslmode=disable")

	ES_URL      = getEnvOrDefault("ES_URL", "http://elasticsearch:9200")
	ES_USERNAME = getEnvOrDefault("ES_USERNAME", "elastic")
	ES_PASSWORD = os.Getenv("ES_PASSWORD")

	REDIS_ADDRESS = getEnvOrDefault("REDIS_ADDRESS", "redis:6379")

	KAFKA_BROKERS = strings.Split(getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"), ",")

	OPENAI_API_KEY = os.Getenv("OPENAI_API_KEY")

	GCS_BUCKET = getEnvOrDefault("GCS_BUCKET", "socialai_laioffer_202512")
)

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
