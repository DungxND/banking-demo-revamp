// Package service provides shared scaffolding for all banking-demo services:
// infrastructure dependency initialisation (Deps/InitDeps) and the three-goroutine
// run-loop (Runner) that drives the NATS consumer, HTTP server, and OS signal handler.
//
// Each binary's run() function is responsible only for service-specific wiring
// (handler registration, router setup, server config) and then delegates lifecycle
// management here.
package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stephenafamo/bob"
	"golang.org/x/sync/errgroup"

	internnats "banking-demo/internal/nats"
	"banking-demo/internal/db"
	iredis "banking-demo/internal/redis"
)

// Deps holds the live infrastructure clients shared by all services.
// Use InitDeps to construct it; defer the returned cleanup function.
type Deps struct {
	Pool        *pgxpool.Pool
	BDB         bob.DB
	RedisClient *iredis.Client
}

// InitDeps opens the database pool and Redis client, then returns a cleanup
// function that closes both in the correct order (Redis then pool).
// On a partial failure the already-opened resource is closed before returning.
// databaseURL is the PostgreSQL connection string; if empty, DATABASE_URL env var is used.
func InitDeps(ctx context.Context, databaseURL, redisURL string, logger *slog.Logger) (Deps, func(), error) {
	pool, err := db.NewPool(ctx, databaseURL)
	if err != nil {
		logger.Error("db_init_failed", "error", err.Error())
		return Deps{}, func() {}, err
	}

	redisClient, err := iredis.NewClient(redisURL)
	if err != nil {
		pool.Close()
		logger.Error("redis_init_failed", "error", err.Error())
		return Deps{}, func() {}, err
	}

	cleanup := func() {
		redisClient.Close()
		pool.Close()
	}
	return Deps{Pool: pool, BDB: db.NewBobDB(pool), RedisClient: redisClient}, cleanup, nil
}

// LogPoolStatus logs the pgxpool statistics at startup.
// Convenience wrapper so callers don't need to import banking-demo/internal/db directly.
func (d Deps) LogPoolStatus(logger *slog.Logger) {
	db.LogPoolStatus(d.Pool, logger)
}

// errShutdown is returned by the signal goroutine to trigger errgroup
// cancellation and propagate a clean shutdown to all other goroutines.
// It is an internal implementation detail; callers of Runner.Run receive nil on
// clean shutdown.
var errShutdown = errors.New("shutdown")

// shutdownTimeout is the maximum time the HTTP server is given to drain
// in-flight requests after a shutdown signal is received.
const shutdownTimeout = 10 * time.Second

// Runner manages the three-goroutine lifecycle shared by every service:
//  1. NATS consumer (nats.go reconnects automatically; blocks until ctx cancelled).
//  2. HTTP server (returns on ErrServerClosed or a real error).
//  3. OS signal handler (initiates graceful HTTP drain, then cancels the group).
//
// Construct with NewRunner; call Run to block until the service exits.
type Runner struct {
	consumer *internnats.Consumer
	server   *http.Server
	logger   *slog.Logger
}

// NewRunner creates a Runner. consumer and server must be fully configured
// before calling NewRunner; Run does not modify them.
func NewRunner(consumer *internnats.Consumer, server *http.Server, logger *slog.Logger) *Runner {
	return &Runner{consumer: consumer, server: server, logger: logger}
}

// Run starts all three goroutines and blocks until all have exited.
// It returns nil on a clean shutdown (SIGINT/SIGTERM or ctx cancellation)
// and a non-nil error if the HTTP server fails unexpectedly.
func (r *Runner) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	// Goroutine 1: NATS consumer — nats.go reconnects automatically.
	g.Go(func() error {
		r.consumer.Run(ctx)
		return nil
	})

	// Goroutine 2: HTTP server.
	g.Go(func() error {
		r.logger.Info("http_server_started", "addr", r.server.Addr)
		if err := r.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	// Goroutine 3: OS signal → graceful HTTP drain → group cancellation.
	// Returns errShutdown (not nil) so that the errgroup context is cancelled,
	// which causes consumer.Run and ListenAndServe to exit.
	// shutdownCtx uses WithoutCancel so the drain window is not itself cancelled
	// by the group cancellation that errShutdown triggers.
	g.Go(func() error {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		select {
		case sig := <-sigCh:
			r.logger.Info("shutdown_signal_received", "signal", sig.String())
		case <-ctx.Done():
		}
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		_ = r.server.Shutdown(shutdownCtx)
		return errShutdown
	})

	if err := g.Wait(); !errors.Is(err, errShutdown) && err != nil {
		r.logger.Error("fatal", "error", err.Error())
		return err
	}
	return nil
}
