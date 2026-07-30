package deployment_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bootstrapDigestEnvironment copies the example inventory, applies the supplied
// mutation, and bootstraps it in a private directory.
func bootstrapDigestEnvironment(
	t *testing.T,
	mutate func(string) string,
) (root string, environmentPath string) {
	t.Helper()
	root = repositoryRootForDeploymentPolicy(t)
	example, err := os.ReadFile(filepath.Join(root, "deploy", "environment.example"))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	environmentPath = filepath.Join(directory, "environment")
	content := string(example)
	if mutate != nil {
		content = mutate(content)
	}
	if err := os.WriteFile(environmentPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		filepath.Join(root, "deploy", "bin", "bootstrap-environment.sh"), environmentPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap environment: %v\n%s", err, output)
	}
	return root, environmentPath
}

func environmentSetting(t *testing.T, environmentPath, name string) string {
	t.Helper()
	content, err := os.ReadFile(environmentPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		if value, found := strings.CutPrefix(line, name+"="); found {
			return value
		}
	}
	t.Fatalf("environment has no %s", name)
	return ""
}

// TestBootstrapBindsBuiltInDigestsToTheGeneratedCatalog proves the built-in
// Profile digests and the development signed-asset catalog cannot drift: one
// value is written to both.
func TestBootstrapBindsBuiltInDigestsToTheGeneratedCatalog(t *testing.T) {
	_, environmentPath := bootstrapDigestEnvironment(t, nil)

	catalogPath := environmentSetting(t, environmentPath, "SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH")
	catalog, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Assets []struct {
			ManifestDigest string `json:"manifestDigest"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(catalog, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Assets) != 1 {
		t.Fatalf("development catalog assets = %d; want 1", len(decoded.Assets))
	}
	want := decoded.Assets[0].ManifestDigest
	if want == "" {
		t.Fatal("development catalog carries no manifest digest")
	}
	for _, setting := range []string{
		"SECONDBOX_BUILTIN_AGENT_COMPARTMENT_RUNTIME_BUNDLE_DIGEST",
		"SECONDBOX_BUILTIN_AGENT_COMPARTMENT_TOOLCHAIN_BUNDLE_DIGEST",
		"SECONDBOX_BUILTIN_CODING_ENVIRONMENT_RUNTIME_BUNDLE_DIGEST",
		"SECONDBOX_BUILTIN_CODING_ENVIRONMENT_TOOLCHAIN_BUNDLE_DIGEST",
	} {
		if got := environmentSetting(t, environmentPath, setting); got != want {
			t.Errorf("%s = %q; want the catalog digest %q", setting, got, want)
		}
	}
}

func TestBootstrappedDevelopmentInventoryValidates(t *testing.T) {
	root, environmentPath := bootstrapDigestEnvironment(t, nil)
	command := exec.Command(
		filepath.Join(root, "deploy", "bin", "validate-environment.sh"), environmentPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("validate bootstrapped development environment: %v\n%s", err, output)
	}
}

// TestOperatorSuppliedCatalogRequiresVerifiedDigests proves the convenience is
// confined to development: a deployment bringing its own signed-asset catalog
// keeps the placeholder and is refused until it names verified bundles.
func TestOperatorSuppliedCatalogRequiresVerifiedDigests(t *testing.T) {
	root, environmentPath := bootstrapDigestEnvironment(t, func(content string) string {
		return strings.Replace(
			content,
			"SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH=GENERATE_DEVELOPMENT_ASSET_CATALOG",
			"SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH=/secure/deployment/signed-assets.json",
			1,
		)
	})
	digest := environmentSetting(
		t, environmentPath, "SECONDBOX_BUILTIN_AGENT_COMPARTMENT_RUNTIME_BUNDLE_DIGEST",
	)
	if digest != "GENERATE_DEVELOPMENT_BUNDLE_DIGEST" {
		t.Errorf("digest = %q; want the placeholder left in place", digest)
	}
	command := exec.Command(
		filepath.Join(root, "deploy", "bin", "validate-environment.sh"), environmentPath,
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("an unverified built-in digest must be refused")
	}
	if !strings.Contains(string(output), "still contains a placeholder for SECONDBOX_BUILTIN_") {
		t.Errorf("validator output = %s; want a placeholder rejection", output)
	}
}
