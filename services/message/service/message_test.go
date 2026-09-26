package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"socialai/shared/db/dbtest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ctx = context.Background()

func newTestMessageService(t *testing.T) (*MessageService, *pgxpool.Pool) {
	pool := dbtest.New(t)
	for _, u := range []string{"alice", "bob", "carol"} {
		_, err := pool.Exec(ctx, `INSERT INTO users (user_id, password_hash) VALUES ($1, 'x')`, u)
		require.NoError(t, err)
	}
	return NewMessageService(pool), pool
}

func send(t *testing.T, s *MessageService, from, to, text string) int64 {
	t.Helper()
	m, _, err := s.SendMessage(ctx, from, to, text, "")
	require.NoError(t, err)
	return m.Seq
}

func TestConversationIDIsSymmetric(t *testing.T) {
	assert.Equal(t, "dm:alice:bob", ConversationID("alice", "bob"))
	assert.Equal(t, "dm:alice:bob", ConversationID("bob", "alice"))
	assert.Equal(t, "bob", otherParticipant("dm:alice:bob", "alice"))
	assert.Equal(t, "alice", otherParticipant("dm:alice:bob", "bob"))
}

func TestSendAndRead(t *testing.T) {
	s, _ := newTestMessageService(t)
	m, replayed, err := s.SendMessage(ctx, "alice", "bob", "hi bob", "")
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, int64(1), m.Seq)
	assert.Equal(t, "dm:alice:bob:1", m.MessageId)
	send(t, s, "bob", "alice", "hi alice")

	page, err := s.GetMessages(ctx, "bob", "alice", 0, 0, 50)
	require.NoError(t, err)
	require.Len(t, page.Messages, 2)
	assert.Equal(t, "hi bob", page.Messages[0].Content, "oldest first")
	assert.Equal(t, "alice", page.Messages[0].SenderId)
	assert.Equal(t, "bob", page.Messages[0].ReceiverId)
	assert.Equal(t, "alice", page.Messages[1].ReceiverId)
	assert.Zero(t, page.NextBeforeSeq)

	other, err := s.GetMessages(ctx, "carol", "bob", 0, 0, 50)
	require.NoError(t, err)
	assert.Empty(t, other.Messages, "carol only ever reads her own conversations")
}

func TestSendValidation(t *testing.T) {
	s, _ := newTestMessageService(t)
	_, _, err := s.SendMessage(ctx, "alice", "alice", "me", "")
	assert.ErrorIs(t, err, ErrCannotMessageSelf)
	_, _, err = s.SendMessage(ctx, "alice", "bob", "", "")
	assert.ErrorIs(t, err, ErrInvalidContent)
	_, _, err = s.SendMessage(ctx, "alice", "bob", strings.Repeat("é", 4001), "")
	assert.ErrorIs(t, err, ErrInvalidContent)
	_, _, err = s.SendMessage(ctx, "alice", "ghost", "hello?", "")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

// Scrolling back through 25 messages 10 at a time, then catching up with after_seq.
func TestPagingBackAndCatchingUp(t *testing.T) {
	s, _ := newTestMessageService(t)
	for i := 1; i <= 25; i++ {
		send(t, s, "alice", "bob", fmt.Sprintf("m%02d", i))
	}

	var seen []int64
	before := int64(0)
	for pages := 0; pages < 10; pages++ {
		page, err := s.GetMessages(ctx, "bob", "alice", before, 0, 10)
		require.NoError(t, err)
		chunk := make([]int64, 0, len(page.Messages))
		for _, m := range page.Messages {
			chunk = append(chunk, m.Seq)
		}
		seen = append(chunk, seen...) // prepend: we are going back in time
		if page.NextBeforeSeq == 0 {
			break
		}
		before = page.NextBeforeSeq
	}
	require.Len(t, seen, 25)
	for i, seq := range seen {
		assert.Equal(t, int64(i+1), seq)
	}

	// The client has seen up to 20 and polls for newer messages.
	page, err := s.GetMessages(ctx, "bob", "alice", 0, 20, 3)
	require.NoError(t, err)
	require.Len(t, page.Messages, 3)
	assert.Equal(t, int64(21), page.Messages[0].Seq)
	assert.True(t, page.HasMore)
	page, err = s.GetMessages(ctx, "bob", "alice", 0, 23, 3)
	require.NoError(t, err)
	assert.Len(t, page.Messages, 2)
	assert.False(t, page.HasMore)
}

// Both users fire 50 messages at once into a conversation that does not exist yet. No
// deadlock, no error, and seq is exactly 1..100: gap-free, unique, and in commit order
// (created_at never goes backwards as seq goes up).
func TestConcurrentSendersGetGapFreeCommitOrderedSeqs(t *testing.T) {
	s, pool := newTestMessageService(t)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 50; i++ {
		for _, pair := range [][2]string{{"alice", "bob"}, {"bob", "alice"}} {
			wg.Add(1)
			go func(from, to string, i int) {
				defer wg.Done()
				if _, _, err := s.SendMessage(ctx, from, to, fmt.Sprintf("%s %d", from, i), ""); err != nil {
					errs <- err
				}
			}(pair[0], pair[1], i)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("send failed: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT seq, created_at FROM messages WHERE conversation_id = 'dm:alice:bob' ORDER BY seq`)
	require.NoError(t, err)
	var prev time.Time
	var n int64
	for rows.Next() {
		var seq int64
		var at time.Time
		require.NoError(t, rows.Scan(&seq, &at))
		n++
		assert.Equal(t, n, seq, "gap-free")
		assert.False(t, at.Before(prev), "seq %d created before seq %d", seq, seq-1)
		prev = at
	}
	assert.EqualValues(t, 100, n)
	var members int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM conversation_members`).Scan(&members))
	assert.Equal(t, 2, members)
}

func TestIdempotentSend(t *testing.T) {
	s, _ := newTestMessageService(t)
	first, replayed, err := s.SendMessage(ctx, "alice", "bob", "hello", "key-1")
	require.NoError(t, err)
	assert.False(t, replayed)

	again, replayed, err := s.SendMessage(ctx, "alice", "bob", "hello", "key-1")
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, first.MessageId, again.MessageId)
	assert.True(t, first.CreatedAt.Equal(again.CreatedAt))

	_, _, err = s.SendMessage(ctx, "alice", "bob", "something else", "key-1")
	assert.ErrorIs(t, err, ErrKeyReused)

	// The same key from the other participant is a different message.
	other, replayed, err := s.SendMessage(ctx, "bob", "alice", "hello", "key-1")
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, int64(2), other.Seq)
}

// A flaky connection retries the same send ten times at once: one message is stored.
func TestConcurrentRetriesStoreOneMessage(t *testing.T) {
	s, pool := newTestMessageService(t)
	send(t, s, "bob", "alice", "warm up") // conversation exists

	var wg sync.WaitGroup
	seqs := make([]int64, 10)
	for i := range seqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, _, err := s.SendMessage(ctx, "alice", "bob", "only once", "retry-key")
			assert.NoError(t, err)
			seqs[i] = m.Seq
		}(i)
	}
	wg.Wait()
	for _, seq := range seqs {
		assert.Equal(t, seqs[0], seq, "every retry gets the first message")
	}
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE content = 'only once'`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestInboxUnreadAndMarkRead(t *testing.T) {
	s, _ := newTestMessageService(t)
	send(t, s, "bob", "alice", "one")
	send(t, s, "bob", "alice", "two")
	time.Sleep(2 * time.Millisecond)
	send(t, s, "carol", "alice", "hey")
	time.Sleep(2 * time.Millisecond)
	send(t, s, "alice", "bob", "reply")

	inbox, err := s.ListConversations(ctx, "alice", 10, "")
	require.NoError(t, err)
	require.Len(t, inbox.Conversations, 2)
	assert.Equal(t, "bob", inbox.Conversations[0].WithUserId, "most recent activity first")
	assert.Equal(t, "reply", inbox.Conversations[0].LastMessage)
	assert.EqualValues(t, 0, inbox.Conversations[0].Unread, "replying reads the conversation")
	assert.Equal(t, "carol", inbox.Conversations[1].WithUserId)
	assert.EqualValues(t, 1, inbox.Conversations[1].Unread)

	bobInbox, err := s.ListConversations(ctx, "bob", 10, "")
	require.NoError(t, err)
	require.Len(t, bobInbox.Conversations, 1)
	assert.EqualValues(t, 1, bobInbox.Conversations[0].Unread)

	// Paging the inbox one conversation at a time.
	first, err := s.ListConversations(ctx, "alice", 1, "")
	require.NoError(t, err)
	require.NotEmpty(t, first.NextCursor)
	second, err := s.ListConversations(ctx, "alice", 1, first.NextCursor)
	require.NoError(t, err)
	require.Len(t, second.Conversations, 1)
	assert.Equal(t, "carol", second.Conversations[0].WithUserId)
	assert.Empty(t, second.NextCursor)

	// Read markers only move forward and never past the last message.
	require.NoError(t, s.MarkRead(ctx, "alice", "carol", 99))
	require.NoError(t, s.MarkRead(ctx, "alice", "carol", 0))
	inbox, err = s.ListConversations(ctx, "alice", 10, "")
	require.NoError(t, err)
	assert.EqualValues(t, 0, inbox.Conversations[1].Unread)
	send(t, s, "carol", "alice", "again")
	inbox, err = s.ListConversations(ctx, "alice", 10, "")
	require.NoError(t, err)
	assert.Equal(t, "carol", inbox.Conversations[0].WithUserId)
	assert.EqualValues(t, 1, inbox.Conversations[0].Unread, "clamped at 99 would have hidden this one")

	page, err := s.GetMessages(ctx, "carol", "alice", 0, 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, page.LastReadSeq, "carol sees how far alice has read")

	assert.ErrorIs(t, s.MarkRead(ctx, "bob", "carol", 1), ErrConversationNotFound)
}
