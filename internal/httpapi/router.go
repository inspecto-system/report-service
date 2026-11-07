package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// readinessTimeout bounds the dependency checks of /health/ready.
const readinessTimeout = 2 * time.Second

// Pinger checks that a dependency is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Dependencies are the parts the HTTP layer works with.
type Dependencies struct {
	Logger *slog.Logger
	// Ready lists the dependencies checked by /health/ready.
	Ready []Pinger
}

// NewRouter builds the HTTP handler of the service. Report endpoints under /api/v1 (routed by nginx,
// ADR-013) and /internal/v1 (service tokens only) arrive with T-402.
func NewRouter(deps Dependencies) http.Handler {
	r := chi.NewRouter()
	r.Use(withRequestID, withSecurityHeaders, withAccessLog(deps.Logger), withRecovery(deps.Logger))
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, http.StatusNotFound, CodeNotFound, "Resource not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, http.StatusMethodNotAllowed, CodeValidation, "Method not allowed")
	})
	r.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "UP"})
	})
	r.Get("/health/ready", readiness(deps.Logger, deps.Ready))
	return r
}

// readiness reports UP when every dependency answers; otherwise DOWN with 503, so that docker
// compose and nginx stop routing to the instance. The cause is logged, not returned.
func readiness(logger *slog.Logger, dependencies []Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		for _, dependency := range dependencies {
			if err := dependency.Ping(ctx); err != nil {
				logger.WarnContext(r.Context(), "dependency is not ready", slog.Any("error", err))
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "DOWN"})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "UP"})
	}
}
