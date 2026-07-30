package config

import (
	"strings"
	"testing"
)

const testBundleDigest = "sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func setBuiltInProfileEnvironment(t *testing.T, profile string, pool, runtime, toolchain string) {
	t.Helper()
	prefix := "SECONDBOX_BUILTIN_" + profile + "_"
	t.Setenv(prefix+"POOL", pool)
	t.Setenv(prefix+"RUNTIME_BUNDLE_DIGEST", runtime)
	t.Setenv(prefix+"TOOLCHAIN_BUNDLE_DIGEST", toolchain)
}

func TestRequiredBuiltInProfileBindingReadsEveryValue(t *testing.T) {
	setBuiltInProfileEnvironment(
		t, "AGENT_COMPARTMENT", "runners-a", testBundleDigest, testBundleDigest,
	)
	binding, err := requiredBuiltInProfileBinding("AGENT_COMPARTMENT")
	if err != nil {
		t.Fatal(err)
	}
	if binding.Pool != "runners-a" ||
		binding.RuntimeBundleDigest != testBundleDigest ||
		binding.ToolchainBundleDigest != testBundleDigest {
		t.Fatalf("binding = %+v", binding)
	}
}

// TestRequiredBuiltInProfileBindingHasNoDefault proves the control plane cannot
// start with an unstated built-in Profile binding.
func TestRequiredBuiltInProfileBindingHasNoDefault(t *testing.T) {
	for _, absent := range []string{
		"POOL", "RUNTIME_BUNDLE_DIGEST", "TOOLCHAIN_BUNDLE_DIGEST",
	} {
		t.Run(absent, func(t *testing.T) {
			setBuiltInProfileEnvironment(
				t, "CODING_ENVIRONMENT", "runners-b", testBundleDigest, testBundleDigest,
			)
			t.Setenv("SECONDBOX_BUILTIN_CODING_ENVIRONMENT_"+absent, "")
			_, err := requiredBuiltInProfileBinding("CODING_ENVIRONMENT")
			if err == nil ||
				!strings.Contains(err.Error(), "SECONDBOX_BUILTIN_CODING_ENVIRONMENT_"+absent) {
				t.Fatalf("error = %v; want the absent variable named", err)
			}
		})
	}
}

func TestRequiredDigestRejectsAnythingButASha256Digest(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"no algorithm", strings.Repeat("a", 64)},
		{"wrong algorithm", "sha512:" + strings.Repeat("a", 64)},
		{"too short", "sha256:" + strings.Repeat("a", 63)},
		{"too long", "sha256:" + strings.Repeat("a", 65)},
		{"uppercase hex", "sha256:" + strings.Repeat("A", 64)},
		{"not hex", "sha256:" + strings.Repeat("z", 64)},
		{"placeholder", "REPLACE_WITH_VERIFIED_RUNTIME_BUNDLE_DIGEST"},
		{"trailing content", "sha256:" + strings.Repeat("a", 64) + " "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("SECONDBOX_TEST_DIGEST", test.value)
			_, err := requiredDigest("SECONDBOX_TEST_DIGEST")
			if err == nil || !strings.Contains(err.Error(), "sha256:") {
				t.Fatalf("error = %v; want a digest-format rejection", err)
			}
		})
	}
}

func TestRequiredDigestAcceptsACanonicalDigest(t *testing.T) {
	t.Setenv("SECONDBOX_TEST_DIGEST", testBundleDigest)
	value, err := requiredDigest("SECONDBOX_TEST_DIGEST")
	if err != nil {
		t.Fatal(err)
	}
	if value != testBundleDigest {
		t.Fatalf("digest = %q", value)
	}
}
