package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnercontrol/conformance"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"google.golang.org/protobuf/proto"
)

func TestAssignmentBackendComputeConformance(t *testing.T) {
	conformance.Run(t, newFirecrackerConformanceFixture)
}

func TestSignedArtifactCompatibilityMetadataFailsClosed(t *testing.T) {
	for name, manifest := range map[string]string{
		"missing architecture": `{"artifactVersion":"v1","guestProtocol":{"minimum":1,"maximum":1}}`,
		"wrong architecture":   `{"artifactVersion":"v1","architecture":"incompatible","guestProtocol":{"minimum":1,"maximum":1}}`,
		"missing protocol":     `{"artifactVersion":"v1","architecture":"` + runtime.GOARCH + `"}`,
		"future protocol":      `{"artifactVersion":"v1","architecture":"` + runtime.GOARCH + `","guestProtocol":{"minimum":2,"maximum":2}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadSignedArtifactManifest(path); err == nil {
				t.Fatalf("accepted incompatible signed metadata: %s", manifest)
			}
		})
	}
}

func TestAssignmentBackendRejectsImmutableAssetSubstitution(t *testing.T) {
	for name, mutate := range map[string]func(*runnerprotocol.AssignmentCommand){
		"missing toolchain": func(assignment *runnerprotocol.AssignmentCommand) {
			assignment.Assets = assignment.Assets[:1]
		},
		"runtime substituted for toolchain": func(assignment *runnerprotocol.AssignmentCommand) {
			assignment.Assets[1] = proto.Clone(assignment.Assets[0]).(*runnerprotocol.SignedAssetReference)
		},
		"wrong signing key": func(assignment *runnerprotocol.AssignmentCommand) {
			assignment.Assets[0].SignatureKeyId = "untrusted-key"
		},
		"wrong architecture": func(assignment *runnerprotocol.AssignmentCommand) {
			assignment.Assets[1].Architecture = "incompatible"
		},
		"unsupported guest generation": func(assignment *runnerprotocol.AssignmentCommand) {
			assignment.Assets[0].GuestProtocolGeneration = 2
		},
		"guessed mandatory feature": func(assignment *runnerprotocol.AssignmentCommand) {
			assignment.Assets[1].MandatoryGuestFeatures = []string{"workspace-session-env"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newFirecrackerConformanceFixture(t)
			assignment := proto.Clone(fixture.Assignment).(*runnerprotocol.AssignmentCommand)
			mutate(assignment)
			if err := fixture.Backend.ValidateAssignment(context.Background(), assignment); err == nil {
				t.Fatalf("accepted immutable asset substitution: %+v", assignment.Assets)
			}
		})
	}
}

func TestAssignmentBackendRestoresVerifiedCheckpointAcrossIndependentRunnerRoots(t *testing.T) {
	sourceRunnerWorkspaceDirectory := t.TempDir()
	sourceRunnerRestoreSpoolDirectory := t.TempDir()
	sourceBackend, err := NewAssignmentBackend(&Manager{cfg: &config.Config{
		MicroVMWorkspaceDir: sourceRunnerWorkspaceDirectory, MicroVMWorkspaceSizeMiB: 1,
		MicroVMCheckpointRestoreSpoolDir:           sourceRunnerRestoreSpoolDirectory,
		MicroVMWorkspaceBackend:                    "ext4",
		MicroVMStoragePressureRecoveryPercent:      70,
		MicroVMStoragePressureWarningPercent:       80,
		MicroVMStoragePressureAdmissionDenyPercent: 90,
	}})
	if err != nil {
		t.Fatal(err)
	}
	restoringRunnerWorkspaceDirectory := t.TempDir()
	restoringRunnerRestoreSpoolDirectory := t.TempDir()
	restoringBackend, err := NewAssignmentBackend(&Manager{cfg: &config.Config{
		MicroVMWorkspaceDir: restoringRunnerWorkspaceDirectory, MicroVMWorkspaceSizeMiB: 1,
		MicroVMCheckpointRestoreSpoolDir:           restoringRunnerRestoreSpoolDirectory,
		MicroVMWorkspaceBackend:                    "ext4",
		MicroVMStoragePressureRecoveryPercent:      70,
		MicroVMStoragePressureWarningPercent:       80,
		MicroVMStoragePressureAdmissionDenyPercent: 90,
	}})
	if err != nil {
		t.Fatal(err)
	}
	restoringBackend.restoreSpoolPressure, err = newStoragePressureController(
		storagePressurePolicy{RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90},
		&mutableStoragePressureProbe{sample: storagePressureSample{
			Backend: "restore-spool", TotalBytes: 100 << 20, UsedBytes: 1 << 20,
		}},
		func(context.Context, string) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	content := make([]byte, 1<<20)
	copy(content, []byte("cross-runner workspace"))
	sourceCheckpointPath := filepath.Join(
		sourceRunnerWorkspaceDirectory, "committed-checkpoint.ext4",
	)
	if err := os.WriteFile(sourceCheckpointPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	checkpointBytes, err := os.ReadFile(sourceCheckpointPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(checkpointBytes)
	fence := &runnerprotocol.AssignmentFence{
		AssignmentId: "assignment-restore", SandboxId: "sandbox-restore",
		InstanceId: "instance-restore", SandboxGeneration: 2,
		FencingToken: []byte("01234567890123456789012345678901"),
	}
	begin := &runnerprotocol.RestoreBegin{
		Fence: fence, CheckpointId: "checkpoint-restore",
		StorageObjectId: "checkpoints/checkpoint-restore.ext4",
		Sha256:          hex.EncodeToString(sum[:]), SizeBytes: uint64(len(checkpointBytes)),
		Compatibility: map[string]string{
			"architecture": runtime.GOARCH, "backend": "firecracker",
			"profileRevisionId": "profile-restore", "workspaceFormat": "ext4",
		},
		DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
	}
	if err := restoringBackend.BeginRestore(t.Context(), begin); err != nil {
		t.Fatal(err)
	}
	first := checkpointBytes[:256*1024]
	if err := restoringBackend.WriteRestoreChunk(t.Context(), &runnerprotocol.RestoreChunk{
		Fence: fence, CheckpointId: begin.CheckpointId, StorageObjectId: begin.StorageObjectId,
		Data: first,
	}); err != nil {
		t.Fatal(err)
	}
	if err := restoringBackend.WriteRestoreChunk(t.Context(), &runnerprotocol.RestoreChunk{
		Fence: fence, CheckpointId: begin.CheckpointId, StorageObjectId: begin.StorageObjectId,
		Data: first,
	}); err != nil {
		t.Fatalf("identical restore replay failed: %v", err)
	}
	if err := restoringBackend.WriteRestoreChunk(t.Context(), &runnerprotocol.RestoreChunk{
		Fence: fence, CheckpointId: begin.CheckpointId, StorageObjectId: begin.StorageObjectId,
		Offset: uint64(len(first) + 1), Data: []byte("gap"),
	}); err == nil {
		t.Fatal("restore offset gap succeeded")
	}
	if err := restoringBackend.BeginRestore(t.Context(), begin); err != nil {
		t.Fatalf("restart failed restore: %v", err)
	}
	if err := restoringBackend.WriteRestoreChunk(t.Context(), &runnerprotocol.RestoreChunk{
		Fence: fence, CheckpointId: begin.CheckpointId, StorageObjectId: begin.StorageObjectId,
		Data: first,
	}); err != nil {
		t.Fatal(err)
	}
	if err := restoringBackend.WriteRestoreChunk(t.Context(), &runnerprotocol.RestoreChunk{
		Fence: fence, CheckpointId: begin.CheckpointId, StorageObjectId: begin.StorageObjectId,
		Offset: uint64(len(first)), Data: checkpointBytes[len(first):],
	}); err != nil {
		t.Fatal(err)
	}
	if err := restoringBackend.WriteRestoreChunk(t.Context(), &runnerprotocol.RestoreChunk{
		Fence: fence, CheckpointId: begin.CheckpointId, StorageObjectId: begin.StorageObjectId,
		Offset: uint64(len(checkpointBytes)), EndOfObject: true,
	}); err != nil {
		t.Fatal(err)
	}
	restore := restoringBackend.restores[begin.CheckpointId]
	if restore == nil || !restore.complete {
		t.Fatalf("verified restore = %#v", restore)
	}
	if len(sourceBackend.restores) != 0 ||
		!strings.HasPrefix(
			restore.path,
			restoringRunnerRestoreSpoolDirectory+string(os.PathSeparator),
		) ||
		strings.HasPrefix(
			restore.path,
			sourceRunnerWorkspaceDirectory+string(os.PathSeparator),
		) {
		t.Fatalf(
			"cross-Runner restore paths share local authority: source=%q restoring=%q",
			sourceCheckpointPath, restore.path,
		)
	}
	rebound := proto.Clone(begin).(*runnerprotocol.RestoreBegin)
	rebound.Fence = proto.Clone(fence).(*runnerprotocol.AssignmentFence)
	rebound.Fence.AssignmentId = "assignment-restore-rebound"
	rebound.Fence.InstanceId = "instance-restore-rebound"
	if err := restoringBackend.BeginRestore(t.Context(), rebound); err != nil {
		t.Fatalf("verified checkpoint rebind failed: %v", err)
	}
	if restoringBackend.restores[begin.CheckpointId].fence.AssignmentId != rebound.Fence.AssignmentId {
		t.Fatal("verified checkpoint did not bind to the new fenced assignment")
	}
	destination := filepath.Join(restoringRunnerWorkspaceDirectory, "generation.ext4")
	if err := os.WriteFile(destination, make([]byte, len(checkpointBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreWorkspaceImage(restore.path, destination, int64(len(checkpointBytes))); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, checkpointBytes) {
		t.Fatal("generation workspace bytes differ from verified checkpoint")
	}
}

func TestReadinessEvidenceHelpers(t *testing.T) {
	if !containsSpaceSeparated("cpu io memory pids", "memory") ||
		containsSpaceSeparated("cpu memory", "mem") {
		t.Fatal("cgroup controller matching is not token-exact")
	}
	addresses := []net.Addr{&net.IPNet{
		IP:   net.ParseIP("198.18.0.1"),
		Mask: net.CIDRMask(24, 32),
	}}
	if !containsNetworkAddress(addresses, "198.18.0.1/24") ||
		containsNetworkAddress(addresses, "198.18.0.2/24") {
		t.Fatal("bridge address matching is not interface-and-prefix exact")
	}
	cache := artifactCacheEvidenceForManifest(signedArtifactManifest{
		RuntimeBundle: signedArtifactComponent{
			ArtifactID: "runtime-v1", ManifestDigest: "sha256:runtime",
		},
		ToolchainBundle: signedArtifactComponent{
			ArtifactID: "toolchain-v1", ManifestDigest: "sha256:toolchain",
		},
	}, time.UnixMilli(1234))
	if len(cache) != 2 ||
		cache[0].ArtifactId != "runtime-v1" ||
		cache[0].ManifestDigest != "sha256:runtime" ||
		cache[1].ArtifactId != "toolchain-v1" ||
		cache[1].ManifestDigest != "sha256:toolchain" ||
		cache[0].VerifiedAtUnixMs != 1234 ||
		cache[1].VerifiedAtUnixMs != 1234 {
		t.Fatalf("component cache evidence = %+v", cache)
	}
}

func newFirecrackerConformanceFixture(t *testing.T) conformance.Fixture {
	t.Helper()
	artifactDir := filepath.Join(t.TempDir(), "artifact-v1")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeComponent := []byte(`{"kind":"runtime","artifactVersion":"artifact-v1"}`)
	toolchainComponent := []byte(`{"kind":"toolchain","artifactVersion":"artifact-v1"}`)
	if err := os.WriteFile(filepath.Join(artifactDir, "runtime-manifest.json"), runtimeComponent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "toolchain-manifest.json"), toolchainComponent, 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeDigest := sha256.Sum256(runtimeComponent)
	toolchainDigest := sha256.Sum256(toolchainComponent)
	runtimeDigestString := "sha256:" + hex.EncodeToString(runtimeDigest[:])
	toolchainDigestString := "sha256:" + hex.EncodeToString(toolchainDigest[:])
	manifest := []byte(fmt.Sprintf(
		`{"artifactVersion":"artifact-v1","architecture":%q,"guestProtocol":{"minimum":1,"maximum":1},"runtimeBundle":{"artifactId":"artifact-v1-runtime","path":"runtime-manifest.json","manifestDigest":%q,"mandatoryGuestFeatures":[]},"toolchainBundle":{"artifactId":"artifact-v1-toolchain","path":"toolchain-manifest.json","manifestDigest":%q,"mandatoryGuestFeatures":[]}}`,
		runtime.GOARCH,
		runtimeDigestString,
		toolchainDigestString,
	))
	if err := os.WriteFile(filepath.Join(artifactDir, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	keyID := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	manager := &Manager{
		cfg: &config.Config{
			MicroVMKernelPath:                          filepath.Join(artifactDir, "kernel"),
			MicroVMPublicKeySHA256:                     keyID,
			MicroVMVCPUs:                               2,
			MicroVMMemoryMiB:                           1024,
			MicroVMWorkspaceSizeMiB:                    2048,
			MicroVMWorkspaceDir:                        t.TempDir(),
			MicroVMCheckpointRestoreSpoolDir:           t.TempDir(),
			MicroVMWorkspaceBackend:                    "ext4",
			MicroVMStoragePressureRecoveryPercent:      70,
			MicroVMStoragePressureWarningPercent:       80,
			MicroVMStoragePressureAdmissionDenyPercent: 90,
			MicroVMBridgeCIDR:                          "198.18.0.1/24",
			MicroVMMaxConcurrentGlobal:                 4,
			NetworkPolicyMaximumDNSPins:                4,
			NetworkPolicyMaximumDNSTTL:                 time.Minute,
		},
		instances:      map[string]*instance{},
		instancesByKey: map[runtimeInstanceKey]string{},
		provisioning:   map[runtimeInstanceKey]chan struct{}{},
		pendingSpawns:  map[runtimeInstanceKey]int{},
		guestIPs:       map[string]string{},
		networkPolicy:  &recordingHostNetworkPolicyEnforcer{},
		runnerID:       "runner-1",
	}
	manager.startCompartment = func(_ context.Context, sandboxID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		if opts.WorkspaceAttachmentID != compartmentID+"-generation-7" {
			t.Fatalf("workspace attachment identity = %q", opts.WorkspaceAttachmentID)
		}
		if opts.NetworkPolicy == nil || opts.NetworkPolicy.Mode() != networkpolicy.ModeDenyAll {
			t.Fatalf("assignment network policy was not compiled and forwarded: %#v", opts.NetworkPolicy)
		}
		if opts.RequestID != "request-1" ||
			opts.OperationID != "operation-1" ||
			opts.LeaseID != "lease-1" ||
			opts.AssignmentID != "assignment-1" {
			t.Fatalf("assignment evidence correlation was not forwarded: %+v", opts)
		}
		id := "fc-conformance"
		manager.mu.Lock()
		manager.addInstanceLocked(&instance{
			id:                id,
			sandboxID:         sandboxID,
			sandboxGeneration: opts.SandboxGeneration,
			compartmentID:     compartmentID,
			requestID:         opts.RequestID,
			operationID:       opts.OperationID,
			leaseID:           opts.LeaseID,
			assignmentID:      opts.AssignmentID,
			guestProtocolSession: &GuestProtocolSession{
				Binding: &guestv1.ConnectionBinding{
					InstanceId:        compartmentID,
					SandboxId:         sandboxID,
					SandboxGeneration: opts.SandboxGeneration,
				},
				Generation:              currentGuestProtocolGeneration,
				GuestBuildID:            opts.GuestBuildID,
				ImageManifestDigest:     opts.ImageManifestDigest,
				ToolchainManifestDigest: opts.ToolchainManifestDigest,
			},
			done: make(chan struct{}),
		})
		manager.mu.Unlock()
		return id, nil
	}
	backend, err := NewAssignmentBackend(manager)
	if err != nil {
		t.Fatal(err)
	}
	backend.restoreSpoolPressure, err = newStoragePressureController(
		storagePressurePolicy{
			RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90,
		},
		&mutableStoragePressureProbe{sample: storagePressureSample{
			Backend: "restore-spool", TotalBytes: 100 << 30, UsedBytes: 1 << 30,
		}},
		func(context.Context, string) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	backend.storagePressure, err = newStoragePressureController(
		storagePressurePolicy{
			RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90,
		},
		&mutableStoragePressureProbe{sample: storagePressureSample{
			Backend: "ext4", TotalBytes: 100 << 30, UsedBytes: 1 << 30,
		}},
		func(context.Context, string) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	return conformance.Fixture{
		Backend: backend,
		Assignment: &runnerprotocol.AssignmentCommand{
			Fence: &runnerprotocol.AssignmentFence{
				AssignmentId:      "assignment-1",
				SandboxId:         "sandbox-1",
				InstanceId:        "instance-1",
				SandboxGeneration: 7,
				FencingToken:      []byte("opaque-fencing-token"),
			},
			ProfileRevisionId: "profile-revision-1",
			Requirements: &runnerprotocol.ProfileRequirements{
				VcpuCount:            1,
				MemoryBytes:          512 << 20,
				DiskBytes:            1024 << 20,
				Architecture:         runtime.GOARCH,
				RequiredCapabilities: []string{"firecracker", "evidence"},
			},
			Assets: []*runnerprotocol.SignedAssetReference{
				{
					ArtifactId:              "artifact-v1-runtime",
					ManifestDigest:          runtimeDigestString,
					SignatureKeyId:          keyID,
					Architecture:            runtime.GOARCH,
					GuestProtocolGeneration: 1,
				},
				{
					ArtifactId:              "artifact-v1-toolchain",
					ManifestDigest:          toolchainDigestString,
					SignatureKeyId:          keyID,
					Architecture:            runtime.GOARCH,
					GuestProtocolGeneration: 1,
				},
			},
			DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
			Correlation: &runnerprotocol.Correlation{
				RequestId: "request-1", OperationId: "operation-1", LeaseId: "lease-1",
			},
			NetworkPolicy: &runnerprotocol.NetworkPolicy{
				Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_DENY_ALL,
			},
		},
	}
}
