// Package health provides shared HTTP and NATS health-check handlers
// that are reused across all services.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	natsinternal "banking-demo/internal/nats"
	iredis "banking-demo/internal/redis"
)

// Pinger is satisfied by *pgxpool.Pool.
type Pinger interface {
	Ping(context.Context) error
}

// healthTimeout is the per-dependency ping deadline used by checkHealth.
// The K8s readiness probe sets timeout=1s on the HTTP GET, which means the
// entire handler — including two network round-trips — must complete in <1s.
// Using a separate context with a generous-but-bounded timeout decouples the
// ping deadline from the probe's HTTP timeout so a slow dependency is detected
// cleanly rather than racing against the probe cancellation.
const healthTimeout = 3 * time.Second

// checkHealth pings both dependencies and returns their status.
// Extracted so HTTPHandler and NATSHandler share identical check logic;
// adding a new dependency check touches this function only.
func checkHealth(ctx context.Context, pool Pinger, rc *iredis.Client) (dbOK, redisOK bool) {
	pingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthTimeout)
	defer cancel()
	dbOK = pool.Ping(pingCtx) == nil
	redisOK = rc.Ping(pingCtx).Err() == nil
	return
}

// HTTPHandler returns an http.HandlerFunc that writes a JSON readiness response.
// On success: 200 {"status":"healthy","service":"<name>","database":"ok","redis":"ok"}
// On failure: 503 with per-dependency statuses.
func HTTPHandler(serviceName string, pool Pinger, rc *iredis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbOK, redisOK := checkHealth(r.Context(), pool, rc)

		w.Header().Set("Content-Type", "application/json")
		if dbOK && redisOK {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"healthy","service":%q,"database":"ok","redis":"ok"}`, serviceName)
			return
		}

		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"status":"unhealthy","service":%q,"database":%q,"redis":%q}`,
			serviceName, statusStr(dbOK), statusStr(redisOK))
	}
}

// NATSHandler returns a nats.Handler that replies with a JSON readiness response.
func NATSHandler(serviceName string, pool Pinger, rc *iredis.Client) natsinternal.Handler {
	return func(ctx context.Context, _ string, _ json.RawMessage, _ map[string]string) (any, error) {
		dbOK, redisOK := checkHealth(ctx, pool, rc)

		if dbOK && redisOK {
			return natsinternal.Reply(200, map[string]string{
				"status": "healthy", "service": serviceName,
				"database": "ok", "redis": "ok",
			}), nil
		}
		return natsinternal.Reply(503, map[string]string{
			"status":   "unhealthy",
			"service":  serviceName,
			"database": statusStr(dbOK),
			"redis":    statusStr(redisOK),
		}), nil
	}
}

func statusStr(ok bool) string {
	if ok {
		return "ok"
	}
	return "error"
}
