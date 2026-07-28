package operations_test

import (
	"strings"
	"testing"
)

func TestContinuousIntegrationRunsEntireNonKVMMatrixInsideCleanClone(t *testing.T) {
	workflow := readRepositoryFile(t, ".github/workflows/ci.yml")
	for _, required := range []string{
		"clean-clone-non-kvm:",
		"postgres:18.4-bookworm",
		"SECONDBOX_TEST_DATABASE_URL:",
		"cache: false",
		"scripts/test-clean-clone-isolation.sh --non-kvm",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("SecondBox CI clean-clone gate must contain %q", required)
		}
	}
}

func TestNonKVMMatrixIsCompleteAndExcludesQualifiedHostGates(t *testing.T) {
	matrix := readRepositoryFile(t, "scripts/test-non-kvm.sh")
	for _, required := range []string{
		"npm ci --ignore-scripts",
		"scripts/verify-generated.sh",
		"scripts/test-go.sh",
		"scripts/test-contract.sh",
		"scripts/test-compose.sh",
		"scripts/test-image-policy.sh",
		"scripts/build-artifacts.sh",
	} {
		if !strings.Contains(matrix, required) {
			t.Errorf("SecondBox non-KVM matrix must contain %q", required)
		}
	}
	for _, qualifiedHostGate := range []string{
		"scripts/test-firecracker.sh",
		"scripts/test-multirunner.sh",
	} {
		if strings.Contains(matrix, qualifiedHostGate) {
			t.Errorf("SecondBox non-KVM matrix must not invoke %q", qualifiedHostGate)
		}
	}
}

func TestCleanCloneMatrixUsesIsolatedCachesAndExactCommit(t *testing.T) {
	isolation := readRepositoryFile(t, "scripts/test-clean-clone-isolation.sh")
	for _, required := range []string{
		"--no-local",
		`export GOWORK=off`,
		`export GOMODCACHE="$clean_module_cache"`,
		`export GOCACHE="$clean_build_cache"`,
		`export NPM_CONFIG_CACHE="$clean_npm_cache"`,
		`if [[ "$clean_clone_commit" != "$source_commit" ]]`,
		`status --porcelain=v1 --untracked-files=all`,
		`scripts/test-non-kvm.sh`,
	} {
		if !strings.Contains(isolation, required) {
			t.Errorf("SecondBox clean-clone isolation must contain %q", required)
		}
	}
}
