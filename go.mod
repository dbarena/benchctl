module github.com/dbarena/benchctl

// The `go` directive is the minimum Go version this module supports.
// `toolchain` is the version CI and local dev actually build with, and it is the
// single source of truth: mise reads it from here (see mise.toml).
//
// Two rules when bumping Go:
//   1. `toolchain` must be fully qualified (go1.26.1, not go1.26). mise silently
//      reads no version at all from a short form, and CI then picks its own Go.
//   2. `toolchain` must stay above the `go` directive, or `go mod tidy` deletes it.
go 1.26.0

toolchain go1.26.1

require (
	github.com/fatih/color v1.19.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/spf13/cobra v1.10.2
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.59.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.29.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
