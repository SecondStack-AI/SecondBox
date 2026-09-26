package executionimage

import (
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/executionimage/executionimagetest"
)

// writeSignedBundleFixture publishes a bundle that passes the real signature
// verification, so a test can exercise cache admission instead of stubbing it.
func writeSignedBundleFixture(t *testing.T, directory string) (publicKeyPath string, publicKeySHA256 string) {
	t.Helper()
	publisher := executionimagetest.NewPublisher(t)
	publisher.WriteBundle(t, directory, nil)
	return publisher.PublicKeyPath, publisher.PublicKeySHA256
}
