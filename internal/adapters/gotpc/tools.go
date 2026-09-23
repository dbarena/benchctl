package gotpc

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// ensureGoTPC returns the path to the go-tpc binary, or an error if it isn't
// on PATH. Remote bootstrap installs it system-wide on machine bootstrap
// and local use installs it via `mise run install-go-tpc`.
func ensureGoTPC(_ context.Context, _ io.Writer) (string, error) {
	path, err := exec.LookPath("go-tpc")
	if err != nil {
		return "", fmt.Errorf("go-tpc not found in PATH\n" +
			"Install it locally: mise run install-go-tpc\n" +
			"(remote load drivers install it automatically during bootstrap.)")
	}
	return path, nil
}
