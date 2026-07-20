# Instana Docs — banking-demo

Reference for Instana observability on the banking-demo k3s stack. Condensed from the [official IBM Instana docs](https://www.ibm.com/docs/en/instana-observability).

## Stack

| Component | Monitored by |
|-----------|-------------|
| k3s (Kubernetes) | Agent DaemonSet + k8sensor (Helm) |
| EC2 Ubuntu | Host metrics (same DaemonSet) |
| Traefik | Traefik sensor (auto) |
| Kong 3.9 | Kong sensor (remote, configured) |
| api-producer, auth/account/transfer/notification | OTel OTLP → agent :4317 |
| NATS 2.x + JetStream | nats-exporter → Prometheus |
| PostgreSQL 18 | PostgreSQL sensor |
| Redis 8.x | Redis sensor |
| Synthetic tests | Instana PoP (cloud) |

**Recommended agent install:** DaemonSet via Helm (`13-k8s-agent-install.md`). The legacy EC2 host-agent (`01-agent-install.md`) is kept for reference.

---

## Docs Index

| File | What it covers |
|------|---------------|
| [`13-k8s-agent-install.md`](./13-k8s-agent-install.md) | **✅ Recommended.** DaemonSet (Helm/Operator) and Remote Agent modes. Helm values, Operator CRs, OTLP endpoint, git-based config, upgrade/uninstall |
| [`01-agent-install.md`](./01-agent-install.md) | *(Legacy)* Host agent on Ubuntu EC2 — one-liner install, directory layout, kubeconfig setup |
| [`02-kubernetes-monitoring.md`](./02-kubernetes-monitoring.md) | k8s sensor discovery, OTLP pod-to-agent flow, Traefik HelmChartConfig |
| [`03-opentelemetry.md`](./03-opentelemetry.md) | OTel OTLP ingestion. Go instrumentation, pod env vars, NATS trace propagation, OTel vs go-sensor |
| [`04-kong-sensor.md`](./04-kong-sensor.md) | Kong sensor — NodePort prerequisite, `configuration.yaml` block, metrics |
| [`05-traefik-sensor.md`](./05-traefik-sensor.md) | Traefik v3 OTLP tracing — HelmChartConfig, Prometheus metrics, troubleshooting |
| [`06-redis-sensor.md`](./06-redis-sensor.md) | Redis sensor — NodePort config, ACL requirements, metrics, `instaredis` native spans |
| [`07-postgresql-sensor.md`](./07-postgresql-sensor.md) | PostgreSQL sensor — stats tracking, agent config, metrics, `instapgx` native spans |
| [`08-synthetic-monitoring.md`](./08-synthetic-monitoring.md) | API Script synthetic tests — creating tests, variables, Smart Alerts, PoP selection |
| [`09-pod-service-detection.md`](./09-pod-service-detection.md) | **⚠️ Services show 0 calls.** Infrastructure vs Application detection, OTLP checklist, AP setup, Concert correlation |
| [`10-host-agent-k8s-detection-fix.md`](./10-host-agent-k8s-detection-fix.md) | **⚠️ (Host-agent only)** k8s pods not detected — `enabled: true` fix, kubeconfig, Traefik restart, recovery |
| [`11-nats-monitoring.md`](./11-nats-monitoring.md) | NATS metrics via `nats-exporter` + Prometheus, JetStream consumer lag, W3C trace propagation |
| [`12-ansible-automation-action.md`](./12-ansible-automation-action.md) | Automation Action Ansible sensor — triggering job templates from Instana events |
| [`14-go-sensor.md`](./14-go-sensor.md) | Instana Go Collector alongside OTel — Go process dashboard, AutoProfile, `instapgx`/`instaredis` |
| [`15-concert-sbom-upload.md`](./15-concert-sbom-upload.md) | Concert SBOM upload — CI/CD pipeline artifacts, manual upload steps |

---

## Quick Start (Helm — recommended)

### Option A — Ansible

```bash
# From infra/ansible/ on your workstation:
./deploy.sh -p install-instana.yml \
  -e agent_key='<AGENT_KEY>' \
  -e agent_download_key='<DOWNLOAD_KEY>' \
  -e endpoint_host='<INSTANA_BACKEND_HOST>'
```

### Option B — Helm directly (on EC2)

```bash
helm upgrade --install instana-agent \
  --repo https://agents.instana.io/helm \
  --namespace instana-agent --create-namespace \
  -f instana/helm-agent-values.yaml \
  --set agent.key='<AGENT_KEY>' \
  --set agent.downloadKey='<DOWNLOAD_KEY>' \
  --set agent.endpointHost='<INSTANA_BACKEND_HOST>' \
  instana-agent

kubectl -n instana-agent get pods
# Expected: instana-agent-<hash> 1/1 Running   k8sensor-<hash> 1/1 Running
```

After ~30–60 s: cluster appears in **Infrastructure → Kubernetes**. Services appear in **Applications → Services** after the first traces.

> Full Helm/Operator reference → [`13-k8s-agent-install.md`](./13-k8s-agent-install.md)  
> Pods not detected after host-agent install → [`10-host-agent-k8s-detection-fix.md`](./10-host-agent-k8s-detection-fix.md)

---

## Related Files

| File | Description |
|------|-------------|
| [`instana/helm-agent-values.yaml`](../helm-agent-values.yaml) | Helm values for k8s agent — pass with `-f`; secrets via `--set` |
| [`instana/instana-agent-remote.cr.yaml`](../instana-agent-remote.cr.yaml) | InstanaAgentRemote CR — for external systems (DB2, IBM Concert) |
| [`instana/configuration.yaml`](../configuration.yaml) | Sensor config (zone, tags, Kong, Redis, PostgreSQL, OTel, tracing headers) |
| [`instana/configuration-docker-compose.yaml`](../configuration-docker-compose.yaml) | Agent config for Docker Compose mode |
| [`instana/synthetic/`](../synthetic/) | API Script tests (`health-checks.js`, `user-login-flow.js`, `transfer-flow.js`, `auth-edge-cases.js`) |
| [`concert/SBOM-GUIDE.md`](../../concert/SBOM-GUIDE.md) | Full SBOM guide — CycloneDX vs ConcertDef, toolkit commands, CI/CD flow |
| [`infra/ansible/install-instana.yml`](../../infra/ansible/install-instana.yml) | Ansible playbook — installs/upgrades the agent via Helm |
| [`helm/`](../../helm/) | Helm chart for all banking-demo services |
| [`monitoring/`](../../monitoring/) | Self-hosted Prometheus + Grafana + Jaeger + OTel Collector |
| [`OBSERVABILITY.md`](../../OBSERVABILITY.md) | Full observability reference |

---

## Official Instana Docs

- **Kubernetes agent (Helm/Operator):** <https://www.ibm.com/docs/en/instana-observability?topic=agents-installing-kubernetes>
- **Remote agent:** <https://www.ibm.com/docs/en/instana-observability?topic=agents-installing-remote-agent>
- **Go Collector:** <https://www.ibm.com/docs/en/instana-observability?topic=technologies-monitoring-go>
- **Go OTel integration:** <https://www.ibm.com/docs/en/instana-observability?topic=go-opentelemetry-integration>
- **OpenTelemetry:** <https://www.ibm.com/docs/en/instana-observability/current?topic=opentelemetry>
- **Kong:** <https://www.ibm.com/docs/en/instana-observability/current?topic=technologies-monitoring-kong-api-gateway>
- **Traefik:** <https://www.ibm.com/docs/en/instana-observability/current?topic=technologies-monitoring-traefik>
- **Redis:** <https://www.ibm.com/docs/en/instana-observability/current?topic=technologies-monitoring-redis>
- **PostgreSQL:** <https://www.ibm.com/docs/en/instana-observability/current?topic=technologies-monitoring-postgresql>
- **Synthetic monitoring:** <https://www.ibm.com/docs/en/instana-observability/current?topic=instana-synthetic-monitoring>
- **Linux host-agent:** <https://www.ibm.com/docs/en/instana-observability/current?topic=linux-installing-agent>
- **Application Perspectives:** <https://www.ibm.com/docs/en/instana-observability/current?topic=instana-application-perspectives>
- **Helm chart reference:** <https://github.com/instana/helm-charts/tree/main/instana-agent#configuration-reference>
