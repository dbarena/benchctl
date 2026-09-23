#!/bin/sh
# Installs the latest tagged benchctl release for this machine's OS/arch.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/dbarena/benchctl/main/scripts/install.sh | sh
#
# Override the install directory with INSTALL_DIR (default /usr/local/bin),
# or the source repo with BENCHCTL_REPO (default dbarena/benchctl).
set -eu

repo="${BENCHCTL_REPO:-dbarena/benchctl}"
install_dir="${INSTALL_DIR:-/usr/local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

case "$os" in
  linux|darwin) ;;
  *)
    echo "error: unsupported OS: $os (benchctl releases only ship for linux and darwin)" >&2
    exit 1
    ;;
esac
case "$arch" in
  amd64|arm64) ;;
  *)
    echo "error: unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

asset="benchctl-${os}-${arch}"
base_url="https://github.com/${repo}/releases/latest/download"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "==> downloading ${asset} from the latest ${repo} release"
curl -fsSL --retry 5 --retry-delay 10 -o "$tmp/$asset" "${base_url}/${asset}"
curl -fsSL --retry 5 --retry-delay 10 -o "$tmp/checksums.txt" "${base_url}/checksums.txt"

echo "==> verifying checksum"
(cd "$tmp" && grep " ${asset}\$" checksums.txt | sha256sum -c -)

chmod +x "$tmp/$asset"

if [ -w "$install_dir" ]; then
  mv "$tmp/$asset" "$install_dir/benchctl"
else
  echo "==> $install_dir is not writable, using sudo"
  sudo mv "$tmp/$asset" "$install_dir/benchctl"
fi

echo "==> installed benchctl to $install_dir/benchctl"
"$install_dir/benchctl" --version
