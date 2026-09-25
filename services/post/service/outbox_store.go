package service

import (
	"context"
	"time"

	"socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/model"
	"socialai/shared/outbox"

	"github.com/olivere/elastic/v7"
)

// esOutboxStore keeps post.created events inside the post documents themselves. A single
// Elasticsearch document write is atomic, so "post saved" and "event recorded" cannot
// diverge — which two separate writes (ES, then Kafka) could.
type esOutboxStore struct {
	es backend.ElasticsearchBackendInterface
}

func (s *esOutboxStore) Pending(_ context.Context, now time.Time, limit int) ([]outbox.Record, error) {
	query := elastic.NewBoolQuery().
		Filter(elastic.NewTermQuery("outbox_status", model.OutboxPending)).
		Filter(elastic.NewRangeQuery("outbox_next_at").Lte(now.Unix()))
	res, err := s.es.SearchSorted(query, constants.POST_INDEX, "created_at", true, limit)
	if err != nil {
		return nil, err
	}
	var recs []outbox.Record
	for _, p := range getDeletedPostFromSearchResult(res) {
		// Re-check in code: cheap, and keeps the relay correct if the index mapping lags.
		if p.OutboxStatus != model.OutboxPending || p.OutboxNextAt > now.Unix() {
			continue
		}
		recs = append(recs, recordFor(p))
	}
	return recs, nil
}

func (s *esOutboxStore) MarkPublished(_ context.Context, id string) error {
	return s.es.UpdateFieldsInES(constants.POST_INDEX, id, map[string]interface{}{
		"outbox_status": model.OutboxPublished,
		"outbox_error":  "",
	})
}

func (s *esOutboxStore) MarkFailed(_ context.Context, id string, attempts int, next time.Time, lastErr string, dead bool) error {
	status := model.OutboxPending
	if dead {
		status = model.OutboxDead
	}
	return s.es.UpdateFieldsInES(constants.POST_INDEX, id, map[string]interface{}{
		"outbox_status":   status,
		"outbox_attempts": attempts,
		"outbox_next_at":  next.Unix(),
		"outbox_error":    lastErr,
	})
}

// recordFor builds the post.created outbox record for a post. The partition key is the
// author id, so one author's events are consumed in order.
func recordFor(p model.Post) outbox.Record {
	return outbox.Record{
		ID:       p.PostId,
		Topic:    model.TopicPostCreated,
		Key:      p.UserId,
		Attempts: p.OutboxAttempts,
		Payload: model.PostCreatedEvent{
			PostId:    p.PostId,
			UserId:    p.UserId,
			Message:   p.Message,
			Url:       p.Url,
			Type:      p.Type,
			CreatedAt: p.CreatedAt,
		},
	}
}
