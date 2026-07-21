locals {
  name_prefix = "banking-demo-${var.environment}"

  # Ports exposed by the k3s + Helm stack on the EC2 node:
  #   30080 — Kong NodePort (bound on every node)
  #   6443  — k3s API server (kubectl / Helm access from your workstation)
  app_ports = [
    30080, # Kong NodePort
    6443,  # k3s API server
  ]
}
