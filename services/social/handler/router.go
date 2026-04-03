package handler

import (
	"net/http"
	"os"

	"socialai/services/social/service"
	sharedBackend "socialai/shared/backend"
	"socialai/shared/middleware"

	jwtMiddleware "github.com/auth0/go-jwt-middleware"
	jwt "github.com/form3tech-oss/jwt-go"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func InitRouter(esBackend sharedBackend.ElasticsearchBackendInterface) http.Handler {
	jwtSecret := []byte(os.Getenv("JWT_SECRET"))

	socialSvc := service.NewSocialService(esBackend)
	h := NewSocialHandler(socialSvc)

	jwtAuth := jwtMiddleware.New(jwtMiddleware.Options{
		ValidationKeyGetter: func(token *jwt.Token) (interface{}, error) {
			return jwtSecret, nil
		},
		SigningMethod: jwt.SigningMethodHS256,
	})

	router := mux.NewRouter()
	router.Handle("/metrics", promhttp.Handler()).Methods("GET")
	router.Handle("/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})).Methods("GET")

	router.Use(middleware.MetricsMiddleware)
	router.Use(middleware.LoggingMiddleware)

	// POST /follow   → follow a user (body: {"followee_id": "..."})
	// DELETE /follow → unfollow a user (body: {"followee_id": "..."})
	// GET /follow/followers → users who follow me
	// GET /follow/following → users I follow
	router.Handle("/follow", jwtAuth.Handler(http.HandlerFunc(h.addFollowHandler))).Methods("POST")
	router.Handle("/follow", jwtAuth.Handler(http.HandlerFunc(h.removeFollowHandler))).Methods("DELETE")
	router.Handle("/follow/followers", jwtAuth.Handler(http.HandlerFunc(h.getFollowersHandler))).Methods("GET")
	router.Handle("/follow/following", jwtAuth.Handler(http.HandlerFunc(h.getFollowingHandler))).Methods("GET")

	origins := handlers.AllowedOrigins([]string{"*"})
	methods := handlers.AllowedMethods([]string{"GET", "POST", "DELETE", "OPTIONS"})
	headers := handlers.AllowedHeaders([]string{"Content-Type", "Authorization"})

	return handlers.CORS(origins, methods, headers)(router)
}
