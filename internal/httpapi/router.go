package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Routable is any domain handler that registers itself on the authenticated router.
type Routable interface{ Routes(r chi.Router) }

type RouterOptions struct {
	Log        *slog.Logger
	Auth       func(http.Handler) http.Handler
	Public     []Routable
	Protected  []Routable
	HealthFunc func() error
}

func NewRouter(opts RouterOptions) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(requestLogger(opts.Log))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if opts.HealthFunc != nil {
			if err := opts.HealthFunc(); err != nil {
				JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded"})
				return
			}
		}
		JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Route("/api/v1", func(api chi.Router) {
		for _, h := range opts.Public {
			h.Routes(api)
		}
		api.Group(func(protected chi.Router) {
			protected.Use(opts.Auth)
			for _, h := range opts.Protected {
				h.Routes(protected)
			}
		})
	})
	return r
}

// requestLogger emits structured request logs without any credential material.
func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			log.InfoContext(r.Context(), "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", middleware.GetReqID(r.Context()),
			)
		})
	}
}
