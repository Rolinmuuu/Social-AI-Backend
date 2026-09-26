package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strconv"

	"socialai/services/post/service"
	"socialai/shared/idempotency"
	"socialai/shared/model"
	"socialai/shared/utils"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
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

// writeJSON writes status and body as JSON.
func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// withIdempotency runs fn at most once per (user, Idempotency-Key header). Retries of a
// finished request get the stored response (header Idempotent-Replayed: true); a retry while
// the first attempt is still running gets 409; reusing a key for a different request gets
// 422. Without the header, fn just runs. Only 2xx responses are stored, so a failed attempt
// can be retried with the same key.
func (h *PostHandler) withIdempotency(w http.ResponseWriter, r *http.Request, scope, userId, fingerprint string, fn func() (int, interface{})) {
	key := r.Header.Get("Idempotency-Key")
	store := h.postSvc.Idempotency
	if key == "" || store == nil {
		status, body := fn()
		writeJSON(w, status, body)
		return
	}
	if len(key) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key too long"})
		return
	}
	// Detached from the request: if the client disconnects mid-request, the work still
	// finishes and its result must still be recorded (or the claim released), otherwise the
	// retry that follows would see 409 until the lock expires and then run the work again.
	ctx := context.WithoutCancel(r.Context())
	replay, err := store.Begin(ctx, scope, userId, key, fingerprint)
	switch {
	case errors.Is(err, idempotency.ErrInProgress):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a request with this Idempotency-Key is in progress"})
		return
	case errors.Is(err, idempotency.ErrKeyReused):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Idempotency-Key was used for a different request"})
		return
	case err != nil:
		// Redis unavailable: serve the request rather than fail it; log that it was unprotected.
		log.Printf("idempotency disabled for this request: %v", err)
		status, body := fn()
		writeJSON(w, status, body)
		return
	case replay != nil:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotent-Replayed", "true")
		w.WriteHeader(replay.Status)
		_, _ = w.Write(replay.Body)
		return
	}

	status, body := fn()
	data, _ := json.Marshal(body)
	if status >= 200 && status < 300 {
		if err := store.Complete(ctx, scope, userId, key, fingerprint, idempotency.Response{Status: status, Body: data}); err != nil {
			log.Printf("idempotency: store response: %v", err)
		}
	} else if err := store.Release(ctx, scope, userId, key); err != nil {
		log.Printf("idempotency: release: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func (h *PostHandler) uploadPostHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	p := model.Post{
		PostId:  uuid.New().String(),
		UserId:  userId,
		Message: r.FormValue("message"),
	}

	file, header, err := r.FormFile("media_file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "media_file is required"})
		return
	}
	defer file.Close()

	suffix := filepath.Ext(header.Filename)
	if t, ok := mediaTypes[suffix]; ok {
		p.Type = t
	} else {
		p.Type = "unknown"
	}

	fp := idempotency.Fingerprint(p.Message, header.Filename, strconv.FormatInt(header.Size, 10))
	h.withIdempotency(w, r, "upload", userId, fp, func() (int, interface{}) {
		// Detached from the connection, like the idempotency bookkeeping: a client that gives
		// up mid-upload must not roll back a post whose retry will be answered from the store.
		if err := h.postSvc.SavePost(context.WithoutCancel(r.Context()), &p, file); err != nil {
			log.Printf("upload error: %v", err)
			return http.StatusInternalServerError, map[string]string{"error": "failed to save post"}
		}
		return http.StatusCreated, map[string]string{"post_id": p.PostId}
	})
}

// GET /feed?limit=20&cursor=... — the signed-in user's home feed.
func (h *PostHandler) homeFeedHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, err := h.postSvc.GetHomeFeed(r.Context(), userId, limit, r.URL.Query().Get("cursor"))
	if errors.Is(err, service.ErrBadCursor) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
		return
	}
	if err != nil {
		log.Printf("feed error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load feed"})
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *PostHandler) searchPostHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	userId := r.URL.Query().Get("user_id")
	keywords := r.URL.Query().Get("keywords")
	mode := r.URL.Query().Get("mode")

	var posts []model.Post
	var err error
	if userId != "" {
		posts, err = h.postSvc.SearchPostByUserId(r.Context(), userId)
	} else if mode == "semantic" && keywords != "" {
		posts, err = h.postSvc.SemanticSearch(r.Context(), keywords, 20)
	} else {
		posts, err = h.postSvc.SearchPostByKeywords(r.Context(), keywords)
	}
	if err != nil {
		log.Printf("search error: %v", err)
		http.Error(w, `{"error":"failed to search posts"}`, http.StatusInternalServerError)
		return
	}
	public := make([]model.Post, 0, len(posts))
	for _, p := range posts {
		public = append(public, p.Public())
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"posts": public})
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

	deleted, err := h.postSvc.DeletePost(r.Context(), postId, userId)
	if err != nil {
		if errors.Is(err, service.ErrPostNotFound) {
			http.Error(w, `{"error":"post not found"}`, http.StatusNotFound)
			return
		}
		if errors.Is(err, service.ErrNotPostOwner) {
			http.Error(w, `{"error":"only the author can delete this post"}`, http.StatusForbidden)
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

	liked, err := h.postSvc.LikePost(r.Context(), postId, userId)
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

	shared, err := h.postSvc.SharePost(r.Context(), postId, userId, req.Platform)
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

	commentId, err := h.postSvc.AddComment(r.Context(), postId, req.ParentCommentId, userId, req.Content)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidComment):
			http.Error(w, `{"error":"comment must be 1-2000 characters"}`, http.StatusBadRequest)
		case errors.Is(err, service.ErrPostNotFound):
			http.Error(w, `{"error":"post not found"}`, http.StatusNotFound)
		case errors.Is(err, service.ErrCommentNotFound):
			http.Error(w, `{"error":"parent comment not found on this post"}`, http.StatusNotFound)
		default:
			log.Printf("comment error: %v", err)
			http.Error(w, `{"error":"failed to add comment"}`, http.StatusInternalServerError)
		}
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"comment_id": commentId})
}

func (h *PostHandler) generateImageFromOpenAIHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Image generation is slow and paid: a client retry after a timeout must not generate
	// and publish a second image. Clients send an Idempotency-Key per prompt submission.
	h.withIdempotency(w, r, "generate", userId, idempotency.Fingerprint(req.Prompt), func() (int, interface{}) {
		// Not tied to the client connection: a paid generation that was started is finished
		// and recorded, so the client's retry gets the stored result instead of a new image.
		post, err := h.postSvc.GenerateImageFromOpenAIAndSavePost(context.WithoutCancel(r.Context()), userId, req.Prompt)
		if err != nil {
			return http.StatusInternalServerError, map[string]string{"error": "failed to generate image"}
		}
		return http.StatusCreated, post.Public()
	})
}

// DELETE /post/{id}/like — remove the caller's like.
func (h *PostHandler) unlikePostHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := utils.GetUserIdFromJwtToken(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	switch err := h.postSvc.UnlikePost(r.Context(), mux.Vars(r)["id"], userId); {
	case errors.Is(err, service.ErrNotLiked):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "post not liked"})
	case err != nil:
		log.Printf("unlike error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to unlike post"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"message": "like removed"})
	}
}

// GET /post/{id}/comments?limit=&cursor= — comments on a post, oldest first.
func (h *PostHandler) listCommentsHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := utils.GetUserIdFromJwtToken(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, err := h.postSvc.ListComments(r.Context(), mux.Vars(r)["id"], limit, r.URL.Query().Get("cursor"))
	switch {
	case errors.Is(err, service.ErrBadCursor):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
	case errors.Is(err, service.ErrPostNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "post not found"})
	case err != nil:
		log.Printf("list comments error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load comments"})
	default:
		writeJSON(w, http.StatusOK, page)
	}
}
