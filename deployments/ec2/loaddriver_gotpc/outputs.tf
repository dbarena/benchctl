output "public_ip" {
  description = "Public IP of the driver instance. Used by benchctl for SSH/SCP bootstrap."
  value       = aws_instance.driver.public_ip
}

output "ssh_private_key_path" {
  description = "Absolute path to the SSH private key on the machine running benchctl. Written into the per-run tofu working directory (./tofu-state/<runID>/loaddriver_gotpc)."
  value       = local_file.driver_key.filename
}
