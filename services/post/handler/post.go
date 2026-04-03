package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"socialai/services/post/service"
	"socialai/shared/model"
	"socialai/shared/utils"

	jwt "github.com/form3tech-oss/jwt-go"
	"github.com/gorilla/mux"
	"github.com/google/uuid"
)

var mediaTypes = map[string]string{
	".jpg":  "image",
	".jpeg": "image",
	".gif":  "image",
	".png":  "image",
	".mp4":  "video",
	".avi":  "video",
	".mov":  "video",
	".flv":  "video",
	".wmv":  "video",
}

// PostHandler holds the post service dependency.
type PostHandler struct {
	postSvc *service.PostService
}

func NewPostHandler(postSvc *service.PostService) *PostHandler {
	return &PostHandler{postSvc: postSvc}
}

func (h *PostHandler) uploadPostHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	token := r.Context().Value("user")
	claims := token.(*jwt.Token).Claims.(jwt.MapClaims)
	userId := claims["user_id"].(string)

	p := model.Post{
		PostId:  uuid.New().String(),
		UserId:  userId,
		Message: r.FormValue("message"),
	}

	file, header, err := r.FormFile("media_file")
	if err != nil {
		http.Error(w, `{"error":"media_file is required"}`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	suffix := filepath.Ext(header.Filename)
	if t, ok := mediaTypes[suffix]; ok {
		p.Type = t
	} else {
		p.Type = "unknown"
	}

	if err := h.postSvc.SavePost(&p, file); err != nil {
		fmt.Printf("upload error: %v\n", err)
		http.Error(w, `{"error":"failed to save post"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"post_id": p.PostId})
}

func (h *PostHandler) searchPostHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	userId := r.URL.Query().Get("user_id")
	keywords := r.URL.Query().Get("keywords")

	var posts []model.Post
	var err error
	if userId != "" {
		posts, err = h.postSvc.SearchPostByUserId(userId)
	} else {
		posts, err = h.postSvc.SearchPostByKeywords(keywords)
	}
	if err != nil {
		http.Error(w, `{"error":"failed to search posts"}`, http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"posts": posts})
}

func (h *PostHandler) deletePostHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	postId := mux.Vars(r)["id"]
	if postId == "" {
		http.Error(w, `{"error":"post id required"}`, http.StatusBadRequest)
		return
	}

	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	deleted, err := h.postSvc.DeletePost(postId, userId)
	if err != nil {
		if errors.Is(err, service.ErrPostNotFound) {
			http.Error(w, `{"error":"post not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"failed to delete post"}`, http.StatusInternalServerError)
		return
	}
	if !deleted {
		http.Error(w, `{"error":"post not found"}`, http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"message": "post deleted, cleanup in progress"})
}

func (h *PostHandler) likePostHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	postId := mux.Vars(r)["id"]
	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	liked, err := h.postSvc.LikePost(postId, userId)
	if err != nil {
		if errors.Is(err, service.ErrAlreadyLiked) {
			http.Error(w, `{"error":"post already liked"}`, http.StatusConflict)
			return
		}
		if errors.Is(err, service.ErrPostNotFound) {
			http.Error(w, `{"error":"post not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"failed to like post"}`, http.StatusInternalServerError)
		return
	}
	if !liked {
		http.Error(w, `{"error":"post already liked"}`, http.StatusConflict)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"message": "post liked"})
}

func (h *PostHandler) sharePostHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	postId := mux.Vars(r)["id"]
	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		Platform string `json:"platform"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Platform == "" {
		req.Platform = "external"
	}

	shared, err := h.postSvc.SharePost(postId, userId, req.Platform)
	if err != nil {
		if errors.Is(err, service.ErrPostNotFound) {
			http.Error(w, `{"error":"post not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"failed to share post"}`, http.StatusInternalServerError)
		return
	}
	if !shared {
		http.Error(w, `{"error":"post not found"}`, http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"message": "post shared"})
}

func (h *PostHandler) addCommentToPostHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	postId := mux.Vars(r)["id"]
	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var req struct {
		Content         string `json:"content"`
		ParentCommentId string `json:"parent_comment_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	commentId, err := h.postSvc.AddComment(postId, req.ParentCommentId, userId, req.Content)
	if err != nil {
		if errors.Is(err, service.ErrPostNotFound) {
			http.Error(w, `{"error":"post not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"failed to add comment"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"comment_id": commentId})
}
