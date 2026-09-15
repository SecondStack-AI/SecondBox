package assetcatalog

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

// ExecutionImageAuthority imports component identities from the existing signed bundle.
// Runner materialization reports alone cannot grant execution authority.
type ExecutionImageAuthority struct{ key *rsa.PublicKey }

func LoadExecutionImageAuthority(path, fingerprint string) (*ExecutionImageAuthority, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("SecondBox execution image authority read: %w", err)
	}
	block, _ := pem.Decode(content)
	if block == nil {
		return nil, errors.New("SecondBox execution image authority requires a PEM public key")
	}
	digest := sha256.Sum256(block.Bytes)
	if hex.EncodeToString(digest[:]) != fingerprint {
		return nil, errors.New("SecondBox execution image authority fingerprint mismatch")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("SecondBox execution image authority parse: %w", err)
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("SecondBox execution image authority requires an RSA public key")
	}
	return &ExecutionImageAuthority{key: key}, nil
}

func (authority *ExecutionImageAuthority) ImportManifest(manifest, signature []byte) ([]*runnerv1.AssetReference, error) {
	if authority == nil || authority.key == nil || len(manifest) == 0 || len(manifest) > 1<<20 || len(signature) > 8192 {
		return nil, errors.New("SecondBox execution image import requires bounded signed metadata and configured trust")
	}
	digest := sha256.Sum256(manifest)
	if err := rsa.VerifyPKCS1v15(authority.key, crypto.SHA256, digest[:], signature); err != nil {
		return nil, fmt.Errorf("SecondBox execution image manifest signature: %w", err)
	}
	var document struct {
		Architecture    string                            `json:"architecture"`
		GuestProtocol   struct{ Minimum, Maximum uint32 } `json:"guestProtocol"`
		RuntimeBundle   Asset                             `json:"runtimeBundle"`
		ToolchainBundle Asset                             `json:"toolchainBundle"`
	}
	if err := json.Unmarshal(manifest, &document); err != nil {
		return nil, fmt.Errorf("SecondBox execution image manifest decode: %w", err)
	}
	if (document.Architecture != "amd64" && document.Architecture != "arm64") || document.GuestProtocol.Minimum == 0 || document.GuestProtocol.Maximum < document.GuestProtocol.Minimum {
		return nil, errors.New("SecondBox execution image manifest architecture or guest protocol is invalid")
	}
	assets := make([]*runnerv1.AssetReference, 0, 2)
	for _, component := range []Asset{document.RuntimeBundle, document.ToolchainBundle} {
		if component.ArtifactID == "" || !catalogDigestPattern.MatchString(component.ManifestDigest) {
			return nil, errors.New("SecondBox execution image manifest component identity is invalid")
		}
		assets = append(assets, &runnerv1.AssetReference{ArtifactId: component.ArtifactID, ManifestDigest: component.ManifestDigest, Architecture: document.Architecture, GuestProtocolGeneration: document.GuestProtocol.Maximum, MandatoryGuestFeatures: component.MandatoryGuestFeatures})
	}
	return assets, nil
}
