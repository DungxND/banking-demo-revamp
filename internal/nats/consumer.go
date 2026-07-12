// Package nats provides a reusable RPC consumer framework backed by NATS micro.
// Each registered action maps to its own NATS endpoint (e.g. banking.auth.login),
// giving per-action latency and request counts in "nats micro stats <name>".
package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"banking-demo/internal/metrics"
	iredis "banking-demo/internal/redis"
)

// serviceVersion is embedded in the micro service descriptor visible via
// "nats micro info <name>". Bump on significant releases.
const serviceVersion = "1.0.0"

// Handler is the function signature every service action must implement.
// action is the leaf segment of the NATS subject (e.g. "login", "balance").
// payload is the raw JSON value of the "payload" field from the request body.
// headers carries x-session and x-admin-secret forwarded by the producer.
// Return (result, nil) to reply normally or (nil, err) to reply with 500.
type Handler func(ctx context.Context, action string, payload json.RawMessage, headers map[string]string) (any, error)

// RequireSession wraps next with session validation. It extracts x-session from
// headers, resolves it via Redis, and injects the user ID into the context.
// Handlers wrapped with RequireSession can retrieve the user ID via UserIDFromContext.
// Returns Reply(401) when the session is missing or expired.
func RequireSession(rc *iredis.Client, next Handler) Handler {
	return func(ctx context.Context, action string, payload json.RawMessage, headers map[string]string) (any, error) {
		userID, err := iredis.GetUserIDFromSession(ctx, rc, headers["x-session"])
		if errors.Is(err, iredis.ErrUnauthorized) {
			return Reply(401, map[string]string{"detail": "Unauthorized"}), nil
		}
		if err != nil {
			return Reply(500, map[string]string{"detail": "session error"}), nil
		}
		return next(withUserID(ctx, userID), action, payload, headers)
	}
}

// RequireAdmin wraps next with admin-secret + session validation.
// It checks x-admin-secret first (fast reject), then resolves the session.
// Returns Reply(403) when the secret is wrong; Reply(401) when the session is invalid.
func RequireAdmin(secret string, rc *iredis.Client, next Handler) Handler {
	return func(ctx context.Context, action string, payload json.RawMessage, headers map[string]string) (any, error) {
		if headers["x-admin-secret"] != secret {
			return Reply(403, map[string]string{"detail": "Forbidden"}), nil
		}
		userID, err := iredis.GetUserIDFromSession(ctx, rc, headers["x-session"])
		if errors.Is(err, iredis.ErrUnauthorized) {
			return Reply(401, map[string]string{"detail": "Unauthorized"}), nil
		}
		if err != nil {
			return Reply(500, map[string]string{"detail": "session error"}), nil
		}
		return next(withUserID(ctx, userID), action, payload, headers)
	}
}

// SessionMiddleware returns a function that applies RequireSession with the
// given Redis client. Use it to avoid repeating the 3-line closure in each
// service main.go:
//
//	wrap := nats.SessionMiddleware(rc)
//	consumer.WithHandler("action", wrap(handler))
func SessionMiddleware(rc *iredis.Client) func(Handler) Handler {
	return func(h Handler) Handler { return RequireSession(rc, h) }
}

// AdminMiddleware returns a function that applies RequireAdmin with the
// given secret and Redis client.
func AdminMiddleware(secret string, rc *iredis.Client) func(Handler) Handler {
	return func(h Handler) Handler { return RequireAdmin(secret, rc, h) }
}

type ctxKey struct{}

// withUserID stores the resolved user ID in the context for downstream handlers.
func withUserID(ctx context.Context, id int) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// UserIDFromContext retrieves the user ID injected by RequireSession or RequireAdmin.
// Returns (0, false) when not present.
func UserIDFromContext(ctx context.Context) (int, bool) {
	v, ok := ctx.Value(ctxKey{}).(int)
	return v, ok
}

// rpcRequest is the JSON body received from api-producer.
// Action is encoded in the NATS subject; only Payload travels in the body.
type rpcRequest struct {
	Payload json.RawMessage `json:"payload"`
}

// rpcResponse is the reply format expected by api-producer.
type rpcResponse struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}

// Consumer manages a NATS micro service with one endpoint per registered action.
// Construct with NewConsumer; add handlers with WithHandler and configure
// behaviour with functional options. Call Run to start the service.
type Consumer struct {
	url           string
	name          string // service display name, e.g. "auth-service" — visible in nats micro ls
	subjectPrefix string // subject namespace, e.g. "banking.auth" — endpoints: banking.auth.login, etc.
	logger        *slog.Logger
	handlers      map[string]Handler
	pendingMsgs   int                      // per-endpoint pending message limit
	metrics       *metrics.ConsumerMetrics // optional; nil = no instrumentation
	nc            *nats.Conn               // optional pre-connected conn; if nil, Run connects itself
	tracer        trace.Tracer             // OTel tracer for consumer-side spans; set in NewConsumer
}

// Option is a functional option for Consumer.
type Option func(*Consumer)

// WithHandler registers a Handler for the given action name.
// The endpoint subject is subjectPrefix + "." + action
// (e.g. "banking.auth" + "." + "login" = "banking.auth.login").
func WithHandler(action string, h Handler) Option {
	return func(c *Consumer) {
		c.handlers[action] = h
	}
}

// WithPendingMsgs sets the per-endpoint in-memory message limit (default 64).
// NATS will drop messages and fire the error handler once this limit is exceeded.
// Lower values surface backpressure problems earlier in staging.
func WithPendingMsgs(n int) Option {
	return func(c *Consumer) {
		if n > 0 {
			c.pendingMsgs = n
		}
	}
}

// WithMetrics enables Prometheus instrumentation for this consumer.
// Pass a *metrics.ConsumerMetrics created with metrics.NewConsumerMetrics.
func WithMetrics(m *metrics.ConsumerMetrics) Option {
	return func(c *Consumer) {
		c.metrics = m
	}
}

// WithConn supplies a pre-connected *nats.Conn to the Consumer.
// When provided, Run uses this connection instead of dialling a new one.
// This allows transfer-service and account-service to share a single NATS
// connection for both the micro RPC layer and the JetStream event bus.
// The caller owns the connection lifetime; Consumer.Run will NOT drain it.
func WithConn(nc *nats.Conn) Option {
	return func(c *Consumer) { c.nc = nc }
}

// NewConsumer constructs a Consumer and applies the given options.
//   - url is the NATS server URL (e.g. "nats://nats:4222").
//   - name is the human-readable service name (e.g. "auth-service") — visible
//     in "nats micro ls" and $SRV.INFO/<name> responses.
//   - subjectPrefix is the shared subject namespace for this service
//     (e.g. "banking.auth"). Each registered action creates an endpoint at
//     subjectPrefix + "." + action (e.g. "banking.auth.login").
func NewConsumer(url, name, subjectPrefix string, logger *slog.Logger, opts ...Option) *Consumer {
	c := &Consumer{
		url:           url,
		name:          name,
		subjectPrefix: subjectPrefix,
		logger:        logger,
		handlers:      make(map[string]Handler),
		pendingMsgs:   64,
		tracer:        otel.Tracer(name),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Connect opens a new NATS connection with the standard options for this
// service. Call this from main.go when the connection must be shared with
// JetStream or other consumers. The caller is responsible for calling
// nc.Drain() when the process exits.
func Connect(url, name string, logger *slog.Logger, reconnectMetric func()) (*nats.Conn, error) {
	return nats.Connect(url,
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.ReconnectJitter(500*time.Millisecond, 2*time.Second),
		nats.RetryOnFailedConnect(true),
		nats.PingInterval(20*time.Second),
		nats.MaxPingsOutstanding(5),
		nats.ReconnectHandler(func(conn *nats.Conn) {
			if reconnectMetric != nil {
				reconnectMetric()
			}
			logger.Info("nats_reconnected", "url", conn.ConnectedUrl())
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Error("nats_disconnected", "error", err)
		}),
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, natErr error) {
			if errors.Is(natErr, nats.ErrSlowConsumer) {
				dropped, _ := sub.Dropped()
				logger.Error("nats_slow_consumer",
					"subject", sub.Subject,
					"dropped", dropped,
				)
			}
		}),
	)
}

// Run connects to NATS (or uses an injected connection), registers a micro
// service with one endpoint per action, and blocks until ctx is cancelled.
// Connection management (reconnect, jitter, ping) is handled by nats.go automatically.
//
// Each endpoint gets its own NATS subject (banking.auth.login etc.) so
// "nats micro stats <name>" shows per-action request counts, error counts,
// and average latency independently.
func (c *Consumer) Run(ctx context.Context) {
	nc := c.nc
	if nc == nil {
		// No pre-connected conn — dial now using shared Connect helper.
		var reconnectInc func()
		if c.metrics != nil {
			reconnectInc = c.metrics.ReconnectsTotal.Inc
		}
		var err error
		nc, err = Connect(c.url, c.name, c.logger, reconnectInc)
		if err != nil {
			// RetryOnFailedConnect=true: only reachable if MaxReconnects exhausted (-1 → never).
			c.logger.Error("nats_connect_failed", "error", err)
			return
		}
		defer nc.Drain() // flush pending outbound before close; only drain conn we own
	}

	// Register the micro service. This starts answering:
	//   $SRV.PING.<name>   — nats micro ping <name>
	//   $SRV.INFO.<name>   — service descriptor + all endpoint subjects
	//   $SRV.STATS.<name>  — per-action request count, errors, avg latency
	svc, err := micro.AddService(nc, micro.Config{
		Name:        c.name,
		Version:     serviceVersion,
		Description: fmt.Sprintf("%s RPC handler", c.name),
	})
	if err != nil {
		c.logger.Error("nats_micro_add_service_failed", "error", err)
		return
	}
	defer svc.Stop() //nolint:errcheck // Stop is best-effort on shutdown

	// Register one endpoint per action. Each endpoint:
	//   • listens on subjectPrefix + "." + action (e.g. "banking.auth.login")
	//   • uses the subject as the queue-group so multiple replicas load-balance
	//   • tracks per-action stats independently in $SRV.STATS
	for action, h := range c.handlers {
		subject := c.subjectPrefix + "." + action
		if err := svc.AddEndpoint(action,
			micro.HandlerFunc(func(req micro.Request) {
				go c.dispatch(ctx, req, action, h)
			}),
			micro.WithEndpointSubject(subject),
			micro.WithEndpointQueueGroup(subject),
			micro.WithEndpointPendingLimits(c.pendingMsgs, c.pendingMsgs*4096),
		); err != nil {
			c.logger.Error("nats_micro_add_endpoint_failed",
				"action", action,
				"subject", subject,
				"error", err,
			)
			return
		}
	}

	c.logger.Info("nats_micro_service_started",
		"name", c.name,
		"version", serviceVersion,
		"subject_prefix", c.subjectPrefix,
		"endpoints", len(c.handlers),
	)

	<-ctx.Done()
	// svc.Stop() (deferred above) drains in-flight handler goroutines and
	// unsubscribes all endpoints. nc.Drain() (deferred) flushes outbound.
}

// dispatch handles a single micro.Request for the given action and handler.
// The action string is known at endpoint-registration time and passed in from
// the closure — no parsing of the request body or subject is needed.
func (c *Consumer) dispatch(ctx context.Context, req micro.Request, action string, h Handler) {
	// Drop silently on shutdown — the producer's RequestMsgWithContext will
	// time out and return an error to the HTTP caller.
	if ctx.Err() != nil {
		return
	}

	var rpcReq rpcRequest
	if err := json.Unmarshal(req.Data(), &rpcReq); err != nil {
		c.logger.Error("nats_decode_error",
			"name", c.name,
			"action", action,
			"error", err.Error(),
		)
		// Error sends Nats-Service-Error and Nats-Service-Error-Code headers
		// so the producer gets a structured reply instead of timing out.
		_ = req.Error("400", "invalid request body", nil)
		return
	}

	// Extract the W3C traceparent from the NATS headers to continue the
	// producer's trace as a child span on the consumer side.
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(req.Headers()))

	// Start a child span for this handler invocation. The extracted context
	// above carries the producer's traceparent as the remote parent, so this
	// span appears as a child of the producer's rpc.request span in the trace.
	subject := c.subjectPrefix + "." + action
	ctx, span := c.tracer.Start(ctx, "nats.handler",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("messaging.destination.name", subject),
			attribute.String("messaging.operation", "process"),
		),
	)
	defer span.End()

	// Auth headers travel in NATS message headers, not in the JSON body.
	headers := map[string]string{
		"x-session":      req.Headers().Get("x-session"),
		"x-admin-secret": req.Headers().Get("x-admin-secret"),
	}

	start := time.Now()

	result, err := h(ctx, action, rpcReq.Payload, headers)
	var resp rpcResponse
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		c.logger.Error("nats_handler_error",
			"name", c.name,
			"action", action,
			"error", err.Error(),
		)
		resp = rpcResponse{Status: 500, Body: map[string]string{"detail": "internal error"}}
	} else {
		if r, ok := result.(rpcResponse); ok {
			resp = r
		} else {
			resp = rpcResponse{Status: 200, Body: result}
		}
		span.SetAttributes(attribute.Int("rpc.status", resp.Status))
	}

	if c.metrics != nil {
		c.metrics.HandlerDuration.WithLabelValues(action).Observe(time.Since(start).Seconds())
		c.metrics.MessagesTotal.WithLabelValues(action, strconv.Itoa(resp.Status)).Inc()
	}

	body, err := json.Marshal(resp)
	if err != nil {
		c.logger.Error("nats_marshal_reply_error", "action", action, "error", err.Error())
		fallback := []byte(`{"status":500,"body":{"detail":"internal marshal error"}}`)
		_ = req.Respond(fallback)
		return
	}
	if err := req.Respond(body); err != nil {
		c.logger.Error("nats_respond_error", "action", action, "error", err.Error())
	}
}

// Reply is a convenience constructor for a well-formed rpcResponse.
// Service handlers can return Reply(200, body) instead of building the struct.
func Reply(status int, body any) rpcResponse {
	return rpcResponse{Status: status, Body: body}
}
