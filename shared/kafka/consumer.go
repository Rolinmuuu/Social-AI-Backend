package kafka

import (
	"context"

	"socialai/shared/consumer"

	"github.com/segmentio/kafka-go"
)

// KafkaConsumer adapts a kafka-go consumer-group Reader to consumer.Source. Processing,
// retries, dead-lettering and commit ordering live in shared/consumer.
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
			// Commits are explicit (CommitMessages) and only for fully processed offsets.
			CommitInterval: 0,
		}),
	}
}

// Fetch returns the next message without committing it.
func (c *KafkaConsumer) Fetch(ctx context.Context) (consumer.Message, error) {
	m, err := c.reader.FetchMessage(ctx)
	if err != nil {
		return consumer.Message{}, err
	}
	return consumer.Message{Topic: m.Topic, Partition: m.Partition, Offset: m.Offset, Key: m.Key, Value: m.Value}, nil
}

// Commit marks everything up to and including m.Offset in m's partition as processed.
func (c *KafkaConsumer) Commit(ctx context.Context, m consumer.Message) error {
	return c.reader.CommitMessages(ctx, kafka.Message{Topic: m.Topic, Partition: m.Partition, Offset: m.Offset})
}

func (c *KafkaConsumer) Close() error {
	return c.reader.Close()
}
