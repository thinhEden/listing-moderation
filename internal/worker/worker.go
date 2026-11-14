// Package worker runs the automated moderation phase: it claims pending
// listings, screens them for banned content, and commits one result each.
//
// It adds nothing to the correctness guarantees, which live in the store: a
// result is committed with a compare-and-set on status, duplicates are decided
// under the seller lock, and a lease token guards lease updates. What this
// package adds is liveness: stalled work is retried with backoff, and work that
// keeps failing is brought to a human instead of staying pending forever.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"listing-moderation/internal/listing"
	"listing-moderation/internal/rules"
	"listing-moderation/internal/store"
)

// Config tunes the pool.
type Config struct {
	Workers      int           // concurrent workers; each handles one listing at a time
	PollInterval time.Duration // how long an idle worker waits before looking again
	Lease        time.Duration // how long a claim is held before another worker may take it
	MaxAttempts  int           // normal attempts; the next claim takes the retries-exhausted path
	BackoffBase  time.Duration // wait after the first failed attempt; doubles each time
	BackoffMax   time.Duration // upper bound for the wait
}

// Backoff is how long a listing rests after its attempt-th attempt failed:
// BackoffBase, doubling each time, never more than BackoffMax.
func (c Config) Backoff(attempt int) time.Duration {
	d := c.BackoffBase
	for i := 1; i < attempt && d < c.BackoffMax; i++ {
		d *= 2
	}
	if d > c.BackoffMax {
		d = c.BackoffMax
	}
	return d
}

// Store is the part of the store the workers use.
type Store interface {
	ClaimPending(ctx context.Context, limit int, lease time.Duration) ([]store.Claim, error)
	CommitAutomated(ctx context.Context, c store.Claim, decide store.Decide) (rules.Decision, error)
	RecordFailure(ctx context.Context, id, token, message string, backoff time.Duration) error
}

// Screener runs R1. The error is for the screening itself failing; a listing
// that merely contains banned terms is a normal result, not an error.
type Screener func(f listing.Fields) ([]rules.Match, error)

// Option customises a Pool.
type Option func(*Pool)

// WithScreener replaces the R1 screening step, which tests use to make R1 fail.
func WithScreener(s Screener) Option { return func(p *Pool) { p.screen = s } }

// Pool is a fixed set of workers polling for pending listings.
type Pool struct {
	cfg    Config
	store  Store
	engine *rules.Engine
	screen Screener
	log    *slog.Logger

	stop       chan struct{} // closed to stop claiming new work
	hard       context.Context
	cancelHard context.CancelFunc // cancels in-flight database calls
	wg         sync.WaitGroup
	beats      []atomic.Int64 // per worker: unix nanos of the last loop iteration
	state      atomic.Int32   // 0 new, 1 running, 2 stopped
}

func New(cfg Config, st Store, engine *rules.Engine, log *slog.Logger, opts ...Option) *Pool {
	p := &Pool{
		cfg:    cfg,
		store:  st,
		engine: engine,
		log:    log,
		stop:   make(chan struct{}),
		beats:  make([]atomic.Int64, cfg.Workers),
	}
	p.screen = func(f listing.Fields) ([]rules.Match, error) { return p.engine.Screen(f), nil }
	for _, o := range opts {
		o(p)
	}
	p.hard, p.cancelHard = context.WithCancel(context.Background())
	return p
}

// Start launches the workers. Call it once.
func (p *Pool) Start() {
	if !p.state.CompareAndSwap(0, 1) {
		return
	}
	for i := range p.beats {
		p.beats[i].Store(time.Now().UnixNano())
		p.wg.Add(1)
		go p.run(i)
	}
}

// Shutdown stops claiming new listings and waits for the ones in progress to
// finish. If ctx expires first, in-flight database calls are cancelled: the
// transactions roll back and the listings are picked up again when their
// leases run out, so nothing is lost, only delayed.
func (p *Pool) Shutdown(ctx context.Context) error {
	if p.state.Swap(2) == 2 {
		return nil
	}
	close(p.stop)

	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
		p.cancelHard()
		return nil
	case <-ctx.Done():
		p.cancelHard()
		<-done
		return fmt.Errorf("workers did not finish in time; unfinished listings will be retried: %w", ctx.Err())
	}
}

// Check is the readiness check: the pool is running and every worker has gone
// round its loop recently. A worker wedged for longer than two leases (a
// database call that never returns, say) makes the instance not ready.
func (p *Pool) Check(context.Context) error {
	if p.state.Load() != 1 {
		return errors.New("worker pool is not running")
	}
	limit := 2*p.cfg.Lease + p.cfg.PollInterval
	now := time.Now()
	for i := range p.beats {
		if age := now.Sub(time.Unix(0, p.beats[i].Load())); age > limit {
			return fmt.Errorf("worker %d has not made progress for %s", i, age.Round(time.Second))
		}
	}
	return nil
}

func (p *Pool) run(i int) {
	defer p.wg.Done()
	for {
		p.beats[i].Store(time.Now().UnixNano())
		select {
		case <-p.stop:
			return
		default:
		}

		claims, err := p.store.ClaimPending(p.hard, 1, p.cfg.Lease)
		if err != nil {
			if p.hard.Err() == nil {
				p.log.Error("claim failed", "worker", i, "error", err.Error())
			}
			if !p.idle() {
				return
			}
			continue
		}
		if len(claims) == 0 {
			if !p.idle() {
				return
			}
			continue
		}
		p.handle(claims[0])
		p.beats[i].Store(time.Now().UnixNano())
	}
}

// idle waits one poll interval. It reports false if the pool is stopping.
func (p *Pool) idle() bool {
	t := time.NewTimer(p.cfg.PollInterval)
	defer t.Stop()
	select {
	case <-p.stop:
		return false
	case <-t.C:
		return true
	}
}

// handle runs one claimed listing through screening and commit.
//
// Order is the point:
//
//  1. R1 runs first, outside any transaction. If R1 cannot run, nothing is
//     committed and the listing is retried. There is no path from here to an
//     approval, or an escalation, that has not first run R1 successfully.
//  2. The rest of the decision runs inside CommitAutomated, under the seller
//     lock. Normally that is rules.Decide. Once the listing has used up
//     MaxAttempts, this claim is the retries-exhausted attempt and it uses
//     rules.DecideAfterRetries instead: banned content is still rejected, a
//     duplicate is still rejected, and everything else goes to a human with the
//     recorded error as the explanation.
//  3. A failure anywhere records the error and a backoff, guarded by the lease
//     token, and the listing is tried again later.
func (p *Pool) handle(c store.Claim) {
	defer func() {
		if r := recover(); r != nil {
			p.fail(c, fmt.Errorf("panic: %v", r))
		}
	}()

	banned, err := p.screen(c.Fields)
	if err != nil {
		p.fail(c, fmt.Errorf("R1 screening failed: %w", err))
		return
	}

	exhausted := c.Attempts > p.cfg.MaxAttempts
	decide := func(findDuplicate rules.DuplicateFinder) (rules.Decision, error) {
		if exhausted {
			return p.engine.DecideAfterRetries(banned, findDuplicate, c.Attempts-1, c.LastError)
		}
		return p.engine.Decide(c.Fields, banned, findDuplicate)
	}

	d, err := p.store.CommitAutomated(p.hard, c, decide)
	switch {
	case err == nil:
		p.log.Info("listing decided", "listing", c.ID, "status", d.Status, "attempt", c.Attempts, "retries_exhausted", exhausted)
	case errors.Is(err, store.ErrNotPending):
		// Another attempt, or a moderator, got there first. That is fine.
		p.log.Info("listing already decided by another attempt", "listing", c.ID, "attempt", c.Attempts)
	default:
		p.fail(c, err)
	}
}

// fail records why the attempt failed and when to try again. A worker that has
// lost its lease changes nothing.
func (p *Pool) fail(c store.Claim, cause error) {
	if c.Attempts > p.cfg.MaxAttempts {
		// Past the point where the listing should have been escalated: either
		// R1 keeps failing, or the escalation itself cannot be committed.
		p.log.Error("listing cannot be completed; needs operator attention",
			"listing", c.ID, "attempt", c.Attempts, "error", cause.Error())
	} else {
		p.log.Warn("moderation attempt failed; will retry",
			"listing", c.ID, "attempt", c.Attempts, "error", cause.Error())
	}

	err := p.store.RecordFailure(p.hard, c.ID, c.LeaseToken, cause.Error(), p.cfg.Backoff(c.Attempts))
	switch {
	case err == nil:
	case errors.Is(err, store.ErrLeaseLost):
		p.log.Info("lease was taken over; leaving the listing to the newer attempt", "listing", c.ID)
	default:
		// The lease will simply expire and the listing be claimed again.
		p.log.Error("could not record failure", "listing", c.ID, "error", err.Error())
	}
}
