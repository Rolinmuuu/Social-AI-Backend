package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/db"
	"socialai/shared/model"
	"socialai/shared/outbox"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Backfill copies the legacy Elasticsearch indices into PostgreSQL.
//
// Every insert is ON CONFLICT DO NOTHING keyed by the legacy id, so the tool can be stopped
// and re-run: a second run inserts only what the first did not. Rows the new schema refuses
// (a like on a post that no longer exists, a comment over the length limit, a self-follow)
// are counted as rejected and listed in the report instead of aborting the run; the old
// store never enforced those rules, so some bad rows are expected.
type Backfill struct {
	ES   backend.ElasticsearchBackendInterface
	DB   *pgxpool.Pool
	Logf func(format string, args ...interface{})
}

// Stats counts what happened to one entity.
type Stats struct {
	Read     int            `json:"read"`
	Inserted int            `json:"inserted"`
	Existing int            `json:"existing"` // already present (re-run, or a duplicate in the old store)
	Rejected int            `json:"rejected"` // refused by a constraint; see Reasons
	Reasons  map[string]int `json:"reasons,omitempty"`
}

func (s *Stats) reject(reason string) {
	s.Rejected++
	if s.Reasons == nil {
		s.Reasons = map[string]int{}
	}
	s.Reasons[reason]++
}

func (s *Stats) result(tag int64) {
	if tag == 1 {
		s.Inserted++
	} else {
		s.Existing++
	}
}

// Report is the outcome of a run, per entity.
type Report map[string]*Stats

func (b *Backfill) logf(format string, args ...interface{}) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

// Run copies every entity, parents before children (users and posts before the rows that
// reference them), then recomputes like/share counts from the copied rows.
func (b *Backfill) Run(ctx context.Context) (Report, error) {
	rep := Report{}
	steps := []struct {
		name string
		fn   func(context.Context, *Stats) error
	}{
		{"users", b.users},
		{"follows", b.follows},
		{"posts", b.posts},
		{"likes", b.likes},
		{"shares", b.shares},
		{"comments", b.comments},
		{"messages", b.messages},
		{"notifications", b.notifications},
	}
	for _, st := range steps {
		s := &Stats{}
		rep[st.name] = s
		if err := st.fn(ctx, s); err != nil {
			return rep, fmt.Errorf("backfill %s: %w", st.name, err)
		}
		b.logf("backfill: %-13s read=%d inserted=%d existing=%d rejected=%d %v", st.name, s.Read, s.Inserted, s.Existing, s.Rejected, s.Reasons)
	}
	// Legacy counters were updated separately from the like documents and could drift; the
	// copied rows are the truth. Nothing else writes during a backfill, so a plain recount is
	// safe here (unlike in production, where the reconciler must skip busy posts).
	if _, err := b.DB.Exec(ctx, `
		UPDATE posts p SET
			like_count  = (SELECT count(*) FROM post_likes l WHERE l.post_id = p.post_id),
			share_count = (SELECT count(*) FROM post_shares s WHERE s.post_id = p.post_id)`); err != nil {
		return rep, fmt.Errorf("backfill: recount: %w", err)
	}
	return rep, nil
}

// insert runs one INSERT ... ON CONFLICT DO NOTHING and classifies the outcome. Constraint
// violations are rejections, anything else is a real error.
func (b *Backfill) insert(ctx context.Context, q db.Querier, s *Stats, sql string, args ...any) error {
	tag, err := q.Exec(ctx, sql, args...)
	switch {
	case db.IsForeignKeyViolation(err):
		s.reject("references a missing row")
	case db.IsCheckViolation(err):
		s.reject("violates a check constraint")
	case err != nil:
		return err
	default:
		s.result(tag.RowsAffected())
	}
	return nil
}

func unixOrEpoch(sec int64) time.Time { return time.Unix(sec, 0) }

func (b *Backfill) users(ctx context.Context, s *Stats) error {
	return b.ES.Scan(ctx, constants.USER_INDEX, func(id string, src json.RawMessage) error {
		s.Read++
		var u model.User
		if err := json.Unmarshal(src, &u); err != nil || u.UserId == "" || u.Password == "" {
			s.reject("unreadable document")
			return nil
		}
		// The legacy document already holds a bcrypt hash in "password".
		return b.insert(ctx, b.DB, s, `
			INSERT INTO users (user_id, username, password_hash, age, gender)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
			u.UserId, u.Username, u.Password, u.Age, u.Gender)
	})
}

func (b *Backfill) follows(ctx context.Context, s *Stats) error {
	return b.ES.Scan(ctx, constants.FOLLOW_INDEX, func(id string, src json.RawMessage) error {
		s.Read++
		var f model.Follow
		if err := json.Unmarshal(src, &f); err != nil || f.FollowerId == "" || f.FolloweeId == "" {
			s.reject("unreadable document")
			return nil
		}
		if f.CreatedAt.IsZero() {
			f.CreatedAt = time.Unix(0, 0)
		}
		// Duplicate follow documents (the old check-then-write race) collapse into one row.
		return b.insert(ctx, b.DB, s, `
			INSERT INTO follows (follower_id, followee_id, created_at)
			VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, f.FollowerId, f.FolloweeId, f.CreatedAt)
	})
}

// legacyOutboxPending marks a post whose post.created was never published by the old
// document-embedded outbox.
const legacyOutboxPending = "pending"

// legacyPost is the old ES post document, including the fields that are now columns
// elsewhere or gone.
type legacyPost struct {
	PostId        string    `json:"post_id"`
	UserId        string    `json:"user_id"`
	Message       string    `json:"message"`
	Url           string    `json:"url"`
	Type          string    `json:"type"`
	Deleted       bool      `json:"deleted"`
	DeletedAt     int64     `json:"deleted_at"`
	CleanupStatus string    `json:"cleanup_status"`
	RetryCount    int       `json:"retry_count"`
	LastError     string    `json:"last_error"`
	Embedding     []float32 `json:"embedding"`
	CreatedAt     int64     `json:"created_at"`
	OutboxStatus  string    `json:"outbox_status"`
}

func (b *Backfill) posts(ctx context.Context, s *Stats) error {
	return b.ES.Scan(ctx, constants.POST_INDEX, func(id string, src json.RawMessage) error {
		s.Read++
		var p legacyPost
		if err := json.Unmarshal(src, &p); err != nil || p.PostId == "" || p.UserId == "" {
			s.reject("unreadable document")
			return nil
		}
		var deletedAt *time.Time
		if p.Deleted {
			t := time.Now()
			if p.DeletedAt > 0 {
				t = time.Unix(p.DeletedAt, 0)
			}
			deletedAt = &t
		}
		switch p.CleanupStatus {
		case "", "pending", "completed", "failed":
		default:
			p.CleanupStatus = ""
		}
		var emb []float32
		if len(p.Embedding) == 1536 {
			emb = p.Embedding // keeps the OpenAI cost of the old embeddings
		}

		// A post whose post.created was still waiting in the old document outbox gets a row in
		// the new outbox, in the same transaction, so the event is not lost in the move.
		return db.InTx(ctx, b.DB, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				INSERT INTO posts (post_id, user_id, message, url, type, created_at, deleted_at,
				                   cleanup_status, cleanup_attempts, cleanup_error, embedding)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
				ON CONFLICT DO NOTHING`,
				p.PostId, p.UserId, p.Message, p.Url, p.Type, unixOrEpoch(p.CreatedAt), deletedAt,
				p.CleanupStatus, p.RetryCount, p.LastError, emb)
			if err != nil {
				return err
			}
			s.result(tag.RowsAffected())
			if tag.RowsAffected() == 1 && p.OutboxStatus == legacyOutboxPending && !p.Deleted {
				_, err = outbox.Enqueue(ctx, tx, outbox.Event{
					AggregateType: "post", AggregateID: p.PostId, Topic: model.TopicPostCreated, Key: p.UserId,
					Payload: model.PostCreatedEvent{PostId: p.PostId, UserId: p.UserId, Message: p.Message, Url: p.Url, Type: p.Type, CreatedAt: p.CreatedAt},
				}, time.Now())
			}
			return err
		})
	})
}

func (b *Backfill) likes(ctx context.Context, s *Stats) error {
	return b.ES.Scan(ctx, constants.LIKE_INDEX, func(id string, src json.RawMessage) error {
		s.Read++
		var l model.PostLike
		if err := json.Unmarshal(src, &l); err != nil || l.PostId == "" || l.UserId == "" {
			s.reject("unreadable document")
			return nil
		}
		return b.insert(ctx, b.DB, s, `
			INSERT INTO post_likes (post_id, user_id, created_at) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, l.PostId, l.UserId, unixOrEpoch(l.CreatedAt))
	})
}

func (b *Backfill) shares(ctx context.Context, s *Stats) error {
	return b.ES.Scan(ctx, constants.SHARE_INDEX, func(id string, src json.RawMessage) error {
		s.Read++
		var sh model.PostShare
		if err := json.Unmarshal(src, &sh); err != nil || sh.PostId == "" {
			s.reject("unreadable document")
			return nil
		}
		if sh.PostShareId == "" {
			sh.PostShareId = id
		}
		return b.insert(ctx, b.DB, s, `
			INSERT INTO post_shares (share_id, post_id, user_id, platform, created_at)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
			sh.PostShareId, sh.PostId, sh.UserId, sh.Platform, unixOrEpoch(sh.CreatedAt))
	})
}

// comments are inserted shallowest first, so a reply's parent row exists when the reply's
// foreign key is checked. Needs all comments in memory; fine for this data set, and the
// place to switch to per-depth scans if it ever is not.
func (b *Backfill) comments(ctx context.Context, s *Stats) error {
	var all []model.Comment
	err := b.ES.Scan(ctx, constants.COMMENT_INDEX, func(id string, src json.RawMessage) error {
		s.Read++
		var c model.Comment
		if err := json.Unmarshal(src, &c); err != nil || c.CommentId == "" || c.PostId == "" {
			s.reject("unreadable document")
			return nil
		}
		all = append(all, c)
		return nil
	})
	if err != nil {
		return err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Depth < all[j].Depth })
	for _, c := range all {
		var parent *string
		if c.ParentCommentId != "" {
			parent = &c.ParentCommentId
		}
		if c.RootCommentId == "" {
			c.RootCommentId = c.CommentId
		}
		var deletedAt *time.Time
		if c.Deleted {
			t := unixOrEpoch(c.DeletedAt)
			deletedAt = &t
		}
		if err := b.insert(ctx, b.DB, s, `
			INSERT INTO comments (comment_id, post_id, parent_comment_id, root_comment_id, depth, user_id, content, created_at, deleted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT DO NOTHING`,
			c.CommentId, c.PostId, parent, c.RootCommentId, c.Depth, c.UserId, c.Content, unixOrEpoch(c.CreatedAt), deletedAt); err != nil {
			return err
		}
	}
	return nil
}

// legacyMessage is the old ES message document.
type legacyMessage struct {
	MessageId  string    `json:"message_id"`
	SenderId   string    `json:"sender_id"`
	ReceiverId string    `json:"receiver_id"`
	Content    string    `json:"content"`
	CreatedAt  time.Time `json:"created_at"`
}

// messages groups the old flat messages into conversations and numbers them by time. The
// legacy message id becomes client_msg_id, which is what makes a re-run skip messages it
// already copied. Imported history counts as read, so nobody gets a flood of "unread".
func (b *Backfill) messages(ctx context.Context, s *Stats) error {
	byConv := map[string][]legacyMessage{}
	err := b.ES.Scan(ctx, constants.MESSAGE_INDEX, func(id string, src json.RawMessage) error {
		s.Read++
		var m legacyMessage
		if err := json.Unmarshal(src, &m); err != nil || m.SenderId == "" || m.ReceiverId == "" || m.SenderId == m.ReceiverId {
			s.reject("unreadable document")
			return nil
		}
		if m.MessageId == "" {
			m.MessageId = id
		}
		if n := len([]rune(m.Content)); n == 0 || n > 4000 {
			s.reject("violates a check constraint")
			return nil
		}
		conv := conversationID(m.SenderId, m.ReceiverId)
		byConv[conv] = append(byConv[conv], m)
		return nil
	})
	if err != nil {
		return err
	}
	convs := make([]string, 0, len(byConv))
	for c := range byConv {
		convs = append(convs, c)
	}
	sort.Strings(convs)
	for _, conv := range convs {
		msgs := byConv[conv]
		sort.SliceStable(msgs, func(i, j int) bool {
			if !msgs[i].CreatedAt.Equal(msgs[j].CreatedAt) {
				return msgs[i].CreatedAt.Before(msgs[j].CreatedAt)
			}
			return msgs[i].MessageId < msgs[j].MessageId
		})
		if err := db.InTx(ctx, b.DB, func(tx pgx.Tx) error { return b.copyConversation(ctx, tx, conv, msgs, s) }); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backfill) copyConversation(ctx context.Context, tx pgx.Tx, conv string, msgs []legacyMessage, s *Stats) error {
	if _, err := tx.Exec(ctx, `INSERT INTO conversations (conversation_id, created_at) VALUES ($1, $2) ON CONFLICT DO NOTHING`, conv, msgs[0].CreatedAt); err != nil {
		return err
	}
	var lastSeq int64
	if err := tx.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE conversation_id = $1 FOR NO KEY UPDATE`, conv).Scan(&lastSeq); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT client_msg_id FROM messages WHERE conversation_id = $1 AND client_msg_id IS NOT NULL`, conv)
	if err != nil {
		return err
	}
	have, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	copied := make(map[string]bool, len(have))
	for _, h := range have {
		copied[h] = true
	}
	var lastAt time.Time
	for _, m := range msgs {
		if copied[m.MessageId] {
			s.Existing++
			continue
		}
		lastSeq++
		if _, err := tx.Exec(ctx, `
			INSERT INTO messages (conversation_id, seq, sender_id, content, created_at, client_msg_id)
			VALUES ($1, $2, $3, $4, $5, $6)`, conv, lastSeq, m.SenderId, m.Content, m.CreatedAt, m.MessageId); err != nil {
			return err
		}
		s.Inserted++
		lastAt = m.CreatedAt
	}
	if lastAt.IsZero() {
		return nil // nothing new
	}
	if _, err := tx.Exec(ctx, `UPDATE conversations SET last_seq = $2, last_message_at = $3 WHERE conversation_id = $1`, conv, lastSeq, lastAt); err != nil {
		return err
	}
	a, bb := msgs[0].SenderId, msgs[0].ReceiverId
	_, err = tx.Exec(ctx, `
		INSERT INTO conversation_members (conversation_id, user_id, last_read_seq, last_message_at)
		VALUES ($1, $2, $4, $5), ($1, $3, $4, $5)
		ON CONFLICT (conversation_id, user_id) DO UPDATE
		SET last_read_seq = EXCLUDED.last_read_seq, last_message_at = EXCLUDED.last_message_at`,
		conv, a, bb, lastSeq, lastAt)
	return err
}

func conversationID(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return "dm:" + a + ":" + b
}

func (b *Backfill) notifications(ctx context.Context, s *Stats) error {
	return b.ES.Scan(ctx, constants.NOTIFICATION_INDEX, func(id string, src json.RawMessage) error {
		s.Read++
		var n model.Notification
		if err := json.Unmarshal(src, &n); err != nil || n.UserId == "" {
			s.reject("unreadable document")
			return nil
		}
		if n.NotificationId == "" {
			n.NotificationId = id
		}
		return b.insert(ctx, b.DB, s, `
			INSERT INTO notifications (notification_id, user_id, type, actor_id, post_id, read, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
			n.NotificationId, n.UserId, n.Type, n.ActorId, n.PostId, n.Read, unixOrEpoch(n.CreatedAt))
	})
}
