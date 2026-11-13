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
