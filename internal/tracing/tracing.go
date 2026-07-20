// Package tracing configures the global OpenTelemetry tracer provider and
// the Instana Go Collector (go-sensor). Call Init at startup and defer the
// returned shutdown function.
package tracing

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	instana "github.com/instana/go-sensor"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// collector holds the TracerLogger returned by InitCollector so that other
// packages (e.g. db) can attach Instana tracers to their drivers.
// Set once by Init before any other package code runs.
var collector instana.TracerLogger

// Collector returns the global Instana TracerLogger initialised by Init.
// Returns nil before Init has been called; callers must guard accordingly.
func Collector() instana.TracerLogger { return collector }

// shutdownTimeout is the maximum time the tracer provider is given to flush
// buffered spans to the OTLP exporter on shutdown. A hung or unreachable
// collector must not block the process indefinitely on SIGTERM.
const shutdownTimeout = 5 * time.Second

// Init configures the global OTel tracer provider and the Instana Go
// Collector, then returns a shutdown function that must be deferred by the
// caller.
//
// OTel handles distributed tracing (spans → OTLP → Instana agent :4317).
// The Instana Collector adds Go process metrics (memory, GC, goroutines),
// health signatures, and AutoProfile to the Instana UI — these are not
// available via OTel alone.
//
// When otlpEndpoint is empty the OTel provider is skipped (no-op propagator
// installed). The Instana collector is always initialised; it connects to
// the agent at INSTANA_AGENT_HOST:INSTANA_AGENT_PORT (defaults: localhost:42699).
// In Kubernetes set INSTANA_AGENT_HOST=$(NODE_IP) via the Helm chart env block.
//
// The returned shutdown function always completes within shutdownTimeout
// regardless of the context passed by the caller, so
// defer shutdownTracing(context.Background()) is always safe.
func Init(serviceName, otlpEndpoint string, logger *slog.Logger) func(context.Context) {
	// ── Instana Go Collector ────────────────────────────────────────────────
	// InitCollector connects to the Instana agent in the background (non-blocking).
	// If the agent is unreachable it retries silently — no startup penalty.
	// INSTANA_SERVICE_NAME env var overrides serviceName when set.
	collector = instana.InitCollector(&instana.Options{
		Service:           serviceName,
		EnableAutoProfile: autoProfileEnabled(),
		Tracer:            instana.DefaultTracerOptions(),
	})

	// ── OpenTelemetry tracer provider ──────────────────────────────────────
	// instanaFlush is shared by all early-return paths so Instana-native spans
	// are always flushed even when the OTel provider is absent or fails to init.
	instanaFlush := func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := instana.Flush(flushCtx); err != nil {
			logger.Error("instana_flush_failed", "error", err.Error())
		}
	}

	if otlpEndpoint == "" {
		otel.SetTextMapPropagator(propagation.TraceContext{})
		return func(context.Context) { instanaFlush() }
	}

	endpoint := strings.TrimPrefix(strings.TrimPrefix(otlpEndpoint, "https://"), "http://")
	exporter, err := otlptracegrpc.New(
		context.Background(),
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		logger.Error("otel_init_failed", "error", err.Error())
		return func(context.Context) { instanaFlush() }
	}

	res, err := resource.New(
		context.Background(),
		resource.WithTelemetrySDK(),  // adds telemetry.sdk.language=go — required for Instana Go tech detection on OTel/HTTP services
		resource.WithFromEnv(),       // reads OTEL_RESOURCE_ATTRIBUTES (e.g. service.namespace=banking-demo) and OTEL_SERVICE_NAME
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		logger.Error("otel_resource_failed", "error", err.Error())
		return func(context.Context) { instanaFlush() }
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Wrap the caller-supplied context with a hard deadline so a hung OTLP
	// exporter cannot block graceful shutdown indefinitely.
	return func(ctx context.Context) {
		shutdownCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
		_ = provider.Shutdown(shutdownCtx)
		instanaFlush()
	}
}

// autoProfileEnabled returns true when INSTANA_AUTO_PROFILE=true is set.
// AutoProfile is off by default; opt in per-service via env var.
func autoProfileEnabled() bool {
	return os.Getenv("INSTANA_AUTO_PROFILE") == "true"
}
