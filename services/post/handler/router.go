package handler

import (
	"net/http"

	"socialai/services/post/service"
	"socialai/shared/middleware"

	jwtMiddleware "github.com/auth0/go-jwt-middleware"
	jwt "github.com/form3tech-oss/jwt-go"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// InitRouter wires HTTP routes. Accepts a pre-built PostService so that
// the cleanup goroutine in main.go can share the same instance.
func InitRouter(
	postSvc *service.PostService,
	jwtSecret []byte,
) http.Handler {
	h := NewPostHandler(postSvc)

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

	router.Handle("/upload", jwtAuth.Handler(http.HandlerFunc(h.uploadPostHandler))).Methods("POST")
	router.Handle("/search", jwtAuth.Handler(http.HandlerFunc(h.searchPostHandler))).Methods("GET")
	router.Handle("/post/{id}", jwtAuth.Handler(http.HandlerFunc(h.deletePostHandler))).Methods("DELETE")
	router.Handle("/post/{id}/like", jwtAuth.Handler(http.HandlerFunc(h.likePostHandler))).Methods("POST")
	router.Handle("/post/{id}/share", jwtAuth.Handler(http.HandlerFunc(h.sharePostHandler))).Methods("POST")
	router.Handle("/post/{id}/comment", jwtAuth.Handler(http.HandlerFunc(h.addCommentToPostHandler))).Methods("POST")

	origins := handlers.AllowedOrigins([]string{"*"})
	methods := handlers.AllowedMethods([]string{"GET", "POST", "DELETE", "OPTIONS"})
	headers := handlers.AllowedHeaders([]string{"Content-Type", "Authorization"})

	return handlers.CORS(origins, methods, headers)(router)
}
