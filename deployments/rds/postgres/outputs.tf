output "host" {
  description = "RDS endpoint address. Private/VPC-only."
  value       = aws_db_instance.this.address
}

output "port" {
  description = "PostgreSQL port exposed by the RDS instance."
  value       = aws_db_instance.this.port
}

output "db" {
  description = "Database name created for benchmarking."
  value       = aws_db_instance.this.db_name
}

output "user" {
  description = "Database username for the benchmark."
  value       = var.pg_user
}

output "password" {
  description = "Database user's password."
  value       = var.pg_password
  # Sensitive only hides this from plan/apply CLI output but this is a throwaway benchmark-credential.
  sensitive   = true
}

output "subnet_id" {
  description = "Subnet in the RDS instance's pinned AZ. Pass to the loaddriver module so both land in the same VPC and AZ."
  value       = aws_subnet.a.id
}

output "db_instance_identifier" {
  description = "RDS DB instance identifier. Useful to fetch logs."
  value       = aws_db_instance.this.identifier
}

output "sslmode" {
  description = "Forces TLS on all connections."
  value       = "require"
}

output "dbi_resource_id" {
  description = "Performance Insights' API (GetResourceMetrics, DescribeDimensionKeys) resource id."
  value       = aws_db_instance.this.resource_id
}

output "vendor" {
  description = "Vendor this target runs on. Used e.g. for fetching diagnostics."
  value       = "aws"
}
