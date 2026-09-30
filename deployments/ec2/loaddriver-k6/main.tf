terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.63"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
    local = {
      source  = "hashicorp/local"
      version = "~> 2.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.0"
    }
  }
}

provider "aws" {
  region = var.region
}

resource "random_string" "suffix" {
  length  = 8
  special = false
  upper   = false
}

locals {
  name   = "benchctl-${random_string.suffix.result}"
  ami_id = lookup(var.ami, var.region, data.aws_ami.ubuntu_arm64.id)
}

# ── AMI ───────────────────────────────────────────────────────────────────────
# Kept as a reference for refreshing the pinned AMI in variables.tf.

data "aws_ami" "ubuntu_arm64" {
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd*/ubuntu-noble-24.04-arm64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }

  filter {
    name   = "architecture"
    values = ["arm64"]
  }
}

# ── VPC ───────────────────────────────────────────────────────────────────────
# This module owns its own VPC because the target is noop (an external service,
# not an EC2 target). A minimal single-AZ public subnet is sufficient for the
# load driver.

data "aws_availability_zones" "available" {
  state = "available"
}

resource "aws_vpc" "main" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = merge(var.tags, { Name = "${local.name}-vpc" })
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = merge(var.tags, { Name = "${local.name}-igw" })
}

resource "aws_subnet" "public" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.1.0/24"
  availability_zone = data.aws_availability_zones.available.names[0]
  tags              = merge(var.tags, { Name = "${local.name}-public" })
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = merge(var.tags, { Name = "${local.name}-rt-public" })
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

# ── SSH key pair ──────────────────────────────────────────────────────────────

resource "tls_private_key" "driver" {
  algorithm = "RSA"
  rsa_bits  = 4096
}

resource "aws_key_pair" "driver" {
  key_name   = "${local.name}-driver"
  public_key = tls_private_key.driver.public_key_openssh
  tags       = merge(var.tags, { Name = "${local.name}-driver" })
}

# Written into the per-run tofu working directory (path.cwd, e.g.
# ./tofu-state/<runID>/loaddriver-k6). benchctl reads this path from the
# driver outputs to SSH/SCP during bootstrap.
resource "local_file" "driver_key" {
  content         = tls_private_key.driver.private_key_pem
  filename        = "${path.cwd}/${local.name}-driver.pem"
  file_permission = "0600"
}

# ── Security group ────────────────────────────────────────────────────────────

resource "aws_security_group" "driver" {
  name   = "${local.name}-driver"
  vpc_id = aws_vpc.main.id

  # SSH — benchctl bootstraps the driver from the laptop over SSH.
  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, { Name = "${local.name}-driver" })
}

resource "aws_vpc_security_group_egress_rule" "driver_all" {
  security_group_id = aws_security_group.driver.id
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

# ── User data ─────────────────────────────────────────────────────────────────
# Install k6 via the official pre-built binary from GitHub releases.
# The benchctl binary is SCP'd by the bootstrap step after provisioning.

locals {
  user_data = <<-EOT
    #!/bin/bash
    set -euxo pipefail
    export DEBIAN_FRONTEND=noninteractive

    # ── Reduce variability ────────────────────────────────────────────────────
    # Shared across all EC2/GCE target and driver modules — see
    # deployments/shared/reduce-variability.sh. Referenced via a symlink
    # (reduce-variability.sh in this module dir) rather than a "../../shared"
    # relative path: opentofu.Provider copies only the module's own directory
    # into the per-run tofu working dir (see internal/providers/opentofu),
    # so a path escaping the module dir wouldn't resolve there.
    ${file("${path.module}/reduce-variability.sh")}

    # ── k6 (pre-built binary from GitHub releases) ──────────────────────────────
    # curl ships on the stock Ubuntu server AMI already, so this needs no
    # apt-get at all -- one less dependency on the (occasionally flaky, see
    # below) apt mirror for the common case.
    # Fetch the latest release from GitHub API and download the appropriate binary.
    ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
    DOWNLOAD_URL=$(curl -fsSL -L --connect-timeout 15 --max-time 60 --retry 5 --retry-delay 10 https://api.github.com/repos/grafana/k6/releases/latest | \
      grep -o "\"browser_download_url\": \"[^\"]*linux-$${ARCH}\.tar\.gz\"" | \
      cut -d'"' -f4)
    curl -fsSL -L --connect-timeout 15 --max-time 120 --retry 5 --retry-delay 10 "$${DOWNLOAD_URL}" -o /tmp/k6.tar.gz
    tar -xzf /tmp/k6.tar.gz -C /tmp/
    mv /tmp/k6*/k6 /usr/local/bin/k6
    chmod +x /usr/local/bin/k6

    # ── Vector (metrics agent, optional) ──────────────────────────────────────
    # Only installed when metrics collection is actually requested: this is
    # the only remaining step that needs apt-get, so runs whose scenario
    # leaves collection off (the default) skip apt entirely instead of
    # installing Vector just to immediately stop/disable it.
    # Best-effort: apt-get occasionally wedges indefinitely on this mirror (a
    # connection gets closed by the server, observed stuck in CLOSE-WAIT, but
    # apt's http method never notices and never times out on its own), so
    # each call gets a hard per-attempt timeout with retries. If it still
    # fails, log and move on rather than failing the whole driver bootstrap
    # over an optional sidecar.
    if [ "${var.vector_enabled}" = "true" ]; then
      apt_get_retry() {
        local attempt
        for attempt in 1 2 3; do
          if timeout 60 apt-get "$@"; then
            return 0
          fi
          echo "apt-get $* timed out or failed (attempt $attempt/3), retrying..." >&2
          sleep 5
        done
        return 1
      }

      if curl -fsSL --connect-timeout 15 --max-time 60 --retry 5 --retry-delay 10 https://setup.vector.dev | timeout 60 bash \
        && apt_get_retry update -y \
        && apt_get_retry install -y vector; then
        mkdir -p /var/log/vector
        chown vector:vector /var/log/vector
        echo '${base64encode(templatefile("${path.module}/vector.yaml.tftpl", {
  sink                              = var.vector_sink
  victoriametrics_endpoint          = trimsuffix(var.vector_sink_victoriametrics_endpoint, "/")
  victoriametrics_token             = var.vector_sink_victoriametrics_token
  host_metrics_scrape_interval_secs = var.vector_host_metrics_scrape_interval_secs
}))}' | base64 -d > /etc/vector/vector.yaml
        systemctl enable vector
        systemctl start vector || true
      else
        echo "WARNING: vector install failed; continuing without metrics collection" >&2
      fi
    fi
  EOT
}

# ── EC2 instance ──────────────────────────────────────────────────────────────

resource "aws_instance" "driver" {
  ami                                  = local.ami_id
  instance_type                        = var.instance_type
  subnet_id                            = aws_subnet.public.id
  vpc_security_group_ids               = [aws_security_group.driver.id]
  key_name                             = aws_key_pair.driver.key_name
  associate_public_ip_address          = true
  instance_initiated_shutdown_behavior = "terminate"

  user_data = local.user_data

  root_block_device {
    volume_size = 30
    volume_type = "gp3"
  }

  tags = merge(var.tags, { Name = "${local.name}-driver" })
}
