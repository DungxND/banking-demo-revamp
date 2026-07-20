# Instana Agent — Kubernetes Install (DaemonSet vs Remote Agent)

> **Sources:**
> - https://www.ibm.com/docs/en/instana-observability?topic=agents-installing-kubernetes
> - https://www.ibm.com/docs/en/instana-observability?topic=agents-installing-remote-agent
>
> Condensed for: k3s single-node on EC2, banking-demo, namespace `banking`.

---

## Two Deployment Modes — Choose One

The Instana Kubernetes agent has **two distinct modes**. They are not addons to each other —
pick the one that fits your monitoring target.

| | **Mode 1 — DaemonSet** (`InstanaAgent`) | **Mode 2 — Remote Agent** (`InstanaAgentRemote`) |
|---|---|---|
| K8s resource | DaemonSet — one pod per node | Deployment — one (or more) pods total |
| Config | Shared CR — same config on every node | Per-instance CR — unique config per deployment |
| Best for | Host metrics, OTel traces, process auto-discovery, K8s object monitoring | Monitoring systems you **can't install an agent on**: DB2, IBM Concert, IBM Turbonomic, any remote API |
| Remote sensor duplication | ❌ Every node opens a connection to the same remote system | ✅ Exactly one connection per remote system |
| Operator required | Yes (bundled with Helm chart v2) | Yes — Operator must be installed first |
| OTLP receiver | ✅ Listens on :4317/:4318 per node | ❌ Not applicable |
| K8s object discovery (k8sensor) | ✅ Included | ❌ Not included |

**For banking-demo:** Mode 1 (DaemonSet) is the right choice. All workloads are pods inside
k3s — the DaemonSet agent co-locates with them, collects host metrics, receives OTel traces,
and auto-discovers processes. The remote agent mode is for cases like monitoring a DB2 instance
or IBM Concert that have no Kubernetes presence at all.

---

## How the DaemonSet Mode Works

```
k3s node (EC2)
  instana-agent namespace
    ├── DaemonSet: instana-agent         ← one pod per node
    │     ├── host metrics, process sensor, OTel :4317/:4318, Prometheus scrape
    │     └── git-pull instana/configuration.yaml on startup + hot-reload
    └── Deployment: k8sensor             ← dedicated Kubernetes API watcher
          └── reports pods, services, namespaces, workloads to Instana backend
```

The `k8sensor` Deployment is created automatically when `k8s_sensor.deployment.enabled=true`
(the default in `helm-agent-values.yaml`). It handles all Kubernetes object monitoring so
the DaemonSet pods are free to focus on host and process metrics.

---

## Install: Option A — Helm (recommended for banking-demo)

Helm is the recommended install method. It bundles the Operator, CRDs, RBAC, and DaemonSet
into a single versioned release with idempotent upgrades.

### Prerequisites

```bash
helm version   # must be ≥ 3.0
kubectl get nodes
# Expected: <node>  Ready  control-plane,master  ...  v1.xx.x+k3s1
```

### 1. Add the Instana Helm repo

```bash
helm repo add instana https://agents.instana.io/helm
helm repo update
```

### 2. Install via Ansible (recommended)

The Ansible playbook handles SSH, kubeconfig patching, and idempotent install in one command:

```bash
# From infra/ansible/ on your workstation:
./deploy.sh -p install-instana.yml \
  -e agent_key="${INSTANA_AGENT_KEY}" \
  -e agent_download_key="${INSTANA_DOWNLOAD_KEY}" \
  -e endpoint_host="${INSTANA_ENDPOINT_HOST}" \
  -e git_token="${GITHUB_PAT}"
# GITHUB_PAT: Fine-grained PAT with Contents: read-only on banking-demo.
# The playbook sets INSTANA_GIT_REMOTE_USERNAME=<git_token> on the agent pod.
```

### 3. Install directly via Helm

```bash
helm upgrade --install instana-agent \
  --repo https://agents.instana.io/helm \
  --namespace instana-agent \
  --create-namespace \
  -f instana/helm-agent-values.yaml \
  --set agent.key="${INSTANA_AGENT_KEY}" \
  --set agent.downloadKey="${INSTANA_DOWNLOAD_KEY}" \
  --set agent.endpointHost="${INSTANA_ENDPOINT_HOST}" \
  --set agent.env.INSTANA_GIT_REMOTE_REPOSITORY="https://github.com/dungxnd/banking-demo.git" \
  --set agent.env.INSTANA_GIT_REMOTE_BRANCH="main" \
  --set "agent.env.INSTANA_GIT_REMOTE_USERNAME=${GITHUB_PAT}" \
  instana-agent
```

> **Do NOT pass `INSTANA_GIT_REMOTE_PASSWORD`.**
> The `InstanaAgent` CRD schema requires all `spec.agent.env` values to be non-null strings.
> Both `--set key=` and `--set-string key=` produce a null YAML scalar when the value is empty,
> which fails CRD server-side apply with:
> `spec.agent.env.INSTANA_GIT_REMOTE_PASSWORD in body must be of type string: "null"`
> GitHub basic auth only needs the PAT as the username — omitting the password field entirely
> is correct and the agent works without it.

**Key parameters (secrets via `--set`, static values in `instana/helm-agent-values.yaml`):**

| Parameter | Value | Notes |
|-----------|-------|-------|
| `agent.key` | From Instana UI → Settings → Agents | Required — via `--set` only |
| `agent.endpointHost` | e.g. `ingress-orange-saas.instana.io` | Required — via `--set` only |
| `agent.endpointPort` | `443` | Set in values file |
| `agent.env.INSTANA_GIT_REMOTE_REPOSITORY` | `https://github.com/dungxnd/banking-demo.git` | Via `--set` only |
| `agent.env.INSTANA_GIT_REMOTE_BRANCH` | `main` | Via `--set` only |
| `agent.env.INSTANA_GIT_REMOTE_USERNAME` | GitHub Fine-grained PAT | Via `--set` only — never commit |
| `agent.env.INSTANA_GIT_REMOTE_PASSWORD` | *(omit entirely)* | Do NOT pass — CRD rejects null string |
| `cluster.name` | `banking-dung` | Set in values file |
| `zone.name` | `banking-dung-ec2` | Set in values file |
| `k8s_sensor.deployment.enabled` | `true` | Set in values file |

### 4. Verify installation

```bash
# DaemonSet and k8sensor pods should be Running
kubectl -n instana-agent get pods
# Expected:
#   instana-agent-<hash>   1/1   Running   (one per node)
#   k8sensor-<hash>        1/1   Running

# OTLP port is listening on the DaemonSet pod
kubectl -n instana-agent exec ds/instana-agent -- sh -c 'ss -tlnp | grep 4317'
# Expected: LISTEN 0.0.0.0:4317

# Confirm k8s sensor and OTLP activated
kubectl -n instana-agent logs ds/instana-agent --tail=50 \
  | grep -i "kubernetes\|Activated\|otlp\|ERROR"
```

---

## Install: Option B — Raw Operator YAML + InstanaAgent CR

GitOps-friendly alternative. No Helm — the agent config is a Kubernetes CR applied with
`kubectl apply`. Produces the same DaemonSet end state as Option A.

### 1. Install the Operator

```bash
kubectl apply -f https://github.com/instana/instana-agent-operator/releases/latest/download/instana-agent-operator.yaml
```

The Operator deploys into the `instana-agent` namespace and watches for `InstanaAgent` CRs.

### 2. Create and apply the InstanaAgent CR

From **Instana UI → Agents & Collectors → Install agents → Kubernetes – Operator**, copy the
pre-filled YAML and save as `instana/instana-agent.cr.yaml`:

```yaml
# instana/instana-agent.cr.yaml
apiVersion: instana.io/v1
kind: InstanaAgent
metadata:
  name: instana-agent
  namespace: instana-agent
spec:
  agent:
    key: '<YOUR_AGENT_KEY>'
    endpointHost: '<YOUR_INSTANA_BACKEND_HOST>'
    endpointPort: 443
    env:
      INSTANA_GIT_REMOTE_REPOSITORY: "https://github.com/dungxnd/banking-demo.git"
      INSTANA_GIT_REMOTE_BRANCH: "main"
      INSTANA_GIT_REMOTE_USERNAME: "<YOUR_GITHUB_PAT>"  # Fine-grained PAT, Contents: read
      INSTANA_GIT_REMOTE_PASSWORD: ""                   # intentionally empty

  cluster:
    name: banking-dung

  zone:
    name: banking-dung-ec2

  k8s_sensor:
    deployment:
      enabled: true
```

```bash
kubectl apply -f instana/instana-agent.cr.yaml

# Watch rollout
kubectl -n instana-agent get pods -w
```

### 3. Update configuration

Edit the CR and re-apply — the Operator performs a rolling restart automatically:

```bash
kubectl apply -f instana/instana-agent.cr.yaml
```

---

## Install: Option C — Remote Agent (InstanaAgentRemote)

> **Official docs:** https://www.ibm.com/docs/en/instana-observability?topic=agents-installing-remote-agent
> Operator v2.1.30+ required. Kubernetes-only.

The remote agent is a **separate deployment mode** for monitoring systems that have no
Kubernetes presence — you cannot install a standard agent on them. Examples: DB2, IBM Concert
(CVE), IBM Turbonomic, any system with only a remote API.

It runs as a **Deployment** (not a DaemonSet) so each instance has a **unique configuration**.
This prevents the duplicate-connection problem the DaemonSet mode has with remote sensors
(every node would open an identical connection to the same remote system).

**This is NOT the right mode for banking-demo's PostgreSQL and Redis** — those are pods
inside k3s and are best monitored by the DaemonSet agent co-located on the same node.
Use this mode if you add an external system (e.g. a standalone DB2 instance on a separate VM)
that the cluster can reach over the network but cannot host an agent itself.

### Prerequisites

The Operator must already be running (installed via Option A or B above).

```bash
# Verify operator is running
kubectl -n instana-agent get deploy instana-agent-controller-manager
```

On **OpenShift only**:
```bash
oc adm policy add-scc-to-user anyuid -z instana-agent-remote -n instana-agent
```

### 1. Create a keys secret

Keep the agent key out of the CR YAML (and therefore out of git):

```bash
kubectl -n instana-agent create secret generic instana-remote-key \
  --from-literal=key="${INSTANA_AGENT_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -
```

### 2. Apply the InstanaAgentRemote CR

The CR template is committed at [`instana/instana-agent-remote.cr.yaml`](../instana-agent-remote.cr.yaml).
Edit `configuration_yaml` for your specific remote sensors before applying:

```bash
kubectl apply -f instana/instana-agent-remote.cr.yaml

# Watch rollout
kubectl -n instana-agent get pods -w
# Expected: remote-agent-<hash>   1/1   Running
```

### 3. Verify

```bash
kubectl -n instana-agent logs deploy/remote-agent --tail=50 \
  | grep -i "sensor\|activated\|ERROR"
```

### Uninstall

```bash
# CR must be deleted first, then cluster-level RBAC objects
kubectl delete -f instana/instana-agent-remote.cr.yaml
kubectl delete clusterrole instana-agent-remote
kubectl delete clusterrolebinding instana-agent-remote
kubectl -n instana-agent delete secret instana-remote-key
```

---

## OTLP Endpoint in Pods (DaemonSet mode only)

With the DaemonSet agent running inside the cluster, pods send OTLP traces to the
**agent pod on the same node** via `status.hostIP`:

```yaml
# In each Deployment (e.g. helm/templates/auth-service.yaml)
env:
  - name: NODE_IP
    valueFrom:
      fieldRef:
        fieldPath: status.hostIP
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: "http://$(NODE_IP):4317"
  - name: OTEL_SERVICE_NAME
    value: auth-service
```

Alternatively, use the cluster-wide Service DNS (works from any namespace):

```
http://instana-agent.instana-agent.svc.cluster.local:4317  # gRPC
http://instana-agent.instana-agent.svc.cluster.local:4318  # HTTP
```

---

## Differences vs Host-Agent (EC2 systemd) Install

| Concern | Host-Agent (systemd) | DaemonSet mode | Remote agent mode |
|---------|---------------------|----------------|-------------------|
| Kubeconfig permission hack | Required (`chmod 644`) | Not needed | Not needed |
| K8s object discovery | Via kubeconfig | k8sensor Deployment | ❌ Not included |
| Sensor config location | File on EC2 host | Helm values / CR in git | CR YAML in git |
| OTLP receiver | ✅ via host port | ✅ per-node pod | ❌ not applicable |
| Remote sensor connections | One per host-agent | One **per node** ✗ | One **total** ✓ |
| Upgrade path | `apt upgrade` | `helm upgrade` | `kubectl apply` |
| Config change | `systemctl restart` | Git push + hot-reload | `kubectl apply` |

---

## Git-based Configuration Management (DaemonSet mode)

The DaemonSet agent pulls `instana/configuration.yaml` directly from the `main` branch on
every startup and on every hot-reload triggered via the Instana Web API. A `helm upgrade`
is **not required** when only sensor configuration changes.

### How it works

```
git push instana/configuration.yaml → main branch
        ↓
GitHub Actions (.github/workflows/instana-gitops.yml)
        ↓  POST /api/host-agent/update?query=entity.zone:"banking-dung-ec2"
Instana backend sends reload command to matching agents
        ↓
instana-agent DaemonSet pod: git pull main branch → applies configuration.yaml
        ↓  (~30 s)
New sensor config active
```

### Workflow: change sensor config

```bash
# 1. Edit locally
vim instana/configuration.yaml

# 2. Commit and push
git add instana/configuration.yaml
git commit -m "fix(instana): update postgresql password"
git push origin main

# 3. GitHub Actions triggers automatically — watch the Actions tab

# 4. Verify (~30 s after push)
kubectl -n instana-agent logs ds/instana-agent --tail=20 \
  | grep -i "git\|configuration\|reload"
```

### Required GitHub secrets

| Secret | Description |
|--------|-------------|
| `INSTANA_API_TOKEN` | Instana API token — Settings → API Tokens → **Agent Configuration** read+write |
| `INSTANA_ENDPOINT_HOST` | e.g. `ingress-red-saas.instana.io` |

### Manual reload (without a push)

```bash
curl --request POST \
  "https://${INSTANA_ENDPOINT_HOST}/api/host-agent/update?query=entity.zone%3A%22banking-dung-ec2%22" \
  --header "authorization: apiToken ${INSTANA_API_TOKEN}" \
  --header "content-type: application/json"
```

---

## Upgrading the Agent (DaemonSet mode, Helm)

CRDs are **not** updated automatically on `helm upgrade` — apply them manually first:

```bash
helm pull --repo https://agents.instana.io/helm --untar instana-agent
kubectl apply -f instana-agent/crds
helm upgrade instana-agent instana-agent \
  --namespace instana-agent \
  --repo https://agents.instana.io/helm \
  --reuse-values
```

---

## Uninstalling

```bash
# DaemonSet mode — Helm
helm uninstall instana-agent -n instana-agent
kubectl patch agent instana-agent -n instana-agent \
  -p '{"metadata":{"finalizers":null}}' --type=merge
kubectl delete crd agents.instana.io agentsremote.instana.io
kubectl delete namespace instana-agent

# DaemonSet mode — Operator-only install
kubectl delete -f instana/instana-agent.cr.yaml
kubectl delete -f https://github.com/instana/instana-agent-operator/releases/latest/download/instana-agent-operator.yaml

# Remote agent mode
kubectl delete -f instana/instana-agent-remote.cr.yaml
kubectl delete clusterrole instana-agent-remote
kubectl delete clusterrolebinding instana-agent-remote
kubectl -n instana-agent delete secret instana-remote-key
```

---

## Related Docs

| File | What it covers |
|------|---------------|
| [`01-agent-install.md`](./01-agent-install.md) | Legacy EC2 host-agent (systemd) install — kept for reference |
| [`02-kubernetes-monitoring.md`](./02-kubernetes-monitoring.md) | What the k8s sensor discovers and the OTLP pod flow |
| [`03-opentelemetry.md`](./03-opentelemetry.md) | Go OTel SDK, OTLP endpoint configuration per pod |
| [`10-host-agent-k8s-detection-fix.md`](./10-host-agent-k8s-detection-fix.md) | Host-agent troubleshooting reference (EC2 systemd only) |

> **Official docs:**
> - DaemonSet install: https://www.ibm.com/docs/en/instana-observability?topic=agents-installing-kubernetes
> - Remote agent install: https://www.ibm.com/docs/en/instana-observability?topic=agents-installing-remote-agent
