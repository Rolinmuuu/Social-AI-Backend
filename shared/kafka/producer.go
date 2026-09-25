package kafka

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

type KafkaProducerInterface interface {
	Publish(ctx context.Context, topic, key string, message interface{}) error
}

type KafkaProducer struct {
	brokers []string
	mu      sync.Mutex // HTTP handlers publish concurrently; the map needs a lock
	writers map[string]*kafka.Writer
}

func NewKafkaProducer(brokers []string) *KafkaProducer {
	return &KafkaProducer{
		brokers: brokers,
		writers: make(map[string]*kafka.Writer),
	}
}

func (p *KafkaProducer) getWriter(topic string) *kafka.Writer {
	p.mu.Lock()
	defer p.mu.Unlock()
	if writer, ok := p.writers[topic]; ok {
		return writer
	}
	writer := &kafka.Writer{
		Addr:  kafka.TCP(p.brokers...),
		Topic: topic,
		// Hash by key so all events of one author land on one partition, in order.
		// (LeastBytes spread them across partitions, so a consumer could see them reordered.)
		Balancer:     &kafka.Hash{},
		RequiredAcks: kafka.RequireAll,
		// kafka-go waits up to BatchTimeout (default 1s) to fill a batch before a synchronous
		// WriteMessages returns, which put ~1s on every request that published an event.
		BatchTimeout: 10 * time.Millisecond,
		// Fail fast and let the caller decide: the outbox relay already retries with backoff,
		// so the writer's own default of 10 attempts would only hold requests longer.
		MaxAttempts:            3,
		WriteBackoffMax:        200 * time.Millisecond,
		WriteTimeout:           5 * time.Second,
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
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, writer := range p.writers {
		if err := writer.Close(); err != nil {
			return err
		}
	}
	return nil
}
