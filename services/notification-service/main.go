package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	internnats "banking-demo/internal/nats"
	"banking-demo/internal/health"
	ilogging "banking-demo/internal/logging"
	"banking-demo/internal/metrics"
	"banking-demo/internal/service"
	"banking-demo/internal/tracing"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	serviceName   = "notification-service"
	subjectPrefix = "banking.notification"
)

type config struct {
	Port         string `env:"PORT"         envDefault:"8004"`
	NATSURL      string `env:"NATS_URL"     envDefault:"nats://nats:4222"`
	DatabaseURL  string `env:"DATABASE_URL" envDefault:"postgresql://banking:bankingpass@postgres:5432/banking"`
	RedisURL     string `env:"REDIS_URL"    envDefault:"redis://redis:6379/0"`
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
}

func main() {
	var cfg config
	if err := env.Parse(&cfg); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: parse config: %v\n", err)
		os.Exit(1)
	}
	if err := run(context.Background(), cfg); err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config) error {
	logger := ilogging.NewLogger(serviceName)

	shutdownTracing := tracing.Init(serviceName, cfg.OTLPEndpoint, logger)
	defer shutdownTracing(context.Background())

	d, cleanup, err := service.InitDeps(ctx, cfg.DatabaseURL, cfg.RedisURL, logger)
	if err != nil {
		return err
	}
	defer cleanup()
	d.LogPoolStatus(logger)

	requireSession := internnats.SessionMiddleware(d.RedisClient)

	m := metrics.NewConsumerMetrics(serviceName)

	consumer := internnats.NewConsumer(cfg.NATSURL, serviceName, subjectPrefix, logger,
		internnats.WithMetrics(m),
		internnats.WithHandler("notifications", requireSession(handleNotifications(d.BDB, logger))),
		internnats.WithHandler("ack", requireSession(handleAck(d.BDB, logger))),
		internnats.WithHandler("health", health.NATSHandler(serviceName, d.Pool, d.RedisClient)),
	)

	// HTTP server — /ws WebSocket, /health, /metrics.
	router := chi.NewRouter()
	router.Get("/health", health.HTTPHandler(serviceName, d.Pool, d.RedisClient))
	router.Handle("/metrics", promhttp.Handler())
	router.Get("/ws", wsHandler(d.RedisClient, logger))

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           otelhttp.NewHandler(router, serviceName),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // WebSocket connections are long-lived; no write deadline.
	}

	return service.NewRunner(consumer, server, logger).Run(ctx)
}
