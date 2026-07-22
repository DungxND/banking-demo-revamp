# Kubernetes Monitoring — k3s on EC2

> **Source:** https://www.ibm.com/docs/en/instana-observability/current?topic=kubernetes-installing-agent
> https://www.ibm.com/docs/en/instana-observability/current?topic=kubernetes-checking-agent-prerequisites
> Condensed for: k3s single-node on EC2 (banking-demo golang branch, namespace `banking`)
>
> **Current install:** Kubernetes Helm DaemonSet agent (namespace `instana-agent`).
> See [`13-k8s-agent-install.md`](./13-k8s-agent-install.md) for full install reference.
> The legacy EC2 host-agent (systemd) mode is documented in [`01-agent-install.md`](./01-agent-install.md) for reference only.

---

## How the Instana DaemonSet Agent Works with k3s

The Instana agent runs **inside** the k3s cluster as a DaemonSet (one pod per node) plus a
dedicated `k8sensor` Deployment:

```
instana-agent namespace
  ├── DaemonSet: instana-agent   (one pod on the k3s node)
  │     ├── host metrics, process sensor
  │     ├── OTLP receiver  :4317 (gRPC) / :4318 (HTTP)
  │     ├── go-sensor receiver  :42699
  │     ├── Prometheus scrape (pod annotations, nats-exporter)
  │     ├── PostgreSQL sensor  → postgres.banking-demo.svc.cluster.local:5432
  │     ├── Redis sensor       → auto-discovered via containerd
  │     └── Kong sensor        → kong.banking-demo.svc.cluster.local:8001
  └── Deployment: k8sensor       (dedicated Kubernetes API watcher)
        └── reports pods / services / namespaces / workloads to Instana backend
```

The k8sensor Deployment handles all Kubernetes object discovery via in-cluster RBAC —
no kubeconfig file or `chmod 644` is required.

---

## What the Kubernetes Sensor Discovers

| Resource | Auto-discovered |
|----------|----------------|
| Pods (all namespaces) | ✔ |
| Deployments / StatefulSets | ✔ |
| Services | ✔ |
| Namespaces | ✔ |
| NATS JetStream | ✔ (port 8222 HTTP monitor, if enabled) |

> **Traefik:** k3s is installed with `--disable traefik` (see `roles/k3s/tasks/main.yml`).
> Traefik is **not running** in this cluster. Kong handles all ingress via hostPort.
> The Traefik sensor block in `configuration.yaml` is commented out.

### banking-demo namespace resources discovered

| Component | k8s Kind | Instana service name |
|-----------|----------|---------------------|
| api-producer | Deployment | `api-producer` |
| auth-service | Deployment | `auth-service` |
| account-service | Deployment | `account-service` |
| transfer-service | Deployment | `transfer-service` |
| notification-service | Deployment | `notification-service` |
| frontend | Deployment | `frontend` |
| kong | Deployment | `kong` |
| nats | StatefulSet | `nats` |
| postgres | StatefulSet | `postgresql` (sensor) |
| redis | StatefulSet | `redis` (sensor) |

---

## Agent Configuration

The DaemonSet agent pulls [`instana/configuration.yaml`](../configuration.yaml) from the
`main` git branch on every startup (git-based config management). Key blocks:

```yaml
# K8s context awareness — DaemonSet mode uses in-cluster RBAC automatically.
# kubeconfig is NOT needed; do NOT set it here.
# (Commented out — k8sensor handles K8s object discovery.)
# com.instana.plugin.kubernetes:
#   enabled: true

com.instana.zone:
  name: banking-dung-ec2

com.instana.tags:
  - environment: production
  - team: banking
  - project: banking-demo
```

> **DaemonSet vs host-agent:** With the Helm DaemonSet agent, `com.instana.plugin.kubernetes`
> does not need to be set — the k8sensor Deployment handles K8s API discovery using the
> mounted ServiceAccount token. The `kubeconfig` field and `enabled: true` are only required
> for the legacy EC2 host-agent (systemd) install where the agent runs outside the cluster.

---

## OTLP Flow: Pods → DaemonSet Agent

Each banking-demo pod sends OTLP traces to the DaemonSet agent using one of two patterns:

```yaml
# Pattern 1 — NODE_IP (used by all Go services and Traefik)
# Stays on-node: $(NODE_IP) = status.hostIP = the k3s node IP.
# The DaemonSet pod runs on the host network of that same node.
env:
  - name: NODE_IP
    valueFrom:
      fieldRef:
        fieldPath: status.hostIP
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: "http://$(NODE_IP):4317"
  - name: INSTANA_AGENT_HOST
    value: "$(NODE_IP)"        # go-sensor native protocol :42699

# Pattern 2 — cluster-DNS Service (used by Nginx/frontend)
# nginx.conf cannot interpolate env vars in directives; uses the stable FQDN instead.
# otel_exporter { endpoint instana-agent.instana-agent.svc.cluster.local:4317; }
```

The Helm chart creates a `instana-agent` ClusterIP Service automatically, so both
patterns work simultaneously from any namespace.

---

## Verifying Kubernetes Monitoring

```bash
# Agent pods running
kubectl -n instana-agent get pods
# Expected:
#   instana-agent-<hash>   1/1   Running   (DaemonSet — one per node)
#   k8sensor-<hash>        1/1   Running   (k8s object watcher)

# k8s resources visible in Instana
kubectl -n instana-agent logs ds/instana-agent --tail=50 \
  | grep -i "kubernetes\|k8s\|banking\|Activated"

# OTLP ports listening inside the agent pod
kubectl -n instana-agent exec ds/instana-agent -- sh -c 'ss -tlnp | grep -E "4317|4318|42699"'
```

After the agent starts:

1. **Instana UI → Infrastructure → Kubernetes** — shows the `banking` namespace with all workloads
2. **Instana UI → Infrastructure → Hosts** — EC2 node appears
3. **Instana UI → Applications → Services** — `api-producer`, `auth-service`, etc. appear **only after first OTLP traces arrive**

> **Important**: Pods being visible in Infrastructure ≠ services visible in Applications.
> Services appear only after traces flow. See [`09-pod-service-detection.md`](./09-pod-service-detection.md).

---

## Kong Integration

Kong handles all ingress in this cluster (no Traefik). The Instana Kong sensor polls
Kong's Admin API from within the cluster:

```
DaemonSet agent pod
  └─ HTTP poll every 30 s → kong.banking-demo.svc.cluster.local:8001 (Admin API)
```

`KONG_ADMIN_LISTEN=0.0.0.0:8001` is set in `helm/values.yaml` so the admin port is
reachable from the `instana-agent` namespace via the Kong ClusterIP Service.

See [`04-kong-sensor.md`](./04-kong-sensor.md) for full Kong sensor details.

---

## Legacy: EC2 Host-Agent (systemd) Notes

> These notes apply **only** to the legacy systemd install on EC2. Skip if using the DaemonSet agent.

In the legacy host-agent mode the agent runs on the EC2 host outside the cluster and
requires:

- `com.instana.plugin.kubernetes: enabled: true` + `kubeconfig: /etc/rancher/k3s/k3s.yaml`
- `sudo chmod 644 /etc/rancher/k3s/k3s.yaml` (k3s recreates it `600` on every restart)
- ClusterIP services for all internal components — host-agent reaches pods via ClusterIP DNS within the cluster
- `sudo systemctl restart instana-agent` after any config change

See [`01-agent-install.md`](./01-agent-install.md) and
[`10-host-agent-k8s-detection-fix.md`](./10-host-agent-k8s-detection-fix.md) for the
full host-agent setup and troubleshooting guide.
