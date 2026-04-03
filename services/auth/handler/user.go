package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"socialai/services/auth/service"
	"socialai/shared/model"

	jwt "github.com/form3tech-oss/jwt-go"
)

var validUserIdRegex = regexp.MustCompile(`^[a-z0-9_]+$`)

// AuthHandler holds dependencies for auth HTTP handlers.
type AuthHandler struct {
	userSvc   *service.UserService
	jwtSecret []byte
}

func NewAuthHandler(userSvc *service.UserService, jwtSecret []byte) *AuthHandler {
	return &AuthHandler{userSvc: userSvc, jwtSecret: jwtSecret}
}

func (h *AuthHandler) signupHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var user model.User
	if err := json.NewDecoder(r.Body).Decode(&user); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if user.UserId == "" || user.Password == "" || !validUserIdRegex.MatchString(user.UserId) {
		http.Error(w, `{"error":"user_id must be non-empty lowercase alphanumeric/underscore, password required"}`, http.StatusBadRequest)
		return
	}

	if err := h.userSvc.AddUser(&user); err != nil {
		if errors.Is(err, service.ErrUserAlreadyExisted) {
			http.Error(w, `{"error":"user already exists"}`, http.StatusConflict)
			return
		}
		fmt.Printf("signup internal error: %v\n", err)
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"user_id": user.UserId})
}

func (h *AuthHandler) signinHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if len(h.jwtSecret) == 0 {
		http.Error(w, `{"error":"server misconfiguration"}`, http.StatusInternalServerError)
		return
	}

	var user model.User
	if err := json.NewDecoder(r.Body).Decode(&user); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if err := h.userSvc.CheckUser(user.UserId, user.Password); err != nil {
		if errors.Is(err, service.ErrInvalidCredentials) {
			// Return 401 with a generic message — do not distinguish "user not found" vs "wrong password"
			http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
			return
		}
		fmt.Printf("signin internal error: %v\n", err)
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": user.UserId,
		"exp":     time.Now().Add(24 * time.Hour).Unix(),
	})
	tokenString, err := token.SignedString(h.jwtSecret)
	if err != nil {
		http.Error(w, `{"error":"failed to generate token"}`, http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"token": tokenString})
}
