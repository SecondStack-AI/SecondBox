package firecracker

import (
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
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
	"google.golang.org/protobuf/proto"
)

func TestAssignmentBackendComputeConformance(t *testing.T) {
	conformance.Run(t, newFirecrackerConformanceFixture)
}

func TestAssignmentBackendRequiresWorkspaceStore(t *testing.T) {
	manager := &Manager{cfg: &config.Config{}}
	if _, err := NewAssignmentBackend(manager); err == nil ||
		!strings.Contains(err.Error(), "requires a WorkspaceStore") {
		t.Fatalf("assignment backend without WorkspaceStore error = %v", err)
	}
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

func TestReadinessEvidenceHelpers(t *testing.T) {
	if !firecrackerJailerReady(&config.Config{}) ||
		firecrackerJailerReady(&config.Config{MicroVMAllowUnjailed: true}) {
		t.Fatal("Jailer readiness does not reflect unjailed mode")
	}
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

func TestRunnerAllocatableCapacityUsesIndependentOperationLimit(t *testing.T) {
	capacity := runnerAllocatableCapacity(&config.Config{
		MicroVMVCPUs:                         4,
		MicroVMMemoryBudgetMiB:               16384,
		MicroVMWorkspaceSizeMiB:              51200,
		MicroVMMaxConcurrentGlobal:           16,
		MicroVMMaxConcurrentOperationsGlobal: 64,
	})
	if capacity.VcpuMillis != 64000 ||
		capacity.MemoryBytes != 16<<30 ||
		capacity.DiskBytes != 800<<30 ||
		capacity.Instances != 16 ||
		capacity.Operations != 64 {
		t.Fatalf("Runner allocatable capacity = %#v", capacity)
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
			RunnerWorkspaceRoot:                        t.TempDir(),
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
	workspacePath := filepath.Join(t.TempDir(), "workspace.raw")
	if err := os.WriteFile(workspacePath, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceStore := &conformanceWorkspaceStore{
		workspaceID: "workspace-1",
		generation:  7,
		imagePath:   workspacePath,
	}
	manager.startCompartment = func(_ context.Context, sandboxID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		if opts.WorkspaceAttachment == nil ||
			opts.WorkspaceAttachment.WorkspaceID() != "workspace-1" ||
			opts.WorkspaceAttachment.Generation() != opts.SandboxGeneration ||
			opts.WorkspaceAttachment.Image() == nil {
			t.Fatalf("resolved Workspace attachment = %#v", opts.WorkspaceAttachment)
		}
		marker := []byte("SecondBox-attachment")
		switch opts.SandboxGeneration {
		case 7:
			if _, err := opts.WorkspaceAttachment.Image().WriteAt(marker, 0); err != nil {
				t.Fatalf("write Workspace mutation: %v", err)
			}
		case 8:
			got := make([]byte, len(marker))
			if _, err := opts.WorkspaceAttachment.Image().ReadAt(got, 0); err != nil {
				t.Fatalf("read persisted Workspace mutation: %v", err)
			}
			if string(got) != string(marker) {
				t.Fatalf("persisted Workspace mutation = %q", got)
			}
		default:
			t.Fatalf("unexpected conformance generation %d", opts.SandboxGeneration)
		}
		if opts.NetworkPolicy == nil || opts.NetworkPolicy.Mode() != networkpolicy.ModeDenyAll {
			t.Fatalf("assignment network policy was not compiled and forwarded: %#v", opts.NetworkPolicy)
		}
		if opts.RequestID == "" ||
			opts.OperationID == "" ||
			opts.LeaseID == "" ||
			opts.AssignmentID == "" {
			t.Fatalf("assignment evidence correlation was not forwarded: %+v", opts)
		}
		if opts.StartupProgress == nil {
			t.Fatal("assignment startup progress callback was not forwarded")
		}
		if err := opts.StartupProgress(runtimemanager.StartupStageNetworkReady); err != nil {
			return "", err
		}
		if err := opts.StartupProgress(runtimemanager.StartupStageComputeStarted); err != nil {
			return "", err
		}
		id := fmt.Sprintf("fc-conformance-%d", opts.SandboxGeneration)
		manager.mu.Lock()
		manager.addInstanceLocked(&instance{
			id:                  id,
			sandboxID:           sandboxID,
			sandboxGeneration:   opts.SandboxGeneration,
			compartmentID:       compartmentID,
			workspaceAttachment: opts.WorkspaceAttachment,
			requestID:           opts.RequestID,
			operationID:         opts.OperationID,
			leaseID:             opts.LeaseID,
			assignmentID:        opts.AssignmentID,
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
		if err := opts.StartupProgress(runtimemanager.StartupStageGuestNegotiated); err != nil {
			return "", err
		}
		return id, nil
	}
	if err := manager.SetWorkspaceStore(workspaceStore); err != nil {
		t.Fatal(err)
	}
	backend, err := NewAssignmentBackend(manager)
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
		AdvanceWorkspace: func(
			_ context.Context,
			workspaceID string,
			expectedGeneration uint64,
			nextGeneration uint64,
		) error {
			return workspaceStore.Advance(
				workspaceID,
				expectedGeneration,
				nextGeneration,
			)
		},
		Assignment: &runnerprotocol.AssignmentCommand{
			Fence: &runnerprotocol.AssignmentFence{
				AssignmentId:      "assignment-1",
				SandboxId:         "sandbox-1",
				InstanceId:        "instance-1",
				SandboxGeneration: 7,
				FencingToken:      []byte("opaque-fencing-token"),
			},
			ProfileRevisionId: "profile-revision-1",
			WorkspaceId:       "workspace-1",
			Requirements: &runnerprotocol.ProfileRequirements{
				VcpuCount:    1,
				VcpuMillis:   1000,
				ProcessLimit: 128,
				MemoryBytes:  512 << 20,
				DiskBytes:    1024 << 20,
				Architecture: runtime.GOARCH,
				RequiredCapabilities: []string{
					"network-policy",
					"storage",
					"cleanup",
					"local-workspace",
				},
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

type conformanceWorkspaceStore struct {
	workspacestore.WorkspaceStore
	workspaceID string
	generation  uint64
	imagePath   string
}

func (store *conformanceWorkspaceStore) Open(
	_ context.Context,
	workspaceID string,
	generation uint64,
) (workspacestore.ComputeAttachment, error) {
	if store.workspaceID != workspaceID || store.generation != generation {
		return nil, workspacestore.ErrStaleGeneration
	}
	image, err := os.OpenFile(store.imagePath, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &conformanceComputeAttachment{
		workspaceID: workspaceID,
		generation:  generation,
		image:       image,
	}, nil
}

func (store *conformanceWorkspaceStore) Advance(
	workspaceID string,
	expectedGeneration uint64,
	nextGeneration uint64,
) error {
	if store.workspaceID != workspaceID ||
		store.generation != expectedGeneration ||
		nextGeneration != expectedGeneration+1 {
		return workspacestore.ErrStaleGeneration
	}
	store.generation = nextGeneration
	return nil
}

type conformanceComputeAttachment struct {
	workspaceID string
	generation  uint64
	image       *os.File
}

func (*conformanceComputeAttachment) Handle() workspacestore.WorkspaceHandle {
	return workspacestore.WorkspaceHandle{}
}

func (attachment *conformanceComputeAttachment) WorkspaceID() string {
	return attachment.workspaceID
}

func (attachment *conformanceComputeAttachment) Generation() uint64 {
	return attachment.generation
}

func (attachment *conformanceComputeAttachment) Image() *os.File {
	return attachment.image
}

func (attachment *conformanceComputeAttachment) Close() error {
	if attachment.image == nil {
		return nil
	}
	err := attachment.image.Close()
	attachment.image = nil
	return err
}
