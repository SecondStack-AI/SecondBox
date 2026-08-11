package deployconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/install"
)

func TestPublishedUpdateFileMustMatchVerifiedTargetBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secondbox.toml")
	expected := []byte("verified target\n")
	if err := os.WriteFile(path, expected, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := install.InstallPlan{Paths: []install.PlannedPath{{
		Name: "manifest", Path: path, Class: install.PathUserDeployment,
		Kind: install.ResourceFile, Mode: 0o600,
		OwnerUID: int64(os.Getuid()), OwnerGID: int64(os.Getgid()),
	}}}
	if err := validatePublishedUpdateFile(plan, "manifest", expected); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unverified drift\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePublishedUpdateFile(plan, "manifest", expected); err == nil || !strings.Contains(err.Error(), "verified target") {
		t.Fatalf("published drift was adopted: %v", err)
	}
}
