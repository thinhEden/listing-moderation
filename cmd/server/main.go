// Command server runs the listing moderation API and its worker pool in one process.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"listing-moderation/internal/config"
	"listing-moderation/internal/httpapi"
	"listing-moderation/internal/rules"
	"listing-moderation/internal/seed"
	"listing-moderation/internal/store"
	"listing-moderation/internal/worker"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

// run wires the service together. Startup order matters: everything the
// process needs (seed, database, schema) is ready before it opens the port or
// starts the workers, so a listening process is a usable one and /readyz only
// has to track dependencies that can fail later.
func run(log *slog.Logger) error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	data, err := seed.Load(cfg.SeedDir)
	if err != nil {
		return err
	}
	log.Info("seed loaded", "sellers", len(data.Sellers), "categories", len(data.Categories), "banned_keywords", len(data.Keywords))
	engine, err := rules.NewEngine(data)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.DBWait, log)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}

	pool := worker.New(worker.Config{
		Workers:      cfg.Workers,
		PollInterval: cfg.PollInterval,
		Lease:        cfg.LeaseTime,
		MaxAttempts:  cfg.MaxAttempts,
		BackoffBase:  cfg.BackoffBase,
		BackoffMax:   cfg.BackoffMax,
	}, st, engine, log)

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewHandler(httpapi.Deps{
			Log:   log,
			Seed:  data,
			Store: st,
			Readiness: []httpapi.Check{
				{Name: "database", Fn: st.Ping},
				{Name: "worker", Fn: pool.Check},
			},
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	pool.Start()
	log.Info("workers started", "workers", cfg.Workers, "max_attempts", cfg.MaxAttempts, "lease", cfg.LeaseTime.String())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.HTTPAddr)

	var serveFailed error
	select {
	case err := <-serveErr:
		serveFailed = err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Stop taking requests and stop claiming work at the same time, then wait
	// for both, within one deadline. Work still in flight when it passes is not
	// lost: its transaction rolls back and the lease expiry hands it to a later
	// attempt, possibly in another process.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	var (
		wg                 sync.WaitGroup
		httpErr, workerErr error
	)
	wg.Add(2)
	go func() { defer wg.Done(); httpErr = srv.Shutdown(shutdownCtx) }()
	go func() { defer wg.Done(); workerErr = pool.Shutdown(shutdownCtx) }()
	wg.Wait()

	if serveFailed != nil && !errors.Is(serveFailed, http.ErrServerClosed) {
		return serveFailed
	}
	if httpErr != nil && !errors.Is(httpErr, http.ErrServerClosed) {
		return fmt.Errorf("shutdown http: %w", httpErr)
	}
	if workerErr != nil {
		log.Warn("worker shutdown incomplete", "error", workerErr.Error())
	}
	return nil
}
