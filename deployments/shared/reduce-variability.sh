# Disable services and timers that produce unpredictable I/O or CPU spikes.
# Units absent on the running image are silently skipped (e.g. the AWS SSM
# agent unit on a GCE image). Shared across every EC2/GCE target and driver
# module via file("${path.module}/reduce-variability.sh") — a fragment
# embedded into each module's own user_data/startup-script heredoc, not a
# standalone script (no shebang; runs under whatever shell/flags the
# embedding heredoc already set).
systemctl disable --now \
  unattended-upgrades.service \
  snapd.service \
  snap.amazon-ssm-agent.amazon-ssm-agent.service \
  ModemManager.service \
  multipathd.service \
  udisks2.service \
  sysstat-collect.timer \
  sysstat-summary.timer \
  apt-daily.timer \
  apt-daily-upgrade.timer \
  e2scrub_all.timer \
  fstrim.timer \
  fwupd-refresh.timer \
  man-db.timer \
  motd-news.timer \
  update-notifier-download.timer \
  update-notifier-motd.timer \
  logrotate.timer \
  dpkg-db-backup.timer 2>/dev/null || true
