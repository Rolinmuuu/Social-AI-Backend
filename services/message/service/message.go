package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/model"

	"github.com/google/uuid"
	elastic "github.com/olivere/elastic/v7"
)

// MessageService handles private messaging between users.
type MessageService struct {
	es backend.ElasticsearchBackendInterface
}

func NewMessageService(es backend.ElasticsearchBackendInterface) *MessageService {
	return &MessageService{es: es}
}

// SendMessage persists a new message from senderId to receiverId.
func (s *MessageService) SendMessage(senderId, receiverId, content string) (string, error) {
	if senderId == receiverId {
		return "", ErrCannotMessageSelf
	}

	message := model.Message{
		MessageId:  uuid.New().String(),
		SenderId:   senderId,
		ReceiverId: receiverId,
		Content:    content,
		CreatedAt:  time.Now(),
	}
	if err := s.es.SaveToES(message, constants.MESSAGE_INDEX, message.MessageId); err != nil {
		return "", fmt.Errorf("failed to save message: %w", err)
	}
	return message.MessageId, nil
}

// GetMessages returns the conversation between userId1 and userId2, sorted by time.
func (s *MessageService) GetMessages(userId1, userId2 string) ([]model.Message, error) {
	// Match messages in either direction between the two users.
	query := elastic.NewBoolQuery().Should(
		elastic.NewBoolQuery().
			Filter(elastic.NewTermQuery("sender_id", userId1)).
			Filter(elastic.NewTermQuery("receiver_id", userId2)),
		elastic.NewBoolQuery().
			Filter(elastic.NewTermQuery("sender_id", userId2)).
			Filter(elastic.NewTermQuery("receiver_id", userId1)),
	).MinimumNumberShouldMatch(1)

	result, err := s.es.ReadFromES(query, constants.MESSAGE_INDEX)
	if err != nil {
		return nil, fmt.Errorf("failed to read messages: %w", err)
	}

	var messages []model.Message
	for _, hit := range result.Hits.Hits {
		var msg model.Message
		if err := json.Unmarshal(hit.Source, &msg); err == nil {
			messages = append(messages, msg)
		}
	}

	sort.Slice(messages, func(i, j int) bool {
		return messages[i].CreatedAt.Before(messages[j].CreatedAt)
	})
	return messages, nil
}
