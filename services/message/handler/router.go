package handler

import (
	"net/http"
	"os"

	"socialai/services/message/service"
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

	msgSvc := service.NewMessageService(esBackend)
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

	// POST /message              → send a message
	// GET  /message?with_user_id → get conversation with a user
	router.Handle("/message", jwtAuth.Handler(http.HandlerFunc(h.sendMessageHandler))).Methods("POST")
	router.Handle("/message", jwtAuth.Handler(http.HandlerFunc(h.getMessageHandler))).Methods("GET")

	origins := handlers.AllowedOrigins([]string{"*"})
	methods := handlers.AllowedMethods([]string{"GET", "POST", "OPTIONS"})
	headers := handlers.AllowedHeaders([]string{"Content-Type", "Authorization"})

	return handlers.CORS(origins, methods, headers)(router)
}
