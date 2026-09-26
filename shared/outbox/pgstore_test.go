package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"socialai/shared/db"
	"socialai/shared/db/dbtest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func countOutbox(t *testing.T, pool *pgxpool.Pool, where string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), "SELECT count(*) FROM outbox WHERE "+where).Scan(&n))
	return n
}

func enqueue(t *testing.T, pool *pgxpool.Pool, key string, due time.Time) Record {
	t.Helper()
	r, err := Enqueue(context.Background(), pool, Event{
		AggregateType: "post", AggregateID: key, Topic: "post.created", Key: key,
		Payload: map[string]string{"post_id": key},
	}, due)
	require.NoError(t, err)
	return r
}

// The event exists exactly when the business write exists.
func TestEnqueueCommitsOrRollsBackWithTheBusinessWrite(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	insertPost := func(tx pgx.Tx, id string) error {
		_, err := tx.Exec(ctx, `INSERT INTO posts (post_id, user_id) VALUES ($1, 'alice')`, id)
		return err
	}

	boom := errors.New("handler failed after the insert")
	err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		require.NoError(t, insertPost(tx, "p1"))
		_, err := Enqueue(ctx, tx, Event{Topic: "post.created", Key: "alice", AggregateType: "post", AggregateID: "p1", Payload: 1}, time.Now())
		require.NoError(t, err)
		return boom
	})
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 0, countOutbox(t, pool, "true"), "rolled back: no event for a post that does not exist")

	require.NoError(t, db.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := insertPost(tx, "p2"); err != nil {
			return err
		}
		_, err := Enqueue(ctx, tx, Event{Topic: "post.created", Key: "alice", AggregateType: "post", AggregateID: "p2", Payload: 1}, time.Now())
		return err
	}))
	assert.Equal(t, 1, countOutbox(t, pool, "aggregate_id = 'p2' AND status = 'pending'"))
}

func TestPGRelaySurvivesBrokerOutage(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	now := time.Now()
	c := &clock{t: now}
	enqueue(t, pool, "p1", now)
	pub := &flakyPublisher{down: true}
	r := &Relay{Store: &PGStore{Pool: pool}, Publisher: pub, Now: c.now, BaseBackoff: time.Minute}

	res, err := r.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Failed)
	assert.Equal(t, 1, countOutbox(t, pool, "status = 'pending' AND attempts = 1 AND last_error <> ''"))

	res, err = r.RunOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Published+res.Failed, "backing off")

	pub.down = false
	c.t = c.t.Add(2 * time.Minute)
	res, err = r.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Published)
	assert.Equal(t, []string{"post.created/p1"}, pub.sent)
	assert.Equal(t, 1, countOutbox(t, pool, "status = 'published' AND published_at IS NOT NULL"))

	res, err = r.RunOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Published, "published rows are never sent again")
}

// The payload the relay publishes is byte-for-byte the JSON that was stored.
func TestPGRelayPublishesTheStoredPayload(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	_, err := Enqueue(ctx, pool, Event{Topic: "post.created", Key: "alice", AggregateType: "post", AggregateID: "p1",
		Payload: map[string]interface{}{"post_id": "p1", "created_at": 1700000000}}, time.Now())
	require.NoError(t, err)

	var got []byte
	pub := publisherFunc(func(_ context.Context, _, _ string, m interface{}) error {
		got, _ = json.Marshal(m)
		return nil
	})
	_, err = (&Relay{Store: &PGStore{Pool: pool}, Publisher: pub}).RunOnce(ctx)
	require.NoError(t, err)
	assert.JSONEq(t, `{"post_id":"p1","created_at":1700000000}`, string(got))
}

type publisherFunc func(ctx context.Context, topic, key string, m interface{}) error

func (f publisherFunc) Publish(ctx context.Context, topic, key string, m interface{}) error {
	return f(ctx, topic, key, m)
}

// Eight relays (eight post-service instances) drain one table: every event is published
// exactly once, and no relay waits on another's locked rows.
func TestConcurrentRelaysPublishEachEventOnce(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	const events = 400
	for i := 0; i < events; i++ {
		enqueue(t, pool, fmt.Sprintf("p%03d", i), time.Now().Add(-time.Second))
	}

	pub := &flakyPublisher{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &Relay{Store: &PGStore{Pool: pool}, Publisher: pub, BatchSize: 25}
			for {
				res, err := r.RunOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if res.Published == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	seen := map[string]int{}
	for _, s := range pub.sent {
		seen[s]++
	}
	assert.Len(t, seen, events, "every event published")
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s published %d times", k, n)
		}
	}
	assert.Equal(t, events, countOutbox(t, pool, "status = 'published'"))
}

// A relay that claims rows and then dies does not strand them: once the lease runs out
// another relay takes them over.
func TestClaimedRowsComeBackWhenTheLeaseExpires(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	now := time.Now()
	enqueue(t, pool, "p1", now.Add(-time.Second))
	store := &PGStore{Pool: pool, Lease: time.Minute}

	claimed, err := store.Pending(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	// ...the relay crashes here, before publishing...

	again, err := store.Pending(ctx, now.Add(30*time.Second), 10)
	require.NoError(t, err)
	assert.Empty(t, again, "still leased to the first relay")

	again, err = store.Pending(ctx, now.Add(61*time.Second), 10)
	require.NoError(t, err)
	assert.Len(t, again, 1, "lease expired: another relay picks it up")
}

// The fast path publishes inline; the relay must leave rows alone until they are due.
func TestRowsAreNotDueBeforeNotBefore(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	now := time.Now()
	enqueue(t, pool, "p1", now.Add(10*time.Second))
	store := &PGStore{Pool: pool}

	recs, err := store.Pending(ctx, now, 10)
	require.NoError(t, err)
	assert.Empty(t, recs)
	recs, err = store.Pending(ctx, now.Add(11*time.Second), 10)
	require.NoError(t, err)
	assert.Len(t, recs, 1)
}

func TestPruneKeepsRecentAndUnpublishedRows(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	store := &PGStore{Pool: pool}
	old := enqueue(t, pool, "old", time.Now())
	recent := enqueue(t, pool, "recent", time.Now())
	enqueue(t, pool, "pending", time.Now())
	require.NoError(t, store.MarkPublished(ctx, old.ID))
	require.NoError(t, store.MarkPublished(ctx, recent.ID))
	_, err := pool.Exec(ctx, `UPDATE outbox SET published_at = now() - interval '8 days' WHERE id = $1`, old.ID)
	require.NoError(t, err)

	n, err := store.Prune(ctx, 7*24*time.Hour, 100)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	assert.Equal(t, 2, countOutbox(t, pool, "true"))

	pending, dead, oldest, err := store.Backlog(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, pending)
	assert.EqualValues(t, 0, dead)
	assert.Greater(t, oldest, time.Duration(0))
}
