package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"socialai/services/social/service"
	"socialai/shared/pagecursor"
	"socialai/shared/utils"
)

// SocialHandler handles follow/unfollow HTTP requests.
type SocialHandler struct {
	socialSvc *service.SocialService
}

func NewSocialHandler(socialSvc *service.SocialService) *SocialHandler {
	return &SocialHandler{socialSvc: socialSvc}
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *SocialHandler) addFollowHandler(w http.ResponseWriter, r *http.Request) {
	followerId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		FolloweeId string `json:"followee_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FolloweeId == "" {
		writeError(w, http.StatusBadRequest, "followee_id is required")
		return
	}

	followId, err := h.socialSvc.AddFollow(r.Context(), followerId, req.FolloweeId)
	switch {
	case errors.Is(err, service.ErrCannotFollowSelf):
		writeError(w, http.StatusBadRequest, "cannot follow yourself")
	case errors.Is(err, service.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, service.ErrAlreadyFollowing):
		writeError(w, http.StatusConflict, "already following")
	case err != nil:
		log.Printf("addFollow error: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to follow user")
	default:
		writeJSON(w, http.StatusCreated, map[string]string{"follow_id": followId})
	}
}

func (h *SocialHandler) removeFollowHandler(w http.ResponseWriter, r *http.Request) {
	followerId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		FolloweeId string `json:"followee_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FolloweeId == "" {
		writeError(w, http.StatusBadRequest, "followee_id is required")
		return
	}

	switch err := h.socialSvc.RemoveFollow(r.Context(), followerId, req.FolloweeId); {
	case errors.Is(err, service.ErrNotFollowing):
		writeError(w, http.StatusNotFound, "not following this user")
	case err != nil:
		log.Printf("removeFollow error: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to unfollow user")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"message": "unfollowed successfully"})
	}
}

// listHandler serves GET /follow/followers and /follow/following.
//
// Query: user_id (default: the caller), limit (default 50, max 200), cursor (next_cursor of
// the previous page). Response: {"<key>": [...ids], "next_cursor": "..."}; next_cursor is
// omitted on the last page.
func (h *SocialHandler) listHandler(key string, list func(ctx context.Context, userId string, limit int, cursor string) (service.FollowPage, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, err := utils.GetUserIdFromJwtToken(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		q := r.URL.Query()
		userId := q.Get("user_id")
		if userId == "" {
			userId = me
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		page, err := list(r.Context(), userId, limit, q.Get("cursor"))
		if errors.Is(err, pagecursor.ErrBad) {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		if err != nil {
			log.Printf("%s error: %v", key, err)
			writeError(w, http.StatusInternalServerError, "failed to get "+key)
			return
		}
		body := map[string]interface{}{key: page.IDs}
		if page.NextCursor != "" {
			body["next_cursor"] = page.NextCursor
		}
		writeJSON(w, http.StatusOK, body)
	}
}
