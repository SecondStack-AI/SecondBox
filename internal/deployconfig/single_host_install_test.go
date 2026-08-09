package deployconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

func TestInitSingleHostFromReleaseMaterializesEveryAcceptedRunnerValue(t *testing.T) {
	release, err := developmentReleaseManifest()
	if err != nil {
		t.Fatal(err)
	}
	releaseBytes, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	deployment := filepath.Join(base, "deployment")
	workspaceMount := filepath.Join(base, "dedicated-workspace")
	facts := install.HostFacts{SchemaVersion: install.HostFactsSchema, ObservedAt: time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC), HostIdentity: "machine-id:fixture", OS: "linux", Architecture: "amd64", InvokingUID: int64(os.Getuid()), InvokingGID: int64(os.Getgid()), KernelVersion: "6.12", CgroupVersion: 2, CPUCount: 8, MemoryBytes: 32 << 30, BtrfsSupported: true, KVMAccessible: true, TUNAccessible: true, Devices: []install.DeviceFact{{Path: "/dev/fixture", Identity: "8:99", Filesystem: "xfs", SizeBytes: 200 << 30, AvailableBytes: 100 << 30, Mountpoint: workspaceMount}}, Routes: []install.RouteFact{}, DNSUpstreams: []string{"192.0.2.53"}, CandidateUIDRanges: []install.UIDRange{{Start: 200000, Count: 64}}, Utilities: map[string]string{"docker": "fixture"}, Findings: []install.Finding{{ID: "platform", Class: install.FindingPass, Summary: "Linux amd64"}}}
	releasePlan := install.ReleasePlan{Version: release.Version, ArtifactManifestURL: releasecontract.ArtifactManifestLocation(release.Version), ArtifactManifestDigest: releasecontract.Digest(releaseBytes), SigningKeyFingerprint: release.MicroVM.SigningKeyFingerprint, Images: map[string]string{"control-plane": release.ControlPlane.Reference, "runner": release.Runner.Reference, "microvm-artifacts": release.MicroVM.ImageReference, "installer-tools": release.InstallerTools.Reference, "postgres": release.BundledServices.Postgres, "object-store": release.BundledServices.ObjectStore, "object-store-client": release.BundledServices.ObjectStoreClient}, BinaryDigests: map[string]string{}, ExpectedDownloadBytes: install.ExecutionBundleEstimateBytes}
	for _, binary := range release.Binaries {
		if binary.Platform == "linux/amd64" {
			releasePlan.BinaryDigests[binary.Name] = binary.SHA256
		}
	}
	plan, err := install.ProposePlan(facts, install.ProposalInput{OperationID: "install_0123456789abcdef", CreatedAt: facts.ObservedAt, DeploymentDirectory: deployment, BinaryDirectory: filepath.Join(base, "bin"), CLIConfigPath: filepath.Join(base, "config", "secondbox", "config.json"), CLITenantRef: "tenant-reviewed", CLISubjectRef: "subject-reviewed", BackingAvailableBytes: 100 << 30, DeploymentAvailableBytes: 100 << 30, Release: releasePlan, StorageChoice: install.StorageExistingMount, ExistingMountpoint: workspaceMount, StandardBundles: []string{"agent-compartment", "durable-coding"}, RetentionSeconds: 7200})
	if err != nil {
		t.Fatal(err)
	}
	plan.Network.DNSUpstream = "2001:db8::53"
	if err := os.Mkdir(deployment, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(deployment, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	keyID := strings.ToLower(strings.TrimPrefix(release.MicroVM.SigningKeyFingerprint, "SHA256:"))
	verifiedArtifact := install.VerifiedArtifact{SigningKeyID: keyID, ManifestDigest: release.MicroVM.SignedManifestDigest, SigningPublicKeyPEM: []byte("unused after verified extraction")}
	planDigest, err := install.PlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(singleHostMaterializationMarker(plan), []byte(planDigest+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(installPath(plan, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installPath(plan, "secrets"), "partial"), []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := InitSingleHostFromReleaseOrValidate(plan, release, releaseBytes, verifiedArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if resumed, err := InitSingleHostFromReleaseOrValidate(plan, release, releaseBytes, verifiedArtifact); err != nil || resumed != result {
		t.Fatalf("completed materialization was not safely adopted: %#v, %v", resumed, err)
	}
	originalManifestBytes, err := os.ReadFile(result.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ReadManifest(result.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Deployment.Mode != "development" || manifest.Deployment.ControlPlaneImage != release.ControlPlane.Reference || manifest.Deployment.RunnerImage != release.Runner.Reference || len(manifest.Runners) != 1 {
		t.Fatalf("release-backed deployment = %#v", manifest.Deployment)
	}
	runner := manifest.Runners[0]
	if runner.RunnerID != result.RunnerID || runner.IdentityHostDirectory != result.RunnerIdentityDirectory || runner.WorkspaceHostDirectory != plan.Storage.WorkspacePath || runner.ArtifactHostDirectory != installPath(plan, "artifacts") || runner.StateHostDirectory != installPath(plan, "state") || runner.FirecrackerJailerUIDStart == nil || *runner.FirecrackerJailerUIDStart != plan.Network.JailerUIDRange.Start || runner.MaxConcurrentOperationsGlobal == nil || *runner.MaxConcurrentOperationsGlobal != plan.Capacity.ConcurrentOperations || *runner.MaxConcurrentOperationsGlobal < install.DurableCodingConcurrentOperations || runner.NetworkPolicyDNSUpstream != "[2001:db8::53]:53" || runner.DataPlaneAdvertisedAddress != plan.Network.DataPlaneAddress || runner.ArtifactPublicKeySHA256 != keyID {
		t.Fatalf("Runner does not match accepted plan: %#v", runner)
	}
	storedRelease, err := os.ReadFile(filepath.Join(deployment, "release-artifact-manifest.json"))
	if err != nil || string(storedRelease) != string(releaseBytes) {
		t.Fatalf("stored release bytes changed: %v", err)
	}
	for _, target := range plan.SecretTargets {
		info, err := os.Stat(target.Path)
		if err != nil {
			t.Fatalf("secret target %s is absent: %v", target.Category, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("secret target %s is exposed: %#o", target.Category, info.Mode().Perm())
		}
	}
	if err := os.WriteFile(result.ManifestPath, append([]byte{}, []byte("changed\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitSingleHostFromReleaseOrValidate(plan, release, releaseBytes, verifiedArtifact); err == nil {
		t.Fatal("changed existing materialization was adopted")
	}
	if err := os.Remove(result.ManifestPath); err != nil {
		t.Fatal(err)
	}
	redirectedManifest := filepath.Join(base, "redirected-manifest.toml")
	if err := os.WriteFile(redirectedManifest, originalManifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(redirectedManifest, result.ManifestPath); err != nil {
		t.Fatal(err)
	}
	if _, err := InitSingleHostFromReleaseOrValidate(plan, release, releaseBytes, verifiedArtifact); err == nil {
		t.Fatal("symlinked existing materialization was adopted")
	}
}

func TestTrackedRunnerPKIExposesPartialFilesToRollback(t *testing.T) {
	directory := t.TempDir()
	created := []string{}
	err := generateTrackedRunnerPKI(directory, &created, func(path, _ string, _ int64) error {
		if err := os.WriteFile(filepath.Join(path, "runner-ca.key"), []byte("partial"), 0o600); err != nil {
			return err
		}
		return errors.New("injected PKI failure")
	})
	if err == nil || len(created) != 4 || !slices.Contains(created, filepath.Join(directory, "runner-ca.key")) {
		t.Fatalf("tracked PKI paths = %#v, error = %v", created, err)
	}
	for index := len(created) - 1; index >= 0; index-- {
		_ = os.Remove(created[index])
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("partial PKI cleanup = %#v, %v", entries, err)
	}
}
