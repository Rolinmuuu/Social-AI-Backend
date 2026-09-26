//go:build integration

// Run against a real broker: KAFKA_BROKERS=localhost:9092 go test -tags=integration ./shared/kafka/
//
// EnsureTopics is what lets a consumer group subscribe to a topic nobody has written to yet
// (the search indexer and post.deleted). These tests run it against a real broker: it must
// create missing topics with the broker's defaults, tolerate topics that already exist and
// several services starting at once, and give up when the broker never comes up.
package kafka

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testBrokers(t *testing.T) []string {
	t.Helper()
	v := os.Getenv("KAFKA_BROKERS")
	if v == "" {
		t.Fatal("KAFKA_BROKERS is not set (e.g. localhost:9092)")
	}
	return strings.Split(v, ",")
}

func uniqueTopic(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func partitionsOf(t *testing.T, broker, topic string) []kafka.Partition {
	t.Helper()
	conn, err := kafka.Dial("tcp", broker)
	require.NoError(t, err)
	defer conn.Close()
	parts, err := conn.ReadPartitions(topic)
	require.NoError(t, err)
	return parts
}

func TestEnsureTopicsCreatesMissingTopicsWithBrokerDefaults(t *testing.T) {
	brokers := testBrokers(t)
	a, b := uniqueTopic("it-ensure-a"), uniqueTopic("it-ensure-b")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	require.NoError(t, EnsureTopics(ctx, brokers, a, b))

	for _, topic := range []string{a, b} {
		parts := partitionsOf(t, brokers[0], topic)
		require.NotEmpty(t, parts, "topic %s exists", topic)
		for _, p := range parts {
			assert.Equal(t, topic, p.Topic)
			assert.NotEmpty(t, p.Replicas, "replication factor comes from the broker default")
		}
	}
}

func TestEnsureTopicsToleratesExistingTopicsAndConcurrentCallers(t *testing.T) {
	brokers := testBrokers(t)
	topic := uniqueTopic("it-ensure-race")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// In docker compose feed-worker, notification-worker and search-indexer start together.
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = EnsureTopics(ctx, brokers, topic)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		assert.NoError(t, err, "caller %d", i)
	}

	before := partitionsOf(t, brokers[0], topic)
	require.NoError(t, EnsureTopics(ctx, brokers, topic), "already exists is not an error")
	assert.Len(t, partitionsOf(t, brokers[0], topic), len(before), "an existing topic is left alone")
}

// The case EnsureTopics exists for: a consumer group reading two topics, one of which has
// never been written to, must join and receive the first message published to it.
func TestGroupConsumerOnEnsuredTopicReceivesFirstMessage(t *testing.T) {
	brokers := testBrokers(t)
	busy, quiet := uniqueTopic("it-busy"), uniqueTopic("it-quiet")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	require.NoError(t, EnsureTopics(ctx, brokers, busy, quiet))

	c := NewKafkaGroupConsumer(brokers, []string{busy, quiet}, uniqueTopic("it-group"))
	defer c.Close()

	p := NewKafkaProducer(brokers)
	defer p.Close()
	require.NoError(t, p.Publish(ctx, quiet, "author-1", map[string]string{"post_id": "p1"}))

	m, err := c.Fetch(ctx)
	require.NoError(t, err, "the group joined and received the message")
	assert.Equal(t, quiet, m.Topic)
	assert.Equal(t, "author-1", string(m.Key))
	assert.JSONEq(t, `{"post_id":"p1"}`, string(m.Value))
	require.NoError(t, c.Commit(ctx, m))
}

func TestEnsureTopicsGivesUpWhenTheBrokerNeverAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := EnsureTopics(ctx, []string{"127.0.0.1:1"}, "never")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ensure topics")
	assert.Less(t, time.Since(start), 10*time.Second, "returns once ctx is done")
}
