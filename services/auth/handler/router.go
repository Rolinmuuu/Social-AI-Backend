package handler

import (
	"net/http"
	"os"

	"socialai/services/auth/service"
	sharedBackend "socialai/shared/backend"
	"socialai/shared/middleware"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// InitRouter wires up the auth service routes with dependency injection.
// jwtSecret is read once from the environment and closed over in handlers.
func InitRouter(esBackend sharedBackend.ElasticsearchBackendInterface) http.Handler {
	jwtSecret := []byte(os.Getenv("JWT_SECRET"))

	userSvc := service.NewUserService(esBackend)
	h := NewAuthHandler(userSvc, jwtSecret)

	router := mux.NewRouter()
	router.Handle("/metrics", promhttp.Handler()).Methods("GET")
	router.Handle("/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})).Methods("GET")

	router.Use(middleware.RateLimitMiddleware)
	router.Use(middleware.MetricsMiddleware)
	router.Use(middleware.LoggingMiddleware)

	router.Handle("/signup", http.HandlerFunc(h.signupHandler)).Methods("POST")
	router.Handle("/signin", http.HandlerFunc(h.signinHandler)).Methods("POST")

	origins := handlers.AllowedOrigins([]string{"*"})
	methods := handlers.AllowedMethods([]string{"GET", "POST", "OPTIONS"})
	headers := handlers.AllowedHeaders([]string{"Content-Type", "Authorization"})

	return handlers.CORS(origins, methods, headers)(router)
}
