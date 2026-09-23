package engine

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// discardSettledThreshold is the discard completion rate (discards/s) below
// which the storage device counts as done processing TRIM commands.
//
// On the AWS NVMe instances used for benchmarking, fstrim peaks at ~87-96
// discards/s (~11 GB/s per device), then declines over ~2-3 minutes; the
// quiescent baseline is 0-0.10 discards/s. A threshold of 10/s sits ~100x
// above that noise floor, and once the rate drops under it the device has
// worked through the bulk of the TRIM backlog, leaving residual activity too
// small to skew benchmarks.
const discardSettledThreshold = 10.0 // discards/s

const trimPollInterval = 2 * time.Second

// runTrimStep issues sudo fstrim -av on the remote target. When wait is true
// it additionally blocks until the block devices' discard-completion rate
// drops below discardSettledThreshold or the timeout expires. A negative
// timeout means wait indefinitely.
func runTrimStep(ctx context.Context, out io.Writer, keyPath, user, host string, wait bool, timeout time.Duration) error {
	fmt.Fprintln(out, "  trim: issuing fstrim -av...")
	if err := RunSSH(ctx, out, keyPath, user, host, "sudo fstrim -av"); err != nil {
		return fmt.Errorf("fstrim: %w", err)
	}

	if !wait {
		fmt.Fprintln(out, "  trim: fstrim issued (not waiting for completion)")
		return nil
	}

	hasDeadline := timeout > 0
	var deadline time.Time
	if hasDeadline {
		deadline = time.Now().Add(timeout)
	}

	fmt.Fprintln(out, "  trim: waiting for discards to settle...")

	var prevCount int64
	var prevTime time.Time

	for {
		if hasDeadline && time.Now().After(deadline) {
			fmt.Fprintln(out, "  trim: timeout reached before discards settled")
			return nil
		}

		count, err := readBlockDeviceDiscards(ctx, keyPath, user, host)
		if err != nil {
			return fmt.Errorf("trim: reading diskstats: %w", err)
		}
		now := time.Now()

		if prevTime.IsZero() {
			prevCount = count
			prevTime = now
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(trimPollInterval):
			}
			continue
		}

		elapsed := now.Sub(prevTime).Seconds()
		rate := float64(count-prevCount) / elapsed
		prevCount = count
		prevTime = now

		if rate < discardSettledThreshold {
			fmt.Fprintf(out, "  trim: discards settled (%.1f/s)\n", rate)
			return nil
		}

		fmt.Fprintf(out, "  trim: discards active (%.1f/s), waiting...\n", rate)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(trimPollInterval):
		}
	}
}

// readBlockDeviceDiscards reads /proc/diskstats on the remote host and sums
// discards_completed for block devices only (those listed in /sys/block/,
// which excludes partitions). Field index 14 (0-based) in diskstats is the
// discards_completed counter.
func readBlockDeviceDiscards(ctx context.Context, keyPath, user, host string) (int64, error) {
	// Single round-trip: list block device names, then dump diskstats.
	const remoteCmd = `ls /sys/block/ && printf '\n---\n' && cat /proc/diskstats`
	raw, err := RunSSHCapture(ctx, keyPath, user, host, remoteCmd)
	if err != nil {
		return 0, err
	}

	parts := strings.SplitN(raw, "\n---\n", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("unexpected diskstats output format")
	}

	blockDevs := make(map[string]bool)
	for _, name := range strings.Fields(parts[0]) {
		blockDevs[name] = true
	}

	var total int64
	for _, line := range strings.Split(parts[1], "\n") {
		fields := strings.Fields(line)
		// diskstats lines with discard stats have at least 15 fields:
		// major minor name ... discards_completed(14) ...
		if len(fields) < 15 {
			continue
		}
		if !blockDevs[fields[2]] {
			continue
		}
		n, err := strconv.ParseInt(fields[14], 10, 64)
		if err != nil {
			continue
		}
		total += n
	}
	return total, nil
}
