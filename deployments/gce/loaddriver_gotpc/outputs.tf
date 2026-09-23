output "public_ip" {
  description = "Public (ephemeral external) IP of the driver instance. Used by benchctl for SSH/SCP bootstrap."
  value       = google_compute_instance.driver.network_interface[0].access_config[0].nat_ip
}

output "ssh_private_key_path" {
  description = "Absolute path to the SSH private key on the machine running benchctl. Written into the per-run tofu working directory (./tofu-state/<runID>/loaddriver_gotpc)."
  value       = local_file.driver_key.filename
}
