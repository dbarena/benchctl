// Package buildinfo holds build-time metadata injected via linker flags
// (see mise.toml's build/build-linux/build-darwin tasks). It replaces
// filesystem sniffing (e.g. checking for a .git directory in the current
// working directory) as the source of truth for "is this a developer build
// running from a live checkout, or a release build". Sniffing CWD breaks once
// benchctl can be invoked from an arbitrary directory.
package buildinfo

var (
	// Version is a human-readable build identifier: "0.1.0+20260904-abcdef1"
	// for a dev build, or the exact git tag ("v0.2.0") for a release build.
	Version = "dev"

	// Commit is the short git commit hash benchctl was built from,
	// independent of Version (which may be an exact release tag with no
	// embedded hash).
	Commit = "unknown"

	// BuildKind is "dev" (built via `mise run build*` from a live checkout by
	// developers and internal CI) or "release" (built by the tag-triggered
	// release workflow). Consumers use this to decide, e.g., whether to read
	// deployments/ from a live checkout vs. the embedded bundle, and whether
	// to SCP a locally built binary vs. have a remote host self-fetch a
	// tagged release from GitHub.
	BuildKind = "dev"

	// SourceDir is the absolute path to the checkout a dev build was built
	// from. Empty for release builds, which have no matching checkout to fall
	// back to on an arbitrary machine.
	SourceDir = ""

	// Repo is the GitHub "org/repo" release assets are published under and
	// fetched from. Overridable so another wrapper binary embedding this
	// package as a library can point at its own release feed.
	Repo = "dbarena/benchctl"
)
