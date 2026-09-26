// Package worker builds the Elasticsearch search projection of posts from PostgreSQL.
//
// Events (post.created, post.deleted) only say *which* post changed. The indexer reads the
// row's current state and writes it with the row's version as an external version. That
// makes the projection converge whatever the delivery order or number of duplicates:
//
//   - a redelivered event rewrites the same version: Elasticsearch refuses it (a no-op);
//   - an old event processed late reads the *current* row, never the state it described;
//   - two indexers racing on one post both write the latest version, or one is refused.
//
// Deleted posts become tombstones (deleted=true) instead of being removed, see
// backend.PostSearchDoc for why. Because every reader re-reads hits from PostgreSQL, a
// projection that lags only affects ranking, never what a user can see.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"socialai/shared/backend"
	"socialai/shared/db"

	"github.com/jackc/pgx/v5"
)

// Indexer syncs posts into one search index (normally the alias).
type Indexer struct {
	DB     db.Querier
	ES     backend.ElasticsearchBackendInterface
	OpenAI backend.OpenAIBackendInterface // nil: no embeddings (keyword search only)
	Index  string
	Logf   func(format string, args ...interface{})
}

type postRow struct {
	id, userID, message, typ string
	createdAt                time.Time
	deleted                  bool
	embedding                []float32
	version                  int64
}

func (x *Indexer) logf(format string, args ...interface{}) {
	if x.Logf != nil {
		x.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Handle processes one event from post.created or post.deleted.
func (x *Indexer) Handle(ctx context.Context, value []byte) error {
	var ev struct {
		PostID string `json:"post_id"`
	}
	if err := json.Unmarshal(value, &ev); err != nil || ev.PostID == "" {
		return fmt.Errorf("indexer: bad event %q: %v", value, err)
	}
	return x.Sync(ctx, ev.PostID)
}

// Sync writes the current state of one post to the index.
func (x *Indexer) Sync(ctx context.Context, postID string) error {
	row, err := x.load(ctx, postID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Only possible for a hard-deleted row: the outbox never records an event for a post
		// whose transaction did not commit.
		x.logf("indexer: post %s no longer exists, nothing to index", postID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("indexer: load %s: %w", postID, err)
	}

	if !row.deleted && row.embedding == nil && row.message != "" && x.OpenAI != nil {
		row, err = x.embed(ctx, row)
		if err != nil {
			return err
		}
	}

	doc := backend.PostSearchDoc{
		PostId: row.id, UserId: row.userID, Message: row.message, Type: row.typ,
		CreatedAt: row.createdAt.Unix(), Deleted: row.deleted,
	}
	if !row.deleted {
		doc.Embedding = row.embedding
	} else {
		doc.Message = "" // a tombstone keeps nothing searchable
	}
	applied, err := x.ES.IndexVersioned(x.Index, row.id, doc, row.version)
	if err != nil {
		return fmt.Errorf("indexer: index %s v%d: %w", row.id, row.version, err)
	}
	if !applied {
		x.logf("indexer: %s v%d already indexed (duplicate or stale event)", row.id, row.version)
	}
	return nil
}

func (x *Indexer) load(ctx context.Context, postID string) (postRow, error) {
	var r postRow
	var deletedAt *time.Time
	err := x.DB.QueryRow(ctx, `
		SELECT post_id, user_id, message, type, created_at, deleted_at, embedding, version
		FROM posts WHERE post_id = $1`, postID).
		Scan(&r.id, &r.userID, &r.message, &r.typ, &r.createdAt, &deletedAt, &r.embedding, &r.version)
	r.deleted = deletedAt != nil
	return r, err
}

// embed computes the post's embedding and stores it on the row, bumping the version so the
// index accepts the richer document. If OpenAI fails, the post is indexed without a vector
// (keyword search still finds it) and RepairMissingEmbeddings retries later: an OpenAI
// outage must not push events into the dead-letter topic.
func (x *Indexer) embed(ctx context.Context, row postRow) (postRow, error) {
	emb, err := x.OpenAI.GetEmbedding(ctx, row.message)
	if err != nil {
		x.logf("indexer: embedding for %s failed, indexing without vector: %v", row.id, err)
		return row, nil
	}
	var version int64
	err = x.DB.QueryRow(ctx, `
		UPDATE posts SET embedding = $2, version = version + 1
		WHERE post_id = $1 AND embedding IS NULL AND deleted_at IS NULL
		RETURNING version`, row.id, emb).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another indexer stored one first, or the post was deleted meanwhile: use the row as
		// it is now.
		return x.load(ctx, row.id)
	}
	if err != nil {
		return row, fmt.Errorf("indexer: store embedding for %s: %w", row.id, err)
	}
	row.embedding, row.version = emb, version
	return row, nil
}

// RepairMissingEmbeddings re-syncs up to limit live posts that have no embedding yet (their
// first sync ran while OpenAI was failing). Returns how many were attempted.
func (x *Indexer) RepairMissingEmbeddings(ctx context.Context, limit int) (int, error) {
	if x.OpenAI == nil {
		return 0, nil
	}
	rows, err := x.DB.Query(ctx, `
		SELECT post_id FROM posts
		WHERE embedding IS NULL AND deleted_at IS NULL AND message <> ''
		ORDER BY created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := x.Sync(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// Reindex writes every post to the index, walking the table in post_id order, pageSize rows
// at a time. It is how the projection is rebuilt from scratch (a new index behind the alias,
// a lost cluster) or backfilled after the migration from Elasticsearch. Safe to run while
// the event-driven indexer is running: both write versioned documents.
func (x *Indexer) Reindex(ctx context.Context, pageSize int, progress func(done int)) (int, error) {
	after := ""
	done := 0
	for {
		rows, err := x.DB.Query(ctx, `SELECT post_id FROM posts WHERE post_id > $1 ORDER BY post_id LIMIT $2`, after, pageSize)
		if err != nil {
			return done, err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return done, err
		}
		for _, id := range ids {
			if err := x.Sync(ctx, id); err != nil {
				return done, err
			}
			done++
		}
		if progress != nil {
			progress(done)
		}
		if len(ids) < pageSize {
			return done, nil
		}
		after = ids[len(ids)-1]
	}
}
