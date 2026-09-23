terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.1"
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

provider "google" {
  # Same resolution convention as deployments/gcp-cloudsql/postgres: falls
  # back to GOOGLE_CLOUD_PROJECT/gcloud config when project_id is empty.
  project = var.project_id != "" ? var.project_id : null
  region  = local.region
}

resource "random_string" "suffix" {
  length  = 8
  special = false
  upper   = false
}

locals {
  name = "benchctl-${random_string.suffix.result}"

  # Region is derived from zone (e.g. "us-central1-a" -> "us-central1") since
  # the scenario only threads a zone through (matching target.outputs.zone),
  # not a separate region input.
  zone_parts = split("-", var.zone)
  region     = join("-", slice(local.zone_parts, 0, length(local.zone_parts) - 1))

  # GCE has no single naming convention for ARM families the way AWS embeds
  # 'g' after the generation digit (see internal/providers/gce's own
  # arch-detection allow-list) -- t2a (Tau T2A) and c4a (Axion) are ARM64,
  # everything else (n2, n4, c3, c4, ...) is amd64.
  instance_family = split("-", var.instance_type)[0]
  is_arm          = contains(["t2a", "c4a"], local.instance_family)

  # The C4 generation (c4, c4a, c4d) requires Hyperdisk exclusively and
  # rejects classic Persistent Disk types outright ("pd-balanced disk type
  # cannot be used by c4a-standard-4 machine type"). This is a machine-family
  # split, not an ARM/x86 one -- t2a (the other ARM option) still takes
  # classic PD, same as n2/the x86 fallback the instance_type variable
  # documents.
  requires_hyperdisk = contains(["c4", "c4a", "c4d"], local.instance_family)
  boot_disk_type     = local.requires_hyperdisk ? "hyperdisk-balanced" : "pd-balanced"

  # Unlike EC2's pinned-AMI-per-region map (deployments/ec2/loaddriver_gotpc/variables.tf),
  # this resolves to the latest image in the family at apply time rather than
  # a pinned build -- there's no verified pinned build to reference here. If
  # run-to-run image-version drift becomes a reproducibility concern, switch
  # to a pinned `image` variable (map keyed by arch) the same way the EC2
  # module does, once a specific build has been chosen and verified.
  image_family = local.is_arm ? "ubuntu-2404-lts-arm64" : "ubuntu-2404-lts-amd64"

  effective_network    = var.network != null ? var.network : google_compute_network.self[0].id
  effective_subnetwork = var.subnetwork != null ? var.subnetwork : google_compute_subnetwork.self[0].id
}

data "google_compute_image" "ubuntu" {
  family  = local.image_family
  project = "ubuntu-os-cloud"
}

# ── Self-provisioned network (standalone mode only) ──────────────────────────
# Created when no network/subnetwork is provided (no co-located target module).

resource "google_compute_network" "self" {
  count                   = var.network == null ? 1 : 0
  name                    = "${local.name}-driver"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "self" {
  count         = var.network == null ? 1 : 0
  name          = "${local.name}-driver"
  ip_cidr_range = "10.0.1.0/24"
  region        = local.region
  network       = google_compute_network.self[0].id
}

# ── Firewall ──────────────────────────────────────────────────────────────────
# Custom-mode VPCs (auto_create_subnetworks = false) get none of the "default"
# network's implied allow rules, so SSH ingress needs an explicit rule. This
# mirrors EC2's own security group (created by the driver module even in
# co-located mode, rather than reusing the target's) -- egress is allowed by
# GCP's implied default and needs no rule.

resource "google_compute_firewall" "ssh" {
  name    = "${local.name}-driver-ssh"
  network = local.effective_network

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }

  # benchctl bootstraps the driver from the laptop over SSH.
  source_ranges = ["0.0.0.0/0"]
  target_tags   = [local.name]
}

# ── SSH key pair ──────────────────────────────────────────────────────────────

resource "tls_private_key" "driver" {
  algorithm = "RSA"
  rsa_bits  = 4096
}

# Written into the per-run tofu working directory (path.cwd, e.g.
# ./tofu-state/<runID>/loaddriver_gotpc). benchctl reads this path from the
# driver outputs to SSH/SCP during bootstrap.
resource "local_file" "driver_key" {
  content         = tls_private_key.driver.private_key_pem
  filename        = "${path.cwd}/${local.name}-driver.pem"
  file_permission = "0600"
}

# ── Boot script ───────────────────────────────────────────────────────────────
# Delivered via the "user-data" metadata key (cloud-init), not GCE's native
# "startup-script" key -- internal/providers/sshdriver's Bootstrap runs
# `cloud-init status --wait` unconditionally regardless of cloud, so cloud-init
# has to be the thing actually running this script for that wait to mean
# anything. Ubuntu's GCE images ship cloud-init configured to read this key,
# same as AWS user_data.
#
# Install go-tpc via the official install script (pre-built binary, no Go
# toolchain needed). The benchctl binary is SCP'd by the bootstrap step after
# provisioning.

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

    apt-get update -y
    apt-get install -y curl

    # ── go-tpc (pre-built binary from GitHub releases) ────────────────────────
    # Assets are named go-tpc_latest_linux_{arch}.tar.gz, published under the
    # rolling "latest" tag. That release is a prerelease, so GitHub's
    # /latest/download redirect (which only resolves non-prerelease releases)
    # won't find it — use the tag-scoped download path instead.
    ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
    curl -fsSL -L --retry 5 --retry-delay 10 \
      "https://github.com/supabase/go-tpc/releases/download/latest/go-tpc_latest_linux_$${ARCH}.tar.gz" \
      -o /tmp/go-tpc.tar.gz
    tar -xzf /tmp/go-tpc.tar.gz -C /tmp/
    mv /tmp/go-tpc /usr/local/bin/go-tpc
    chmod +x /usr/local/bin/go-tpc

    # ── Vector (metrics agent) ────────────────────────────────────────────────
    # Install Vector in agent mode. Config is generated from Terraform variables
    # at provision time so the sink matches the benchmark environment.
    curl -fsSL --retry 5 --retry-delay 10 https://setup.vector.dev | bash
    apt-get install -y vector
    mkdir -p /var/log/vector
    chown vector:vector /var/log/vector

    echo '${base64encode(templatefile("${path.module}/vector.yaml.tftpl", {
  sink                              = var.vector_sink
  victoriametrics_endpoint          = trimsuffix(var.vector_sink_victoriametrics_endpoint, "/")
  victoriametrics_token             = var.vector_sink_victoriametrics_token
  host_metrics_scrape_interval_secs = var.vector_host_metrics_scrape_interval_secs
}))}' | base64 -d > /etc/vector/vector.yaml

    if [ "${var.vector_enabled}" = "true" ]; then
      systemctl enable vector
      systemctl start vector || true
    else
      systemctl stop vector 2>/dev/null || true
      systemctl disable vector 2>/dev/null || true
    fi
  EOT
}

# ── GCE instance ──────────────────────────────────────────────────────────────

resource "google_compute_instance" "driver" {
  name         = "${local.name}-driver"
  machine_type = var.instance_type
  zone         = var.zone
  tags         = [local.name] # network tag, matched by google_compute_firewall.ssh's target_tags

  boot_disk {
    initialize_params {
      image = data.google_compute_image.ubuntu.self_link
      size  = 30
      type  = local.boot_disk_type
    }
  }

  network_interface {
    network    = local.effective_network
    subnetwork = local.effective_subnetwork
    access_config {} # ephemeral external IP -- benchctl SSHes/SCPs from the operator's laptop, mirrors EC2's associate_public_ip_address
  }

  metadata = {
    # Username must match sshdriver.Config.DefaultSSHUser (internal/providers/gce),
    # i.e. "ubuntu", unless overridden via cfg["ssh_user"] at the benchctl layer.
    ssh-keys       = "ubuntu:${tls_private_key.driver.public_key_openssh}"
    enable-oslogin = "FALSE" # OS Login would take over SSH auth instead of the ssh-keys metadata above
    user-data      = local.user_data
  }

  labels = var.tags

  allow_stopping_for_update = true
}
