package kafka

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"
)

// EnsureTopics creates topics that do not exist yet, with the broker's default partition
// count and replication factor. A consumer group subscribed to a topic nobody has written to
// (post.deleted before the first delete) otherwise fails to join until the topic appears.
// Existing topics are left alone. In a managed cluster topics come from infrastructure code
// and this is a no-op.
//
// It returns once every topic has a leader for each partition in the broker's metadata: a
// successful CreateTopics only means the controller accepted it, and a consumer group that
// joins before the topic is in metadata is assigned no partitions of it (found against a real
// broker: "Unknown Topic Or Partition" right after creation).
//
// It retries until ctx is done, since in docker compose the broker may still be starting.
func EnsureTopics(ctx context.Context, brokers []string, topics ...string) error {
	var last error
	for {
		if last = createTopics(brokers[0], topics); last == nil {
			if last = topicsReady(brokers[0], topics); last == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("kafka: ensure topics %v: %w", topics, last)
		case <-time.After(2 * time.Second):
		}
	}
}

func createTopics(broker string, topics []string) error {
	conn, err := kafka.Dial("tcp", broker)
	if err != nil {
		return err
	}
	defer conn.Close()
	controller, err := conn.Controller()
	if err != nil {
		return err
	}
	cc, err := kafka.Dial("tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	if err != nil {
		return err
	}
	defer cc.Close()
	cfgs := make([]kafka.TopicConfig, len(topics))
	for i, t := range topics {
		cfgs[i] = kafka.TopicConfig{Topic: t, NumPartitions: -1, ReplicationFactor: -1}
	}
	return cc.CreateTopics(cfgs...) // "already exists" is not an error
}

// topicsReady reports nil when the broker's metadata lists every topic with a leader for each
// of its partitions.
func topicsReady(broker string, topics []string) error {
	conn, err := kafka.Dial("tcp", broker)
	if err != nil {
		return err
	}
	defer conn.Close()
	parts, err := conn.ReadPartitions(topics...)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range parts {
		if p.Leader.Host == "" {
			return fmt.Errorf("topic %s partition %d has no leader yet", p.Topic, p.ID)
		}
		seen[p.Topic] = true
	}
	for _, t := range topics {
		if !seen[t] {
			return fmt.Errorf("topic %s not in metadata yet", t)
		}
	}
	return nil
}
