# infra

Infrastructure-as-code for the banking-demo project.

```
infra/
├── terraform/   # Provision AWS resources (VPC, EC2, security groups, key pair)
└── ansible/     # Configure the server and deploy the application
```

The two tools are intentionally sequential: **Terraform runs first** and writes
Ansible's inventory file directly into `ansible/inventories/vps/`, so Ansible
always knows the current server IP and SSH key without manual copy-paste.

---

## Quick start

### Prerequisites

| Tool | Min version | Install |
|---|---|---|
| Terraform | 1.15 | https://developer.hashicorp.com/terraform/install |
| Ansible | 2.15 | `pip install ansible-core` (WSL2/Linux/macOS only) |
| Ansible collections | — | see [below](#ansible-collections) |
| AWS credentials | — | `aws configure` |

> **Windows:** Terraform runs fine on Windows. Ansible requires
> [WSL2](https://learn.microsoft.com/en-us/windows/wsl/install) — run all
> `ansible-playbook` commands from inside a WSL Ubuntu shell.

### 1 — Configure Terraform

```bash
cd infra/terraform
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars:
#   key_name        = "your-key-label"
#   public_key_path = "~/.ssh/id_ed25519.pub"
#   private_key_path = "~/.ssh/id_ed25519"
#   allowed_ssh_cidrs = ["your.ip.address/32"]
```

### 2 — Provision the EC2 instance

```bash
cd infra/terraform
terraform init
terraform apply
```

On success Terraform writes two files used by Ansible:

| File | Contents |
|---|---|
| `ansible/inventories/vps/hosts.ini` | Host IP + SSH key path |
| `ansible/inventories/vps/group_vars/all.yml` | `server_public_ip`, `aws_region` |

### 3 — Install Ansible collections <a id="ansible-collections"></a>

```bash
ansible-galaxy collection install community.general ansible.posix
```

### 4 — Deploy the application

```bash
cd infra/ansible
ansible-playbook site.yml
```

This runs three roles in order:

| Role | What it does |
|---|---|
| `common` | apt upgrade, base packages, 2 GiB swap, sysctl tuning |
| `k3s` | Installs k3s (Traefik disabled) + Helm v4; copies kubeconfig for `ubuntu` user |
| `app` | Clones the repo, runs `helm upgrade --install`, waits for Kong on port 30080 |

---

## Re-deploying after a code change

```bash
cd infra/ansible
ansible-playbook site.yml --tags app
```

---

## Terraform resources

| Resource | Description |
|---|---|
| `aws_vpc` | VPC `10.0.0.0/16` with DNS enabled |
| `aws_subnet` | Single public subnet `10.0.1.0/24` |
| `aws_internet_gateway` + route table | Internet access for the instance |
| `aws_security_group` | Inbound: SSH (22), Kong proxy hostPort (80), k3s API (6443) |
| `aws_instance` | Ubuntu 26.04 LTS, `t3a.medium`, 30 GiB gp3 encrypted root |
| `aws_key_pair` | Registers your existing `~/.ssh/id_ed25519.pub` |

All resources are tagged `Project=banking-demo`, `ManagedBy=Terraform`.

### Key variables

| Variable | Default | Description |
|---|---|---|
| `aws_region` | `ap-southeast-1` | AWS region |
| `environment` | `dev` | `dev` / `staging` / `prod` |
| `instance_type` | `t3a.medium` | EC2 instance type |
| `key_name` | `banking-demo-key` | Label shown in AWS console |
| `public_key_path` | `~/.ssh/id_ed25519.pub` | Your SSH public key |
| `allowed_ssh_cidrs` | `["0.0.0.0/0"]` | Restrict to your IP in production |

---

## Tear down

```bash
cd infra/terraform
terraform destroy
```

> **Note** — the Ansible-generated files (`hosts.ini`, `group_vars/all.yml`)
> are git-ignored and will be removed when you next run `terraform apply`.
