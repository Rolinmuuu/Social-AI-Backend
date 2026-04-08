package kafka

import (
	"context"
	"encoding/json"

	"github.com/segmentio/kafka-go"
)

type KafkaProducerInterface interface {
	Publish(ctx context.Context, topic, key string, message interface{}) error
}

type KafkaProducer struct {
	brokers []string
	writers map[string]*kafka.Writer
}

func NewKafkaProducer(brokers []string) *KafkaProducer {
	return &KafkaProducer{
		brokers: brokers,
		writers: make(map[string]*kafka.Writer),
	}
}

func (p *KafkaProducer) getWriter(topic string) *kafka.Writer {
	if writer, ok := p.writers[topic]; ok {
		return writer
	}
	writer := &kafka.Writer{
		Addr: kafka.TCP(p.brokers...),
		Topic:   topic,
		Balancer: &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
	}
	p.writers[topic] = writer
	return writer
}

func (p *KafkaProducer) Publish(ctx context.Context, topic, key string, message interface{}) error {
	jsonMessage, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return p.getWriter(topic).WriteMessages(ctx, kafka.Message{
		Key:   []byte(key),
		Value: jsonMessage,
	})
}

func (p *KafkaProducer) Close() error {
	for _, writer := range p.writers {
		if err := writer.Close(); err != nil {
			return err
		}
	}
	return nil
}