package workspacestore

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	portableExt4FixtureBytes  = int64(8 << 20)
	portableExt4FixtureUUID   = "9e98fc46-3ca1-5e74-9b6c-bbb0fbf36dd6"
	portableExt4FixtureSHA256 = "93a1b0a91ce6a1db192cd9852daf4557688fe66bff5e4c6473bffee2522703a2"
)

func TestPortableExt4FixtureHasStableCrossPlatformStructure(t *testing.T) {
	transportPath := filepath.Join("testdata", "portable-ext4-v1.img.gz.b64")
	encoded, err := os.ReadFile(transportPath)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(io.LimitReader(reader, portableExt4FixtureBytes+1))
	closeErr := reader.Close()
	if err != nil || closeErr != nil || int64(len(content)) != portableExt4FixtureBytes {
		t.Fatalf("decode portable ext4 fixture: bytes=%d read=%v close=%v", len(content), err, closeErr)
	}
	digest := sha256.Sum256(content)
	if actual := hex.EncodeToString(digest[:]); actual != portableExt4FixtureSHA256 {
		t.Fatalf("portable ext4 fixture SHA-256 = %s", actual)
	}
	path := filepath.Join(t.TempDir(), "portable-ext4-v1.img")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateExt4ImageUUID(path, portableExt4FixtureBytes, portableExt4FixtureUUID); err != nil {
		t.Fatalf("portable ext4 fixture structure: %v", err)
	}
	if os.Getenv("SECONDBOX_REQUIRE_PORTABLE_EXT4") != "1" {
		return
	}
	e2fsck, err := exec.LookPath("e2fsck")
	if err != nil {
		t.Fatalf("portable ext4 qualification requires e2fsck: %v", err)
	}
	output, err := exec.Command(e2fsck, "-fn", path).CombinedOutput()
	if err != nil {
		t.Fatalf("portable ext4 e2fsck failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
}
