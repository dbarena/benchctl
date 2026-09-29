terraform {
  required_providers {
    google = {
      source = "hashicorp/google"
      version = "~> 8.1"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.0"
    }
    time = {
      source  = "hashicorp/time"
      version = "~> 0.13"
    }
  }
}

provider "google" {
  # Falls back to the google provider's own default resolution (GOOGLE_CLOUD_PROJECT
  # env var, then gcloud's active config) when project_id is left empty -- see
  # variables.tf. Credentials likewise come from the environment (Application
  # Default Credentials via `gcloud auth application-default login`, or
  # GOOGLE_APPLICATION_CREDENTIALS).
  project = var.project_id != "" ? var.project_id : null
  region  = var.region
}

# Unique 8-char suffix per apply so multiple runs don't collide on resource
# names. Google enforces roughly a week-long cooldown before a deleted
# instance's name can be reused so a fixed name would break re-runs shortly
# after a teardown.
resource "random_string" "suffix" {
  length  = 8
  special = false
  upper   = false
}

locals {
  name = "benchctl-${random_string.suffix.result}"
}

# ── Network (Private IP via VPC peering) ──────────────────────────────────────
# Cloud SQL Private IP requires peering this VPC with Google's managed
# service-producer network via a reserved internal IP range. Meant to be
# created once and reused forever, never torn down per run: GCP's own
# Private Service Access is designed to let many concurrent Cloud SQL
# instances safely share one reserved peering range, and destroying
# google_service_networking_connection is a confirmed, long-standing upstream
# bug (hashicorp/terraform-provider-google#16275) that can hang for a very
# long time after the last Cloud SQL instance using it is gone.
#
# So these 4 resources use a fixed name (not local.name's random suffix) and
# are never actually destroyed by this module: lifecycle.destroy = false means
# this module's own `tofu destroy` can only ever forget them, never delete
# them. Every run gets a brand-new, empty tofu state dir though (see
# deriveWorkDir in internal/providers/opentofu/provider.go), so this module
# has no way to know from its own state whether these already exist in GCP.
# Rather than branch the resources themselves on a pre-flight existence
# check, benchctl's Go orchestration layer runs `tofu import` for each of
# these 4 resources before every apply (see importSharedGCPNetworkResources
# in internal/providers/gcpcloudsql/network.go) -- a failed import just means
# "doesn't exist yet, let apply create it fresh"; a successful import adopts
# the pre-existing object into this run's state with no further branching
# needed here.

locals {
  shared_network_name    = "benchctl-gcp-cloudsql-shared"
  shared_subnetwork_name = "benchctl-gcp-cloudsql-shared"

  # One VPC (global), one subnet per region -- every region's subnet must
  # use a non-overlapping CIDR, since subnet ranges are unique across the
  # whole VPC, not just per-region (confirmed via a real apply: creating a
  # second region's subnet with the same 10.0.1.0/24 block used in
  # us-central1 failed with "Invalid IPCidrRange: ... conflicts with
  # existing subnetwork"). us-central1's entry matches the range already
  # provisioned there in production -- don't change it. Add a new,
  # non-overlapping entry here before ever using this module in any 
  # additional region.
  region_subnet_cidr = {
    "us-central1"  = "10.0.1.0/24"
    "europe-west3" = "10.0.2.0/24"
    "us-east1"     = "10.0.3.0/24"
  }

  network_self_link    = google_compute_network.this.self_link
  subnetwork_self_link = google_compute_subnetwork.this.self_link
}

resource "google_compute_network" "this" {
  name                    = local.shared_network_name
  auto_create_subnetworks = false

  lifecycle {
    destroy = false
  }
}

# Subnetwork the co-located GCE driver attaches to. Cloud SQL's Private IP
# itself lives in the peered range below, not in this subnetwork -- this
# exists purely so the driver has somewhere to live in the same VPC/region.
resource "google_compute_subnetwork" "this" {
  name          = local.shared_subnetwork_name
  ip_cidr_range = local.region_subnet_cidr[var.region]
  region        = var.region
  network       = google_compute_network.this.id

  lifecycle {
    destroy = false
  }
}

resource "google_compute_global_address" "private_ip_alloc" {
  name          = "${local.shared_network_name}-psa"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  prefix_length = 16
  network       = google_compute_network.this.id

  lifecycle {
    destroy = false
  }
}

resource "google_service_networking_connection" "this" {
  network                 = google_compute_network.this.id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.private_ip_alloc.name]

  lifecycle {
    destroy = false
  }
}

# The peering connection API reports success before route propagation across
# Google's network fabric actually finishes. The first Cloud SQL instance to
# use a brand-new peering can hit this gap directly -- the create call itself
# succeeds but OpenTofu's own wait-for-completion polling aborts early with an
# empty error, even though the instance goes on to finish creating fine. Only
# needed when creating the peering fresh -- an adopted, already-established
# peering needs no fresh propagation wait. var.wait_for_peering_propagation is
# set by the same Go import step above: true when the peering connection
# import failed (created fresh this run), false when it was adopted.
resource "time_sleep" "wait_for_peering" {
  count           = var.wait_for_peering_propagation ? 1 : 0
  depends_on      = [google_service_networking_connection.this]
  create_duration = "180s"

  lifecycle {
    destroy = false
  }
}

# ── Cloud SQL instance ────────────────────────────────────────────────────────

resource "google_sql_database_instance" "this" {
  name                = local.name
  database_version    = var.engine_version
  region              = var.region
  deletion_protection = false # benchctl must be able to `tofu destroy` on teardown

  settings {
    tier              = var.db_instance_type
    edition           = "ENTERPRISE"
    availability_type = "ZONAL"      # single-zone, no HA for comparison with other providers

    location_preference {
      zone = var.zone
    }

    disk_type       = var.disk_type # required to be HYPERDISK_BALANCED by the N4 machine series -- PD_SSD isn't valid with N4, see variables.tf
    disk_size       = var.disk_size_gb
    disk_autoresize = false # fixed size for predictable benchmarking
    data_disk_provisioned_iops       = var.disk_provisioned_iops
    data_disk_provisioned_throughput = var.disk_provisioned_throughput_mibps

    ip_configuration {
      ipv4_enabled    = false # no public endpoint
      private_network = local.network_self_link
      ssl_mode        = "ENCRYPTED_ONLY"
    }

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
      transaction_log_retention_days = var.backup_retention_period
    }

    # Closes the same observability gap RDS's aws_db_parameter_group closes:
    # checkpoint timing and slow queries, exported to Cloud Logging (on by
    # default for Cloud SQL, no separate export resource needed like RDS's
    # CloudWatch log group). See scripts/fetch-gcp-cloudsql-logs.sh.
    database_flags {
      name  = "log_checkpoints"
      value = "on"
    }

    database_flags {
      name  = "log_min_duration_statement"
    # ms. 1000 would log go-tpc's bulk-load INSERTs during data loading, just adding noise to the logs.
    # 5000 keeps genuine benchmark-phase anomalies.
      value = "5000"
    }

    user_labels = var.tags
  }

  # Sets the built-in postgres superuser's password explicitly so it's known
  # and in Terraform state, rather than Google generating one we can't
  # retrieve. The pg_user/pg_password application account below is what
  # benchctl actually connects with.
  root_password = var.pg_password

  depends_on = [
    google_compute_network.this,
    time_sleep.wait_for_peering,
  ]
}

resource "google_sql_database" "tpcc" {
  name     = "tpcc"
  instance = google_sql_database_instance.this.name

  # Forces destroy order: OpenTofu destroys dependents before dependencies, so
  # this makes the database (which owns objects created by the bench role)
  # get dropped before the user -- without this, the two resources have no
  # relationship to each other and OpenTofu can destroy them concurrently,
  # which races DROP DATABASE against DROP USER and can fail with "role bench
  # cannot be dropped because some objects depend on it".
  depends_on = [google_sql_user.bench]
}

resource "google_sql_user" "bench" {
  name     = var.pg_user
  instance = google_sql_database_instance.this.name
  password = var.pg_password
}
