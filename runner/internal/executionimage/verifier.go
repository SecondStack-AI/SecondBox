package executionimage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// NewVerifier builds the Runner side of a backend whose fixed assets are not a
// bundle. It admits only selected images already in the cache, through the
// same memoized bundle verification the Firecracker Runner uses, and never
// retrieves registry content.
func NewVerifier(cacheRoot string) (*Manager, error) {
	if !filepath.IsAbs(cacheRoot) || filepath.Clean(cacheRoot) != cacheRoot {
		return nil, errors.New("SecondBox execution image verifier requires a clean absolute cache root")
	}
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return nil, fmt.Errorf("SecondBox execution image cache creation failed: %w", err)
	}
	return &Manager{
		cacheRoot:     cacheRoot,
		pins:          make(map[string]int),
		verifications: make(map[string]bundleVerification),
	}, nil
}
