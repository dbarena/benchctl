package engine

import (
	"os"
	"path/filepath"
	"strings"
)

// RenderComposeTemplate reads a docker-compose template file (identified by a
// ".tmpl.yml" extension), substitutes ${SERVICE_NAME} with serviceName, and writes
// the result to a per-service filename in the same directory (so relative
// volume paths continue to resolve correctly for Docker Compose).
//
// The output filename embeds the service name to prevent collisions when
// multiple services share the same template:
//
//	docker-compose.tmpl.yml + "acme-jemalloc" → docker-compose.acme-jemalloc.yml
//
// It returns the path of the rendered file.
func RenderComposeTemplate(tmplPath, serviceName string) (string, error) {
	data, err := os.ReadFile(tmplPath)
	if err != nil {
		return "", err
	}
	rendered := strings.ReplaceAll(string(data), "${SERVICE_NAME}", serviceName)
	outPath := filepath.Join(filepath.Dir(tmplPath), composeRenderedBase(filepath.Base(tmplPath), serviceName))
	return outPath, os.WriteFile(outPath, []byte(rendered), 0o644)
}

// ResolveComposePath returns the docker-compose file path to use for a service.
// If definition ends in ".tmpl.yml", it is rendered via RenderComposeTemplate
// and the rendered path is returned. Otherwise definition is returned unchanged.
func ResolveComposePath(definition, serviceName string) (string, error) {
	if !strings.HasSuffix(definition, ".tmpl.yml") {
		return definition, nil
	}
	return RenderComposeTemplate(definition, serviceName)
}

// composeRenderedBase converts a template basename and service name into the
// rendered output basename, e.g. "docker-compose.tmpl.yml" + "acme-jemalloc"
// → "docker-compose.acme-jemalloc.yml".
func composeRenderedBase(tmplBase, serviceName string) string {
	stem := strings.TrimSuffix(tmplBase, ".tmpl.yml") // "docker-compose"
	return stem + "." + serviceName + ".yml"
}
