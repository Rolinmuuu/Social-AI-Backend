package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"socialai/services/social/service"
	"socialai/shared/utils"
)

// SocialHandler handles follow/unfollow HTTP requests.
type SocialHandler struct {
	socialSvc *service.SocialService
}

func NewSocialHandler(socialSvc *service.SocialService) *SocialHandler {
	return &SocialHandler{socialSvc: socialSvc}
}

func (h *SocialHandler) addFollowHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	followerId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		FolloweeId string `json:"followee_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FolloweeId == "" {
		http.Error(w, `{"error":"followee_id is required"}`, http.StatusBadRequest)
		return
	}

	followId, err := h.socialSvc.AddFollow(followerId, req.FolloweeId)
	if err != nil {
		if errors.Is(err, service.ErrCannotFollowSelf) {
			http.Error(w, `{"error":"cannot follow yourself"}`, http.StatusBadRequest)
			return
		}
		if errors.Is(err, service.ErrAlreadyFollowing) {
			http.Error(w, `{"error":"already following"}`, http.StatusConflict)
			return
		}
		fmt.Printf("addFollow error: %v\n", err)
		http.Error(w, `{"error":"failed to follow user"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"follow_id": followId})
}

func (h *SocialHandler) removeFollowHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	followerId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		FolloweeId string `json:"followee_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FolloweeId == "" {
		http.Error(w, `{"error":"followee_id is required"}`, http.StatusBadRequest)
		return
	}

	if err := h.socialSvc.RemoveFollow(followerId, req.FolloweeId); err != nil {
		if errors.Is(err, service.ErrNotFollowing) {
			http.Error(w, `{"error":"not following this user"}`, http.StatusNotFound)
			return
		}
		fmt.Printf("removeFollow error: %v\n", err)
		http.Error(w, `{"error":"failed to unfollow user"}`, http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"message": "unfollowed successfully"})
}

func (h *SocialHandler) getFollowersHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	ids, err := h.socialSvc.GetFollowerIds(userId)
	if err != nil {
		http.Error(w, `{"error":"failed to get followers"}`, http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string][]string{"follower_ids": ids})
}

func (h *SocialHandler) getFollowingHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	ids, err := h.socialSvc.GetFollowingIds(userId)
	if err != nil {
		http.Error(w, `{"error":"failed to get following"}`, http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string][]string{"following_ids": ids})
}
