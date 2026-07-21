variable "aws_region" {
  description = "AWS region where resources will be created"
  type        = string
  default     = "ap-southeast-1"
}

variable "environment" {
  description = "Deployment environment name"
  type        = string
  default     = "dev"

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "Environment must be dev, staging, or prod."
  }
}

variable "instance_type" {
  description = "EC2 instance type; t3a.medium gives 2 vCPU / 4 GiB RAM — enough for k3s + Helm + all services"
  type        = string
  default     = "t3a.medium"
}

variable "ami_id" {
  description = "AMI ID for the EC2 instance. Defaults to latest Ubuntu 26.04 LTS (auto-resolved via data source when left empty)"
  type        = string
  default     = ""
}

variable "key_name" {
  description = "Name of the existing AWS EC2 key pair (EC2 → Key Pairs in the console)"
  type        = string
  default     = "banking-demo-key"
}

variable "private_key_path" {
  description = "Path to the matching local private key — written into the Ansible inventory so Ansible knows which key to use (Terraform never reads this file)"
  type        = string
  default     = "~/.ssh/id_ed25519"
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC"
  type        = string
  default     = "10.0.0.0/16"
}

variable "public_subnet_cidr" {
  description = "CIDR block for the public subnet"
  type        = string
  default     = "10.0.1.0/24"
}

variable "allowed_ssh_cidrs" {
  description = "List of CIDR blocks allowed to SSH into the instance. Restrict to your IP in production."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "allowed_app_cidrs" {
  description = "List of CIDR blocks allowed to reach application ports (30080 Kong NodePort, 6443 k3s API)"
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "root_volume_size_gb" {
  description = "Root EBS volume size in GiB"
  type        = number
  default     = 30
}

variable "ansible_inventory_path" {
  description = "Path where Terraform renders the Ansible inventory file"
  type        = string
  default     = "../ansible/inventories/vps/hosts.ini"
}

variable "ansible_group_vars_path" {
  description = "Path where Terraform renders the Ansible group_vars/all.yml file"
  type        = string
  default     = "../ansible/inventories/vps/group_vars/all.yml"
}
