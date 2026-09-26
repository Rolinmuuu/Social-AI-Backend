package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"socialai/services/message/service"
	"socialai/shared/pagecursor"
	"socialai/shared/utils"
)

// MessageHandler handles private messaging HTTP requests.
type MessageHandler struct {
	msgSvc *service.MessageService
}

func NewMessageHandler(msgSvc *service.MessageService) *MessageHandler {
	return &MessageHandler{msgSvc: msgSvc}
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// POST /message {"receiver_id", "content"}; optional Idempotency-Key header.
// 201 with the stored message; a retry with the same key gets 200 and the same message.
func (h *MessageHandler) sendMessageHandler(w http.ResponseWriter, r *http.Request) {
	senderId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		ReceiverId string `json:"receiver_id"`
		Content    string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ReceiverId == "" {
		writeError(w, http.StatusBadRequest, "receiver_id and content are required")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 128 {
		writeError(w, http.StatusBadRequest, "Idempotency-Key too long")
		return
	}

	msg, replayed, err := h.msgSvc.SendMessage(r.Context(), senderId, req.ReceiverId, req.Content, key)
	switch {
	case errors.Is(err, service.ErrCannotMessageSelf):
		writeError(w, http.StatusBadRequest, "cannot send message to yourself")
	case errors.Is(err, service.ErrInvalidContent):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, service.ErrKeyReused):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case err != nil:
		log.Printf("sendMessage error: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to send message")
	case replayed:
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, msg)
	default:
		writeJSON(w, http.StatusCreated, msg)
	}
}

// GET /message?with_user_id=X[&before_seq=N | &after_seq=N][&limit=50]
//
// Without a cursor: the newest messages. before_seq pages back through history (use
// next_before_seq from the previous response); after_seq returns what arrived since.
// Messages are always oldest first.
func (h *MessageHandler) getMessageHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q := r.URL.Query()
	withUserId := q.Get("with_user_id")
	if withUserId == "" {
		writeError(w, http.StatusBadRequest, "with_user_id query param is required")
		return
	}
	before, err1 := optInt(q.Get("before_seq"))
	after, err2 := optInt(q.Get("after_seq"))
	limit, err3 := optInt(q.Get("limit"))
	if err1 != nil || err2 != nil || err3 != nil || (before > 0 && after > 0) {
		writeError(w, http.StatusBadRequest, "before_seq, after_seq and limit must be positive integers (not both cursors)")
		return
	}

	page, err := h.msgSvc.GetMessages(r.Context(), userId, withUserId, before, after, int(limit))
	if err != nil {
		log.Printf("getMessages error: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to get messages")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// GET /conversations?limit=&cursor= — the caller's inbox, most recent first, with unread counts.
func (h *MessageHandler) listConversationsHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, err := h.msgSvc.ListConversations(r.Context(), userId, limit, r.URL.Query().Get("cursor"))
	if errors.Is(err, pagecursor.ErrBad) {
		writeError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	if err != nil {
		log.Printf("listConversations error: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list conversations")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// POST /conversations/read {"with_user_id", "seq"} — mark the conversation read up to seq.
func (h *MessageHandler) markReadHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req struct {
		WithUserId string `json:"with_user_id"`
		Seq        int64  `json:"seq"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.WithUserId == "" || req.Seq <= 0 {
		writeError(w, http.StatusBadRequest, "with_user_id and a positive seq are required")
		return
	}
	switch err := h.msgSvc.MarkRead(r.Context(), userId, req.WithUserId, req.Seq); {
	case errors.Is(err, service.ErrConversationNotFound):
		writeError(w, http.StatusNotFound, "conversation not found")
	case err != nil:
		log.Printf("markRead error: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to mark read")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"message": "ok"})
	}
}

func optInt(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("bad integer")
	}
	return n, nil
}
