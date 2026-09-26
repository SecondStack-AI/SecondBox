package executionimage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
)

// NewVerifier builds the Runner side of a backend whose fixed assets are not a
// signed bundle. It admits only selected images already in the cache, through
// the same memoized signature verification the Firecracker Runner uses, and
// never retrieves registry content.
func NewVerifier(cacheRoot, publicKeyPath, publicKeySHA256 string) (*Manager, error) {
	if !filepath.IsAbs(cacheRoot) || filepath.Clean(cacheRoot) != cacheRoot {
		return nil, errors.New("SecondBox execution image verifier requires a clean absolute cache root")
	}
	if err := config.VerifyPublicKeyFingerprint(publicKeyPath, publicKeySHA256); err != nil {
		return nil, fmt.Errorf("SecondBox execution image verifier: %w", err)
	}
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return nil, fmt.Errorf("SecondBox execution image cache creation failed: %w", err)
	}
	return &Manager{
		cacheRoot:       cacheRoot,
		publicKeyPath:   publicKeyPath,
		publicKeySHA256: publicKeySHA256,
		pins:            make(map[string]int),
		verifications:   make(map[string]bundleVerification),
	}, nil
}
