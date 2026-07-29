package deployment_test

import (
	"strings"
	"testing"
)

func TestCIRunsPortableSuiteEndToEnd(t *testing.T) {
	workflow := readRepositoryFile(t, ".github/workflows/ci.yml")
	if !strings.Contains(workflow, "run: just test-non-kvm") {
		t.Fatal("CI must run just test-non-kvm as its end-to-end portable smoke gate")
	}
}
