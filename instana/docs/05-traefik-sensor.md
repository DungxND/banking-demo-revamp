# Traefik Sensor

> **Source:** https://www.ibm.com/docs/en/instana-observability/current?topic=technologies-monitoring-traefik
> **Traefik v3 tracing ref:** https://doc.traefik.io/traefik/v3.7/reference/install-configuration/observability/tracing/
> **Migration guide:** https://doc.traefik.io/traefik/v3.7/migrate/v2-to-v3-details/#tracing
> Condensed for: banking-demo golang branch — Traefik is **disabled**; Kong handles ingress.

> **⚠️ Traefik is NOT running in this cluster.** k3s is installed with `--disable traefik`
> (see `roles/k3s/tasks/main.yml`). The Traefik sensor block in `configuration.yaml` is
> commented out. This document is retained for reference in case Traefik is re-enabled.

---

## Current State

Traefik is disabled. All external ingress is handled by **Kong** via hostPort 80/443 on the
k3s node. If you need to re-enable Traefik, follow the configuration steps below and
uncomment the sensor block in `configuration.yaml`.

---

## How It Would Work (if re-enabled)

The Instana Traefik sensor would be **automatically deployed** after the agent starts. It collects:
- **Metrics** via Traefik's Prometheus endpoint (scraped by the agent every 1 s)
- **Distributed traces** via OTLP/gRPC → Instana agent `:4317` (Traefik v3)

```
User request
  └─ Traefik (k3s ingress, :80/:443)
       ├─ OTLP span pushed to DaemonSet agent :4317 (NODE_IP or cluster-DNS)
       ├─ W3C traceparent/tracestate headers propagated downstream
       ├─ Prometheus /metrics exposed on :9100
       │       └─ DaemonSet agent scrapes every 1 s
       └─ Request forwarded to kong:8000 or frontend:80
```

> **⚠️ Traefik v3 breaking change:** `--tracing.instana` and `INSTANA_AGENT_ENDPOINT` were
> **removed in Traefik v3**. All vendor-specific tracing backends were removed. Tracing is
> now exclusively via OTLP. Use `OTEL_EXPORTER_OTLP_ENDPOINT` — not `--tracing.instana`.

---

## Supported Versions

| Technology | Support policy | Latest supported |
|------------|---------------|-----------------|
| Traefik | On demand | 3.4 |

k3s v1.28+ ships Traefik v3. Run `kubectl -n kube-system exec -it deploy/traefik -- traefik version` to confirm.

---

## Re-enabling Traefik in k3s

k3s manages Traefik via an internal Helm chart. Patch it with a `HelmChartConfig` resource.
**Do not use `additionalArguments` for tracing** — use the `tracing:` Helm values block directly.

### Step 1 — Remove `--disable traefik` from k3s install args

Edit `/etc/systemd/system/k3s.service` or the Ansible role (`roles/k3s/tasks/main.yml`) and
remove `--disable traefik`. Restart k3s to let it deploy Traefik.

### Step 2 — Apply the HelmChartConfig for OTLP tracing

```bash
kubectl apply -f - <<'EOF'
apiVersion: helm.cattle.io/v1
kind: HelmChartConfig
metadata:
  name: traefik
  namespace: kube-system
spec:
  valuesContent: |-
    env:
      - name: NODE_IP
        valueFrom:
          fieldRef:
            fieldPath: status.hostIP
      # OTEL_EXPORTER_OTLP_ENDPOINT overrides tracing.otlp.grpc.endpoint at runtime.
      # Kubernetes expands $(NODE_IP) in env value: fields — this is how we pass
      # the dynamic node IP since Helm values are static YAML strings.
      - name: OTEL_EXPORTER_OTLP_ENDPOINT
        value: "http://$(NODE_IP):4317"

    # Helm chart tracing values (Traefik v3 — OTLP only)
    # OTEL_EXPORTER_OTLP_ENDPOINT above overrides endpoint at runtime.
    tracing:
      otlp:
        grpc:
          endpoint: "localhost:4317"   # static fallback; env var takes precedence
          insecure: true

    additionalArguments:
      - "--entrypoints.metrics.address=:9100"
      - "--metrics.prometheus.entryPoint=metrics"

    ports:
      metrics:
        port: 9100
        exposedPort: 9100
EOF
```

### Why `OTEL_EXPORTER_OTLP_ENDPOINT` instead of `tracing.otlp.grpc.endpoint`

The Helm values `tracing.otlp.grpc.endpoint` is a **static string** — `$(NODE_IP)` does not
expand inside YAML. Kubernetes **does** expand env var references in `env[].value` fields.
Traefik v3 honours the standard `OTEL_EXPORTER_OTLP_ENDPOINT` env var and uses it to override
the static endpoint, so `http://$(NODE_IP):4317` resolves correctly at pod creation time.

### Step 3 — Apply and restart

```bash
kubectl apply -f <your-helmchartconfig.yaml>
kubectl -n kube-system rollout restart deployment/traefik
kubectl -n kube-system rollout status deployment/traefik
```

### Step 4 — Uncomment the sensor block in configuration.yaml

```yaml
com.instana.plugin.traefik:
  enabled: true
  poll_rate: 1    # seconds between Prometheus metrics scrapes
```

---

## What Would Be Collected

### Performance Metrics

| Metric | Description | Granularity |
|--------|-------------|-------------|
| HTTP requests/sec | Per second across all entrypoints | 1 s |
| Config last reload success | Timestamp of last successful config reload | 1 s |
| Config reload count | Reloads per second | 1 s |
| Entrypoints | HTTP requests/sec per entrypoint (max 100) | 1 s |

### Tracing (Traefik v3 — OTLP)
- Every request through Traefik generates an OTLP span pushed to agent `:4317`
- W3C `traceparent` / `tracestate` headers propagated to all downstream services
- Connects the frontend → Kong → microservice call chain in Instana UI

---

## Troubleshooting (if re-enabled)

| Symptom | Cause | Fix |
|---------|-------|-----|
| `"Instana Tracing backend has been removed in v3"` in Traefik pod log | Old `--tracing.instana=true` still in `additionalArguments` | Remove it — use `tracing.otlp.grpc` Helm values instead |
| `"Instana tracing is not enabled"` in agent log | Traefik pod not restarted after `HelmChartConfig` applied | `kubectl rollout restart deployment/traefik -n kube-system` |
| `FrameworkEvent ERROR: Service factory returned null` in agent log | Sensor initialised before Traefik reported tracing enabled | **Transient** — resolves once Traefik restarts with OTLP configured |
| `traefik_metrics_api_not_accessible` | Prometheus `/metrics` not on `:9100` | Add `ports.metrics` block and `--entrypoints.metrics.address=:9100` |
| Spans missing in Instana UI | `OTEL_EXPORTER_OTLP_ENDPOINT` not reaching agent | Verify DaemonSet agent pod is running: `kubectl -n instana-agent get pods` |

```bash
# Verify Traefik has the OTLP env var set correctly
kubectl -n kube-system get pod -l app.kubernetes.io/name=traefik \
  -o jsonpath='{.items[0].spec.containers[0].env}' | python3 -m json.tool \
  | grep -A2 OTEL_EXPORTER

# Check Prometheus metrics endpoint
kubectl -n kube-system port-forward svc/traefik 9100:9100 &
curl -s http://localhost:9100/metrics | grep traefik_

# Check agent log for Traefik sensor
kubectl -n instana-agent logs ds/instana-agent --tail=50 | grep -i "traefik"
```
