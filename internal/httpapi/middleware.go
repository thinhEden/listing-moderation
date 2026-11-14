package httpapi

import (
	"log/slog"
	"net/http"
	"time"
)

// withMiddleware wraps the mux with panic recovery and an access log.
func withMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic in handler", "method", r.Method, "path", r.URL.Path, "panic", p)
				if !rec.wrote {
					writeError(rec, http.StatusInternalServerError, "internal", "internal error")
				}
			}
			// Probes would drown everything else; log them only when they fail.
			if rec.status >= 400 || (r.URL.Path != "/livez" && r.URL.Path != "/readyz") {
				log.Info("request", "method", r.Method, "path", r.URL.Path,
					"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status, r.wrote = code, true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}
