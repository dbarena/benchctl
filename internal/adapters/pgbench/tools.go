package pgbench

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// pgbenchGlobs are where packaged pgbench binaries live outside PATH; the
// highest version among all matches wins.
//   - Debian and Ubuntu ship it with the server package (postgresql-NN), not
//     the client one;
//   - Homebrew's postgresql@NN formulae are keg-only.
var pgbenchGlobs = []string{
	"/usr/lib/postgresql/*/bin/pgbench",
	"/opt/homebrew/opt/postgresql@*/bin/pgbench",
	"/usr/local/opt/postgresql@*/bin/pgbench",
}

// ensurePgbench returns the path to pgbench: the one on PATH, or else the
// newest packaged one. Remote load drivers install it during bootstrap when
// the driver sets pgbench_enabled; `mise run install-pgbench` installs it
// locally.
func ensurePgbench() (string, error) {
	if path, err := exec.LookPath("pgbench"); err == nil {
		return path, nil
	}
	if path := newestPgbench(pgbenchGlobs...); path != "" {
		return path, nil
	}
	return "", fmt.Errorf("pgbench not found in PATH or in %v\n"+
		"Install it locally: mise run install-pgbench\n"+
		"(remote load drivers install it during bootstrap when the driver sets pgbench_enabled: true.)",
		pgbenchGlobs)
}

// newestPgbench returns the match of globs with the highest PostgreSQL major
// version, e.g. .../postgresql/17/bin/pgbench over .../16/..., or "" if
// nothing matches.
func newestPgbench(globs ...string) string {
	var paths []string
	for _, g := range globs {
		m, _ := filepath.Glob(g)
		paths = append(paths, m...)
	}
	if len(paths) == 0 {
		return ""
	}
	sort.SliceStable(paths, func(i, j int) bool { return majorVersion(paths[i]) < majorVersion(paths[j]) })
	return paths[len(paths)-1]
}

// versionDirRe matches the version in the install directory: "17" in
// /usr/lib/postgresql/17/bin or "postgresql@17" in Homebrew's opt/.
var versionDirRe = regexp.MustCompile(`^(?:postgresql@)?(\d+)$`)

// majorVersion extracts the major version from the directory two levels above
// the binary, or 0.
func majorVersion(path string) int {
	dir := filepath.Base(filepath.Dir(filepath.Dir(path)))
	m := versionDirRe.FindStringSubmatch(dir)
	if m == nil {
		return 0
	}
	v, _ := strconv.Atoi(m[1])
	return v
}
