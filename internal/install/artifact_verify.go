package install

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

var artifactFileAllowlist = []string{"SHA256SUMS", "kernel", "kernel-provenance.json", "manifest.json", "manifest.sig", "rootfs-debian-license-inventory.json", "rootfs-debian-packages.lock", "rootfs-python-license-inventory.json", "rootfs-python.freeze", "rootfs-source-manifest.json", "rootfs.ext4", "runtime-manifest.json", "secondbox-rootfs-contract.json", "shared.img", "signing.pub", "toolchain-manifest.json"}
var checksummedArtifactFiles = []string{"kernel", "rootfs.ext4", "shared.img", "kernel-provenance.json", "rootfs-source-manifest.json", "secondbox-rootfs-contract.json", "rootfs-debian-packages.lock", "rootfs-python.freeze", "rootfs-debian-license-inventory.json", "rootfs-python-license-inventory.json", "runtime-manifest.json", "toolchain-manifest.json"}

type VerifiedArtifact struct {
	SigningPublicKeyPEM []byte
	SigningKeyID        string
	ManifestDigest      string
}

type artifactEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type rootfsArtifactEntry struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Format  string `json:"format"`
	SizeMiB int64  `json:"sizeMiB"`
}
type sharedArtifactEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Format string `json:"format"`
}
type verifiedContractEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	State  string `json:"state"`
}
type artifactComponent struct {
	ArtifactID             string   `json:"artifactId"`
	Path                   string   `json:"path"`
	ManifestDigest         string   `json:"manifestDigest"`
	MandatoryGuestFeatures []string `json:"mandatoryGuestFeatures"`
}
type artifactProvenance struct {
	DebianPackages artifactEntry `json:"debianPackages"`
	PythonFreeze   artifactEntry `json:"pythonFreeze"`
	DebianLicenses artifactEntry `json:"debianLicenses"`
	PythonLicenses artifactEntry `json:"pythonLicenses"`
}
type signedArtifactManifest struct {
	ArtifactVersion   string                         `json:"artifactVersion"`
	Architecture      string                         `json:"architecture"`
	GuestProtocol     releasecontract.ProtocolWindow `json:"guestProtocol"`
	RuntimeBundle     artifactComponent              `json:"runtimeBundle"`
	ToolchainBundle   artifactComponent              `json:"toolchainBundle"`
	CreatedAt         string                         `json:"createdAt"`
	Kernel            artifactEntry                  `json:"kernel"`
	KernelProvenance  artifactEntry                  `json:"kernelProvenance"`
	RootfsSource      artifactEntry                  `json:"rootfsSource"`
	RootfsContract    verifiedContractEntry          `json:"rootfsContract"`
	RootfsProvenance  artifactProvenance             `json:"rootfsProvenance"`
	Rootfs            rootfsArtifactEntry            `json:"rootfs"`
	Shared            sharedArtifactEntry            `json:"shared"`
	Entrypoint        string                         `json:"entrypoint"`
	RuntimeEntrypoint string                         `json:"runtimeEntrypoint"`
}

func VerifyArtifactDirectory(directory string, release releasecontract.ArtifactManifest) (VerifiedArtifact, error) {
	if err := release.Validate(); err != nil {
		return VerifiedArtifact{}, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return VerifiedArtifact{}, installerError("read extracted artifact directory", err)
	}
	actual := make([]string, 0, len(entries))
	for _, entry := range entries {
		info, err := os.Lstat(filepath.Join(directory, entry.Name()))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
			return VerifiedArtifact{}, installerError("artifact entry "+entry.Name()+" must be a non-writable regular file", err)
		}
		actual = append(actual, entry.Name())
	}
	slices.Sort(actual)
	want := slices.Clone(artifactFileAllowlist)
	slices.Sort(want)
	if !slices.Equal(actual, want) {
		return VerifiedArtifact{}, installerError("extracted artifact directory differs from the fixed file allowlist", nil)
	}
	checksums, err := decodeChecksums(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		return VerifiedArtifact{}, err
	}
	for _, name := range checksummedArtifactFiles {
		actualDigest, err := fileSHA256(filepath.Join(directory, name))
		if err != nil {
			return VerifiedArtifact{}, err
		}
		if checksums[name] != actualDigest {
			return VerifiedArtifact{}, installerError("artifact checksum mismatch for "+name, nil)
		}
	}
	manifestBytes, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return VerifiedArtifact{}, err
	}
	if Digest(manifestBytes) != release.MicroVM.SignedManifestDigest {
		return VerifiedArtifact{}, installerError("signed artifact manifest digest differs from release identity", nil)
	}
	publicPEM, err := os.ReadFile(filepath.Join(directory, "signing.pub"))
	if err != nil {
		return VerifiedArtifact{}, err
	}
	publicKey, keyID, err := verifyArtifactPublicKey(publicPEM, release.MicroVM.SigningKeyFingerprint)
	if err != nil {
		return VerifiedArtifact{}, err
	}
	signature, err := os.ReadFile(filepath.Join(directory, "manifest.sig"))
	if err != nil {
		return VerifiedArtifact{}, err
	}
	manifestHash := sha256.Sum256(manifestBytes)
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, manifestHash[:], signature); err != nil {
		return VerifiedArtifact{}, installerError("verify artifact manifest signature", err)
	}
	var manifest signedArtifactManifest
	if err := decodeStrict(manifestBytes, &manifest); err != nil {
		return VerifiedArtifact{}, installerError("decode signed artifact manifest", err)
	}
	if err := validateSignedArtifactManifest(directory, manifest, release); err != nil {
		return VerifiedArtifact{}, err
	}
	if err := validateRootfsContract(directory, manifest.Rootfs.SHA256); err != nil {
		return VerifiedArtifact{}, err
	}
	return VerifiedArtifact{SigningPublicKeyPEM: slices.Clone(publicPEM), SigningKeyID: keyID, ManifestDigest: release.MicroVM.SignedManifestDigest}, nil
}

func decodeChecksums(path string) (map[string]string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !checksumPattern.MatchString(fields[0]) {
			return nil, installerError("SHA256SUMS is malformed", nil)
		}
		name := strings.TrimPrefix(fields[1], "*")
		if !slices.Contains(checksummedArtifactFiles, name) || result[name] != "" {
			return nil, installerError("SHA256SUMS contains an unexpected or duplicate file", nil)
		}
		result[name] = fields[0]
	}
	if len(result) != len(checksummedArtifactFiles) {
		return nil, installerError("SHA256SUMS is incomplete", nil)
	}
	return result, nil
}

func verifyArtifactPublicKey(content []byte, expected string) (*rsa.PublicKey, string, error) {
	block, remainder := pem.Decode(content)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(remainder)) != 0 {
		return nil, "", installerError("artifact signing key must contain exactly one PKIX PEM public key", nil)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, "", installerError("parse artifact signing key", err)
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok || publicKey.N.BitLen() < 3072 || publicKey.E != 65537 {
		return nil, "", installerError("artifact signing key must be RSA-3072 or stronger with exponent 65537", nil)
	}
	digest := sha256.Sum256(block.Bytes)
	keyID := hex.EncodeToString(digest[:])
	if "SHA256:"+strings.ToUpper(keyID) != expected {
		return nil, "", installerError("artifact signing key fingerprint differs from release identity", nil)
	}
	return publicKey, keyID, nil
}

func validateSignedArtifactManifest(directory string, manifest signedArtifactManifest, release releasecontract.ArtifactManifest) error {
	if manifest.ArtifactVersion == "" || manifest.Architecture != "amd64" || manifest.GuestProtocol != release.GuestProtocol || manifest.CreatedAt == "" || manifest.Entrypoint != "/init" || manifest.RuntimeEntrypoint != "/usr/local/bin/secondbox-runner-guest-entrypoint" {
		return installerError("signed artifact manifest identity or platform contract is invalid", nil)
	}
	components := []struct {
		got  artifactComponent
		want releasecontract.SignedComponent
		path string
	}{{manifest.RuntimeBundle, release.MicroVM.RuntimeBundle, "runtime-manifest.json"}, {manifest.ToolchainBundle, release.MicroVM.ToolchainBundle, "toolchain-manifest.json"}}
	for _, component := range components {
		if component.got.ArtifactID != component.want.ArtifactID || component.got.Path != component.path || component.got.ManifestDigest != component.want.ManifestDigest || !slices.Equal(component.got.MandatoryGuestFeatures, component.want.MandatoryGuestFeatures) {
			return installerError("signed artifact component identity differs from release identity", nil)
		}
		digest, err := fileSHA256(filepath.Join(directory, component.path))
		if err != nil || "sha256:"+digest != component.got.ManifestDigest {
			return installerError("signed artifact component digest mismatch", err)
		}
	}
	entries := []struct {
		entry artifactEntry
		path  string
	}{
		{manifest.Kernel, "kernel"}, {manifest.KernelProvenance, "kernel-provenance.json"}, {manifest.RootfsSource, "rootfs-source-manifest.json"},
		{artifactEntry{Path: manifest.RootfsContract.Path, SHA256: manifest.RootfsContract.SHA256}, "secondbox-rootfs-contract.json"},
		{manifest.RootfsProvenance.DebianPackages, "rootfs-debian-packages.lock"}, {manifest.RootfsProvenance.PythonFreeze, "rootfs-python.freeze"},
		{manifest.RootfsProvenance.DebianLicenses, "rootfs-debian-license-inventory.json"}, {manifest.RootfsProvenance.PythonLicenses, "rootfs-python-license-inventory.json"},
		{artifactEntry{Path: manifest.Rootfs.Path, SHA256: manifest.Rootfs.SHA256}, "rootfs.ext4"}, {artifactEntry{Path: manifest.Shared.Path, SHA256: manifest.Shared.SHA256}, "shared.img"},
	}
	for _, item := range entries {
		if item.entry.Path != item.path || !checksumPattern.MatchString(item.entry.SHA256) {
			return installerError("signed artifact path or checksum is invalid for "+item.path, nil)
		}
		digest, err := fileSHA256(filepath.Join(directory, item.path))
		if err != nil || digest != item.entry.SHA256 {
			return installerError("signed artifact hash mismatch for "+item.path, err)
		}
	}
	if manifest.RootfsContract.State != "verified" || manifest.Rootfs.Format != "ext4" || manifest.Rootfs.SizeMiB <= 0 || manifest.Shared.Format == "" {
		return installerError("signed rootfs/shared artifact contract is invalid", nil)
	}
	return nil
}

func validateRootfsContract(directory, rootfsDigest string) error {
	content, err := os.ReadFile(filepath.Join(directory, "secondbox-rootfs-contract.json"))
	if err != nil {
		return err
	}
	var contract struct {
		SchemaVersion              int    `json:"schemaVersion"`
		Contract                   string `json:"contract"`
		State                      string `json:"state"`
		SurfaceContract            string `json:"surfaceContract"`
		BrowserPolicy              string `json:"browserPolicy"`
		RootfsSHA256               string `json:"rootfsSha256"`
		PolicySHA256               string `json:"policySha256"`
		SecretScanPolicySHA256     string `json:"secretScanPolicySha256"`
		BrowserSurfacePolicySHA256 string `json:"browserSurfacePolicySha256"`
	}
	if err := decodeStrict(content, &contract); err != nil {
		return installerError("decode rootfs contract", err)
	}
	if contract.SchemaVersion != 1 || contract.Contract != "secondbox-guest-rootfs" || contract.State != "verified" || contract.SurfaceContract == "" || (contract.BrowserPolicy != "allow" && contract.BrowserPolicy != "forbid") || contract.RootfsSHA256 != rootfsDigest {
		return installerError("rootfs contract identity or rootfs digest is invalid", nil)
	}
	for _, digest := range []string{contract.PolicySHA256, contract.SecretScanPolicySHA256, contract.BrowserSurfacePolicySHA256} {
		if !checksumPattern.MatchString(digest) {
			return installerError("rootfs contract policy digest is invalid", nil)
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
