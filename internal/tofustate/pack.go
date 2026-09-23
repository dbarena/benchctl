// Package tofustate packs and unpacks an OpenTofu working directory tree
// into a single base64-encoded tar+gzip blob suitable for TEXT column storage.
// All encoding uses the Go standard library, so no external tools are needed.
package tofustate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RunDir returns the run-scoped tofu working-directory root:
// ~/.benchctl/tofu-state/<runID>. Target and driver module subdirectories
// live under it. Callers can use this to locate or remove a run's local
// tofu state without needing any provider outputs; it's a pure function of
// the run ID. Rooting this under the user's home directory (rather than the
// process's current working directory) is what lets benchctl be invoked
// from any directory.
func RunDir(runID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("tofu-state", runID)
	}
	return filepath.Join(home, ".benchctl", "tofu-state", runID)
}

// Pack archives each directory in dirs into a single gzip-compressed tar and
// returns a base64-encoded string. Keys in dirs become path prefixes in the
// archive (e.g. "target" → "target/terraform.tfstate").
func Pack(dirs map[string]string) (string, error) {
	var buf bytes.Buffer
	b64w := base64.NewEncoder(base64.StdEncoding, &buf)
	gzw := gzip.NewWriter(b64w)
	tw := tar.NewWriter(gzw)

	for component, dir := range dirs {
		if err := addDir(tw, component, dir); err != nil {
			return "", fmt.Errorf("tofustate pack %s: %w", component, err)
		}
	}

	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("tofustate: close tar: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return "", fmt.Errorf("tofustate: close gzip: %w", err)
	}
	if err := b64w.Close(); err != nil {
		return "", fmt.Errorf("tofustate: close base64: %w", err)
	}
	return buf.String(), nil
}

// Unpack decodes a blob produced by Pack into a fresh temp directory tree and
// returns a map of component name → extracted directory path.
func Unpack(blob string) (map[string]string, error) {
	b64r := base64.NewDecoder(base64.StdEncoding, strings.NewReader(blob))
	gzr, err := gzip.NewReader(b64r)
	if err != nil {
		return nil, fmt.Errorf("tofustate: gzip: %w", err)
	}
	defer gzr.Close()

	parent, err := os.MkdirTemp("", "benchctl-tofu-*")
	if err != nil {
		return nil, fmt.Errorf("tofustate: mktemp: %w", err)
	}

	tr := tar.NewReader(gzr)
	components := map[string]string{}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tofustate: read tar: %w", err)
		}
		if hdr.FileInfo().IsDir() {
			continue
		}

		parts := strings.SplitN(hdr.Name, "/", 2)
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		component, relPath := parts[0], parts[1]

		destDir := filepath.Join(parent, component)
		if _, ok := components[component]; !ok {
			if err := os.MkdirAll(destDir, 0o755); err != nil {
				return nil, fmt.Errorf("tofustate: mkdir %s: %w", destDir, err)
			}
			components[component] = destDir
		}

		dest := filepath.Join(destDir, filepath.FromSlash(relPath))
		if rel, err := filepath.Rel(destDir, dest); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, fmt.Errorf("tofustate: mkdir: %w", err)
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
		if err != nil {
			return nil, fmt.Errorf("tofustate: create %s: %w", dest, err)
		}
		_, copyErr := io.Copy(f, tr)
		f.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("tofustate: write %s: %w", dest, copyErr)
		}
	}
	return components, nil
}

func addDir(tw *tar.Writer, prefix, dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Skip provider/module cache; `tofu init` re-creates it on restore.
		// Excluding it keeps the blob small (provider binaries can exceed 700 MB).
		if info.IsDir() && info.Name() == ".terraform" {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:    prefix + "/" + filepath.ToSlash(rel),
			Size:    info.Size(),
			Mode:    int64(info.Mode()),
			ModTime: info.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}
