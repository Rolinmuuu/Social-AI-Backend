package handler

import (
	"net/http"
	"os"

	"socialai/services/message/service"
	"socialai/shared/middleware"

	jwtMiddleware "github.com/auth0/go-jwt-middleware"
	jwt "github.com/form3tech-oss/jwt-go"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func InitRouter(pool *pgxpool.Pool) http.Handler {
	jwtSecret := []byte(os.Getenv("JWT_SECRET"))

	msgSvc := service.NewMessageService(pool)
	h := NewMessageHandler(msgSvc)

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

	router.Use(middleware.RateLimitMiddleware)
	router.Use(middleware.MetricsMiddleware)
	router.Use(middleware.LoggingMiddleware)

	// POST /message                    → send a message (Idempotency-Key supported)
	// GET  /message?with_user_id=...    → a window of the conversation (before_seq / after_seq)
	// GET  /conversations               → inbox with unread counts
	// POST /conversations/read          → move the read marker
	router.Handle("/message", jwtAuth.Handler(http.HandlerFunc(h.sendMessageHandler))).Methods("POST")
	router.Handle("/message", jwtAuth.Handler(http.HandlerFunc(h.getMessageHandler))).Methods("GET")
	router.Handle("/conversations", jwtAuth.Handler(http.HandlerFunc(h.listConversationsHandler))).Methods("GET")
	router.Handle("/conversations/read", jwtAuth.Handler(http.HandlerFunc(h.markReadHandler))).Methods("POST")

	origins := handlers.AllowedOrigins([]string{"*"})
	methods := handlers.AllowedMethods([]string{"GET", "POST", "OPTIONS"})
	headers := handlers.AllowedHeaders([]string{"Content-Type", "Authorization", "Idempotency-Key"})

	return handlers.CORS(origins, methods, headers)(router)
}
