package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"socialai/shared/db"
	"socialai/shared/model"
	"socialai/shared/pagecursor"
	"socialai/shared/socialgraph"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MessageService owns conversations, conversation_members and messages.
//
// Data model: a direct conversation has a deterministic id, and its messages are clustered by
// (conversation_id, seq). seq is a per-conversation counter incremented under the
// conversation's row lock, so it is gap-free and increases in commit order. That is what
// makes "give me what I have not seen" (after_seq) exact: paging by created_at could skip a
// message whose transaction started earlier but committed later than one already returned.
type MessageService struct {
	db    *pgxpool.Pool
	graph socialgraph.Graph
}

func NewMessageService(pool *pgxpool.Pool) *MessageService {
	return &MessageService{db: pool, graph: socialgraph.Graph{DB: pool}}
}

const maxContentLen = 4000

// ConversationID is the id of the direct conversation between two users, the same whoever
// asks. User ids are [a-z0-9_], so ':' cannot appear inside one.
func ConversationID(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return "dm:" + a + ":" + b
}

func messageID(conversationID string, seq int64) string {
	return conversationID + ":" + strconv.FormatInt(seq, 10)
}

// otherParticipant returns the member of a direct conversation who is not me.
func otherParticipant(conversationID, me string) string {
	parts := strings.Split(strings.TrimPrefix(conversationID, "dm:"), ":")
	if len(parts) == 2 && parts[0] == me {
		return parts[1]
	}
	if len(parts) == 2 {
		return parts[0]
	}
	return ""
}

// SendMessage stores a message from senderId to receiverId and returns it. clientMsgID is
// the request's Idempotency-Key: a retry with the same key returns the message the first
// attempt stored (replayed=true) instead of sending it twice.
func (s *MessageService) SendMessage(ctx context.Context, senderId, receiverId, content, clientMsgID string) (msg model.Message, replayed bool, err error) {
	if senderId == receiverId {
		return msg, false, ErrCannotMessageSelf
	}
	if content == "" || utf8.RuneCountInString(content) > maxContentLen {
		return msg, false, ErrInvalidContent
	}
	exists, err := s.graph.UserExists(ctx, receiverId)
	if err != nil {
		return msg, false, err
	}
	if !exists {
		return msg, false, ErrUserNotFound
	}

	convID := ConversationID(senderId, receiverId)
	msg = model.Message{ConversationId: convID, SenderId: senderId, ReceiverId: receiverId, Content: content}
	var key *string
	if clientMsgID != "" {
		key = &clientMsgID
	}

	err = db.InTx(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO conversations (conversation_id) VALUES ($1) ON CONFLICT DO NOTHING`, convID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			a, b := senderId, receiverId
			if a > b {
				a, b = b, a
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`,
				convID, a, b); err != nil {
				return err
			}
		}

		// Serialise senders of this conversation. NO KEY UPDATE is the weakest lock that does
		// it: only non-key columns change. FOR UPDATE would also block other transactions that
		// merely reference the row (the KEY SHARE lock a foreign-key check takes), for nothing.
		var lastSeq int64
		if err := tx.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE conversation_id = $1 FOR NO KEY UPDATE`, convID).Scan(&lastSeq); err != nil {
			return err
		}

		if key != nil {
			// Checked after taking the lock, so a concurrent retry with the same key waits for
			// the first attempt and then sees its row.
			var prev model.Message
			err := tx.QueryRow(ctx, `
				SELECT seq, content, created_at FROM messages
				WHERE conversation_id = $1 AND sender_id = $2 AND client_msg_id = $3`,
				convID, senderId, clientMsgID).Scan(&prev.Seq, &prev.Content, &prev.CreatedAt)
			if err == nil {
				if prev.Content != content {
					return ErrKeyReused
				}
				msg.Seq, msg.CreatedAt, replayed = prev.Seq, prev.CreatedAt, true
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}

		msg.Seq = lastSeq + 1
		// clock_timestamp(), not now(): now() is the transaction's start time, which may be
		// earlier than that of a message that already committed with a lower seq.
		if err := tx.QueryRow(ctx, `
			INSERT INTO messages (conversation_id, seq, sender_id, content, client_msg_id, created_at)
			VALUES ($1, $2, $3, $4, $5, clock_timestamp())
			RETURNING created_at`,
			convID, msg.Seq, senderId, content, key).Scan(&msg.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE conversations SET last_seq = $2, last_message_at = $3 WHERE conversation_id = $1`,
			convID, msg.Seq, msg.CreatedAt); err != nil {
			return err
		}
		// Both members' inbox position moves; the sender has read their own message.
		_, err = tx.Exec(ctx, `
			UPDATE conversation_members
			SET last_message_at = $2,
			    last_read_seq = CASE WHEN user_id = $3 THEN $4 ELSE last_read_seq END
			WHERE conversation_id = $1`, convID, msg.CreatedAt, senderId, msg.Seq)
		return err
	})
	if err != nil {
		return model.Message{}, false, err
	}
	msg.MessageId = messageID(convID, msg.Seq)
	return msg, replayed, nil
}

// MessagePage is a window of one conversation, oldest first.
type MessagePage struct {
	ConversationId string          `json:"conversation_id"`
	Messages       []model.Message `json:"messages"`
	// NextBeforeSeq pages further back (pass it as before_seq); 0 when the start is reached.
	NextBeforeSeq int64 `json:"next_before_seq,omitempty"`
	// HasMore is set on an after_seq page that was cut at limit (poll again right away).
	HasMore bool `json:"has_more,omitempty"`
	// LastReadSeq is the other participant's read position (for "seen" markers).
	LastReadSeq int64 `json:"other_last_read_seq"`
}

// GetMessages returns part of userId's conversation with withUserId.
//
//   - no cursor: the newest limit messages;
//   - beforeSeq > 0: the limit messages before it (scrolling back);
//   - afterSeq > 0: the messages after it, oldest first (catching up / polling).
//
// Every variant is one range scan of the (conversation_id, seq) primary key.
func (s *MessageService) GetMessages(ctx context.Context, userId, withUserId string, beforeSeq, afterSeq int64, limit int) (MessagePage, error) {
	limit = pagecursor.Limit(limit, 50, 200)
	convID := ConversationID(userId, withUserId)
	page := MessagePage{ConversationId: convID, Messages: []model.Message{}}

	var rows pgx.Rows
	var err error
	if afterSeq > 0 {
		rows, err = s.db.Query(ctx, `
			SELECT seq, sender_id, content, created_at FROM messages
			WHERE conversation_id = $1 AND seq > $2
			ORDER BY seq LIMIT $3`, convID, afterSeq, limit+1)
	} else {
		if beforeSeq <= 0 {
			beforeSeq = 1<<63 - 1
		}
		rows, err = s.db.Query(ctx, `
			SELECT seq, sender_id, content, created_at FROM messages
			WHERE conversation_id = $1 AND seq < $2
			ORDER BY seq DESC LIMIT $3`, convID, beforeSeq, limit+1)
	}
	if err != nil {
		return page, err
	}
	msgs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (model.Message, error) {
		m := model.Message{ConversationId: convID}
		err := r.Scan(&m.Seq, &m.SenderId, &m.Content, &m.CreatedAt)
		m.MessageId = messageID(convID, m.Seq)
		if m.SenderId == userId {
			m.ReceiverId = withUserId
		} else {
			m.ReceiverId = userId
		}
		return m, err
	})
	if err != nil {
		return page, err
	}

	more := len(msgs) > limit
	if more {
		msgs = msgs[:limit]
	}
	if afterSeq > 0 {
		page.HasMore = more
	} else {
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 { // newest-first scan -> oldest first
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
		if more {
			page.NextBeforeSeq = msgs[0].Seq
		}
	}
	page.Messages = msgs

	err = s.db.QueryRow(ctx, `
		SELECT last_read_seq FROM conversation_members WHERE conversation_id = $1 AND user_id = $2`,
		convID, withUserId).Scan(&page.LastReadSeq)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return page, err
	}
	return page, nil
}

// Conversation is one row of a user's inbox.
type Conversation struct {
	ConversationId string    `json:"conversation_id"`
	WithUserId     string    `json:"with_user_id"`
	LastSeq        int64     `json:"last_seq"`
	LastMessage    string    `json:"last_message"`
	LastSenderId   string    `json:"last_sender_id"`
	LastMessageAt  time.Time `json:"last_message_at"`
	Unread         int64     `json:"unread"`
}

// InboxPage is one page of conversations, most recently active first.
type InboxPage struct {
	Conversations []Conversation `json:"conversations"`
	NextCursor    string         `json:"next_cursor,omitempty"`
}

type inboxCursor struct {
	T  int64  `json:"t"` // last_message_at, unix microseconds
	ID string `json:"id"`
}

// ListConversations returns userId's conversations with the last message and the number of
// unread messages, from one range scan of conversation_members_inbox.
func (s *MessageService) ListConversations(ctx context.Context, userId string, limit int, cursor string) (InboxPage, error) {
	var cur inboxCursor
	if err := pagecursor.Decode(cursor, &cur); err != nil {
		return InboxPage{}, err
	}
	limit = pagecursor.Limit(limit, 20, 100)
	var after *time.Time
	if cur.ID != "" {
		t := time.UnixMicro(cur.T)
		after = &t
	}
	rows, err := s.db.Query(ctx, `
		SELECT m.conversation_id, c.last_seq, m.last_read_seq, m.last_message_at, msg.content, msg.sender_id
		FROM conversation_members m
		JOIN conversations c ON c.conversation_id = m.conversation_id
		JOIN messages msg ON msg.conversation_id = c.conversation_id AND msg.seq = c.last_seq
		WHERE m.user_id = $1 AND m.last_message_at IS NOT NULL
		  AND ($2::timestamptz IS NULL OR (m.last_message_at, m.conversation_id) < ($2, $3))
		ORDER BY m.last_message_at DESC, m.conversation_id DESC
		LIMIT $4`, userId, after, cur.ID, limit+1)
	if err != nil {
		return InboxPage{}, err
	}
	convs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Conversation, error) {
		var c Conversation
		var lastRead int64
		err := r.Scan(&c.ConversationId, &c.LastSeq, &lastRead, &c.LastMessageAt, &c.LastMessage, &c.LastSenderId)
		c.WithUserId = otherParticipant(c.ConversationId, userId)
		c.Unread = c.LastSeq - lastRead
		return c, err
	})
	if err != nil {
		return InboxPage{}, err
	}
	page := InboxPage{Conversations: []Conversation{}}
	for i, c := range convs {
		if i == limit {
			last := convs[limit-1]
			page.NextCursor = pagecursor.Encode(inboxCursor{T: last.LastMessageAt.UnixMicro(), ID: last.ConversationId})
			break
		}
		page.Conversations = append(page.Conversations, c)
	}
	return page, nil
}

// MarkRead records that userId has read their conversation with withUserId up to seq. The
// read position only moves forward and never past the last message, so a late or repeated
// request cannot make messages unread again.
func (s *MessageService) MarkRead(ctx context.Context, userId, withUserId string, seq int64) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE conversation_members m
		SET last_read_seq = GREATEST(m.last_read_seq, LEAST($3, c.last_seq))
		FROM conversations c
		WHERE c.conversation_id = m.conversation_id AND m.conversation_id = $1 AND m.user_id = $2`,
		ConversationID(userId, withUserId), userId, seq)
	if err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConversationNotFound
	}
	return nil
}
