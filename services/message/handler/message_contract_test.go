package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"socialai/services/message/service"
	"socialai/shared/db/dbtest"

	jwt "github.com/form3tech-oss/jwt-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func asUser(r *http.Request, id string) *http.Request {
	token := &jwt.Token{Claims: jwt.MapClaims{"user_id": id}}
	return r.WithContext(context.WithValue(r.Context(), "user", token))
}

func TestMessageContract(t *testing.T) {
	pool := dbtest.New(t)
	for _, u := range []string{"alice", "bob"} {
		_, err := pool.Exec(context.Background(), `INSERT INTO users (user_id, password_hash) VALUES ($1, 'x')`, u)
		require.NoError(t, err)
	}
	h := NewMessageHandler(service.NewMessageService(pool))

	post := func(key, text string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"receiver_id": "bob", "content": text})
		r := asUser(httptest.NewRequest("POST", "/message", bytes.NewReader(body)), "alice")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		h.sendMessageHandler(w, r)
		return w
	}
	first := post("k1", "hello")
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	retry := post("k1", "hello")
	assert.Equal(t, http.StatusOK, retry.Code)
	assert.Equal(t, "true", retry.Header().Get("Idempotent-Replayed"))
	assert.JSONEq(t, first.Body.String(), retry.Body.String())
	assert.Equal(t, http.StatusUnprocessableEntity, post("k1", "changed").Code)
	assert.Equal(t, http.StatusBadRequest, post("", "").Code)
	post("", "second")

	get := func(query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.getMessageHandler(w, asUser(httptest.NewRequest("GET", "/message?"+query, nil), "bob"))
		return w
	}
	w := get("with_user_id=alice&limit=1")
	require.Equal(t, http.StatusOK, w.Code)
	var page struct {
		ConversationID string `json:"conversation_id"`
		Messages       []struct {
			Seq     int64  `json:"seq"`
			Content string `json:"content"`
		} `json:"messages"`
		NextBeforeSeq int64 `json:"next_before_seq"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	assert.Equal(t, "dm:alice:bob", page.ConversationID)
	require.Len(t, page.Messages, 1)
	assert.Equal(t, "second", page.Messages[0].Content)
	assert.EqualValues(t, 2, page.NextBeforeSeq)

	assert.Equal(t, http.StatusBadRequest, get("with_user_id=alice&before_seq=2&after_seq=1").Code)
	assert.Equal(t, http.StatusBadRequest, get("with_user_id=alice&limit=abc").Code)
	assert.Equal(t, http.StatusBadRequest, get("").Code)

	inbox := httptest.NewRecorder()
	h.listConversationsHandler(inbox, asUser(httptest.NewRequest("GET", "/conversations", nil), "bob"))
	require.Equal(t, http.StatusOK, inbox.Code)
	assert.Contains(t, inbox.Body.String(), `"unread":2`)

	body, _ := json.Marshal(map[string]interface{}{"with_user_id": "alice", "seq": 2})
	read := httptest.NewRecorder()
	h.markReadHandler(read, asUser(httptest.NewRequest("POST", "/conversations/read", bytes.NewReader(body)), "bob"))
	assert.Equal(t, http.StatusOK, read.Code)
}
