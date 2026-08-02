// Package deployassets provides the Compose assets compiled into secondbox-deploy.
package deployassets

import (
	"embed"
	"fmt"
	"path/filepath"
	"strings"
)

//go:embed compose*.yml
var composeFiles embed.FS

// ReadComposeFile reads one canonical deploy/compose*.yml asset.
func ReadComposeFile(path string) ([]byte, error) {
	const prefix = "deploy/"
	name := strings.TrimPrefix(path, prefix)
	if name == path || name != filepath.Base(name) {
		return nil, fmt.Errorf("SecondBox deployment asset path %q is invalid", path)
	}
	content, err := composeFiles.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("SecondBox deployment asset %q: %w", path, err)
	}
	return content, nil
}
