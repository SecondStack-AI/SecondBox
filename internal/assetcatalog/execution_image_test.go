package assetcatalog

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionImageManifestAuthority(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "publisher.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(der)
	authority, err := LoadExecutionImageAuthority(path, hex.EncodeToString(fingerprint[:]))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{
		"architecture": "amd64", "guestProtocol": map[string]int{"minimum": 1, "maximum": 1},
		"runtimeBundle":   Asset{ArtifactID: "runtime", ManifestDigest: "sha256:" + strings.Repeat("a", 64)},
		"toolchainBundle": Asset{ArtifactID: "toolchain", ManifestDigest: "sha256:" + strings.Repeat("b", 64)},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	assets, err := authority.ImportManifest(manifest, signature)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 2 || assets[0].ArtifactId != "runtime" || assets[1].ArtifactId != "toolchain" || assets[0].Architecture != "amd64" {
		t.Fatalf("unexpected signed assets: %v", assets)
	}
	if _, err := authority.ImportManifest(append(manifest, ' '), signature); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	if _, err := LoadExecutionImageAuthority(path, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong signing fingerprint accepted")
	}
}
