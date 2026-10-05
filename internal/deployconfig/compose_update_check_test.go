package deployconfig

import (
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

func TestComposeUpdateMinimumSourceVersionSubsumesCleanInstallBoundary(t *testing.T) {
	comparison, err := releasecontract.CompareVersions(ComposeUpdateMinimumSourceVersion, cleanInstallBoundaryVersion)
	if err != nil || comparison < 0 {
		t.Fatalf("Compose update floor %s precedes the clean-install boundary %s: %v", ComposeUpdateMinimumSourceVersion, cleanInstallBoundaryVersion, err)
	}
}

func TestCheckComposeUpdateSourceClassifiesTransitions(t *testing.T) {
	for _, test := range []struct {
		applied, target string
		want            ComposeUpdateVerdict
		message         string
	}{
		{"0.22.0", "0.23.0", ComposeUpdateSupported, "is supported"},
		{"0.23.0", "0.23.0", ComposeUpdateSupported, "is supported"},
		{ComposeUpdateMinimumSourceVersion, "0.23.0", ComposeUpdateSupported, "is supported"},
		{"0.24.0-rc.1", "0.24.0", ComposeUpdateSupported, "is supported"},
		{"0.13.9", "0.23.0", ComposeUpdateBelowMinimumSource, "predates the v0.14.0 database migration baseline"},
		{"0.14.0-rc.1", "0.23.0", ComposeUpdateBelowMinimumSource, "requires clean recreation"},
		{"0.24.0", "0.23.0", ComposeUpdateDowngrade, "downgrades are unsupported"},
		{"0.23.0", "0.23.0-rc.1", ComposeUpdateDowngrade, "downgrades are unsupported"},
	} {
		verdict, message, err := CheckComposeUpdateSource(test.applied, test.target)
		if err != nil || verdict != test.want || !strings.Contains(message, test.message) {
			t.Errorf("CheckComposeUpdateSource(%s, %s) = %s, %q, %v; want %s containing %q", test.applied, test.target, verdict, message, err, test.want, test.message)
		}
	}
	for _, invalid := range [][2]string{{"0.22.0+build.1", "0.23.0"}, {"v0.22.0", "0.23.0"}, {"0.22", "0.23.0"}, {"0.22.0", "0.23.0+sha"}} {
		if _, _, err := CheckComposeUpdateSource(invalid[0], invalid[1]); err == nil || !strings.Contains(err.Error(), "canonical SemVer") {
			t.Errorf("CheckComposeUpdateSource(%s, %s) error = %v", invalid[0], invalid[1], err)
		}
	}
}
