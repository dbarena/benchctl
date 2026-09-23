// Package gce configures a sshdriver.Provider for GCE-based load generators
// provisioned via OpenTofu. All cloud-agnostic bootstrap logic (tofu
// apply/destroy, SSH/SCP bootstrap, benchctl resume handoff) lives in
// internal/providers/sshdriver; this package only supplies GCE-specific
// defaults.
package gce

import (
	"strings"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/providers/sshdriver"
)

// armMachineFamilies are GCE machine-type family prefixes that run on ARM64
// processors: t2a (Tau T2A, Ampere Altra) and c4a (Axion, Google's own ARM CPU).
// Unlike AWS's Graviton naming (a 'g' embedded after the generation digit),
// GCP's ARM families don't share a common naming pattern, so this is an
// explicit allow-list rather than a regex.
var armMachineFamilies = map[string]bool{
	"t2a": true,
	"c4a": true,
}

// New constructs a driver Provider for GCE-based load generators.
func New(cfg *config.Config) *sshdriver.Provider {
	return sshdriver.New(sshdriver.Config{
		ProviderName:         "gce",
		DefaultSSHUser:       "ubuntu",
		ArchFromInstanceType: archFromInstanceType,
		Benchctl:             cfg,
	})
}

// archFromInstanceType returns "arm64" for Axion/Tau T2A GCE machine families
// and "amd64" for all others (N2, N4, C4, C3, etc.).
func archFromInstanceType(instanceType string) string {
	family := strings.SplitN(instanceType, "-", 2)[0]
	if armMachineFamilies[family] {
		return "arm64"
	}
	return "amd64"
}
