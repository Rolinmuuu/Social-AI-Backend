package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"socialai/services/social/service"
	"socialai/shared/db/dbtest"

	jwt "github.com/form3tech-oss/jwt-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func asUser(r *http.Request, id string) *http.Request {
	token := &jwt.Token{Claims: jwt.MapClaims{"user_id": id}}
	return r.WithContext(context.WithValue(r.Context(), "user", token))
}

func TestFollowContract(t *testing.T) {
	pool := dbtest.New(t)
	for _, u := range []string{"alice", "bob", "carol"} {
		_, err := pool.Exec(context.Background(), `INSERT INTO users (user_id, password_hash) VALUES ($1, 'x')`, u)
		require.NoError(t, err)
	}
	svc := service.NewSocialService(pool)
	h := NewSocialHandler(svc)

	follow := func(who, whom string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"followee_id": whom})
		w := httptest.NewRecorder()
		h.addFollowHandler(w, asUser(httptest.NewRequest("POST", "/follow", bytes.NewReader(body)), who))
		return w
	}
	assert.Equal(t, http.StatusCreated, follow("alice", "bob").Code)
	assert.Equal(t, http.StatusCreated, follow("carol", "bob").Code)
	assert.Equal(t, http.StatusConflict, follow("alice", "bob").Code)
	assert.Equal(t, http.StatusNotFound, follow("alice", "ghost").Code)
	assert.Equal(t, http.StatusBadRequest, follow("alice", "alice").Code)

	list := h.listHandler("follower_ids", svc.Followers)
	w := httptest.NewRecorder()
	list(w, asUser(httptest.NewRequest("GET", "/follow/followers?limit=1", nil), "bob"))
	require.Equal(t, http.StatusOK, w.Code)
	var page struct {
		FollowerIDs []string `json:"follower_ids"`
		NextCursor  string   `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	assert.Len(t, page.FollowerIDs, 1)
	require.NotEmpty(t, page.NextCursor)

	w = httptest.NewRecorder()
	list(w, asUser(httptest.NewRequest("GET", "/follow/followers?limit=1&cursor="+page.NextCursor, nil), "bob"))
	var second struct {
		FollowerIDs []string `json:"follower_ids"`
		NextCursor  *string  `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &second))
	assert.Len(t, second.FollowerIDs, 1)
	assert.NotEqual(t, page.FollowerIDs[0], second.FollowerIDs[0])
	assert.Nil(t, second.NextCursor, "no next_cursor on the last page")

	w = httptest.NewRecorder()
	list(w, asUser(httptest.NewRequest("GET", "/follow/followers?cursor=%25%25", nil), "bob"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
