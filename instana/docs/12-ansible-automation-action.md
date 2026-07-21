# Instana Automation Action — Ansible Sensor

> **Source:** https://www.ibm.com/docs/en/instana-observability?topic=technologies-automation-action-ansible  
> Condensed for: banking-demo golang branch, host-agent (systemd) on EC2 running Ansible from `infra/ansible/`.

---

## What It Does

The **Automation Action Ansible sensor** lets Instana trigger Ansible job templates on an
**Ansible Automation Controller** (AWX / Red Hat AAP) in response to Instana events or
alerts. It bridges the Instana event pipeline with your Ansible automation:

```
Instana Event / Smart Alert
  └─ Action Catalog (Instana UI)
        └─ Automation Action: Ansible
              └─ com.instana.plugin.action.ansible (host agent sensor)
                    └─ Ansible Automation Controller REST API
                          └─ Job Template execution (e.g. rolling restart, Helm upgrade)
```

The sensor is **auto-deployed** by the agent — no manual plugin install. It is **disabled by
default** and requires explicit configuration to point at your Ansible Automation Controller.

---

## How banking-demo Uses Ansible

banking-demo uses Ansible for **infrastructure provisioning and deployment**, not for
runtime remediation today. The playbooks live in [`infra/ansible/`](../../infra/ansible/):

| Playbook / Role | What it does |
|-----------------|--------------|
| [`site.yml`](../../infra/ansible/site.yml) | Full-stack provisioning: `common` → `k3s` → `app` |
| `roles/common` | apt upgrade, swap (2 GB), sysctl for k3s + NATS (`fs.file-max`, `net.*`) |
| `roles/k3s` | k3s install (single-node, Traefik disabled), kubeconfig, Helm v4 |
| `roles/app` | git clone `golang` branch → `helm upgrade --install banking-demo` → health check |

The `app` role runs `helm upgrade --install` with all chart values files, waits up to 5 min
for pods to be ready, then polls `http://localhost:30080` (Kong NodePort) until HTTP 200/404.

**Target scenario for Ansible Actions:** once the Ansible Automation Controller is running
(AWX or AAP), the job templates below become natural Instana Automation Actions:

| Event in Instana | Suggested Ansible Job Template |
|------------------|-------------------------------|
| Pod CrashLoopBackOff (banking ns) | `rolling-restart` — `helm rollout restart` target deployment |
| High NATS pending messages | `scale-consumers` — `helm upgrade --set replicas=N` on a consumer |
| PostgreSQL connection exhaustion | `pg-connection-reset` — restart `account-service` / `transfer-service` pods |
| Redis memory > 90% | `redis-flush-cache` — selective `FLUSHDB` on cache keyspace |
| Deployment drift detected | `redeploy-banking-demo` — re-run the `app` role from `site.yml` |

---

## Prerequisites

1. An **Ansible Automation Controller** (AWX or AAP) reachable from the EC2 host where the
   Instana agent runs. The sensor calls the controller REST API — it does **not** run
   `ansible-playbook` locally.

2. A **job template** created in the controller that matches the remediation action.

3. An **API token** for the controller (generate under _Controller UI → User → Tokens_).

4. For self-hosted Instana: the **automation feature flag** must be enabled in the backend
   config. For Instana SaaS: the feature is available by default.

5. Agent version that ships a native sensor (≥ 1.0.56 — earlier versions used a Docker
   container; the current sensor is built-in and requires no Docker/Podman).

---

## Enabling the Sensor

Add to [`instana/configuration.yaml`](../configuration.yaml):

```yaml
com.instana.plugin.action.ansible:
  enabled: true
  url: https://your-awx-or-aap.example.com   # Ansible Automation Controller URL
  # apiPath: /api/v2                          # default; use /api/controller/v2 for AAP 2.5+
  token:
    configuration_from:
      type: vault
      secret_key:
        path: secret/banking-demo/ansible
        key: controller_token
  maxConcurrentActions: 5    # optional, default 10
  defaultTimeout: 120        # optional, default 300 seconds
```

> **Security note:** Always use the `vault` source for `token`. Plain-text tokens in
> `configuration.yaml` are insecure — they appear in agent debug logs and are visible
> in the config file on disk.

### API path for AAP 2.5+

Ansible Automation Controller 2.5+ changed the default API path from `/api/v2` to
`/api/controller/v2`. If you see connection errors or empty job template lists, add:

```yaml
com.instana.plugin.action.ansible:
  enabled: true
  url: https://your-aap.example.com
  apiPath: /api/controller/v2   # required for AAP 2.5+
  token: ...
```

Verify the path is correct before configuring the sensor:

```bash
curl -sk -H "Authorization: Bearer <token>" \
  https://your-aap.example.com/api/controller/v2/job_templates/ | jq '.count'
# Expected: number of templates, not an error
```

---

## Configuration Reference

| Key | Default | Description |
|-----|---------|-------------|
| `enabled` | `false` | Must be `true` to activate the sensor |
| `url` | — | Ansible Automation Controller base URL (required) |
| `apiPath` | `/api/v2` | REST API path prefix. Use `/api/controller/v2` for AAP 2.5+ |
| `token` | — | API token; use `vault` source (see above) |
| `maxConcurrentActions` | `10` | Maximum Ansible jobs running in parallel from this agent |
| `defaultTimeout` | `300` | Seconds before an Ansible action is considered timed out |

---

## Action Catalog Setup (Instana UI)

After the sensor is enabled and the agent restarts:

1. **Instana UI → Automation → Action Catalog → Add Action**
2. Select **Ansible** as the action type
3. Pick the **job template** from the controller (the sensor fetches the list via the API)
4. Optionally set **extra variables** to pass to the playbook (e.g. `target_namespace: banking`)
5. Save and attach to a **Smart Alert** or trigger manually from an event

### Attaching to a Smart Alert

1. **Instana UI → Alerts → Smart Alerts → Edit or Create**
2. Under **Actions**, click **Add Action** → select your Ansible action
3. Configure **trigger conditions** (e.g. "entity type = Pod AND state = CrashLoopBackOff AND
   namespace = banking")
4. Save — the action fires automatically when the condition is met

---

## Example: Redeploy banking-demo on Pod Failure

This is the most useful action for banking-demo: when a service is stuck, re-run the Helm
upgrade to restore the desired state.

**Job template on the controller** (references `infra/ansible/site.yml`):

```yaml
# AWX Job Template config (create in controller UI)
name: redeploy-banking-demo
project: banking-demo                  # project pointing at this git repo
playbook: infra/ansible/site.yml
tags: app                              # only run the app role (skip common + k3s)
extra_vars:
  helm_extra_args: "--atomic --timeout 5m"
credentials:
  - ssh-key-to-ec2                     # SSH key for the ubuntu@<EC2_IP> inventory host
inventory: banking-vps                 # inventory pointing at inventories/vps/
```

**Ansible Action in Instana:**
- Action type: Ansible
- Job template: `redeploy-banking-demo`
- Trigger: Smart Alert on `Pod CrashLoopBackOff` in namespace `banking`

---

## Verifying the Sensor Is Active

```bash
# 1. Check agent log for sensor activation
sudo grep -i "ansible\|action" /opt/instana/agent/log/agent.log | tail -20
# Expected: "Installed instana-action-ansible-sensor ..."
#           "Activated Sensor ..."

# 2. Verify the agent can reach the Ansible controller
curl -sk -H "Authorization: Bearer <token>" \
  https://your-awx.example.com/api/v2/ping/ | jq .
# Expected: {"ha_capacity_adjustment":1,"instance_groups":...}

# 3. Check job template list is visible from the sensor
sudo grep -i "job_template\|ansible.*fetched" /opt/instana/agent/log/agent.log | tail -10

# 4. Confirm OTLP port is still available (sensor uses a separate port — no conflict)
sudo ss -tlnp | grep -E '4317|4318|42699'
```

---

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| Agent log shows `Discovery for com.instana.plugin.action took too long` | Normal at startup — action plugin discovery races with k8s/eBPF plugins | Benign; resolves after agent warms up (same as eBPF timeout warning) |
| Ansible sensor activated but no job templates shown in UI | Wrong `apiPath` for controller version | Set `apiPath: /api/controller/v2` for AAP 2.5+ |
| `401 Unauthorized` in agent log | Token expired or incorrect | Regenerate token in AWX/AAP UI; update vault secret |
| Action triggered but job never starts | `maxConcurrentActions` limit reached | Increase `maxConcurrentActions` or wait for running jobs to complete |
| Action timed out | Job runs longer than `defaultTimeout` | Increase `defaultTimeout` to match longest expected Helm upgrade time (e.g. `600` for `--timeout 10m`) |
| `connection refused` to controller | Controller URL or network unreachable from EC2 | Verify security group allows EC2 → controller HTTPS; test with `curl` from EC2 |

---

## Related Docs

| File | What it covers |
|------|----------------|
| [`01-agent-install.md`](./01-agent-install.md) | Agent install, configuration file location |
| [`02-kubernetes-monitoring.md`](./02-kubernetes-monitoring.md) | k8s pod/service monitoring — what events Ansible Actions would remediate |
| [`10-host-agent-k8s-detection-fix.md`](./10-host-agent-k8s-detection-fix.md) | Recovery sequence for agent issues (complement to automated Ansible fix) |
| [`infra/ansible/site.yml`](../../infra/ansible/site.yml) | Main playbook — entry point for all remediation job templates |
