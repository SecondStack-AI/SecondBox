package install

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeHostApplyExecutor struct {
	euid       int
	failAt     string
	calls      []string
	created    []PlannedPath
	unit       string
	removed    []string
	nonempty   map[ResourceKind]bool
	retained   map[string]bool
	revalidate error
	teardown   error
	reflink    []string
}

func (executor *fakeHostApplyExecutor) RevalidateTeardown(context.Context, InstallPlan, InstallReceipt) error {
	executor.calls = append(executor.calls, "revalidate-teardown")
	return executor.teardown
}

func (executor *fakeHostApplyExecutor) EffectiveUID() int { return executor.euid }
func (executor *fakeHostApplyExecutor) Revalidate(context.Context, InstallPlan, InstallReceipt) error {
	executor.calls = append(executor.calls, "revalidate")
	return executor.revalidate
}
func (executor *fakeHostApplyExecutor) CreateDirectory(path PlannedPath) error {
	executor.calls = append(executor.calls, "mkdir:"+path.Name)
	if executor.failAt == "mkdir:"+path.Name {
		return errors.New("injected mkdir failure")
	}
	executor.created = append(executor.created, path)
	return nil
}
func (executor *fakeHostApplyExecutor) AllocateFilesystemImage(path PlannedPath, _ int64) error {
	executor.calls = append(executor.calls, "allocate:"+path.Name)
	if executor.failAt == "allocate" {
		return errors.New("injected allocation failure")
	}
	executor.created = append(executor.created, path)
	return nil
}
func (executor *fakeHostApplyExecutor) FormatBtrfs(_ context.Context, _, reference string) error {
	executor.calls = append(executor.calls, "format:"+reference)
	if executor.failAt == "format" {
		return errors.New("injected format failure")
	}
	return nil
}
func (executor *fakeHostApplyExecutor) WriteMountUnit(path PlannedPath, content string) error {
	executor.calls = append(executor.calls, "unit:"+path.Name)
	if executor.failAt == "unit" {
		return errors.New("injected unit failure")
	}
	executor.created = append(executor.created, path)
	executor.unit = content
	return nil
}
func (executor *fakeHostApplyExecutor) EnableMountUnit(context.Context, string) error {
	executor.calls = append(executor.calls, "enable")
	if executor.failAt == "enable" {
		return errors.New("injected enable failure")
	}
	return nil
}
func (executor *fakeHostApplyExecutor) SecureDirectory(path PlannedPath) error {
	executor.calls = append(executor.calls, "secure:"+path.Name)
	if executor.failAt == "secure" {
		return errors.New("injected mounted-workspace security failure")
	}
	return nil
}
func (executor *fakeHostApplyExecutor) ProveReflinkTopology(artifactParent, runDirectory, workspace string) (string, error) {
	executor.calls = append(executor.calls, "reflink")
	executor.reflink = []string{artifactParent, runDirectory, workspace}
	if executor.failAt == "reflink" {
		return "", errors.New("injected reflink failure")
	}
	return "btrfs-uuid:01234567-89ab-cdef-0123-456789abcdef", nil
}
func (executor *fakeHostApplyExecutor) RemoveEmpty(resource CreatedResource) (bool, error) {
	executor.calls = append(executor.calls, "remove:"+resource.ID)
	if executor.nonempty[resource.Kind] || executor.retained[resource.ID] {
		return false, nil
	}
	executor.removed = append(executor.removed, resource.ID)
	return true, nil
}

func imageApplyPlan(t *testing.T) InstallPlan {
	t.Helper()
	input := plannerInput(t, StorageBtrfsImage)
	plan, err := ProposePlan(plannerFacts(t), input)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPendingAbsentFilesystemImageStillRequiresAllocationCapacity(t *testing.T) {
	plan := imageApplyPlan(t)
	receipt := acceptedReceipt(t, plan)
	if err := receipt.BeginResource("filesystem-image", plan.CreatedAt); err != nil {
		t.Fatal(err)
	}
	withoutImage := filesystemImageBackingRequired(plan, receipt, false)
	withImage := filesystemImageBackingRequired(plan, receipt, true)
	if withoutImage != withImage+plan.Storage.ImageSizeBytes {
		t.Fatalf("pending absent image requires %d bytes, present image requires %d bytes", withoutImage, withImage)
	}
}

func acceptedReceipt(t *testing.T, plan InstallPlan) InstallReceipt {
	t.Helper()
	receipt, err := NewReceipt(plan, plan.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.CompleteStage(StagePreflight, plan.CreatedAt, nil); err != nil {
		t.Fatal(err)
	}
	if err := receipt.CompleteStage(StagePlanAccepted, plan.CreatedAt, nil); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestHostApplyUsesOnlyAcceptedResourcesAndRecordsEachMutation(t *testing.T) {
	plan := imageApplyPlan(t)
	receipt := acceptedReceipt(t, plan)
	executor := &fakeHostApplyExecutor{euid: 0, nonempty: map[ResourceKind]bool{}}
	persisted := []InstallReceipt{}
	updated, err := ApplyHost(context.Background(), plan, receipt, HostApplyDependencies{Executor: executor, Now: func() time.Time { return plan.CreatedAt.Add(time.Minute) }, PersistReceipt: func(value InstallReceipt) error {
		persisted = append(persisted, value)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.CompletedStages[2].Stage != StageHostApply || updated.CompletedStages[2].Evidence["reflinkMutationIsolation"] != "passed" {
		t.Fatalf("host apply stage = %#v", updated.CompletedStages)
	}
	if len(persisted) != 2*len(updated.CreatedResources)+1 {
		t.Fatalf("receipt persistence count = %d, resources = %d", len(persisted), len(updated.CreatedResources))
	}
	for _, created := range updated.CreatedResources {
		planned, found := plannedPathByName(plan.Paths, created.ID)
		if !found || created.Path != planned.Path || created.Mode != planned.Mode || created.OwnerUID != planned.OwnerUID || created.OwnerGID != planned.OwnerGID {
			t.Fatalf("receipt resource escaped plan: %#v", created)
		}
	}
	if !strings.Contains(executor.calls[len(executor.calls)-1], "reflink") || !strings.Contains(executor.unit, "Options=loop,nosuid") || strings.Contains(executor.unit, "noexec") || strings.Contains(executor.unit, "nodev") || !strings.Contains(executor.unit, "Before=docker.service") {
		t.Fatalf("calls/unit = %#v\n%s", executor.calls, executor.unit)
	}
	if !slices.ContainsFunc(executor.calls, func(call string) bool {
		return strings.HasPrefix(call, "format:ghcr.io/") && strings.Contains(call, "@sha256:")
	}) {
		t.Fatalf("format did not use pinned installer image: %#v", executor.calls)
	}
	callIndex := func(want string) int { return slices.Index(executor.calls, want) }
	for _, assertion := range []struct {
		before string
		after  string
	}{{"mkdir:runner-storage", "enable"}, {"enable", "secure:runner-storage"}, {"secure:runner-storage", "mkdir:artifacts-parent"}, {"secure:runner-storage", "mkdir:run"}, {"secure:runner-storage", "mkdir:workspace"}, {"mkdir:workspace", "reflink"}} {
		if before, after := callIndex(assertion.before), callIndex(assertion.after); before < 0 || after < 0 || before >= after {
			t.Fatalf("host storage mutation order %q -> %q is unsafe: %#v", assertion.before, assertion.after, executor.calls)
		}
	}
	artifactParent, _ := plannedPathByName(plan.Paths, "artifacts-parent")
	run, _ := plannedPathByName(plan.Paths, "run")
	if !slices.Equal(executor.reflink, []string{artifactParent.Path, run.Path, plan.Storage.WorkspacePath}) {
		t.Fatalf("reflink proof escaped accepted storage topology: %#v", executor.reflink)
	}
}

func TestHostApplyFailureRollsBackOnlyEmptyResourcesAndKeepsReceiptAccurate(t *testing.T) {
	plan := imageApplyPlan(t)
	receipt := acceptedReceipt(t, plan)
	executor := &fakeHostApplyExecutor{euid: 0, failAt: "format", nonempty: map[ResourceKind]bool{ResourceFilesystemImage: true}, retained: map[string]bool{"runner-root": true}}
	var persisted InstallReceipt
	updated, err := ApplyHost(context.Background(), plan, receipt, HostApplyDependencies{Executor: executor, Now: time.Now, PersistReceipt: func(value InstallReceipt) error { persisted = value; return nil }})
	if err == nil || !strings.Contains(err.Error(), "format filesystem image") {
		t.Fatalf("format error = %v", err)
	}
	if len(updated.CreatedResources) != 2 || !slices.ContainsFunc(updated.CreatedResources, func(resource CreatedResource) bool { return resource.Kind == ResourceFilesystemImage }) || !slices.ContainsFunc(updated.CreatedResources, func(resource CreatedResource) bool { return resource.ID == "runner-root" }) || len(persisted.CreatedResources) != 2 {
		t.Fatalf("retained receipt = %#v persisted %#v", updated.CreatedResources, persisted.CreatedResources)
	}
	if slices.Contains(executor.removed, "filesystem-image") || slices.Contains(executor.removed, "runner-root") || !slices.Contains(executor.removed, "runner-storage") {
		t.Fatalf("rollback removed wrong resources: %#v", executor.removed)
	}
}

func TestHostApplyResumesFromExactRecordedResources(t *testing.T) {
	plan := imageApplyPlan(t)
	receipt := acceptedReceipt(t, plan)
	firstExecutor := &fakeHostApplyExecutor{euid: 0, failAt: "format", nonempty: map[ResourceKind]bool{ResourceFilesystemImage: true}, retained: map[string]bool{"runner-root": true}}
	persisted := receipt
	if _, err := ApplyHost(context.Background(), plan, receipt, HostApplyDependencies{Executor: firstExecutor, Now: time.Now, PersistReceipt: func(value InstallReceipt) error { persisted = value; return nil }}); err == nil {
		t.Fatal("injected first host apply unexpectedly succeeded")
	}
	secondExecutor := &fakeHostApplyExecutor{euid: 0, nonempty: map[ResourceKind]bool{}}
	completed, err := ApplyHost(context.Background(), plan, persisted, HostApplyDependencies{Executor: secondExecutor, Now: time.Now, PersistReceipt: func(value InstallReceipt) error { persisted = value; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if completed.CompletedStages[len(completed.CompletedStages)-1].Stage != StageHostApply {
		t.Fatalf("resumed host apply stages = %#v", completed.CompletedStages)
	}
	if slices.Contains(secondExecutor.calls, "mkdir:runner-root") || slices.Contains(secondExecutor.calls, "allocate:filesystem-image") || !slices.ContainsFunc(secondExecutor.calls, func(call string) bool { return strings.HasPrefix(call, "format:") }) {
		t.Fatalf("resume did not adopt exact recorded resources: %#v", secondExecutor.calls)
	}
}

func TestHostApplyPersistenceFailureRollsBackNewMutation(t *testing.T) {
	plan := imageApplyPlan(t)
	receipt := acceptedReceipt(t, plan)
	executor := &fakeHostApplyExecutor{euid: 0, nonempty: map[ResourceKind]bool{}}
	_, err := ApplyHost(context.Background(), plan, receipt, HostApplyDependencies{Executor: executor, Now: time.Now, PersistReceipt: func(InstallReceipt) error { return errors.New("injected receipt failure") }})
	if err == nil || slices.Contains(executor.calls, "mkdir:runner-root") || len(executor.removed) != 0 {
		t.Fatalf("intent persistence failure mutated host: calls=%#v removed=%#v err=%v", executor.calls, executor.removed, err)
	}
}

func TestHostApplyCompletionPersistenceFailureRollsBackNewMutation(t *testing.T) {
	plan := imageApplyPlan(t)
	receipt := acceptedReceipt(t, plan)
	executor := &fakeHostApplyExecutor{euid: 0, nonempty: map[ResourceKind]bool{}}
	persistCalls := 0
	var persisted InstallReceipt
	_, err := ApplyHost(context.Background(), plan, receipt, HostApplyDependencies{Executor: executor, Now: time.Now, PersistReceipt: func(value InstallReceipt) error {
		persistCalls++
		if persistCalls == 2 {
			return errors.New("injected completion receipt failure")
		}
		persisted = value
		return nil
	}})
	if err == nil || !slices.Contains(executor.removed, "runner-root") || slices.ContainsFunc(persisted.CreatedResources, func(resource CreatedResource) bool { return resource.ID == "runner-root" }) {
		t.Fatalf("completion failure rollback = removed %#v receipt %#v err %v", executor.removed, persisted.CreatedResources, err)
	}
}

func TestHostApplyPersistenceFailureKeepsNonemptyMutationLedgered(t *testing.T) {
	plan := imageApplyPlan(t)
	receipt := acceptedReceipt(t, plan)
	executor := &fakeHostApplyExecutor{euid: 0, nonempty: map[ResourceKind]bool{}, retained: map[string]bool{"runner-root": true}}
	persistCalls := 0
	var persisted InstallReceipt
	_, err := ApplyHost(context.Background(), plan, receipt, HostApplyDependencies{Executor: executor, Now: time.Now, PersistReceipt: func(value InstallReceipt) error {
		persistCalls++
		if persistCalls == 2 {
			return errors.New("injected receipt failure")
		}
		persisted = value
		return nil
	}})
	if err == nil || !slices.ContainsFunc(persisted.CreatedResources, func(resource CreatedResource) bool { return resource.ID == "runner-root" }) {
		t.Fatalf("retained mutation was not ledgered: receipt=%#v err=%v", persisted.CreatedResources, err)
	}
}

func TestHostApplyFinalPersistenceFailureDoesNotCommitStage(t *testing.T) {
	plan := imageApplyPlan(t)
	receipt := acceptedReceipt(t, plan)
	executor := &fakeHostApplyExecutor{euid: 0, nonempty: map[ResourceKind]bool{}}
	var rollbackReceipt InstallReceipt
	_, err := ApplyHost(context.Background(), plan, receipt, HostApplyDependencies{Executor: executor, Now: time.Now, PersistReceipt: func(value InstallReceipt) error {
		if len(value.CompletedStages) > 0 && value.CompletedStages[len(value.CompletedStages)-1].Stage == StageHostApply {
			return errors.New("injected final receipt failure")
		}
		rollbackReceipt = value
		return nil
	}})
	if err == nil || len(rollbackReceipt.CompletedStages) != 2 || rollbackReceipt.CompletedStages[1].Stage != StagePlanAccepted {
		t.Fatalf("failed host stage leaked into rollback receipt: %#v err=%v", rollbackReceipt.CompletedStages, err)
	}
}

func TestHostApplyRevalidationAndReplayFailBeforeMutation(t *testing.T) {
	plan := imageApplyPlan(t)
	for _, test := range []struct {
		name    string
		receipt InstallReceipt
		exec    *fakeHostApplyExecutor
	}{
		{"not root", acceptedReceipt(t, plan), &fakeHostApplyExecutor{euid: 1000, nonempty: map[ResourceKind]bool{}}},
		{"stale root facts", acceptedReceipt(t, plan), &fakeHostApplyExecutor{euid: 0, revalidate: errors.New("KVM changed"), nonempty: map[ResourceKind]bool{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ApplyHost(context.Background(), plan, test.receipt, HostApplyDependencies{Executor: test.exec, Now: time.Now, PersistReceipt: func(InstallReceipt) error { t.Fatal("receipt persisted"); return nil }})
			if err == nil {
				t.Fatal("unsafe host apply succeeded")
			}
			if slices.ContainsFunc(test.exec.calls, func(call string) bool {
				return strings.HasPrefix(call, "mkdir:") || strings.HasPrefix(call, "allocate:")
			}) {
				t.Fatalf("mutation occurred: %#v", test.exec.calls)
			}
		})
	}
}

func TestCompletedHostApplyReplayOnlyRevalidates(t *testing.T) {
	plan := imageApplyPlan(t)
	receipt := acceptedReceipt(t, plan)
	if err := receipt.CompleteStage(StageHostApply, plan.CreatedAt, map[string]string{"workspaceDeviceIdentity": "btrfs-uuid:01234567-89ab-cdef-0123-456789abcdef", "reflinkMutationIsolation": "passed"}); err != nil {
		t.Fatal(err)
	}
	executor := &fakeHostApplyExecutor{euid: 0, nonempty: map[ResourceKind]bool{}}
	persisted := false
	updated, err := ApplyHost(context.Background(), plan, receipt, HostApplyDependencies{Executor: executor, Now: time.Now, PersistReceipt: func(InstallReceipt) error {
		persisted = true
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted || !slices.Equal(executor.calls, []string{"revalidate"}) || len(updated.CompletedStages) != len(receipt.CompletedStages) {
		t.Fatalf("completed replay mutated state: persisted=%t calls=%#v stages=%#v", persisted, executor.calls, updated.CompletedStages)
	}
}

func TestHostTeardownUsesOnlyNarrowPrivilegedVerification(t *testing.T) {
	plan := imageApplyPlan(t)
	receipt := acceptedReceipt(t, plan)
	if err := receipt.CompleteStage(StageHostApply, plan.CreatedAt, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	executor := &fakeHostApplyExecutor{euid: 0, nonempty: map[ResourceKind]bool{}}
	if err := VerifyHostTeardown(context.Background(), plan, receipt, executor); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(executor.calls, []string{"revalidate-teardown"}) {
		t.Fatalf("host teardown verification calls = %#v", executor.calls)
	}
	executor = &fakeHostApplyExecutor{euid: 0, teardown: errors.New("recorded host resource changed"), nonempty: map[ResourceKind]bool{}}
	if err := VerifyHostTeardown(context.Background(), plan, receipt, executor); err == nil || !strings.Contains(err.Error(), "recorded host resource changed") {
		t.Fatalf("host teardown verification failure = %v", err)
	}
}
