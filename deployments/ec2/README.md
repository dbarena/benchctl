# EC2 deployment modules

Each module provisions one EC2 instance via OpenTofu. Every module runs the same `user_data` hardening step, which disables the systemd services and timers in the Ubuntu 24.04 base image that cause benchmark variability.

## Disabled services and timers

### `unattended-upgrades.service`
Downloads and installs packages at unpredictable times, starting immediately after instance boot. Any apt activity during a benchmark saturates I/O and CPU without showing up in the workload metrics.

### `sysstat-collect.timer` / `sysstat-summary.timer`
`sysstat-collect` fires **every 5 minutes**, so it overlaps almost every benchmark run. `sadc` sweeps all block-device and CPU counters on each tick, adding measurable I/O and CPU overhead at a fixed cadence. `sysstat-summary` produces the daily roll-up from those samples.

### `apt-daily.timer` / `apt-daily-upgrade.timer`
`apt-daily` refreshes the package index; `apt-daily-upgrade` runs the unattended upgrade. Both timers use `RandomizedDelaySec`, so despite their nominal schedule they can fire anywhere within a wide window. The upgrade timer triggers a full `apt-get upgrade` with heavy I/O.

### `e2scrub_all.timer`
Runs `e2fsck` online scrubbing across **all** mounted ext4 filesystems, including `/mnt/nvme` where PostgreSQL data lives. Firing mid-run puts direct read I/O contention on the database.

### `fstrim.timer`
Sends TRIM/discard commands to SSDs. On the NVMe instance store, erase processing introduces latency spikes that surface as tail-latency outliers.

### `snapd.service` / `snap.amazon-ssm-agent.amazon-ssm-agent.service`
The Snap daemon polls for package updates and refreshes metadata on a background schedule. The Amazon SSM agent polls the AWS control plane at regular intervals. Benchmarking needs neither, and both add background network and CPU activity.

### `fwupd-refresh.timer`
Fetches firmware update metadata over the network. A short-lived benchmark instance has no use for it, and it adds unpredictable network I/O.

### `man-db.timer`
Rebuilds the man-page index database, pure I/O with no value on a benchmark host.

### `motd-news.timer` / `update-notifier-download.timer` / `update-notifier-motd.timer`
All three fetch update notifications and MOTD content over the network. Each is minor; together they add background network activity.

### `logrotate.timer` / `dpkg-db-backup.timer`
Both are scheduled at midnight with a randomized delay, so they can fire during any run window. `logrotate` does I/O across multiple log files; `dpkg-db-backup` copies the dpkg status database.

### `ModemManager.service` / `multipathd.service` / `udisks2.service`
An EC2 instance has no modems, no multipath SCSI targets, and no removable disks, so all three sit idle. Disabling them removes their polling and udev-watching overhead.

## Kept services

`irqbalance` stays running on purpose. Disabling it on a multi-queue NVMe and multi-core Graviton instance risks pinning all interrupts to CPU 0, which creates worse and less reproducible contention than letting the daemon distribute them.
