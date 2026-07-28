package operations_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type compatibilityEvidence struct {
	PublicAPI struct {
		Contract                        string `json:"contract"`
		QualifiedMajors                 []int  `json:"qualifiedMajors"`
		ReleasedClientSkewQualification string `json:"releasedClientSkewQualification"`
	} `json:"publicAPI"`
	RunnerProtocol protocolEvidence `json:"runnerProtocol"`
	GuestProtocol  protocolEvidence `json:"guestProtocol"`
	Database       struct {
		MigrationDirectory               string `json:"migrationDirectory"`
		CurrentSchemaQualification       string `json:"currentSchemaQualification"`
		UpgradeQualification             string `json:"upgradeQualification"`
		RollingControlPlaneQualification string `json:"rollingControlPlaneQualification"`
	} `json:"database"`
	Profiles struct {
		RevisionImmutability                  string `json:"revisionImmutability"`
		SchemaQualification                   string `json:"schemaQualification"`
		ReachableRevisionUpgradeQualification string `json:"reachableRevisionUpgradeQualification"`
	} `json:"profiles"`
	Checkpoints struct {
		Qualification string `json:"qualification"`
	} `json:"checkpoints"`
	Artifacts struct {
		Qualification string `json:"qualification"`
	} `json:"artifacts"`
}

type protocolEvidence struct {
	Descriptor                      string `json:"descriptor"`
	QualifiedGenerations            []int  `json:"qualifiedGenerations"`
	AdjacentGenerationQualification string `json:"adjacentGenerationQualification"`
	PriorGenerationQualification    string `json:"priorGenerationQualification"`
}

type initialV1CompatibilityBaseline struct {
	SchemaVersion  int    `json:"schemaVersion"`
	Baseline       string `json:"baseline"`
	ReleaseStatus  string `json:"releaseStatus"`
	PublicAPI      compatibilityBaselineArtifact
	RunnerProtocol struct {
		Generation       uint32 `json:"generation"`
		DescriptorPath   string `json:"descriptorPath"`
		DescriptorSHA256 string `json:"descriptorSHA256"`
	} `json:"runnerProtocol"`
	GuestProtocol struct {
		Generation       uint32 `json:"generation"`
		DescriptorPath   string `json:"descriptorPath"`
		DescriptorSHA256 string `json:"descriptorSHA256"`
	} `json:"guestProtocol"`
	Database struct {
		Migrations []struct {
			Version string `json:"version"`
			Path    string `json:"path"`
			SHA256  string `json:"sha256"`
		} `json:"migrations"`
	} `json:"database"`
	ProfileRevision compatibilityBaselineFixture `json:"profileRevision"`
	Checkpoint      compatibilityBaselineFixture `json:"checkpoint"`
}

type compatibilityBaselineArtifact struct {
	Major          int    `json:"major"`
	ContractPath   string `json:"contractPath"`
	ContractSHA256 string `json:"contractSHA256"`
	ClientPath     string `json:"clientPath"`
	ClientSHA256   string `json:"clientSHA256"`
}

type compatibilityBaselineFixture struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func TestCompatibilityManifestReferencesCommittedContracts(t *testing.T) {
	var evidence compatibilityEvidence
	decodeRepositoryJSON(t, "release/current-compatibility.json", &evidence)

	if len(evidence.PublicAPI.QualifiedMajors) != 1 ||
		evidence.PublicAPI.QualifiedMajors[0] != 1 {
		t.Errorf("qualified public API majors = %v, want [1]", evidence.PublicAPI.QualifiedMajors)
	}
	for _, relativePath := range []string{
		evidence.PublicAPI.Contract,
		evidence.RunnerProtocol.Descriptor,
		evidence.GuestProtocol.Descriptor,
		evidence.Database.MigrationDirectory,
	} {
		if relativePath == "" || filepath.IsAbs(relativePath) {
			t.Fatalf("compatibility manifest has blank or absolute repository path %q", relativePath)
		}
		if _, err := os.Stat(filepath.Join(repositoryRoot(t), filepath.FromSlash(relativePath))); err != nil {
			t.Fatalf("compatibility manifest path %s: %v", relativePath, err)
		}
	}

	if len(evidence.RunnerProtocol.QualifiedGenerations) != 1 ||
		evidence.RunnerProtocol.QualifiedGenerations[0] != 1 {
		t.Error("runner evidence must not claim unavailable prior protocol fixtures")
	}
	if len(evidence.GuestProtocol.QualifiedGenerations) != 1 ||
		evidence.GuestProtocol.QualifiedGenerations[0] != 1 {
		t.Error("guest evidence must not claim unavailable prior protocol fixtures")
	}
	migrationPaths, err := filepath.Glob(filepath.Join(
		repositoryRoot(t),
		filepath.FromSlash(evidence.Database.MigrationDirectory),
		"*.sql",
	))
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(migrationPaths) == 0 {
		t.Fatal("compatibility manifest migration directory has no migrations")
	}
}

func TestUnqualifiedUpgradeDimensionsCannotBeMistakenForReleaseEvidence(t *testing.T) {
	var evidence compatibilityEvidence
	decodeRepositoryJSON(t, "release/current-compatibility.json", &evidence)

	if evidence.PublicAPI.ReleasedClientSkewQualification != "not-qualified" {
		t.Errorf("released-client skew = %q, want not-qualified", evidence.PublicAPI.ReleasedClientSkewQualification)
	}
	if evidence.RunnerProtocol.AdjacentGenerationQualification != "not-qualified" {
		t.Errorf("adjacent Runner generation = %q, want not-qualified", evidence.RunnerProtocol.AdjacentGenerationQualification)
	}
	if evidence.GuestProtocol.PriorGenerationQualification != "not-qualified" {
		t.Errorf("prior guest generation = %q, want not-qualified", evidence.GuestProtocol.PriorGenerationQualification)
	}
	if evidence.Database.UpgradeQualification != "not-qualified" ||
		evidence.Database.RollingControlPlaneQualification != "not-qualified" {
		t.Error("database upgrade and rolling control-plane replacement must remain not-qualified")
	}
	if evidence.Profiles.SchemaQualification != "not-versioned" ||
		evidence.Profiles.ReachableRevisionUpgradeQualification != "not-qualified" {
		t.Error("profile schema and reachable-revision upgrades must remain unqualified")
	}
	if evidence.Checkpoints.Qualification != "integrated-current-version-not-qualified" ||
		evidence.Artifacts.Qualification != "integrated-current-version-not-qualified" {
		t.Error("checkpoint and artifact formats must record integration without claiming released-format qualification")
	}
}

func TestInitialV1CompatibilityBaselineFreezesExecutableUpgradeInputs(t *testing.T) {
	var baseline initialV1CompatibilityBaseline
	decodeRepositoryJSON(t, "tests/compatibility/initial-v1-release-candidate.json", &baseline)
	if baseline.SchemaVersion != 1 ||
		baseline.Baseline != "initial-v1-release-candidate" ||
		baseline.ReleaseStatus != "not-released" {
		t.Fatalf("initial v1 compatibility baseline identity = %#v", baseline)
	}
	if baseline.PublicAPI.Major != 1 ||
		baseline.RunnerProtocol.Generation != 1 ||
		baseline.GuestProtocol.Generation != 1 {
		t.Fatal("initial v1 baseline must freeze API, Runner, and guest generation 1")
	}
	assertCompatibilityBaselineArtifact(
		t,
		baseline.PublicAPI.ContractPath,
		baseline.PublicAPI.ContractSHA256,
	)
	assertCompatibilityBaselineArtifact(
		t,
		baseline.PublicAPI.ClientPath,
		baseline.PublicAPI.ClientSHA256,
	)
	assertCompatibilityBaselineArtifact(
		t,
		baseline.RunnerProtocol.DescriptorPath,
		baseline.RunnerProtocol.DescriptorSHA256,
	)
	assertCompatibilityBaselineArtifact(
		t,
		baseline.GuestProtocol.DescriptorPath,
		baseline.GuestProtocol.DescriptorSHA256,
	)
	if len(baseline.Database.Migrations) == 0 {
		t.Fatal("initial v1 baseline has no database migrations")
	}
	previousVersion := ""
	for _, migration := range baseline.Database.Migrations {
		if migration.Version <= previousVersion ||
			!strings.HasPrefix(filepath.Base(migration.Path), migration.Version+".sql") {
			t.Fatalf(
				"initial v1 migration order/path = %q %q after %q",
				migration.Version,
				migration.Path,
				previousVersion,
			)
		}
		assertCompatibilityBaselineArtifact(t, migration.Path, migration.SHA256)
		previousVersion = migration.Version
	}
	var profileSpec contracts.ProfileRevisionSpec
	assertCompatibilityBaselineArtifact(
		t,
		baseline.ProfileRevision.Path,
		baseline.ProfileRevision.SHA256,
	)
	decodeRepositoryJSON(t, baseline.ProfileRevision.Path, &profileSpec)
	if profileSpec.Backend != "firecracker" ||
		profileSpec.Pool == "" ||
		profileSpec.RuntimeBundleDigest == "" ||
		profileSpec.ToolchainBundleDigest == "" {
		t.Fatalf("initial v1 ProfileRevision fixture is incomplete: %#v", profileSpec)
	}
	var checkpointCompatibility map[string]string
	assertCompatibilityBaselineArtifact(
		t,
		baseline.Checkpoint.Path,
		baseline.Checkpoint.SHA256,
	)
	decodeRepositoryJSON(t, baseline.Checkpoint.Path, &checkpointCompatibility)
	for _, key := range []string{
		"architecture",
		"backend",
		"workspaceFormat",
		"guestProtocolGeneration",
		"mandatoryGuestFeatures",
	} {
		if _, exists := checkpointCompatibility[key]; !exists {
			t.Errorf("initial v1 checkpoint fixture is missing %q", key)
		}
	}
}

func assertCompatibilityBaselineArtifact(t *testing.T, relativePath string, expectedSHA256 string) {
	t.Helper()
	if relativePath == "" ||
		filepath.IsAbs(relativePath) ||
		strings.Contains(filepath.ToSlash(relativePath), "../") {
		t.Fatalf("compatibility baseline path %q escapes the repository", relativePath)
	}
	content, err := os.ReadFile(filepath.Join(repositoryRoot(t), filepath.FromSlash(relativePath)))
	if err != nil {
		t.Fatal(err)
	}
	actual := fmt.Sprintf("%x", sha256.Sum256(content))
	if actual != expectedSHA256 {
		t.Fatalf(
			"compatibility baseline artifact %s SHA-256 = %s, want %s",
			relativePath,
			actual,
			expectedSHA256,
		)
	}
}

func decodeRepositoryJSON(t *testing.T, relativePath string, destination any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), filepath.FromSlash(relativePath)))
	if err != nil {
		t.Fatalf("read %s: %v", relativePath, err)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		t.Fatalf("decode %s: %v", relativePath, err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve upgrade compatibility test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
}
