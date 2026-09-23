// Package ec2 configures a sshdriver.Provider for EC2-based load generators
// provisioned via OpenTofu. All cloud-agnostic bootstrap logic (tofu
// apply/destroy, SSH/SCP bootstrap, benchctl resume handoff) lives in
// internal/providers/sshdriver; this package only supplies EC2-specific
// defaults.
package ec2

import (
	"regexp"
	"strings"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/providers/sshdriver"
)

// gravitonRe matches EC2 instance family names that use Graviton (ARM64) processors.
// Graviton families embed 'g' immediately after the generation digit, e.g. t4g, c8gd, m7g.
var gravitonRe = regexp.MustCompile(`[0-9]+g`)

// New constructs a driver Provider for EC2-based load generators.
func New(cfg *config.Config) *sshdriver.Provider {
	return sshdriver.New(sshdriver.Config{
		ProviderName:         "ec2",
		DefaultSSHUser:       "ubuntu",
		ArchFromInstanceType: archFromInstanceType,
		Benchctl:             cfg,
	})
}

// archFromInstanceType returns "arm64" for Graviton (ARM) instance families and
// "amd64" for all others. Graviton families embed 'g' immediately after the
// generation digit (e.g. t4g, c8gd, m7g).
func archFromInstanceType(instanceType string) string {
	family := strings.SplitN(instanceType, ".", 2)[0]
	if gravitonRe.MatchString(family) {
		return "arm64"
	}
	return "amd64"
}
