output "host" {
  description = "Private IP of the Postgres instance. Used by the driver for the ready_check and go-tpc — both run within the VPC."
  value       = aws_instance.target.private_ip
}

output "public_ip" {
  description = "Public IP of the Postgres instance. For ad-hoc SSH access only."
  value       = aws_instance.target.public_ip
}

output "port" {
  description = "PostgreSQL port exposed by the Postgres instance."
  value       = "5432"
}

output "db" {
  description = "Database name created by the init script for benchmarking."
  value       = "tpcc"
}

output "subnet_id" {
  description = "Subnet the instance was placed in. Pass to the loaddriver module so both instances are colocated."
  value       = aws_subnet.this.id
}

output "placement_group_name" {
  description = "Cluster placement group shared by target and driver. Pass to the loaddriver module so both instances are placed on proximate hardware."
  value       = aws_placement_group.this.name
}

output "ssh_private_key_path" {
  description = "Absolute path to the SSH private key on the machine running benchctl. Written into the per-run tofu working directory (./tofu-state/<runID>/postgres)."
  value       = local_file.target_key.filename
}

output "ssh_user" {
  description = "SSH username for the target instance. Determined by the Ubuntu 24.04 LTS AMI."
  value       = "ubuntu"
}

output "vendor" {
  description = "Vendor this target runs on. Used e.g. for fetching diagnostics."
  value       = "aws"
}
