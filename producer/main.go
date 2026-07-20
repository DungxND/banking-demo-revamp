package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-chi/chi/v5"
	chicors "github.com/go-chi/cors"
	instana "github.com/instana/go-sensor"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sync/errgroup"

	"banking-demo/internal/tracing"
)

const serviceName = "api-producer"

type config struct {
	Port            string        `env:"PORT"                 envDefault:"8080"`
	NATSURL         string        `env:"NATS_URL"             envDefault:"nats://nats:4222"`
	ResponseTimeout time.Duration `env:"NATS_RESPONSE_TIMEOUT" envDefault:"60s"`
	CORSOrigins     []string      `env:"CORS_ORIGINS"         envDefault:"http://localhost:3000,https://npd-banking.co,http://npd-banking.co" envSeparator:","`
	OTLPEndpoint    string        `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: parse config: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("service", serviceName)

	shutdownTracing := initTracing(cfg, logger)
	defer shutdownTracing(context.Background())

	m := newMetrics()
	client := newRPCClient(cfg, logger)

	proxy := func(subject string) http.HandlerFunc {
		return proxyHandler(client, m, logger, subject)
	}

	router := chi.NewRouter()
	router.Use(chicors.Handler(chicors.Options{
		AllowedOrigins:   cfg.CORSOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Content-Type", "X-Session", "X-Admin-Secret"},
		AllowCredentials: true,
		MaxAge:           300,
	}))
	router.Handle("/metrics", promhttp.Handler())
	router.Get("/health", healthHandler(client))

	// --- Auth ---
	// POST   /api/users            → create user account (register)
	// POST   /api/sessions         → create session (login)
	// DELETE /api/sessions         → delete current session (logout)
	router.Post("/api/users", proxy("banking.auth.register"))
	router.Post("/api/sessions", proxy("banking.auth.login"))
	router.Delete("/api/sessions", proxy("banking.auth.logout"))

	// --- Account (user-facing, session-guarded at the consumer) ---
	// GET    /api/users/me         → current user's profile
	// GET    /api/users/me/balance → current user's balance (Redis read model)
	// GET    /api/users            → lookup a user by ?account_number=, ?phone=, or ?username=
	router.Get("/api/users/me/balance", proxy("banking.account.balance"))
	router.Get("/api/users/me", proxy("banking.account.me"))
	router.Get("/api/users", proxy("banking.account.lookup"))

	// --- Transfers ---
	// POST   /api/transfers        → initiate a transfer (session-guarded)
	router.Post("/api/transfers", proxy("banking.transfer.transfer"))

	// --- Notifications ---
	// GET    /api/notifications         → list current user's notifications (session-guarded)
	// PATCH  /api/notifications/:id/ack → mark a single notification as read
	router.Get("/api/notifications", proxy("banking.notification.notifications"))
	router.Patch("/api/notifications/{id}/ack", injectPathParam("id", "id", proxy("banking.notification.ack")))

	// --- Admin (session + X-Admin-Secret guarded at the consumer) ---
	// GET    /api/admin/stats                → aggregate platform stats
	// GET    /api/admin/users                → paginated user list (?page, ?size, ?search)
	// GET    /api/admin/users/{id}           → single user detail
	// GET    /api/admin/transfers            → paginated transfer list
	// GET    /api/admin/notifications        → paginated notification list
	router.Get("/api/admin/stats", proxy("banking.account.stats"))
	router.Get("/api/admin/users", proxy("banking.account.users"))
	router.Get("/api/admin/users/{id}", injectPathParam("id", "user_id", proxy("banking.account.user-detail")))
	router.Get("/api/admin/transfers", proxy("banking.account.transfers"))
	router.Get("/api/admin/notifications", proxy("banking.account.notifications"))

	// --- Per-service health (operational, not versioned) ---
	router.Get("/api/health/auth", proxy("banking.auth.health"))
	router.Get("/api/health/account", proxy("banking.account.health"))
	router.Get("/api/health/transfer", proxy("banking.transfer.health"))
	router.Get("/api/health/notifications", proxy("banking.notification.health"))

	// Wrap with instana.TracingHandlerFunc at the outermost layer so the
	// go-sensor emits native g.http entry spans to the agent on port 42699.
	// This is what makes Instana classify api-producer as technology=Go rather
	// than a generic HTTP service — the go-sensor's native span carries the
	// technology tag, whereas OTel HTTP spans alone only show up as "HTTP".
	// otelhttp sits inside: instana wrapper → otelhttp → chi router.
	// Order matters: instana extracts W3C traceparent first, then otelhttp
	// continues the same trace context downstream.
	instanaHandler := instana.TracingHandlerFunc(
		tracing.Collector(),
		"/",
		otelhttp.NewHandler(router, serviceName).ServeHTTP,
	)
	server := &http.Server{
		Addr:              serverAddr(cfg.Port),
		Handler:           instanaHandler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      cfg.ResponseTimeout + 15*time.Second, // must exceed RPC timeout
	}

	g, ctx := errgroup.WithContext(context.Background())

	// Goroutine 1: NATS client — blocks until ctx is cancelled; reconnection is automatic.
	g.Go(func() error {
		client.run(ctx, m)
		return nil
	})

	// Goroutine 2: HTTP server.
	g.Go(func() error {
		logger.Info("http_server_started", "addr", serverAddr(cfg.Port))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	// Goroutine 3: OS signal → graceful shutdown.
	g.Go(func() error {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		select {
		case sig := <-sigCh:
			logger.Info("shutdown_signal_received", "signal", sig.String())
		case <-ctx.Done():
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		client.Close()
		return nil
	})

	if err := g.Wait(); err != nil {
		logger.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

// loadConfig parses producer configuration from environment variables.
func loadConfig() (config, error) {
	var cfg config
	if err := env.Parse(&cfg); err != nil {
		return config{}, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// serverAddr normalises port to a ":port" listen address.
func serverAddr(port string) string {
	if strings.HasPrefix(port, ":") {
		return port
	}
	return ":" + port
}
