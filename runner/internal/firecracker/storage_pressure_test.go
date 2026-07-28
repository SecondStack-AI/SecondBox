package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/protobuf/proto"
)

type mutableStoragePressureProbe struct {
	sample storagePressureSample
	err    error
}

func (p *mutableStoragePressureProbe) Backend() string {
	return p.sample.Backend
}

func (p *mutableStoragePressureProbe) Sample(context.Context) (storagePressureSample, error) {
	return p.sample, p.err
}

func TestStoragePressureControllerWarnsDeniesAndRecoversWithHysteresis(t *testing.T) {
	probe := &mutableStoragePressureProbe{sample: storagePressureSample{
		Backend: "ext4", TotalBytes: 1000, UsedBytes: 810,
	}}
	var terminalKinds []string
	controller, err := newStoragePressureController(
		storagePressurePolicy{RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90},
		probe,
		func(_ context.Context, terminalKind string) error {
			terminalKinds = append(terminalKinds, terminalKind)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	state, err := controller.Observe(t.Context())
	if err != nil || state != storagePressureStateWarning {
		t.Fatalf("warning observation = state %q, error %v", state, err)
	}
	if err := controller.Reserve(t.Context(), "assignment-1", 100); !errors.Is(err, ErrStoragePressureAdmissionDenied) {
		t.Fatalf("reserve above deny threshold = %v", err)
	}
	probe.sample.UsedBytes = 750
	state, err = controller.Observe(t.Context())
	if err != nil || state != storagePressureStateAdmissionDenied {
		t.Fatalf("hysteresis observation = state %q, error %v", state, err)
	}
	probe.sample.UsedBytes = 690
	state, err = controller.Observe(t.Context())
	if err != nil || state != storagePressureStateHealthy {
		t.Fatalf("recovery observation = state %q, error %v", state, err)
	}
	if err := controller.Reserve(t.Context(), "assignment-1", 100); err != nil {
		t.Fatalf("reserve after recovery: %v", err)
	}
	if got, want := terminalKinds, []string{
		"storage_pressure_warning",
		"storage_pressure_admission_denied",
		"storage_pressure_recovered",
	}; !equalStrings(got, want) {
		t.Fatalf("pressure evidence = %v, want %v", got, want)
	}
}

func TestStoragePressureReservationsAreAtomicAndReleasedForRecovery(t *testing.T) {
	probe := &mutableStoragePressureProbe{sample: storagePressureSample{
		Backend: "dm-thin", TotalBytes: 1000, UsedBytes: 600,
	}}
	var terminalKinds []string
	controller, err := newStoragePressureController(
		storagePressurePolicy{RecoveryPercent: 60, WarningPercent: 75, AdmissionDenyPercent: 90},
		probe,
		func(_ context.Context, terminalKind string) error {
			terminalKinds = append(terminalKinds, terminalKind)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Reserve(t.Context(), "assignment-1", 200); err != nil {
		t.Fatal(err)
	}
	if err := controller.Reserve(t.Context(), "assignment-2", 100); !errors.Is(err, ErrStoragePressureAdmissionDenied) {
		t.Fatalf("combined reservation was not denied: %v", err)
	}
	probe.sample.UsedBytes = 590
	if err := controller.Release(t.Context(), "assignment-1"); err != nil {
		t.Fatal(err)
	}
	if got := terminalKinds[len(terminalKinds)-1]; got != "storage_pressure_recovered" {
		t.Fatalf("release evidence terminal kind = %q", got)
	}
}

func TestStoragePressureProbeFailureIsTypedAndFailsClosed(t *testing.T) {
	probe := &mutableStoragePressureProbe{err: errors.New("simulated probe failure")}
	var terminalKinds []string
	controller, err := newStoragePressureController(
		storagePressurePolicy{RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90},
		probe,
		func(_ context.Context, terminalKind string) error {
			terminalKinds = append(terminalKinds, terminalKind)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.CheckAdmission(t.Context(), 1); !errors.Is(err, ErrStoragePressureProbe) {
		t.Fatalf("probe error = %v", err)
	}
	if err := controller.Reserve(t.Context(), "assignment-1", 1); !errors.Is(err, ErrStoragePressureProbe) {
		t.Fatalf("reserve probe error = %v", err)
	}
	if controller.ReservedBytes() != 0 {
		t.Fatal("probe failure retained a partial reservation")
	}
	if got, want := terminalKinds, []string{
		"storage_pressure_probe_failed",
		"storage_pressure_probe_failed",
	}; !equalStrings(got, want) {
		t.Fatalf("probe failure evidence = %v, want %v", got, want)
	}
}

func TestAssignmentStoragePressureDeniesBeforeAllocationOrProgress(t *testing.T) {
	fixture := newFirecrackerConformanceFixture(t)
	backend := fixture.Backend.(*AssignmentBackend)
	probe := &mutableStoragePressureProbe{sample: storagePressureSample{
		Backend: "ext4", TotalBytes: 10 << 30, UsedBytes: 9 << 30,
	}}
	sink := &recordingManagerEvidenceSink{}
	backend.manager.evidence = sink
	controller, err := newStoragePressureController(
		storagePressurePolicy{RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90},
		probe,
		func(ctx context.Context, terminalKind string) error {
			record := runnerevidence.NewRecord(
				runnerevidence.EventStoragePressure,
				"observed",
				terminalKind,
				time.Now(),
			)
			return sink.Emit(ctx, record)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	backend.storagePressure = controller
	assignment := proto.Clone(fixture.Assignment).(*runnerprotocol.AssignmentCommand)
	progressCalls := 0
	_, err = backend.StartAssignment(
		t.Context(),
		assignment,
		func(runnerprotocol.AssignmentProgressStage) error {
			progressCalls++
			return nil
		},
	)
	if !errors.Is(err, ErrStoragePressureAdmissionDenied) {
		t.Fatalf("start pressure error = %v", err)
	}
	if progressCalls != 0 {
		t.Fatalf("progress was emitted %d times before denial", progressCalls)
	}
	if len(backend.assignments) != 0 || controller.ReservedBytes() != 0 {
		t.Fatalf(
			"denied assignment left state: assignments=%d reserved=%d",
			len(backend.assignments),
			controller.ReservedBytes(),
		)
	}
	records := sink.snapshot()
	if len(records) != 1 ||
		records[0].Event != runnerevidence.EventStoragePressure ||
		records[0].TerminalKind != "storage_pressure_admission_denied" {
		t.Fatalf("storage pressure evidence = %+v", records)
	}
}

func TestRestoreSpoolPressureDeniesBeforeAllocation(t *testing.T) {
	fixture := newFirecrackerConformanceFixture(t)
	backend := fixture.Backend.(*AssignmentBackend)
	spoolDir := backend.manager.cfg.MicroVMCheckpointRestoreSpoolDir
	backend.restoreSpoolPressure, _ = newStoragePressureController(
		storagePressurePolicy{RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90},
		&mutableStoragePressureProbe{sample: storagePressureSample{
			Backend: "restore-spool", TotalBytes: 100, UsedBytes: 90,
		}},
		func(context.Context, string) error { return nil },
	)
	begin := &runnerprotocol.RestoreBegin{
		Fence: fixture.Assignment.Fence, CheckpointId: "pressure-restore",
		StorageObjectId: "restore/object", Sha256: strings.Repeat("a", 64), SizeBytes: 1,
		Compatibility: map[string]string{
			"architecture": runtime.GOARCH, "backend": "firecracker", "workspaceFormat": "ext4",
		},
		DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
	}
	if err := backend.BeginRestore(t.Context(), begin); !errors.Is(err, ErrStoragePressureAdmissionDenied) {
		t.Fatalf("restore pressure error = %v", err)
	}
	entries, err := os.ReadDir(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 || backend.restoreSpoolPressure.ReservedBytes() != 0 {
		t.Fatalf("denied restore allocated spool state: entries=%d reserved=%d", len(entries), backend.restoreSpoolPressure.ReservedBytes())
	}
}

func TestExpiredRestoreCleanupReleasesReservationAndRecovers(t *testing.T) {
	fixture := newFirecrackerConformanceFixture(t)
	backend := fixture.Backend.(*AssignmentBackend)
	probe := &mutableStoragePressureProbe{sample: storagePressureSample{
		Backend: "restore-spool", TotalBytes: 100, UsedBytes: 75,
	}}
	var evidence []string
	backend.restoreSpoolPressure, _ = newStoragePressureController(
		storagePressurePolicy{RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90},
		probe,
		func(_ context.Context, kind string) error {
			evidence = append(evidence, kind)
			return nil
		},
	)
	begin := &runnerprotocol.RestoreBegin{
		Fence: fixture.Assignment.Fence, CheckpointId: "expired-restore",
		StorageObjectId: "restore/object", Sha256: strings.Repeat("a", 64), SizeBytes: 10,
		Compatibility: map[string]string{
			"architecture": runtime.GOARCH, "backend": "firecracker", "workspaceFormat": "ext4",
		},
		DeadlineUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
	}
	if err := backend.BeginRestore(t.Context(), begin); err != nil {
		t.Fatal(err)
	}
	if err := backend.WriteRestoreChunk(t.Context(), &runnerprotocol.RestoreChunk{
		Fence: begin.Fence, CheckpointId: begin.CheckpointId, StorageObjectId: begin.StorageObjectId,
		Data: []byte("abc"),
	}); err != nil {
		t.Fatal(err)
	}
	backend.restores[begin.CheckpointId].deadlineUnixMs = uint64(time.Now().Add(-time.Second).UnixMilli())
	probe.sample.UsedBytes = 60
	if err := backend.cleanupExpiredRestores(t.Context(), uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if backend.restoreSpoolPressure.ReservedBytes() != 0 || backend.restores[begin.CheckpointId] != nil {
		t.Fatal("expired restore retained state")
	}
	if got := evidence[len(evidence)-1]; got != "storage_pressure_recovered" {
		t.Fatalf("expired cleanup evidence = %v", evidence)
	}
}

func TestPressureCleanupPreservesActiveAndReplacementWorkspacesAndRecovers(t *testing.T) {
	fixture := newFirecrackerConformanceFixture(t)
	backend := fixture.Backend.(*AssignmentBackend)
	probe := &mutableStoragePressureProbe{sample: storagePressureSample{
		Backend: "ext4", TotalBytes: 100, UsedBytes: 90,
	}}
	var evidence []string
	backend.storagePressure, _ = newStoragePressureController(
		storagePressurePolicy{RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 95},
		probe,
		func(_ context.Context, kind string) error {
			evidence = append(evidence, kind)
			return nil
		},
	)
	backend.assignments["active"] = activeRunnerAssignment{
		fence:                 &runnerprotocol.AssignmentFence{AssignmentId: "active", SandboxId: "sandbox"},
		workspaceAttachmentID: "replacement-generation-2",
	}
	backend.releasedWorkspaces["released"] = releasedWorkspaceCleanup{
		sandboxID: "sandbox", attachmentID: "released-generation-1",
	}
	var removed []string
	backend.removeReleasedWorkspace = func(_ context.Context, _, attachmentID string) error {
		removed = append(removed, attachmentID)
		probe.sample.UsedBytes = 60
		return nil
	}
	if err := backend.checkWorkspaceAdmission(t.Context(), 10); err != nil {
		t.Fatalf("admission after bounded cleanup: %v", err)
	}
	if got, want := removed, []string{"released-generation-1"}; !equalStrings(got, want) {
		t.Fatalf("removed workspaces = %v, want %v", got, want)
	}
	if backend.assignments["active"].workspaceAttachmentID != "replacement-generation-2" {
		t.Fatal("active replacement workspace changed during cleanup")
	}
	if len(backend.releasedWorkspaces) != 0 || evidence[len(evidence)-1] != "storage_pressure_recovered" {
		t.Fatalf("cleanup recovery state: candidates=%v evidence=%v", backend.releasedWorkspaces, evidence)
	}
}

func TestPressureCleanupFailureIsExplicitAndRetainsCandidate(t *testing.T) {
	fixture := newFirecrackerConformanceFixture(t)
	backend := fixture.Backend.(*AssignmentBackend)
	sink := &recordingManagerEvidenceSink{}
	backend.manager.evidence = sink
	probe := &mutableStoragePressureProbe{sample: storagePressureSample{
		Backend: "ext4", TotalBytes: 100, UsedBytes: 95,
	}}
	backend.storagePressure, _ = newStoragePressureController(
		storagePressurePolicy{RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90},
		probe,
		func(context.Context, string) error { return nil },
	)
	backend.releasedWorkspaces["released"] = releasedWorkspaceCleanup{
		sandboxID: "sandbox", attachmentID: "released-generation-1",
	}
	backend.removeReleasedWorkspace = func(context.Context, string, string) error {
		return errors.New("simulated cleanup failure")
	}
	err := backend.checkWorkspaceAdmission(t.Context(), 1)
	if err == nil || !strings.Contains(err.Error(), "simulated cleanup failure") {
		t.Fatalf("cleanup failure = %v", err)
	}
	if _, exists := backend.releasedWorkspaces["released"]; !exists {
		t.Fatal("failed cleanup discarded retry candidate")
	}
	records := sink.snapshot()
	if len(records) != 1 || records[0].TerminalKind != "storage_pressure_cleanup_failed" {
		t.Fatalf("cleanup failure evidence = %+v", records)
	}
}

func TestPressureCleanupBatchIsBounded(t *testing.T) {
	fixture := newFirecrackerConformanceFixture(t)
	backend := fixture.Backend.(*AssignmentBackend)
	for index := 0; index < maxStoragePressureCleanupBatch+1; index++ {
		assignmentID := fmt.Sprintf("released-%d", index)
		backend.releasedWorkspaces[assignmentID] = releasedWorkspaceCleanup{
			sandboxID: "sandbox", attachmentID: assignmentID,
		}
	}
	removed := 0
	backend.removeReleasedWorkspace = func(context.Context, string, string) error {
		removed++
		return nil
	}
	if err := backend.cleanupReleasedWorkspaces(t.Context()); err != nil {
		t.Fatal(err)
	}
	if removed != maxStoragePressureCleanupBatch || len(backend.releasedWorkspaces) != 1 {
		t.Fatalf("bounded cleanup removed=%d remaining=%d", removed, len(backend.releasedWorkspaces))
	}
}

func TestExt4StoragePressureRejectsHostRootFilesystem(t *testing.T) {
	probe := &ext4StoragePressureProbe{workspaceDir: "/"}
	_, err := probe.Sample(t.Context())
	if !errors.Is(err, ErrStoragePressureDedicatedStorage) {
		t.Fatalf("workspace on host root filesystem = %v", err)
	}
}

func TestDMThinStoragePressureUsesOnlyConfiguredPool(t *testing.T) {
	var commands []string
	probe := &dmThinStoragePressureProbe{
		poolDevice:        "/dev/mapper/secondbox-pool",
		workspaceDir:      "/dedicated/workspaces",
		validateWorkspace: func(string) error { return nil },
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			commands = append(commands, name+" "+joinCommandArgs(args))
			switch args[0] {
			case "status":
				return []byte("0 20971520 thin-pool 7 25/100 75/1000 - rw no_discard_passdown queue_if_no_space -"), nil
			case "table":
				return []byte("0 20971520 thin-pool 253:0 253:1 128 32768 1 skip_block_zeroing"), nil
			default:
				return nil, errors.New("unexpected command")
			}
		},
	}
	sample, err := probe.Sample(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if sample.Backend != "dm-thin" ||
		sample.TotalBytes != 1000*128*512 ||
		sample.UsedBytes != 75*128*512 ||
		sample.MetadataUsedBasisPoints != 2500 {
		t.Fatalf("dm-thin sample = %+v", sample)
	}
	if got, want := commands, []string{
		"dmsetup status /dev/mapper/secondbox-pool --target thin-pool",
		"dmsetup table /dev/mapper/secondbox-pool --target thin-pool",
	}; !equalStrings(got, want) {
		t.Fatalf("dm-thin commands = %v, want %v", got, want)
	}

	probe.run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, os.ErrPermission
	}
	if _, err := probe.Sample(t.Context()); !errors.Is(err, ErrStoragePressureProbe) {
		t.Fatalf("dm-thin command failure = %v", err)
	}
}

func TestDMThinStoragePressureRejectsHostRootSpoolBeforePoolProbe(t *testing.T) {
	commandCalled := false
	probe := &dmThinStoragePressureProbe{
		poolDevice:   "/dev/mapper/secondbox-pool",
		workspaceDir: "/",
		run: func(context.Context, string, ...string) ([]byte, error) {
			commandCalled = true
			return nil, nil
		},
	}
	if _, err := probe.Sample(t.Context()); !errors.Is(err, ErrStoragePressureDedicatedStorage) {
		t.Fatalf("dm-thin host-root spool error = %v", err)
	}
	if commandCalled {
		t.Fatal("dm-thin pool was probed after detecting a host-root restore spool")
	}
}

func TestRestoreSpoolPressureRejectsRootAndSymlink(t *testing.T) {
	for name, spoolDir := range map[string]string{
		"root":    "/",
		"symlink": filepath.Join(t.TempDir(), "spool-link"),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "symlink" {
				target := t.TempDir()
				if err := os.Symlink(target, spoolDir); err != nil {
					t.Fatal(err)
				}
			}
			probe := &ext4StoragePressureProbe{
				workspaceDir: spoolDir,
				backend:      "restore-spool",
			}
			if _, err := probe.Sample(t.Context()); !errors.Is(err, ErrStoragePressureDedicatedStorage) {
				t.Fatalf("unsafe restore spool error = %v", err)
			}
		})
	}
}

func TestStoragePressurePolicyRequiresOrderedExplicitThresholds(t *testing.T) {
	for _, policy := range []storagePressurePolicy{
		{},
		{RecoveryPercent: 70, WarningPercent: 70, AdmissionDenyPercent: 90},
		{RecoveryPercent: 70, WarningPercent: 90, AdmissionDenyPercent: 90},
		{RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 100},
	} {
		if err := policy.Validate(); err == nil {
			t.Fatalf("accepted invalid policy %+v", policy)
		}
	}
	if err := (storagePressurePolicy{
		RecoveryPercent: 70, WarningPercent: 80, AdmissionDenyPercent: 90,
	}).Validate(); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
}

func TestStoragePressureConfigurationSelectsOneBackendWithoutFallback(t *testing.T) {
	base := &config.Config{
		MicroVMWorkspaceDir:                        "/dedicated/workspaces",
		MicroVMThinPoolDevice:                      "/dev/mapper/secondbox-pool",
		MicroVMStoragePressureRecoveryPercent:      70,
		MicroVMStoragePressureWarningPercent:       80,
		MicroVMStoragePressureAdmissionDenyPercent: 90,
	}
	for _, testCase := range []struct {
		backend string
		want    string
	}{
		{backend: "ext4", want: "ext4"},
		{backend: "dm-thin", want: "dm-thin"},
	} {
		cfg := *base
		cfg.MicroVMWorkspaceBackend = testCase.backend
		controller, err := newConfiguredStoragePressureController(
			&cfg,
			func(context.Context, string) error { return nil },
		)
		if err != nil {
			t.Fatal(err)
		}
		if controller.probe.Backend() != testCase.want {
			t.Fatalf("%s selected %q probe", testCase.backend, controller.probe.Backend())
		}
	}
	base.MicroVMWorkspaceBackend = "unknown"
	if _, err := newConfiguredStoragePressureController(base, func(context.Context, string) error { return nil }); err == nil {
		t.Fatal("unknown storage backend selected a fallback")
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func joinCommandArgs(args []string) string {
	var joined string
	for index, arg := range args {
		if index > 0 {
			joined += " "
		}
		joined += arg
	}
	return joined
}
