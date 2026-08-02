package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
		Backend: "local-workspace", TotalBytes: 1000, UsedBytes: 600,
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

func TestExt4StoragePressureRejectsHostRootFilesystem(t *testing.T) {
	probe := &ext4StoragePressureProbe{workspaceDir: "/"}
	_, err := probe.Sample(t.Context())
	if !errors.Is(err, ErrStoragePressureDedicatedStorage) {
		t.Fatalf("workspace on host root filesystem = %v", err)
	}
}

func TestWorkspacePressureRejectsRootAndSymlink(t *testing.T) {
	for name, workspaceRoot := range map[string]string{
		"root":    "/",
		"symlink": filepath.Join(t.TempDir(), "workspace-link"),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "symlink" {
				target := t.TempDir()
				if err := os.Symlink(target, workspaceRoot); err != nil {
					t.Fatal(err)
				}
			}
			probe := &ext4StoragePressureProbe{
				workspaceDir: workspaceRoot,
				backend:      "local-workspace",
			}
			if _, err := probe.Sample(t.Context()); !errors.Is(err, ErrStoragePressureDedicatedStorage) {
				t.Fatalf("unsafe workspace root error = %v", err)
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

func TestStoragePressureConfigurationUsesReflinkWorkspaceRoot(t *testing.T) {
	cfg := &config.Config{
		RunnerWorkspaceRoot:                        "/dedicated/workspaces",
		MicroVMStoragePressureRecoveryPercent:      70,
		MicroVMStoragePressureWarningPercent:       80,
		MicroVMStoragePressureAdmissionDenyPercent: 90,
	}
	controller, err := newConfiguredStoragePressureController(
		cfg,
		func(context.Context, string) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	probe, ok := controller.probe.(*ext4StoragePressureProbe)
	if !ok || probe.workspaceDir != cfg.RunnerWorkspaceRoot {
		t.Fatalf("configured probe = %#v", controller.probe)
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
