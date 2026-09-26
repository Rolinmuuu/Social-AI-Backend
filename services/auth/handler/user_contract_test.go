package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"socialai/shared/db/dbtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testJWTSecret = []byte("test-secret-for-unit-tests")

func newTestAuthRouter(t *testing.T) http.Handler {
	return InitRouter(dbtest.New(t), testJWTSecret)
}

// ──────────────────── Contract: POST /signup ────────────────────

func TestSignup_Contract_201(t *testing.T) {
	router := newTestAuthRouter(t)

	body, _ := json.Marshal(map[string]string{"user_id": "testuser", "password": "pass123"})
	req := httptest.NewRequest("POST", "/signup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "testuser", resp["user_id"], "response should contain user_id")
}

func TestSignup_Contract_400_MissingFields(t *testing.T) {
	router := newTestAuthRouter(t)

	body, _ := json.Marshal(map[string]string{"user_id": ""})
	req := httptest.NewRequest("POST", "/signup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSignup_Contract_400_InvalidUserId(t *testing.T) {
	router := newTestAuthRouter(t)

	body, _ := json.Marshal(map[string]string{"user_id": "UPPER_CASE!", "password": "pass123"})
	req := httptest.NewRequest("POST", "/signup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSignup_Contract_409_Duplicate(t *testing.T) {
	pool := dbtest.New(t)
	_, err := pool.Exec(context.Background(), `INSERT INTO users (user_id, password_hash) VALUES ('alice', 'x')`)
	require.NoError(t, err)
	router := InitRouter(pool, testJWTSecret)

	body, _ := json.Marshal(map[string]string{"user_id": "alice", "password": "pass123"})
	req := httptest.NewRequest("POST", "/signup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

// ──────────────────── Contract: POST /signin ────────────────────

func TestSignin_Contract_401_WrongPassword(t *testing.T) {
	router := newTestAuthRouter(t)

	body, _ := json.Marshal(map[string]string{"user_id": "nobody", "password": "wrong"})
	req := httptest.NewRequest("POST", "/signin", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ──────────────────── Contract: GET /health ────────────────────

func TestHealth_Contract_200(t *testing.T) {
	router := newTestAuthRouter(t)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "ok", resp["status"])
}
