package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"socialai/services/message/service"
	"socialai/shared/utils"
)

// MessageHandler handles private messaging HTTP requests.
type MessageHandler struct {
	msgSvc *service.MessageService
}

func NewMessageHandler(msgSvc *service.MessageService) *MessageHandler {
	return &MessageHandler{msgSvc: msgSvc}
}

func (h *MessageHandler) sendMessageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	senderId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		ReceiverId string `json:"receiver_id"`
		Content    string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.ReceiverId == "" || req.Content == "" {
		http.Error(w, `{"error":"receiver_id and content are required"}`, http.StatusBadRequest)
		return
	}

	messageId, err := h.msgSvc.SendMessage(senderId, req.ReceiverId, req.Content)
	if err != nil {
		if errors.Is(err, service.ErrCannotMessageSelf) {
			http.Error(w, `{"error":"cannot send message to yourself"}`, http.StatusBadRequest)
			return
		}
		fmt.Printf("sendMessage error: %v\n", err)
		http.Error(w, `{"error":"failed to send message"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"message_id": messageId})
}

func (h *MessageHandler) getMessageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	withUserId := r.URL.Query().Get("with_user_id")
	if withUserId == "" {
		http.Error(w, `{"error":"with_user_id query param is required"}`, http.StatusBadRequest)
		return
	}

	messages, err := h.msgSvc.GetMessages(userId, withUserId)
	if err != nil {
		http.Error(w, `{"error":"failed to get messages"}`, http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"messages": messages})
}
