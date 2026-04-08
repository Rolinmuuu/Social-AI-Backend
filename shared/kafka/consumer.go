package kafka

import (
	"context"
	"fmt"

	"github.com/segmentio/kafka-go"
)

const maxRetries = 3

type MessageHandler func(key string, value []byte) error

type KafkaConsumer struct {
	reader *kafka.Reader
}

func NewKafkaConsumer(brokers []string, topic, groupID string) *KafkaConsumer {
	return &KafkaConsumer{
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:  brokers,
			Topic:    topic,
			GroupID:  groupID,
			MinBytes: 1,
			MaxBytes: 10e6,
		}),
	}
}

// Consume blocks and processes messages. If a handler fails after maxRetries
// attempts the message is committed and skipped to prevent poison-message loops.
func (c *KafkaConsumer) Consume(ctx context.Context, handler MessageHandler) error {
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			return err
		}

		var lastErr error
		for attempt := 1; attempt <= maxRetries; attempt++ {
			if lastErr = handler(msg.Key, msg.Value); lastErr == nil {
				break
			}
			fmt.Printf("handler error (attempt %d/%d) topic=%s offset=%d: %v\n",
				attempt, maxRetries, msg.Topic, msg.Offset, lastErr)
		}

		if lastErr != nil {
			fmt.Printf("SKIP poison message after %d retries: topic=%s offset=%d key=%s\n",
				maxRetries, msg.Topic, msg.Offset, string(msg.Key))
		}

		_ = c.reader.CommitMessages(ctx, msg)
	}
}

func (c *KafkaConsumer) Close() error {
	return c.reader.Close()
}