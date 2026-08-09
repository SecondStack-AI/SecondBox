package install

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func validPlan(t *testing.T) InstallPlan {
	t.Helper()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	facts := HostFacts{SchemaVersion: HostFactsSchema, ObservedAt: now, HostIdentity: "machine-id:host-1", OS: "linux", Architecture: "amd64", InvokingUID: 1000, InvokingGID: 1000, KernelVersion: "6.12", CgroupVersion: 2, CPUCount: 8, MemoryBytes: 32 << 30, Devices: []DeviceFact{}, ListeningPorts: []PortFact{}, Routes: []RouteFact{}, AssignedUIDs: []int64{}, CandidateUIDRanges: []UIDRange{{Start: 200000, Count: 64}}, Utilities: map[string]string{"docker": "27"}, Findings: []Finding{{ID: "architecture", Class: FindingPass, Summary: "Linux amd64"}}}
	factsDigest, err := HostFactsDigest(facts)
	if err != nil {
		t.Fatal(err)
	}
	quotas := map[string]int64{"maxSandboxes": 20, "maxActiveInstances": 8, "maxCpuMillis": 32000, "maxMemoryBytes": 24 << 30, "maxArtifactBytes": 100 << 30, "maxSnapshots": 100, "maxArtifacts": 1000, "maxPortSessions": 50, "maxConcurrentOperations": 8}
	images := map[string]string{}
	for _, name := range []string{"control-plane", "runner", "microvm-artifacts", "installer-tools", "postgres", "object-store", "object-store-client"} {
		images[name] = "ghcr.io/secondstack-ai/secondbox/" + name + "@sha256:" + strings.Repeat("b", 64)
	}
	runnerRoot := "/srv/secondbox/secondbox-install_0123456789abcdef"
	runnerStorage := runnerRoot + "/storage"
	workspace := runnerStorage + "/workspaces"
	return InstallPlan{SchemaVersion: PlanSchema, OperationID: "install_0123456789abcdef", CreatedAt: now, HostFacts: facts, HostFactsDigest: factsDigest, Release: ReleasePlan{Version: "0.4.0", ArtifactManifestURL: "https://github.com/SecondStack-AI/SecondBox/releases/download/v0.4.0/secondbox-0.4.0-artifact-manifest.json", ArtifactManifestDigest: "sha256:" + strings.Repeat("a", 64), SigningKeyFingerprint: "SHA256:" + strings.Repeat("A", 64), Images: images, BinaryDigests: map[string]string{"secondbox": strings.Repeat("c", 64), "secondbox-deploy": strings.Repeat("d", 64)}, ExpectedDownloadBytes: 12 << 30}, Storage: StoragePlan{Choice: StorageExistingMount, WorkspacePath: workspace, ExistingDeviceIdentity: "8:2"}, Capacity: CapacityPlan{MaxSandboxes: 8, MaxCPUMillis: 32000, MaxMemoryBytes: 24 << 30, MaxWorkspaceBytes: 200 << 30, ConcurrentStarts: 2, ConcurrentOperations: 8, StoragePressurePercent: 85, SubjectQuotas: quotas}, Compute: ComputePlan{FirecrackerCPUTemplate: SingleHostFirecrackerCPUTemplate}, Network: NetworkPlan{APIAddress: "127.0.0.1:8080", RunnerAddress: "127.0.0.1:9443", DataPlaneAddress: "127.0.0.1:9444", DatabaseAddress: "127.0.0.1:5432", ObjectStoreAddress: "127.0.0.1:9000", ObjectStoreConsoleAddress: "127.0.0.1:9001", GuestBridgeCIDR: "172.30.0.0/24", TAPPrefix: "sbx", CgroupParent: "secondbox", JailerUIDRange: UIDRange{Start: 200000, Count: 64}, DNSUpstream: "1.1.1.1", Gateways: map[string]string{"agent-compartment": "gateway-agent", "durable-coding": "gateway-coding"}}, CLI: CLIPlan{ConfigPath: "/home/operator/.config/secondbox/config.json", TenantRef: "local-tenant", SubjectRef: "local-operator"}, Paths: []PlannedPath{plannedPath("deployment", "/srv/secondbox/deployment", PathUserDeployment, ResourceDirectory, 0o700, 1000, 1000, false, true), plannedPath("platform-token", "/srv/secondbox/deployment/secrets/platform-token", PathUserDeployment, ResourceFile, 0o600, 1000, 1000, false, true), plannedPath("binary-directory-root", "/home/operator/.local", PathUserDeployment, ResourceDirectory, 0o755, 1000, 1000, false, true), plannedPath("binary-directory", "/home/operator/.local/bin", PathUserDeployment, ResourceDirectory, 0o755, 1000, 1000, false, true), plannedPath("cli-config", "/home/operator/.config/secondbox/config.json", PathUserDeployment, ResourceFile, 0o600, 1000, 1000, false, true), plannedPath("runner-root", runnerRoot, PathExistingWorkspace, ResourceDirectory, 0o711, 0, 0, true, true), plannedPath("runner-storage", runnerStorage, PathExistingWorkspace, ResourceDirectory, 0o711, 0, 0, true, true), plannedPath("artifacts-parent", runnerStorage+"/release", PathInstallerHost, ResourceDirectory, 0o700, 1000, 1000, true, true), plannedPath("artifacts", runnerStorage+"/release/artifacts", PathInstallerHost, ResourceDirectory, 0o700, 1000, 1000, false, true), plannedPath("state", runnerStorage+"/state", PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true), plannedPath("jail", runnerStorage+"/jail", PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true), plannedPath("run", runnerStorage+"/state/run", PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true), plannedPath("logs", runnerStorage+"/state/logs", PathInstallerHost, ResourceDirectory, 0o750, runnerContainerUID, runnerContainerGID, true, true), plannedPath("firecracker-logs", runnerStorage+"/state/firecracker-logs", PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true), plannedPath("workspace", workspace, PathExistingWorkspace, ResourceDirectory, 0o750, runnerContainerUID, runnerContainerGID, true, true)}, SecretTargets: []SecretTarget{{Category: "platform-authority", Path: "/srv/secondbox/deployment/secrets/platform-token"}}, GeneratedAuthorityCategories: []string{"platform-authority"}, StandardBundles: []string{"agent-compartment", "durable-coding"}, RetentionSeconds: 86400, PrivilegedActions: []string{"create Runner directories"}}
}

func TestStrictCanonicalPlanAndReceiptIdentity(t *testing.T) {
	plan := validPlan(t)
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	one, err := Canonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Canonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, two) {
		t.Fatal("canonical plan encoding changed")
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		t.Fatalf("digest = %q", digest)
	}
	decoded, err := DecodePlan(one)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.OperationID != plan.OperationID {
		t.Fatal("decoded plan changed")
	}
	unknown := append(one[:len(one)-1], []byte(`,"token":"must-not-be-accepted"}`)...)
	if _, err := DecodePlan(unknown); err == nil {
		t.Fatal("unknown secret field must fail")
	}
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Canonical(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeReceipt(encoded, plan); err != nil {
		t.Fatal(err)
	}
	other := plan
	other.RetentionSeconds++
	if _, err := DecodeReceipt(encoded, other); err == nil {
		t.Fatal("receipt accepted a different plan")
	}
}

func TestPlanRejectsUnsafePathsEnumsAndDigests(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*InstallPlan)
	}{{"root path", func(p *InstallPlan) { p.Storage.WorkspacePath = "/" }}, {"glob path", func(p *InstallPlan) { p.Paths[0].Path = "/srv/*" }}, {"relative path", func(p *InstallPlan) { p.Paths[0].Path = "relative" }}, {"split runner artifacts", func(p *InstallPlan) {
		index := slices.IndexFunc(p.Paths, func(path PlannedPath) bool { return path.Name == "artifacts" })
		p.Paths[index].Path = filepath.Join(p.Paths[0].Path, "artifacts")
	}}, {"invalid storage", func(p *InstallPlan) { p.Storage.Choice = "disk" }}, {"vendor-specific CPU template", func(p *InstallPlan) { p.Compute.FirecrackerCPUTemplate = "T2" }}, {"malformed digest", func(p *InstallPlan) { p.Release.ArtifactManifestDigest = "sha256:no" }}, {"unsupported schema", func(p *InstallPlan) { p.SchemaVersion = "v2" }}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := validPlan(t)
			test.mutate(&plan)
			if err := plan.Validate(); err == nil {
				t.Fatal("invalid plan succeeded")
			}
		})
	}
}

func TestReceiptStagesAreStrictlyMonotonicAndResourcesUnique(t *testing.T) {
	plan := validPlan(t)
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.CompleteStage(StagePlanAccepted, plan.CreatedAt, nil); err == nil {
		t.Fatal("skipped preflight stage")
	}
	if err := receipt.CompleteStage(StagePreflight, plan.CreatedAt, map[string]string{"reportDigest": "sha256:" + strings.Repeat("d", 64)}); err != nil {
		t.Fatal(err)
	}
	if err := receipt.CompleteStage(StagePreflight, plan.CreatedAt, nil); err == nil {
		t.Fatal("duplicate stage")
	}
	resource := CreatedResource{ID: "resource-1", Kind: ResourceDirectory, Path: "/srv/secondbox/state", Class: PathInstallerHost, Stage: StageHostApply, Mode: 0o700}
	if err := receipt.AppendResource(resource); err != nil {
		t.Fatal(err)
	}
	if err := receipt.AppendResource(resource); err == nil {
		t.Fatal("duplicate resource")
	}
}

func TestFailedCompletedReceiptRecoversAfterReadinessPasses(t *testing.T) {
	plan := validPlan(t)
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range StageSequence {
		if err := receipt.CompleteStage(stage, plan.CreatedAt, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := receipt.Fail(StageReadiness, FailureRetryable, plan.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := receipt.RecoverSucceeded(plan.CreatedAt.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != OperationSucceeded || receipt.FailureClass != "" || receipt.FailureStage != "" || len(receipt.CompletedStages) != len(StageSequence) {
		t.Fatalf("recovered receipt = %#v", receipt)
	}
}

func TestPermanentPurgeIntentPreventsInstallationRestore(t *testing.T) {
	plan := validPlan(t)
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range StageSequence {
		if err := receipt.CompleteStage(stage, plan.CreatedAt, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := receipt.MarkUninstalling(plan.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := receipt.MarkUninstalled(plan.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := receipt.MarkPurging(plan.CreatedAt.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := receipt.RestoreSucceeded(plan.CreatedAt.Add(3 * time.Minute)); err == nil {
		t.Fatal("purge-in-progress receipt was restored")
	}
	if receipt.Status != OperationPurging {
		t.Fatalf("purge intent status = %s", receipt.Status)
	}
}

func TestReceiptValidationBindsOperationIdentity(t *testing.T) {
	plan := validPlan(t)
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Validate(digest, plan.HostFacts.HostIdentity, "install_ffffffffffffffff"); err == nil {
		t.Fatal("receipt from a different operation was accepted")
	}
}

func TestAcceptedFilesAreCreateOnlyMode0600AndLockExclusive(t *testing.T) {
	directory := t.TempDir()
	plan := validPlan(t)
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	planPath, receiptPath, err := WriteAccepted(directory, plan, receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{planPath, receiptPath} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
	if _, _, err := WriteAccepted(directory, plan, receipt); err == nil {
		t.Fatal("accepted files were replaced")
	}
	lock, err := AcquireLock(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := AcquireLock(directory); err == nil {
		t.Fatal("second lock succeeded")
	}
}

func TestPlanEncodingContainsTargetsButNoSecretValues(t *testing.T) {
	plan := validPlan(t)
	encoded, err := Canonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(filepath.Join("/srv/secondbox/deployment", "secrets/platform-token"))) {
		t.Fatal("secret target absent")
	}
	for _, forbidden := range []string{"privateKey", "tokenValue", "passwordValue", "credentialValue"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("plan contains secret-bearing field %q", forbidden)
		}
	}
}
