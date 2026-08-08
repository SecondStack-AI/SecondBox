package install

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerSupportBundleIsBoundedCreateOnlyAndSecretRedacted(t *testing.T) {
	plan := validPlan(t)
	secret := filepath.Join(t.TempDir(), "platform-token")
	if err := os.WriteFile(secret, []byte("support-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan.SecretTargets[0].Path = secret
	for index := range plan.Paths {
		if plan.Paths[index].Name == "platform-token" {
			plan.Paths[index].Path = secret
		}
	}
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "support.tar.gz")
	if err := WriteSupportBundle(output, plan, receipt, map[string][]byte{"manifest-inspection.json": []byte(`{"mode":"development"}`)}); err != nil {
		t.Fatal(err)
	}
	archive, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(compressed)
	combined := ""
	for {
		_, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		combined += string(content)
	}
	if strings.Contains(combined, "support-secret-value") || !strings.Contains(combined, "plan-digest") && !strings.Contains(combined, "sha256:") {
		t.Fatalf("support content redaction/identity failed: %q", combined)
	}
	if err := WriteSupportBundle(output, plan, receipt, nil); err == nil {
		t.Fatal("support bundle overwrote an existing archive")
	}
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := WriteSupportBundle(bad, plan, receipt, map[string][]byte{"runner.log.tail": []byte("support-secret-value")}); err == nil {
		t.Fatal("support bundle accepted installed secret material")
	}
}
