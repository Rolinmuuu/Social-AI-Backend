package utils

import (
	"errors"
	"fmt"
	"net/http"

	jwt "github.com/form3tech-oss/jwt-go"
)

// GetUserIdFromJwtToken extracts the user_id claim from the JWT token stored
// in the request context by go-jwt-middleware.
func GetUserIdFromJwtToken(r *http.Request) (string, error) {
	token := r.Context().Value("user")
	jwtToken, ok := token.(*jwt.Token)
	if !ok || jwtToken == nil {
		return "", errors.New("unauthorized: missing or invalid token")
	}
	claims, ok := jwtToken.Claims.(jwt.MapClaims)
	if !ok {
		return "", errors.New("unauthorized: invalid token claims")
	}
	userId, ok := claims["user_id"].(string)
	if !ok || userId == "" {
		return "", errors.New("unauthorized: user_id not found in token")
	}
	return userId, nil
}

// UserFeedCacheKey returns the Redis key for a user's feed cache.
func UserFeedCacheKey(userId string) string {
	return fmt.Sprintf("user_feed:%s", userId)
}
