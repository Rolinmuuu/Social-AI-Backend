package testutil

import (
	"context"
	"encoding/json"
	"sync"
)

// PublishedMessage records a message published to Kafka for test assertions.
type PublishedMessage struct {
	Topic   string
	Key     string
	Payload json.RawMessage
}

// MockKafkaProducer is a mock for KafkaProducerInterface.
type MockKafkaProducer struct {
	mu         sync.Mutex
	Messages   []PublishedMessage
	PublishErr error
}

func NewMockKafkaProducer() *MockKafkaProducer {
	return &MockKafkaProducer{}
}

func (m *MockKafkaProducer) Publish(_ context.Context, topic, key string, message interface{}) error {
	if m.PublishErr != nil {
		return m.PublishErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, _ := json.Marshal(message)
	m.Messages = append(m.Messages, PublishedMessage{Topic: topic, Key: key, Payload: data})
	return nil
}

// Count returns the number of messages published to a given topic.
func (m *MockKafkaProducer) Count(topic string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, msg := range m.Messages {
		if msg.Topic == topic {
			n++
		}
	}
	return n
}
