package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func get(t *testing.T, h http.Handler, method, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	var body map[string]any
	if rec.Body.Len() > 0 && strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response is not JSON: %v: %s", err, rec.Body.String())
		}
	}
	return rec, body
}

func TestLivezIsOKEvenWhenDependenciesAreDown(t *testing.T) {
	down := Check{Name: "database", Fn: func(context.Context) error { return errors.New("down") }}
	rec, body := get(t, NewHandler(Deps{Log: quiet(), Readiness: []Check{down}}), http.MethodGet, "/livez")
	if rec.Code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("livez: %d %v", rec.Code, body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
}

func TestReadyzOKWhenAllChecksPass(t *testing.T) {
	up := Check{Name: "database", Fn: func(context.Context) error { return nil }}
	rec, body := get(t, NewHandler(Deps{Log: quiet(), Readiness: []Check{up}}), http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("readyz: %d %v", rec.Code, body)
	}
	if checks := body["checks"].(map[string]any); checks["database"] != "ok" {
		t.Fatalf("checks: %v", checks)
	}
}

func TestReadyz503WhenACheckFailsAndDoesNotLeakTheError(t *testing.T) {
	bad := Check{Name: "database", Fn: func(context.Context) error { return errors.New("secret-host:5432 refused") }}
	good := Check{Name: "other", Fn: func(context.Context) error { return nil }}
	rec, body := get(t, NewHandler(Deps{Log: quiet(), Readiness: []Check{good, bad}}), http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable || body["status"] != "unavailable" {
		t.Fatalf("readyz: %d %v", rec.Code, body)
	}
	checks := body["checks"].(map[string]any)
	if checks["database"] != "failed" || checks["other"] != "ok" {
		t.Fatalf("checks: %v", checks)
	}
	if strings.Contains(rec.Body.String(), "secret-host") {
		t.Fatalf("error detail leaked: %s", rec.Body.String())
	}
}

func TestReadyzFailsInsteadOfHangingOnASlowCheck(t *testing.T) {
	slow := Check{Name: "database", Fn: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	start := time.Now()
	rec, _ := get(t, NewHandler(Deps{Log: quiet(), Readiness: []Check{slow}}), http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	if d := time.Since(start); d > readyTimeout+time.Second {
		t.Fatalf("probe took %s", d)
	}
}

func TestReadyzWithNoChecksIsReady(t *testing.T) {
	rec, _ := get(t, NewHandler(Deps{Log: quiet()}), http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestProbesOnlyAnswerGET(t *testing.T) {
	h := NewHandler(Deps{Log: quiet()})
	for _, p := range []string{"/livez", "/readyz"} {
		rec, _ := get(t, h, http.MethodPost, p)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s: status %d", p, rec.Code)
		}
	}
}
