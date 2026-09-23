package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/runstate"
	"github.com/dbarena/benchctl/internal/tofustate"
)

// restoreTofuWorkDirs unpacks state.TofuState into a fresh temp directory and
// updates _tofu_work_dir (and any output path rooted under it, such as
// ssh_private_key_path) for any component whose original work dir no longer
// exists on disk. With force it restores from the blob even when the local dirs
// are still there.
func restoreTofuWorkDirs(state *runstate.State, force bool) error {
	targetWD := state.TargetOutputs[engine.OutputKeyTofuWorkDir]
	driverWD := state.DriverOutputs[engine.OutputKeyTofuWorkDir]
	targetMissing := targetWD != "" && (force || !pathExists(targetWD))
	driverMissing := driverWD != "" && (force || !pathExists(driverWD))
	if !targetMissing && !driverMissing {
		return nil
	}
	restored, err := tofustate.Unpack(state.TofuState)
	if err != nil {
		return err
	}
	if targetMissing {
		if dir, ok := restored["target"]; ok {
			patchOutputPaths(state.TargetOutputs, targetWD, dir)
		}
	}
	if driverMissing {
		if dir, ok := restored["driver"]; ok {
			patchOutputPaths(state.DriverOutputs, driverWD, dir)
		}
	}
	return nil
}

// patchOutputPaths rewrites every output value rooted under oldDir to use
// newDir instead. Handles exact matches (the work dir itself) and sub-paths
// (e.g. ssh_private_key_path inside the work dir).
func patchOutputPaths(outputs map[string]string, oldDir, newDir string) {
	prefix := oldDir + string(filepath.Separator)
	for k, v := range outputs {
		if v == oldDir {
			outputs[k] = newDir
		} else if strings.HasPrefix(v, prefix) {
			outputs[k] = filepath.Join(newDir, v[len(prefix):])
		}
	}
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
