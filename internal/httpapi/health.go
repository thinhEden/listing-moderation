// Package httpapi holds the HTTP surface of the service.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Check is one dependency /readyz depends on.
type Check struct {
	Name string
	Fn   func(context.Context) error
}

// readyTimeout bounds the whole /readyz evaluation so a hung dependency makes
// the probe fail instead of hanging.
const readyTimeout = 2 * time.Second

type health struct {
	log    *slog.Logger
	checks []Check
}

// NewHandler returns the service's HTTP handler. Business routes are added to
// the same mux in later steps.
func NewHandler(log *slog.Logger, readiness ...Check) http.Handler {
	h := &health{log: log, checks: readiness}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", h.livez)
	mux.HandleFunc("GET /readyz", h.readyz)
	return mux
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
