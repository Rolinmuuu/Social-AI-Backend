package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"socialai/shared/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAuthRouter() http.Handler {
	es := testutil.NewMockESBackend()
	return InitRouter(es)
}

// ──────────────────── Contract: POST /signup ────────────────────

func TestSignup_Contract_201(t *testing.T) {
	router := newTestAuthRouter()

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
	router := newTestAuthRouter()

	body, _ := json.Marshal(map[string]string{"user_id": ""})
	req := httptest.NewRequest("POST", "/signup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSignup_Contract_400_InvalidUserId(t *testing.T) {
	router := newTestAuthRouter()

	body, _ := json.Marshal(map[string]string{"user_id": "UPPER_CASE!", "password": "pass123"})
	req := httptest.NewRequest("POST", "/signup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSignup_Contract_409_Duplicate(t *testing.T) {
	es := testutil.NewMockESBackend()
	es.SetDoc("user", "alice", map[string]string{"user_id": "alice"})
	router := InitRouter(es)

	body, _ := json.Marshal(map[string]string{"user_id": "alice", "password": "pass123"})
	req := httptest.NewRequest("POST", "/signup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

// ──────────────────── Contract: POST /signin ────────────────────

func TestSignin_Contract_401_WrongPassword(t *testing.T) {
	router := newTestAuthRouter()

	body, _ := json.Marshal(map[string]string{"user_id": "nobody", "password": "wrong"})
	req := httptest.NewRequest("POST", "/signin", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ──────────────────── Contract: GET /health ────────────────────

func TestHealth_Contract_200(t *testing.T) {
	router := newTestAuthRouter()

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "ok", resp["status"])
}
