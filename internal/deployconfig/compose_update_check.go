package deployconfig

import (
	"fmt"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

// ComposeUpdateMinimumSourceVersion is the oldest applied release that a
// Compose-managed in-place update (compose down, then compose up with the
// target images) accepts. The database migration baseline is the binding
// floor: it is later than the v0.6.0 clean-install boundary and the v0.7.2
// tenant-egress recreation boundary, so it subsumes both.
const ComposeUpdateMinimumSourceVersion = updateMigrationBaselineVersion

type ComposeUpdateVerdict string

const (
	ComposeUpdateSupported          ComposeUpdateVerdict = "supported"
	ComposeUpdateBelowMinimumSource ComposeUpdateVerdict = "below_minimum_source_version"
	ComposeUpdateDowngrade          ComposeUpdateVerdict = "downgrade"
)

// CheckComposeUpdateSource classifies an in-place update from the applied
// release to the target release. Equal versions are supported: a source build
// may move between commits of one release version. Both versions must be
// canonical SemVer without build metadata.
func CheckComposeUpdateSource(appliedVersion, targetVersion string) (ComposeUpdateVerdict, string, error) {
	floor, err := releasecontract.CompareVersions(appliedVersion, ComposeUpdateMinimumSourceVersion)
	if err != nil {
		return "", "", fmt.Errorf("SecondBox Compose update check: applied version: %w", err)
	}
	target, err := releasecontract.CompareVersions(appliedVersion, targetVersion)
	if err != nil {
		return "", "", fmt.Errorf("SecondBox Compose update check: target version: %w", err)
	}
	if floor < 0 {
		return ComposeUpdateBelowMinimumSource, fmt.Sprintf("applied release %s predates the v%s database migration baseline; an in-place update to %s is unsupported and the deployment requires clean recreation: back up, remove the database and every Runner state and Workspace, then initialize %s", appliedVersion, ComposeUpdateMinimumSourceVersion, targetVersion, targetVersion), nil
	}
	if target > 0 {
		return ComposeUpdateDowngrade, fmt.Sprintf("applied release %s is newer than this secondbox-deploy %s; downgrades are unsupported", appliedVersion, targetVersion), nil
	}
	return ComposeUpdateSupported, fmt.Sprintf("in-place update from %s to %s is supported", appliedVersion, targetVersion), nil
}
