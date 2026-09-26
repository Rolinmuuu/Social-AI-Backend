package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"socialai/shared/db"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is what a service records next to its own change.
type Event struct {
	AggregateType string      // e.g. "post"
	AggregateID   string      // e.g. the post id
	Topic         string      // Kafka topic
	Key           string      // partition key; events with one key share a partition
	Payload       interface{} // marshalled to JSON
}

// Enqueue inserts ev into the outbox table using q, which should be the transaction that
// writes the change ev describes: both commit or neither does. That is the whole point of
// the pattern: "post saved but event lost" and "event sent for a post that was rolled back"
// both become impossible.
//
// The row becomes due at notBefore. Callers that publish inline right after commit
// (Relay.PublishNow) pass a few seconds from now, so the relay does not race the fast path
// for the same row.
func Enqueue(ctx context.Context, q db.Querier, ev Event, notBefore time.Time) (Record, error) {
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return Record{}, fmt.Errorf("outbox: marshal %s payload: %w", ev.Topic, err)
	}
	var id int64
	var createdAt time.Time
	err = q.QueryRow(ctx, `
		INSERT INTO outbox (aggregate_type, aggregate_id, topic, partition_key, payload, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		ev.AggregateType, ev.AggregateID, ev.Topic, ev.Key, payload, notBefore,
	).Scan(&id, &createdAt)
	if err != nil {
		return Record{}, fmt.Errorf("outbox: insert %s: %w", ev.Topic, err)
	}
	return Record{
		ID:            strconv.FormatInt(id, 10),
		Topic:         ev.Topic,
		Key:           ev.Key,
		Payload:       ev.Payload,
		NextAttemptAt: notBefore,
		CreatedAt:     createdAt,
	}, nil
}

// PGStore is the Store for the outbox table.
//
// Several relays (one per post-service instance) can run at once. Pending *claims* rows:
// in one statement it selects due rows with FOR UPDATE SKIP LOCKED and pushes their
// next_attempt_at forward by Lease. Concurrent relays skip each other's rows instead of
// waiting on them or publishing them twice, and no transaction stays open while Kafka is
// called. If a relay dies after claiming, its rows become due again when the lease runs
// out. A lease shorter than a slow pass can let two relays publish the same row; that is a
// duplicate, which consumers already absorb (delivery is at-least-once), never a loss.
type PGStore struct {
	Pool  *pgxpool.Pool
	Lease time.Duration // default 2 minutes
}

func (s *PGStore) lease() time.Duration {
	if s.Lease <= 0 {
		return 2 * time.Minute
	}
	return s.Lease
}

func (s *PGStore) Pending(ctx context.Context, now time.Time, limit int) ([]Record, error) {
	rows, err := s.Pool.Query(ctx, `
		UPDATE outbox o
		SET next_attempt_at = $2
		FROM (
			SELECT id FROM outbox
			WHERE status = 'pending' AND next_attempt_at <= $1
			ORDER BY id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		) due
		WHERE o.id = due.id
		RETURNING o.id, o.topic, o.partition_key, o.payload, o.attempts, o.created_at`,
		now, now.Add(s.lease()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type claimed struct {
		id  int64
		rec Record
	}
	var got []claimed
	for rows.Next() {
		var c claimed
		var payload []byte
		if err := rows.Scan(&c.id, &c.rec.Topic, &c.rec.Key, &payload, &c.rec.Attempts, &c.rec.CreatedAt); err != nil {
			return nil, err
		}
		c.rec.ID = strconv.FormatInt(c.id, 10)
		c.rec.Payload = json.RawMessage(payload)
		got = append(got, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RETURNING does not keep the subquery's order; publish oldest first.
	sort.Slice(got, func(i, j int) bool { return got[i].id < got[j].id })
	out := make([]Record, len(got))
	for i, c := range got {
		out[i] = c.rec
	}
	return out, nil
}

func (s *PGStore) MarkPublished(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE outbox SET status = 'published', published_at = now(), last_error = ''
		WHERE id = $1 AND status = 'pending'`, id)
	return err
}

func (s *PGStore) MarkFailed(ctx context.Context, id string, attempts int, next time.Time, lastErr string, dead bool) error {
	status := "pending"
	if dead {
		status = "dead"
	}
	_, err := s.Pool.Exec(ctx, `
		UPDATE outbox SET attempts = $2, next_attempt_at = $3, last_error = $4, status = $5
		WHERE id = $1 AND status = 'pending'`, id, attempts, next, lastErr, status)
	return err
}

// Prune deletes up to batch published rows older than retention and returns how many.
// Published rows are kept for a while only to answer "was this event sent, and when".
func (s *PGStore) Prune(ctx context.Context, retention time.Duration, batch int) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `
		DELETE FROM outbox WHERE id IN (
			SELECT id FROM outbox
			WHERE status = 'published' AND published_at < now() - make_interval(secs => $1)
			LIMIT $2
		)`, retention.Seconds(), batch)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Backlog reports how many events are waiting and how old the oldest one is: the two numbers
// to alert on, since with the default relay settings an outage shows up as a growing backlog
// rather than as failures.
func (s *PGStore) Backlog(ctx context.Context) (pending int64, dead int64, oldest time.Duration, err error) {
	var oldestAt *time.Time
	err = s.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'pending'),
		       count(*) FILTER (WHERE status = 'dead'),
		       min(created_at) FILTER (WHERE status = 'pending')
		FROM outbox WHERE status <> 'published'`).Scan(&pending, &dead, &oldestAt)
	if err != nil {
		return 0, 0, 0, err
	}
	if oldestAt != nil {
		oldest = time.Since(*oldestAt)
	}
	return pending, dead, oldest, nil
}
