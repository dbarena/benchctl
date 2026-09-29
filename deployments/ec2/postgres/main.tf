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

# Unique 8-char suffix per apply so multiple runs don't collide on key-pair names.
resource "random_string" "suffix" {
  length  = 8
  special = false
  upper   = false
}

locals {
  name = "benchctl-${random_string.suffix.result}"
}

# ── AMI ───────────────────────────────────────────────────────────────────────
# Kept as a reference for refreshing the pinned AMI in variables.tf.
# Graviton4 (c8gd) requires an ARM64 image. Ubuntu 24.04 LTS (Noble) ships
# arm64 AMIs under the "hvm-ssd-gp3" path; the wildcard covers both variants.

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

locals {
  ami_id = lookup(var.ami, var.region, data.aws_ami.ubuntu_arm64.id)
}

# ── Network ───────────────────────────────────────────────────────────────────

data "aws_availability_zones" "available" {
  state = "available"
}

resource "aws_vpc" "this" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_hostnames = true
  tags                 = merge(var.tags, { Name = local.name })
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = merge(var.tags, { Name = local.name })
}

resource "aws_subnet" "this" {
  vpc_id                  = aws_vpc.this.id
  cidr_block              = "10.0.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true
  tags                    = merge(var.tags, { Name = local.name })
}

resource "aws_route_table" "this" {
  vpc_id = aws_vpc.this.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }

  tags = merge(var.tags, { Name = local.name })
}

resource "aws_route_table_association" "this" {
  subnet_id      = aws_subnet.this.id
  route_table_id = aws_route_table.this.id
}

# ── Security group ────────────────────────────────────────────────────────────

resource "aws_security_group" "target" {
  name   = "${local.name}-target"
  vpc_id = aws_vpc.this.id

  # PostgreSQL — VPC-only. The ready_check and go-tpc both run from the driver
  # within the VPC using the target's private IP; no laptop access needed.
  ingress {
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = [aws_vpc.this.cidr_block]
  }

  # SSH — for ad-hoc debugging.
  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, { Name = "${local.name}-target" })
}

# ── SSH key pair ──────────────────────────────────────────────────────────────

resource "tls_private_key" "target" {
  algorithm = "RSA"
  rsa_bits  = 4096
}

resource "aws_key_pair" "target" {
  key_name   = "${local.name}-target"
  public_key = tls_private_key.target.public_key_openssh
  tags       = merge(var.tags, { Name = "${local.name}-target" })
}

# Written into the per-run tofu working directory (path.cwd, e.g.
# ./tofu-state/<runID>/postgres) so it persists alongside the state file.
resource "local_file" "target_key" {
  content         = tls_private_key.target.private_key_pem
  filename        = "${path.cwd}/${local.name}-target.pem"
  file_permission = "0600"
}

# ── User data ─────────────────────────────────────────────────────────────────
# 1. Disable Transparent Huge Pages (THP) and persist the setting via systemd.
# 2. Format and mount the NVMe instance store (/dev/nvme1n1) as pgdata.
# 3. Install Docker (arm64).

locals {
  user_data = <<-EOT
    #!/bin/bash
    set -euxo pipefail
    export DEBIAN_FRONTEND=noninteractive
    export HOME=/root

    # ── Reduce variability ────────────────────────────────────────────────────
    # Shared across all EC2/GCE target and driver modules — see
    # deployments/shared/reduce-variability.sh. Referenced via a symlink
    # (reduce-variability.sh in this module dir) rather than a "../../shared"
    # relative path: opentofu.Provider copies only the module's own directory
    # into the per-run tofu working dir (see internal/providers/opentofu),
    # so a path escaping the module dir wouldn't resolve there.
    ${file("${path.module}/reduce-variability.sh")}
    # ── Transparent Huge Pages ────────────────────────────────────────────────
    # PostgreSQL is sensitive to THP compaction stalls. Disable both the
    # allocation policy and the background defrag daemon.
    echo never > /sys/kernel/mm/transparent_hugepage/enabled
    echo never > /sys/kernel/mm/transparent_hugepage/defrag
    # Persist across reboots via a systemd unit (user_data only runs once).
    # Unit file is written with echo statements rather than a bash heredoc to
    # keep all lines in this <<-EOT block at a consistent indentation depth.
    # A nested <<'UNIT' heredoc would introduce lines at column 0, preventing
    # Terraform's <<-EOT from stripping the leading whitespace, which causes
    # cloud-init to receive a script whose shebang is indented and therefore
    # not recognised — silently aborting the entire user_data script.
    {
      echo '[Unit]'
      echo 'Description=Disable Transparent Huge Pages'
      echo 'DefaultDependencies=no'
      echo 'After=sysinit.target local-fs.target'
      echo 'Before=basic.target'
      echo ''
      echo '[Service]'
      echo 'Type=oneshot'
      echo "ExecStart=/bin/sh -c 'echo never > /sys/kernel/mm/transparent_hugepage/enabled && echo never > /sys/kernel/mm/transparent_hugepage/defrag'"
      echo ''
      echo '[Install]'
      echo 'WantedBy=basic.target'
    } > /etc/systemd/system/disable-thp.service
    systemctl daemon-reload
    systemctl enable disable-thp.service

    # ── NVMe instance store ───────────────────────────────────────────────────
    # Detect the instance store NVMe device dynamically: it's whichever nvme*n1
    # device is not backing the root filesystem.
    ROOT_DISK=$(lsblk -ndo PKNAME "$(findmnt -no SOURCE /)")
    # `|| true` is relevant because of the `set -o pipefail` at the top of this
    # script, grep exits 1 when the only nvme device is the root disk, which
    # aborts here and leaves the operator with a bare "exit status 1" -- the
    # check below, the one that explains the actual problem, never runs.
    NVME_DEV=$(ls /dev/nvme*n1 2>/dev/null | grep -v "$ROOT_DISK" | head -1 || true)
    if [ -z "$NVME_DEV" ]; then
      echo "ERROR: no instance store NVMe device found." >&2
      echo "ERROR: root disk is $ROOT_DISK; nvme devices present: $(ls /dev/nvme*n1 2>/dev/null | tr '\n' ' ')" >&2
      echo "ERROR: this module puts pgdata on local NVMe, so it needs an instance type that has some." >&2
      exit 1
    fi
    mkfs.ext4 -F "$NVME_DEV"
    mkdir -p /mnt/nvme
    # Register in fstab by UUID so the mount survives reboots. noatime skips
    # inode atime updates on reads, eliminating spurious write I/O.
    NVME_UUID=$(blkid -s UUID -o value "$NVME_DEV")
    echo "UUID=$NVME_UUID /mnt/nvme ext4 noatime 0 2" >> /etc/fstab
    mount /mnt/nvme
    mkdir -p /mnt/nvme/pgdata

    # ── Docker ────────────────────────────────────────────────────────────────
    # apt-get occasionally wedges indefinitely on the Ubuntu mirror: a
    # connection gets closed by the server (observed stuck in CLOSE-WAIT) but
    # apt's http method never notices, so the process never times out on its
    # own. Wrap each call in a hard per-attempt timeout with retries instead
    # of relying on a much longer external deadline. Docker is essential here
    # (unlike Vector below), so a failure after retries is still fatal.
    apt_get_retry() {
      local attempt
      for attempt in 1 2 3; do
        if timeout 90 apt-get "$@"; then
          return 0
        fi
        echo "apt-get $* timed out or failed (attempt $attempt/3), retrying..." >&2
        sleep 5
      done
      return 1
    }

    # curl ships on the stock Ubuntu server AMI already; ca-certificates and
    # retry (the CLI retry-loop helper used elsewhere, e.g. docker compose
    # pull) do not.
    apt_get_retry update -y
    apt_get_retry install -y ca-certificates retry
    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL --connect-timeout 15 --max-time 60 --retry 5 --retry-delay 10 https://download.docker.com/linux/ubuntu/gpg \
      -o /etc/apt/keyrings/docker.asc
    chmod a+r /etc/apt/keyrings/docker.asc
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
      https://download.docker.com/linux/ubuntu \
      $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
      > /etc/apt/sources.list.d/docker.list
    apt_get_retry update -y
    apt_get_retry install -y docker-ce docker-ce-cli containerd.io
    usermod -aG docker ubuntu

    # ── Performance profiling tools ───────────────────────────────────────────
    # linux-tools-$(uname -r) resolves to the correct AWS kernel tools package
    # at boot time (e.g. linux-tools-6.8.0-1021-aws). The || true prevents
    # failures when the exact package name differs from the running kernel;
    # linux-tools-generic provides a fallback for that case.
    timeout 120 apt-get install -y \
      "linux-tools-$(uname -r)" linux-tools-common linux-tools-generic \
      binutils git perl bpftrace 2>/dev/null || true
    # FlameGraph toolkit: stackcollapse-perf.pl, difffolded.pl, flamegraph.pl, etc.
    # http.version=HTTP/1.1: GitHub's git-upload-pack endpoint intermittently
    # returns 401 over HTTP/2 for anonymous clones; HTTP/1.1 avoids it.
    # Best-effort like the perf-tools install above: this only feeds optional
    # profiling, so a GitHub hiccup shouldn't fail target provisioning.
    if timeout 60 git -c http.version=HTTP/1.1 clone --depth 1 https://github.com/brendangregg/FlameGraph /opt/flamegraph; then
      chmod +x /opt/flamegraph/*.pl
    else
      echo "WARNING: FlameGraph clone failed; continuing without it" >&2
    fi
    # Persist perf security settings across reboots.
    # perf_event_paranoid=-1: allow all perf_event_open() calls (required for perf record -p).
    # ptrace_scope=0: allow any process to ptrace any other owned process (required for perf attach).
    {
      echo 'kernel.perf_event_paranoid = -1'
      echo 'kernel.yama.ptrace_scope = 0'
    } > /etc/sysctl.d/99-perf-profiling.conf
    sysctl --system

    # ── perf profiling service ────────────────────────────────────────────────
    # Install the recording script and a systemd unit that profiles all postgres
    # processes. The unit is enabled but left stopped; start it with
    # bench-perf start <run-id>.
    echo '${base64encode(file("${path.module}/perf_record.sh"))}' | base64 -d > /usr/local/bin/perf_benchctl.sh
    chmod +x /usr/local/bin/perf_benchctl.sh
    {
      echo '[Unit]'
      echo 'Description=perf CPU profiling for PostgreSQL (all processes)'
      echo 'After=docker.service network.target'
      echo 'Wants=docker.service'
      echo ''
      echo '[Service]'
      echo 'Type=simple'
      echo 'ExecStart=/usr/local/bin/perf_benchctl.sh'
      echo 'Restart=always'
      echo 'RestartSec=10'
      echo 'StandardOutput=append:/var/log/perf_benchctl.log'
      echo 'StandardError=append:/var/log/perf_benchctl.log'
      echo ''
      echo '[Install]'
      echo 'WantedBy=multi-user.target'
    } > /etc/systemd/system/perf-benchctl.service
    systemctl daemon-reload

    # ── Vector (metrics agent, optional) ──────────────────────────────────────
    # Only installed when metrics collection is actually requested: skips
    # apt entirely instead of installing Vector just to immediately
    # stop/disable it. Best-effort: if it fails (mirror wedge or otherwise),
    # log and continue rather than failing the whole target bootstrap over
    # an optional sidecar.
    if [ "${var.vector_enabled}" = "true" ]; then
      if curl -fsSL --connect-timeout 15 --max-time 60 --retry 5 --retry-delay 10 https://setup.vector.dev | timeout 60 bash \
        && apt_get_retry update -y \
        && apt_get_retry install -y vector; then
        mkdir -p /var/log/vector
        chown vector:vector /var/log/vector
        echo '${base64encode(templatefile("${path.module}/vector.yaml.tftpl", {
  sink                                  = var.vector_sink
  victoriametrics_endpoint              = trimsuffix(var.vector_sink_victoriametrics_endpoint, "/")
  victoriametrics_token                 = var.vector_sink_victoriametrics_token
  host_metrics_scrape_interval_secs     = var.vector_host_metrics_scrape_interval_secs
  postgres_metrics_scrape_interval_secs = var.vector_postgres_metrics_scrape_interval_secs
  postgres_endpoint                     = "postgresql://${var.pg_user}:${var.pg_password}@localhost:5432"
}))}' | base64 -d > /etc/vector/vector.yaml
        systemctl enable vector
        systemctl start vector || true
      else
        echo "WARNING: vector install failed; continuing without metrics collection" >&2
      fi
    fi

  EOT
}

# ── Placement group ───────────────────────────────────────────────────────────
# Cluster placement groups minimise latency between the target and the driver
# by requesting physical proximity within a single AZ. The driver module places
# its instance in the same group via the exported placement_group_name output.

resource "aws_placement_group" "this" {
  name     = local.name
  strategy = "cluster"
  tags     = merge(var.tags, { Name = local.name })
}

# ── EC2 instance ──────────────────────────────────────────────────────────────

resource "aws_instance" "target" {
  ami                                  = local.ami_id
  instance_type                        = var.instance_type
  subnet_id                            = aws_subnet.this.id
  associate_public_ip_address          = true
  vpc_security_group_ids               = [aws_security_group.target.id]
  key_name                             = aws_key_pair.target.key_name
  instance_initiated_shutdown_behavior = "terminate"
  placement_group                      = aws_placement_group.this.name

  user_data = local.user_data

  # EBS root — OS only, pgdata goes on the NVMe instance store.
  root_block_device {
    volume_size = 30
    volume_type = "gp3"
  }

  tags = merge(var.tags, { Name = "${local.name}-target" })
}
