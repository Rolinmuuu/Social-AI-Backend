package constants

import (
	"os"
	"strings"
)

const (
	USER_INDEX    = "user"
	POST_INDEX    = "post"
	LIKE_INDEX    = "like"
	SHARE_INDEX   = "share"
	COMMENT_INDEX = "comment"
	FOLLOW_INDEX  = "follow"
	MESSAGE_INDEX = "message"

	REDIS_ADDRESS  = "redis:6379"
	REDIS_PASSWORD = ""
	REDIS_DB       = 0

	GCS_BUCKET      = "socialai_laioffer_202512"
	LOGSTASH_ADDRESS = "logstash:5000"
)

var (
	ES_URL      = getEnvOrDefault("ES_URL", "http://elasticsearch:9200")
	ES_USERNAME = getEnvOrDefault("ES_USERNAME", "elastic")
	ES_PASSWORD = os.Getenv("ES_PASSWORD")

	KAFKA_BROKERS = strings.Split(getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"), ",")

	OPENAI_API_KEY = os.Getenv("OPENAI_API_KEY")
)

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
