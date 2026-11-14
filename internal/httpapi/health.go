// Package httpapi holds the HTTP surface of the service.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"listing-moderation/internal/seed"
)

// Check is one dependency /readyz depends on.
type Check struct {
	Name string
	Fn   func(context.Context) error
}

// Deps is everything the handler needs. Seed and Store may be nil when only the
// probes are wanted (as in the probe tests).
type Deps struct {
	Log       *slog.Logger
	Seed      *seed.Data
	Store     ListingStore
	Readiness []Check
}

// readyTimeout bounds the whole /readyz evaluation so a hung dependency makes
// the probe fail instead of hanging.
const readyTimeout = 2 * time.Second

type health struct {
	log    *slog.Logger
	checks []Check
}

// NewHandler returns the service's HTTP handler.
func NewHandler(d Deps) http.Handler {
	mux := http.NewServeMux()

	h := &health{log: d.Log, checks: d.Readiness}
	mux.HandleFunc("GET /livez", h.livez)
	mux.HandleFunc("GET /readyz", h.readyz)

	if d.Store != nil {
		a := &listingAPI{log: d.Log, seed: d.Seed, store: d.Store}
		mux.HandleFunc("POST /v1/listings", a.create)
		mux.HandleFunc("GET /v1/listings", a.list)
		mux.HandleFunc("GET /v1/listings/{id}", a.get)
		mux.HandleFunc("POST /v1/listings/{id}/decision", a.decide)
	}
	return withMiddleware(d.Log, mux)
}

// livez answers "is this process running and able to serve a request?". It
// deliberately checks no dependency: if it did, a database outage would make
// an orchestrator restart every instance, which cannot fix the database.
func (h *health) livez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// readyz answers "should this instance receive traffic right now?". It runs
// every readiness check and reports each result. Details of a failure go to
// the log, not the response, so the probe does not leak internals.
func (h *health) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()

	results := make(map[string]string, len(h.checks))
	ok := true
	for _, c := range h.checks {
		if err := c.Fn(ctx); err != nil {
			ok = false
			results[c.Name] = "failed"
			h.log.Warn("readiness check failed", "check", c.Name, "error", err.Error())
			continue
		}
		results[c.Name] = "ok"
	}

	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "checks": results})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "checks": results})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends the API's Error schema: {"error": {"code", "message"}}.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
