package kafka

import (
	"context"

	"github.com/segmentio/kafka-go"
)

type MessageHandler func(key string, value []byte) error

type KafkaConsumer struct {
	readers *kafka.Reader
}

func NewKafkaConsumer(brokers []string, topic, groupID string) *KafkaConsumer {
	return &KafkaConsumer{
		readers: kafka.NewReader(kafka.ReaderConfig{
			Brokers: brokers,
			Topic:   topic,
			GroupID: groupID,
			MinBytes: 1,
			MaxBytes: 10e6, // 10MB
		}),
	}
}

// Consume 阻塞循环，每条消息交给 handler 处理
// handler 出错只记录日志，不停止消费（保证 at-least-once 处理）
func (c *KafkaConsumer) Consume(ctx context.Context, handler MessageHandler) error {
	for {
		msg, err := c.readers.FetchMessage(ctx)
		if err != nil {
			return err
		}
		if err := handler(msg.Key, msg.Value); err != nil {
			continue
		}
		_ = c.readers.CommitMessages(ctx, msg)
	}
}

func (c *KafkaConsumer) Close() error {
	return c.readers.Close()
}