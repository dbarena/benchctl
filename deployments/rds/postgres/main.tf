terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.63"
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

# Unique 8-char suffix per apply so multiple runs don't collide on resource names.
resource "random_string" "suffix" {
  length  = 8
  special = false
  upper   = false
}

locals {
  name = "benchctl-${random_string.suffix.result}"

  # gp3 minimum allocated_storage (GiB) at which RDS Postgres accepts an
  # explicit `iops` argument at all. Below it, `iops` must be omitted
  # entirely (not just left at the default value) or CreateDBInstance fails
  # ("InvalidParameterCombination: You can't specify IOPS or storage
  # throughput for engine name postgres and a storage size less than 400").
  min_storage_gb_for_custom_iops = 400
}

# ── Network ───────────────────────────────────────────────────────────────────
# RDS requires a DB subnet group spanning >= 2 AZs even for a Single-AZ
# instance (hard AWS API constraint, not a choice). The instance itself is
# explicitly pinned to the first AZ via `availability_zone` below, and the
# `subnet_id` output points at that same AZ's subnet, so the co-located
# loaddriver_gotpc EC2 instance deterministically lands in the same AZ instead
# of AWS's default placement algorithm choosing arbitrarily so both instances
# are placed as close as possible.
#
# The second subnet exists only to satisfy the subnet-group AZ-count
# requirement; nothing is launched there.

data "aws_availability_zones" "available" {
  state = "available"
}

resource "aws_vpc" "this" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true
  tags                 = merge(var.tags, { Name = local.name })
}

# Needed so the co-located driver instance (placed in aws_subnet.a via the
# subnet_id output) has internet egress for its bootstrap (go-tpc/Vector
# install, SSH from the operator's machine). The RDS instance itself stays
# unreachable from the internet regardless, via publicly_accessible = false
# and the VPC-CIDR-only security group below.
resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = merge(var.tags, { Name = local.name })
}

resource "aws_route_table" "this" {
  vpc_id = aws_vpc.this.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }

  tags = merge(var.tags, { Name = local.name })
}

resource "aws_subnet" "a" {
  vpc_id                  = aws_vpc.this.id
  cidr_block              = "10.0.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true
  tags                    = merge(var.tags, { Name = "${local.name}-a" })
}

resource "aws_subnet" "b" {
  vpc_id            = aws_vpc.this.id
  cidr_block        = "10.0.2.0/24"
  availability_zone = data.aws_availability_zones.available.names[1]
  tags              = merge(var.tags, { Name = "${local.name}-b" })
}

resource "aws_route_table_association" "a" {
  subnet_id      = aws_subnet.a.id
  route_table_id = aws_route_table.this.id
}

resource "aws_db_subnet_group" "this" {
  name       = local.name
  subnet_ids = [aws_subnet.a.id, aws_subnet.b.id]
  tags       = merge(var.tags, { Name = local.name })
}

# ── Security group ────────────────────────────────────────────────────────────

resource "aws_security_group" "target" {
  name   = "${local.name}-target"
  vpc_id = aws_vpc.this.id

  # PostgreSQL — VPC-only. The driver reaches the instance over its private
  # VPC endpoint address; no public access.
  ingress {
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = [aws_vpc.this.cidr_block]
  }

  tags = merge(var.tags, { Name = "${local.name}-target" })
}

resource "aws_vpc_security_group_egress_rule" "target_all" {
  security_group_id = aws_security_group.target.id
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

# ── Postgres logging ──────────────────────────────────────────────────────────
# Ensure we capture basic logs
resource "aws_db_parameter_group" "postgres" {
  name   = local.name
  family = "postgres${split(".", var.engine_version)[0]}"

  parameter {
    name         = "log_checkpoints"
    value        = "1"
    apply_method = "immediate"
  }

  parameter {
    name         = "log_min_duration_statement"
    # ms. 1000 would log go-tpc's bulk-load INSERTs during data loading, just adding noise to the logs.
    # 5000 keeps genuine benchmark-phase anomalies.
    value        = "5000"
    apply_method = "immediate"
  }

  tags = merge(var.tags, { Name = local.name })
}

# Declared explicitly so retention_in_days here takes precedence over the default (never expires).
resource "aws_cloudwatch_log_group" "postgres" {
  name              = "/aws/rds/instance/${local.name}/postgresql"
  retention_in_days = 1
  tags              = merge(var.tags, { Name = local.name })
}

# ── RDS instance ──────────────────────────────────────────────────────────────

resource "aws_db_instance" "this" {
  identifier     = local.name
  engine         = "postgres"
  engine_version = var.engine_version
  instance_class = var.db_instance_class

  allocated_storage = var.allocated_storage_gb
  storage_type      = "gp3"
  # null (omitted) below local.min_storage_gb_for_custom_iops as AWS rejects
  # the `iops` argument outright at smaller sizes, see that local's comment.
  iops = var.allocated_storage_gb >= local.min_storage_gb_for_custom_iops ? var.iops : null
  # Same gate as iops, plus disk_throughput_mibps==0 (the default) also omits
  # the argument.
  storage_throughput = (var.allocated_storage_gb >= local.min_storage_gb_for_custom_iops && var.disk_throughput_mibps > 0) ? var.disk_throughput_mibps : null

  db_subnet_group_name            = aws_db_subnet_group.this.name
  vpc_security_group_ids          = [aws_security_group.target.id]
  availability_zone               = data.aws_availability_zones.available.names[0]
  multi_az                        = false
  publicly_accessible             = false
  backup_retention_period         = var.backup_retention_period
  parameter_group_name            = aws_db_parameter_group.postgres.name
  enabled_cloudwatch_logs_exports = ["postgresql"]

  # Enabled unconditionally for additional diagnostics. This feature is free for the default 7-day retention window.
  performance_insights_enabled          = true
  performance_insights_retention_period = 7

  db_name  = "tpcc"
  username = var.pg_user
  password = var.pg_password

  skip_final_snapshot = true
  apply_immediately   = true

  tags = merge(var.tags, { Name = local.name })

  depends_on = [aws_cloudwatch_log_group.postgres]
}
