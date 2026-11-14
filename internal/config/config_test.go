package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) Lookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" || c.SeedDir != "seed" || c.DBWait != 60*time.Second || c.ShutdownTimeout != 15*time.Second {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	// The retry policy agreed for the worker: 5 attempts, 30s lease, backoff from 1s.
	if c.Workers != 4 || c.MaxAttempts != 5 || c.LeaseTime != 30*time.Second || c.BackoffBase != time.Second ||
		c.PollInterval != 250*time.Millisecond || c.BackoffMax != 30*time.Second {
		t.Fatalf("unexpected worker defaults: %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"DATABASE_URL":     "postgres://x",
		"HTTP_ADDR":        ":9000",
		"SEED_DIR":         "/data/seed",
		"DB_WAIT":          "5s",
		"SHUTDOWN_TIMEOUT": "2s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":9000" || c.SeedDir != "/data/seed" || c.DBWait != 5*time.Second || c.ShutdownTimeout != 2*time.Second {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	cases := map[string]struct {
		env  map[string]string
		want string
	}{
		"missing database url": {map[string]string{}, "DATABASE_URL is required"},
		"empty database url":   {map[string]string{"DATABASE_URL": ""}, "DATABASE_URL is required"},
		"bad duration":         {map[string]string{"DATABASE_URL": "x", "DB_WAIT": "soon"}, "DB_WAIT"},
		"zero duration":        {map[string]string{"DATABASE_URL": "x", "SHUTDOWN_TIMEOUT": "0s"}, "SHUTDOWN_TIMEOUT"},
		"negative duration":    {map[string]string{"DATABASE_URL": "x", "DB_WAIT": "-1s"}, "DB_WAIT"},
		"zero workers":         {map[string]string{"DATABASE_URL": "x", "WORKERS": "0"}, "WORKERS"},
		"non-numeric attempts": {map[string]string{"DATABASE_URL": "x", "MAX_ATTEMPTS": "many"}, "MAX_ATTEMPTS"},
		"backoff max < base":   {map[string]string{"DATABASE_URL": "x", "BACKOFF_BASE": "10s", "BACKOFF_MAX": "1s"}, "BACKOFF_MAX"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadWorkerOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"DATABASE_URL": "x", "WORKERS": "2", "MAX_ATTEMPTS": "3", "POLL_INTERVAL": "10ms",
		"LEASE_DURATION": "5s", "BACKOFF_BASE": "20ms", "BACKOFF_MAX": "1s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Workers != 2 || c.MaxAttempts != 3 || c.PollInterval != 10*time.Millisecond ||
		c.LeaseTime != 5*time.Second || c.BackoffBase != 20*time.Millisecond || c.BackoffMax != time.Second {
		t.Fatalf("%+v", c)
	}
}
