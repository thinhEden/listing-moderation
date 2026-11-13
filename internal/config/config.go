// Package config reads process configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"time"
)

// Config is the validated runtime configuration.
type Config struct {
	// HTTPAddr is the listen address inside the process. The published host
	// port is decided by docker compose (PORT), not here.
	HTTPAddr string
	// DatabaseURL is a Postgres connection URL. Required.
	DatabaseURL string
	// SeedDir holds sellers.json, categories.json and banned_keywords.json.
	SeedDir string
	// DBWait bounds how long startup waits for Postgres before giving up.
	DBWait time.Duration
	// ShutdownTimeout bounds graceful shutdown after SIGTERM/SIGINT.
	ShutdownTimeout time.Duration
}

// Lookup matches os.LookupEnv so tests can inject a fake environment.
type Lookup func(key string) (string, bool)

// Load builds a Config from the given environment lookup.
func Load(lookup Lookup) (Config, error) {
	c := Config{
		HTTPAddr:        ":8080",
		SeedDir:         "seed",
		DBWait:          60 * time.Second,
		ShutdownTimeout: 15 * time.Second,
	}
	var errs []error

	str := func(key string, dst *string, required bool) {
		if v, ok := lookup(key); ok && v != "" {
			*dst = v
		} else if required {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
	}
	dur := func(key string, dst *time.Duration) {
		v, ok := lookup(key)
		if !ok || v == "" {
			return
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be a positive duration such as 30s, got %q", key, v))
			return
		}
		*dst = d
	}

	str("HTTP_ADDR", &c.HTTPAddr, false)
	str("DATABASE_URL", &c.DatabaseURL, true)
	str("SEED_DIR", &c.SeedDir, false)
	dur("DB_WAIT", &c.DBWait)
	dur("SHUTDOWN_TIMEOUT", &c.ShutdownTimeout)

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}
