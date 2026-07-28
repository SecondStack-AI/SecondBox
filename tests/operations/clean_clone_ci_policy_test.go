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
		"sudo apt-get install -y --no-install-recommends protobuf-compiler ripgrep",
		"scripts/test-clean-clone-isolation.sh --non-kvm",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("SecondBox CI clean-clone gate must contain %q", required)
		}
	}
}

func TestContinuousIntegrationInstallsProtocolCompilerForEveryProtocolConsumer(t *testing.T) {
	workflow := readRepositoryFile(t, ".github/workflows/ci.yml")
	if count := strings.Count(workflow, "sudo apt-get install -y --no-install-recommends protobuf-compiler"); count != 4 {
		t.Errorf("SecondBox CI must install protoc in exactly four protocol-consuming jobs; found %d", count)
	}
	goJobStart := strings.Index(workflow, "  go-tests-vet:")
	goJobEnd := strings.Index(workflow, "  contract-tests:")
	if goJobStart < 0 || goJobEnd <= goJobStart {
		t.Fatal("SecondBox CI does not contain the expected go-tests-vet job")
	}
	goJob := workflow[goJobStart:goJobEnd]
	for _, required := range []string{
		"actions/setup-node@",
		"npm ci --ignore-scripts",
	} {
		if !strings.Contains(goJob, required) {
			t.Errorf("SecondBox Go/release-policy job must contain %q", required)
		}
	}

	releaseWorkflow := readRepositoryFile(t, ".github/workflows/release-evidence.yml")
	for _, required := range []string{
		"sudo apt-get install -y --no-install-recommends protobuf-compiler ripgrep",
		"sudo apt-get install -y --no-install-recommends ripgrep",
	} {
		if !strings.Contains(releaseWorkflow, required) {
			t.Errorf("SecondBox release evidence CI must contain %q", required)
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
