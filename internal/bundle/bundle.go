// Package bundle resolves the scenario YAML paths that point into the trees
// shipped with the benchctl binary, "deployments/" and "services/", to a real
// on-disk location, independent of the process's working directory. Scenarios
// reach those trees through module: and services[].definition fields.
package bundle

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dbarena/benchctl/internal/buildinfo"
)

// bundledRoots are the top-level directories shipped as part of the benchctl
// binary. Every other path (absolute, or relative to a custom module or service
// definition elsewhere on disk) resolves as a plain filesystem path, so custom
// and external modules and services keep working unchanged.
var bundledRoots = []string{"deployments/", "services/"}

func isBundled(rel string) bool {
	for _, root := range bundledRoots {
		if strings.HasPrefix(rel, root) {
			return true
		}
	}
	return false
}

// embedded holds a symlink-free copy of deployments/ and services/, staged
// by `mise run stage-embed` (a dependency of build/build-linux/build-darwin)
// since go:embed rejects the real trees' symlinks (e.g.
// reduce-variability.sh, shared across several modules; service init
// scripts, shared across multiple docker-compose services).
//
//go:embed embedded/deployments embedded/services
var embedded embed.FS

// Dir resolves relPath, a directory under one of the bundled roots such as
// "./deployments/ec2/postgres" or "./services/postgres", to a real on-disk
// directory containing its files.
//
// Dev builds with a discoverable live checkout (buildinfo.SourceDir) read
// straight from that checkout's tree, so editing a file takes effect without
// a rebuild. Every other build, whether a release build or a dev build whose
// checkout no longer exists on this machine, extracts the directory from the
// bundle embedded at build time into a fresh temp directory. The caller must
// invoke cleanup once done with the returned directory; it is a no-op for
// non-bundled paths and the live-checkout case.
func Dir(relPath string) (dir string, cleanup func(), err error) {
	rel := filepath.ToSlash(filepath.Clean(relPath))
	rel = strings.TrimPrefix(rel, "./")

	if !isBundled(rel) {
		abs, err := filepath.Abs(relPath)
		if err != nil {
			return "", nil, err
		}
		return abs, func() {}, nil
	}

	if root := liveCheckoutRoot(rel); root != "" {
		return filepath.Join(root, rel), func() {}, nil
	}

	tmp, err := os.MkdirTemp("", "benchctl-bundle-*")
	if err != nil {
		return "", nil, fmt.Errorf("bundle: create temp dir: %w", err)
	}
	sub, err := fs.Sub(embedded, "embedded/"+rel)
	if err != nil {
		os.RemoveAll(tmp)
		return "", nil, fmt.Errorf("bundle: %q not found in embedded bundle: %w", relPath, err)
	}
	if err := os.CopyFS(tmp, sub); err != nil {
		os.RemoveAll(tmp)
		return "", nil, fmt.Errorf("bundle: extract %q: %w", relPath, err)
	}
	return tmp, func() { _ = os.RemoveAll(tmp) }, nil
}

// File resolves relPath, a file under one of the bundled roots such as
// "./services/postgres/docker-compose.yml", to a real absolute file path. It
// resolves the file's containing directory via Dir, so sibling files such as
// init/ scripts land alongside it in the embedded case, then rejoins the
// file's own name.
func File(relPath string) (path string, cleanup func(), err error) {
	rel := filepath.ToSlash(filepath.Clean(relPath))
	dir, cleanup, err := Dir(filepath.Dir(rel))
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(dir, filepath.Base(rel)), cleanup, nil
}

// liveCheckoutRoot returns the dev build's checkout root if this is a dev
// build and rel still exists under it on this machine (it won't on a remote
// host the binary was cross-compiled for), otherwise "".
func liveCheckoutRoot(rel string) string {
	if buildinfo.BuildKind != "dev" || buildinfo.SourceDir == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(buildinfo.SourceDir, rel)); err != nil {
		return ""
	}
	return buildinfo.SourceDir
}
