package service_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"secondstack/sandbox-service/internal/ports"
	"secondstack/sandbox-service/internal/service"
	"secondstack/sandbox-service/internal/store"
	"secondstack/sandbox-service/pkg/contracts"
)

func TestEnvironmentLifecycleFencesGenerationsAndRetainsWorkspace(t *testing.T) {
	harness := newHarness(t)
	resolved := harness.resolve(t, contracts.LifecyclePolicyAgentCompartment)
	duplicate := harness.resolve(t, contracts.LifecyclePolicyAgentCompartment)
	if duplicate.Created || duplicate.Environment.ID != resolved.Environment.ID {
		t.Fatalf("duplicate resolve = %#v, want existing Environment", duplicate)
	}

	started, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if started.Environment.CurrentGeneration != 1 || started.Instance == nil ||
		started.Instance.State != contracts.InstanceStateReady {
		t.Fatalf("start = %#v", started)
	}
	lease, err := harness.service.AcquireLease(t.Context(), resolved.Environment.ID, contracts.AcquireLeaseRequest{
		ContractVersion: contracts.ContractVersionV1, HolderRef: "agent-manager:run-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.StopEnvironment(t.Context(), resolved.Environment.ID, 99); !errors.Is(err, ports.ErrGenerationFenced) {
		t.Fatalf("stale stop error = %v, want generation fenced", err)
	}
	stopped, err := harness.service.StopEnvironment(t.Context(), resolved.Environment.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Environment.State != contracts.EnvironmentStateStopped || stopped.Environment.WorkspaceID != resolved.Environment.WorkspaceID {
		t.Fatalf("stop = %#v, want retained workspace", stopped)
	}
	if _, err := harness.service.RenewLease(t.Context(), lease.ID, contracts.RenewLeaseRequest{
		ContractVersion: contracts.ContractVersionV1,
	}); !errors.Is(err, ports.ErrLeaseReleased) {
		t.Fatalf("renew fenced lease error = %v, want released/expired", err)
	}
	restarted, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Environment.CurrentGeneration != 2 || restarted.Environment.WorkspaceID != resolved.Environment.WorkspaceID {
		t.Fatalf("restart = %#v, want generation 2 on retained workspace", restarted)
	}
	if harness.backend.destroyCount != 1 {
		t.Fatalf("Destroy() count = %d, want 1", harness.backend.destroyCount)
	}
}

func TestWorkspaceUsageAggregatesCurrentSubjectSnapshotsAndQuotas(t *testing.T) {
	harness := newHarness(t)
	resolved := harness.resolve(t, contracts.LifecyclePolicyAgentCompartment)
	started, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.CommitWorkspaceVersion(t.Context(), resolved.Environment.ID, contracts.CommitWorkspaceVersionRequest{
		ContractVersion:    contracts.ContractVersionV1,
		ExpectedGeneration: started.Environment.CurrentGeneration,
		TerminalTurnID:     "turn-workspace-usage",
		TerminalStatus:     contracts.WorkspaceTerminalCompleted,
	}); err != nil {
		t.Fatal(err)
	}

	usage, err := harness.service.GetWorkspaceUsage(t.Context(), "tenant", "subject")
	if err != nil {
		t.Fatal(err)
	}
	if usage.EnvironmentCount != 1 || usage.QuotaBytes != 8<<30 || usage.UsageBytes != 4096 {
		t.Fatalf("workspace usage = %#v", usage)
	}
}

func TestConcurrentEnvironmentStartsPublishOneGeneration(t *testing.T) {
	beginStartResults := make(chan ports.StartGenerationResult, 2)
	harness := newHarnessWithStoreAdapter(
		t,
		func(environmentStore ports.EnvironmentStore) ports.EnvironmentStore {
			return &observedBeginStartStore{
				EnvironmentStore: environmentStore,
				results:          beginStartResults,
			}
		},
	)
	resolved := harness.resolve(t, contracts.LifecyclePolicyAgentCompartment)
	releaseComputeStart := make(chan struct{})
	var releaseOnce sync.Once
	releaseCompute := func() {
		releaseOnce.Do(func() {
			close(releaseComputeStart)
		})
	}
	defer releaseCompute()
	harness.backend.startRelease = releaseComputeStart

	responses := make(chan contracts.LifecycleResponse, 2)
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	startEnvironment := func() {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 0)
			responses <- response
			errors <- err
		}()
	}
	startEnvironment()
	firstReservation := <-beginStartResults
	if !firstReservation.Created {
		t.Fatal("first concurrent start did not reserve the Environment generation")
	}
	startEnvironment()
	secondReservation := <-beginStartResults
	if secondReservation.Created ||
		secondReservation.Instance.ID != firstReservation.Instance.ID ||
		secondReservation.Instance.Generation != firstReservation.Instance.Generation {
		t.Fatalf(
			"second concurrent start reservation = %#v, want reuse of %#v",
			secondReservation,
			firstReservation,
		)
	}
	releaseCompute()
	wait.Wait()
	close(responses)
	close(errors)

	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for response := range responses {
		if response.Environment.CurrentGeneration != 1 || response.Instance == nil ||
			response.Instance.Generation != 1 {
			t.Fatalf("concurrent start = %#v, want generation 1", response)
		}
	}
	lifecycle, err := harness.service.GetEnvironment(t.Context(), resolved.Environment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.Environment.State != contracts.EnvironmentStateReady ||
		lifecycle.Environment.CurrentGeneration != 1 || harness.backend.startCount != 1 {
		t.Fatalf("published lifecycle = %#v startCount=%d, want one ready generation", lifecycle, harness.backend.startCount)
	}
}

func TestAgentCompartmentStopRevokesAgentExecutionsAfterDurableStop(t *testing.T) {
	harness := newHarness(t)
	resolved := harness.resolve(t, contracts.LifecyclePolicyAgentCompartment)
	if _, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.StopEnvironment(t.Context(), resolved.Environment.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got := harness.revoker.agentIDs; len(got) != 1 || got[0] != "subject" {
		t.Fatalf("revoked Agent IDs = %v, want [subject]", got)
	}

	// A retry after the durable stop repeats the idempotent outbound hook.
	if _, err := harness.service.StopEnvironment(t.Context(), resolved.Environment.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got := harness.revoker.agentIDs; len(got) != 2 || got[1] != "subject" {
		t.Fatalf("revoked Agent IDs after retry = %v, want [subject subject]", got)
	}
}

func TestCodingEnvironmentStopDoesNotRevokeAgentExecutions(t *testing.T) {
	harness := newHarness(t)
	resolved := harness.resolve(t, contracts.LifecyclePolicyCodingEnvironment)
	if _, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.StopEnvironment(t.Context(), resolved.Environment.ID, 1); err != nil {
		t.Fatal(err)
	}
	if len(harness.revoker.agentIDs) != 0 {
		t.Fatalf("revoked Agent IDs = %v, want none", harness.revoker.agentIDs)
	}
}

func TestAgentExecutionRevocationFailureLeavesCommittedStopRetriable(t *testing.T) {
	harness := newHarness(t)
	resolved := harness.resolve(t, contracts.LifecyclePolicyAgentCompartment)
	if _, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 0); err != nil {
		t.Fatal(err)
	}
	harness.revoker.err = errors.New("Agent Service unavailable")
	if _, err := harness.service.StopEnvironment(t.Context(), resolved.Environment.ID, 1); err == nil {
		t.Fatal("StopEnvironment() error = nil, want outbound revocation failure")
	}
	committed, err := harness.store.GetEnvironment(t.Context(), resolved.Environment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committed.State != contracts.EnvironmentStateStopped {
		t.Fatalf("Environment state = %q, want durable stopped", committed.State)
	}

	harness.revoker.err = nil
	if _, err := harness.service.StopEnvironment(t.Context(), resolved.Environment.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got := harness.revoker.agentIDs; len(got) != 2 {
		t.Fatalf("revocation attempts = %v, want retry after committed stop", got)
	}
}

func TestHostLossFencesLeaseAndAllowsReplacement(t *testing.T) {
	harness := newHarness(t)
	resolved := harness.resolve(t, contracts.LifecyclePolicyAgentCompartment)
	started, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := harness.service.AcquireLease(t.Context(), resolved.Environment.ID, contracts.AcquireLeaseRequest{
		ContractVersion: contracts.ContractVersionV1, HolderRef: "agent-manager:run-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.backend.inspectState = contracts.InstanceStateLost
	inspected, err := harness.service.InspectEnvironment(t.Context(), resolved.Environment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Environment.State != contracts.EnvironmentStateLost || inspected.Environment.CurrentInstanceID != "" {
		t.Fatalf("inspect = %#v, want lost compute detached", inspected)
	}
	if _, err := harness.service.RenewLease(t.Context(), lease.ID, contracts.RenewLeaseRequest{
		ContractVersion: contracts.ContractVersionV1,
	}); !errors.Is(err, ports.ErrLeaseReleased) {
		t.Fatalf("renew lost-generation lease error = %v", err)
	}
	harness.backend.inspectState = contracts.InstanceStateReady
	replacement, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, started.Environment.CurrentGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Environment.CurrentGeneration != started.Environment.CurrentGeneration+1 {
		t.Fatalf("replacement generation = %d", replacement.Environment.CurrentGeneration)
	}
}

func TestCheckpointAndArtifactEvidenceAreGenerationFenced(t *testing.T) {
	harness := newHarness(t)
	resolved := harness.resolve(t, contracts.LifecyclePolicyCodingEnvironment)
	started, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := harness.service.CheckpointEnvironment(t.Context(), resolved.Environment.ID, contracts.CheckpointRequest{
		ContractVersion: contracts.ContractVersionV1, ExpectedGeneration: 1,
		Metadata: map[string]string{"reason": "manual"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.WorkspaceID != resolved.Environment.WorkspaceID || snapshot.ContentHash == "" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	artifact, err := harness.service.ExchangeArtifact(t.Context(), resolved.Environment.ID, contracts.ExchangeArtifactRequest{
		ContractVersion: contracts.ContractVersionV1, ExpectedGeneration: 1,
		SourceRef: "workspace:/result.json", Name: "result.json", MimeType: "application/json",
		Metadata: map[string]string{"kind": "result"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Generation != started.Environment.CurrentGeneration || artifact.SHA256 == "" {
		t.Fatalf("artifact = %#v", artifact)
	}
	if _, err := harness.service.CheckpointEnvironment(t.Context(), resolved.Environment.ID, contracts.CheckpointRequest{
		ContractVersion: contracts.ContractVersionV1, ExpectedGeneration: 2, Metadata: map[string]string{},
	}); !errors.Is(err, ports.ErrGenerationFenced) {
		t.Fatalf("stale checkpoint error = %v", err)
	}
}

func TestExecuteEnvironmentRequiresCurrentGenerationLease(t *testing.T) {
	harness := newHarness(t)
	resolved := harness.resolve(t, contracts.LifecyclePolicyAgentCompartment)
	started, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := harness.service.AcquireLease(t.Context(), resolved.Environment.ID, contracts.AcquireLeaseRequest{
		ContractVersion: contracts.ContractVersionV1, HolderRef: "agent-service:turn-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := contracts.ExecuteRequest{
		ContractVersion:      contracts.ContractVersionV1,
		ExpectedGeneration:   started.Environment.CurrentGeneration,
		LeaseID:              lease.ID,
		Operation:            "exec",
		Command:              "sh",
		Args:                 []string{"-c", "printf ok"},
		AllowedConnectionIDs: []string{"connection-1", "connection-2"},
	}
	result, err := harness.service.ExecuteEnvironment(t.Context(), resolved.Environment.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.InstanceID != started.Instance.ID || result.Stdout != "ok" {
		t.Fatalf("execute result = %#v", result)
	}
	if harness.backend.executeInput.Identity.InstanceID != started.Instance.ID ||
		harness.backend.executeInput.Operation.LeaseID != lease.ID ||
		!reflect.DeepEqual(harness.backend.executeInput.Operation.AllowedConnectionIDs, request.AllowedConnectionIDs) {
		t.Fatalf("backend execute input = %#v", harness.backend.executeInput)
	}
	if _, err := harness.service.ReleaseLease(t.Context(), lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.ExecuteEnvironment(t.Context(), resolved.Environment.ID, request); !errors.Is(err, ports.ErrLeaseReleased) {
		t.Fatalf("released-lease execute error = %v, want released lease", err)
	}
	request.ExpectedGeneration++
	if _, err := harness.service.ExecuteEnvironment(t.Context(), resolved.Environment.ID, request); !errors.Is(err, ports.ErrGenerationFenced) {
		t.Fatalf("stale-generation execute error = %v, want generation fenced", err)
	}
}

func TestExecuteEnvironmentRejectsInvalidAllowedConnectionIDsBeforeCompute(t *testing.T) {
	harness := newHarness(t)
	resolved := harness.resolve(t, contracts.LifecyclePolicyAgentCompartment)
	started, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := harness.service.AcquireLease(t.Context(), resolved.Environment.ID, contracts.AcquireLeaseRequest{
		ContractVersion: contracts.ContractVersionV1, HolderRef: "agent-service:turn-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, allowedConnectionIDs := range [][]string{
		{"connection-1", ""},
		{"connection-1", "connection-1"},
	} {
		harness.backend.executeInput = ports.ExecuteInput{}
		_, err := harness.service.ExecuteEnvironment(t.Context(), resolved.Environment.ID, contracts.ExecuteRequest{
			ContractVersion:      contracts.ContractVersionV1,
			ExpectedGeneration:   started.Environment.CurrentGeneration,
			LeaseID:              lease.ID,
			Operation:            "exec",
			Command:              "true",
			AllowedConnectionIDs: allowedConnectionIDs,
		})
		if err == nil {
			t.Fatalf("allowedConnectionIds %#v did not fail closed", allowedConnectionIDs)
		}
		if harness.backend.executeInput.Identity.InstanceID != "" {
			t.Fatalf("compute received invalid allowedConnectionIds %#v", allowedConnectionIDs)
		}
	}
}

func TestWorkspaceFileStreamingRequiresActiveExactGenerationLease(t *testing.T) {
	harness := newHarness(t)
	resolved := harness.resolve(t, contracts.LifecyclePolicyAgentCompartment)
	started, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := harness.service.AcquireLease(t.Context(), resolved.Environment.ID, contracts.AcquireLeaseRequest{
		ContractVersion: contracts.ContractVersionV1, HolderRef: "file-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	reader, size, err := harness.service.OpenWorkspaceFile(
		t.Context(), resolved.Environment.ID, "artifacts/report.txt", started.Environment.CurrentGeneration, lease.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	err = errors.Join(err, reader.Close())
	if err != nil || size != int64(len(content)) || string(content) != "workspace file" {
		t.Fatalf("workspace file read size=%d content=%q err=%v", size, content, err)
	}
	result, err := harness.service.PutWorkspaceFile(
		t.Context(), resolved.Environment.ID, "uploads/input.txt", started.Environment.CurrentGeneration,
		lease.ID, strings.NewReader("input"),
	)
	if err != nil || result.SizeBytes != 5 {
		t.Fatalf("workspace file write result=%+v err=%v", result, err)
	}
	if _, _, err := harness.service.OpenWorkspaceFile(
		t.Context(), resolved.Environment.ID, "artifacts/report.txt", started.Environment.CurrentGeneration+1, lease.ID,
	); !errors.Is(err, ports.ErrGenerationFenced) {
		t.Fatalf("generation fence error = %v", err)
	}
	if _, err := harness.service.ReleaseLease(t.Context(), lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.PutWorkspaceFile(
		t.Context(), resolved.Environment.ID, "uploads/input.txt", started.Environment.CurrentGeneration,
		lease.ID, strings.NewReader("input"),
	); !errors.Is(err, ports.ErrLeaseReleased) {
		t.Fatalf("released lease error = %v", err)
	}
}

func TestWorkspaceVersionsCommitMaterializeAndPurgeUnderFences(t *testing.T) {
	harness := newHarness(t)
	source := harness.resolveWithKey(t, "source", contracts.LifecyclePolicyAgentCompartment)
	started, err := harness.service.StartEnvironment(t.Context(), source.Environment.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := harness.service.CommitWorkspaceVersion(t.Context(), source.Environment.ID, contracts.CommitWorkspaceVersionRequest{
		ContractVersion: contracts.ContractVersionV1, ExpectedGeneration: started.Environment.CurrentGeneration,
		TerminalTurnID: "turn-1", TerminalStatus: contracts.WorkspaceTerminalCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.LogicalVersion != 1 || !first.Dirty || first.SnapshotID == "" {
		t.Fatalf("first version = %#v", first)
	}
	if got := harness.revoker.agentIDs; len(got) != 0 {
		t.Fatalf("terminal workspace commit revoked the completing Agent execution: %v", got)
	}
	second, err := harness.service.CommitWorkspaceVersion(t.Context(), source.Environment.ID, contracts.CommitWorkspaceVersionRequest{
		ContractVersion: contracts.ContractVersionV1, ExpectedGeneration: started.Environment.CurrentGeneration,
		TerminalTurnID: "turn-2", TerminalStatus: contracts.WorkspaceTerminalCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.LogicalVersion != 2 || second.Dirty || second.SnapshotID != first.SnapshotID || second.SnapshotLogicalVersion != 1 {
		t.Fatalf("clean version = %#v", second)
	}

	target := harness.resolveWithKey(t, "target", contracts.LifecyclePolicyAgentCompartment)
	if err := harness.service.MaterializeWorkspaceVersion(t.Context(), target.Environment.ID, contracts.MaterializeWorkspaceVersionRequest{
		ContractVersion: contracts.ContractVersionV1, SourceEnvironmentID: source.Environment.ID,
		SourceLogicalVersion: second.LogicalVersion, ExpectedTargetGeneration: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if harness.backend.materializeCount != 1 {
		t.Fatalf("materialize count = %d", harness.backend.materializeCount)
	}
	lease, err := harness.service.StartEnvironment(t.Context(), target.Environment.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := harness.service.AcquireLease(t.Context(), target.Environment.ID, contracts.AcquireLeaseRequest{
		ContractVersion: contracts.ContractVersionV1, HolderRef: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.service.PurgeEnvironment(t.Context(), target.Environment.ID, contracts.PurgeEnvironmentRequest{
		ContractVersion: contracts.ContractVersionV1, ExpectedGeneration: lease.Environment.CurrentGeneration,
	}); !errors.Is(err, ports.ErrEnvironmentBusy) {
		t.Fatalf("active-lease purge error = %v", err)
	}
	if _, err := harness.service.ReleaseLease(t.Context(), acquired.ID); err != nil {
		t.Fatal(err)
	}
	if err := harness.service.PurgeEnvironment(t.Context(), target.Environment.ID, contracts.PurgeEnvironmentRequest{
		ContractVersion: contracts.ContractVersionV1, ExpectedGeneration: lease.Environment.CurrentGeneration,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUnpublishedComputeIsCleanedUpBeforeReplacement(t *testing.T) {
	harness := newHarness(t)
	resolved := harness.resolve(t, contracts.LifecyclePolicyAgentCompartment)
	harness.backend.startState = contracts.InstanceStatePreparing
	if _, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 0); err == nil {
		t.Fatal("StartEnvironment() error = nil, want non-ready Sandbox Host response failure")
	}
	failed, err := harness.service.GetEnvironment(t.Context(), resolved.Environment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Environment.State != contracts.EnvironmentStateFailed ||
		harness.backend.stopCount != 1 || harness.backend.destroyCount != 1 {
		t.Fatalf("failed start = %#v stop=%d destroy=%d", failed, harness.backend.stopCount, harness.backend.destroyCount)
	}
	harness.backend.startState = contracts.InstanceStateReady
	replacement, err := harness.service.StartEnvironment(t.Context(), resolved.Environment.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Environment.CurrentGeneration != 2 {
		t.Fatalf("replacement generation = %d, want 2", replacement.Environment.CurrentGeneration)
	}
}

func TestLifecyclePoliciesSeparateComputeIdlingFromRetention(t *testing.T) {
	harness := newHarness(t)
	agent := harness.resolveWithKey(t, "agent", contracts.LifecyclePolicyAgentCompartment)
	coding := harness.resolveWithKey(t, "coding", contracts.LifecyclePolicyCodingEnvironment)
	for _, environment := range []contracts.Environment{agent.Environment, coding.Environment} {
		if _, err := harness.service.StartEnvironment(t.Context(), environment.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	*harness.now = harness.now.UTC().Add(301 * time.Second)
	if err := harness.service.ReconcileLifecycle(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	agentState, err := harness.service.GetEnvironment(t.Context(), agent.Environment.ID)
	if err != nil {
		t.Fatal(err)
	}
	codingState, err := harness.service.GetEnvironment(t.Context(), coding.Environment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if agentState.Environment.State != contracts.EnvironmentStateStopped {
		t.Fatalf("agent policy state = %q, want stopped", agentState.Environment.State)
	}
	if codingState.Environment.State != contracts.EnvironmentStateReady {
		t.Fatalf("coding policy state = %q, want ready", codingState.Environment.State)
	}

	*harness.now = harness.now.UTC().Add(604801 * time.Second)
	if err := harness.service.ReconcileLifecycle(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.GetEnvironment(t.Context(), agent.Environment.ID); !errors.Is(err, ports.ErrEnvironmentNotFound) {
		t.Fatalf("retention purge error = %v, want Environment not found", err)
	}
	if harness.backend.purgeCount != 1 {
		t.Fatalf("Purge() count = %d, want 1", harness.backend.purgeCount)
	}
	if _, err := harness.store.GetWorkspace(t.Context(), agent.Environment.WorkspaceID); !errors.Is(err, ports.ErrEnvironmentNotFound) {
		t.Fatalf("purged workspace lookup error = %v", err)
	}
}

type harness struct {
	service *service.SandboxService
	store   *store.MemoryEnvironmentStore
	backend *fakeComputeBackend
	revoker *fakeExecutionRevoker
	now     *time.Time
}

func newHarness(t *testing.T) harness {
	return newHarnessWithStoreAdapter(t, nil)
}

func newHarnessWithStoreAdapter(
	t *testing.T,
	adaptStore func(ports.EnvironmentStore) ports.EnvironmentStore,
) harness {
	t.Helper()
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	environmentStore := store.NewMemoryEnvironmentStore(now)
	serviceStore := ports.EnvironmentStore(environmentStore)
	if adaptStore != nil {
		serviceStore = adaptStore(serviceStore)
	}
	backend := &fakeComputeBackend{
		inspectState: contracts.InstanceStateReady,
		startState:   contracts.InstanceStateReady,
	}
	revoker := &fakeExecutionRevoker{}
	counter := 0
	var counterMu sync.Mutex
	coordinator, err := service.NewSandboxService(service.Config{
		Store: serviceStore, Compute: backend, LeaseTTL: 5 * time.Minute,
		ExecutionRevoker:     revoker,
		MaxFileTransferBytes: 1 << 20, Now: func() time.Time { return now },
		NewID: func(prefix string) string {
			counterMu.Lock()
			defer counterMu.Unlock()
			counter++
			return fmt.Sprintf("%s_%d", prefix, counter)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return harness{service: coordinator, store: environmentStore, backend: backend, revoker: revoker, now: &now}
}

type observedBeginStartStore struct {
	ports.EnvironmentStore
	results chan<- ports.StartGenerationResult
}

func (s *observedBeginStartStore) BeginStart(
	ctx context.Context,
	environmentID string,
	expectedGeneration int64,
	instanceID string,
	now time.Time,
) (ports.StartGenerationResult, error) {
	result, err := s.EnvironmentStore.BeginStart(
		ctx,
		environmentID,
		expectedGeneration,
		instanceID,
		now,
	)
	if err == nil {
		s.results <- result
	}
	return result, err
}

func (h harness) resolve(t *testing.T, policy string) contracts.ResolveEnvironmentResponse {
	t.Helper()
	return h.resolveWithKey(t, "default", policy)
}

func (h harness) resolveWithKey(t *testing.T, key, policy string) contracts.ResolveEnvironmentResponse {
	t.Helper()
	response, err := h.service.ResolveEnvironment(t.Context(), contracts.ResolveEnvironmentRequest{
		ContractVersion: contracts.ContractVersionV1,
		TenantRef:       "tenant", SubjectRef: "subject", EnvironmentKey: key,
		ImageRef: "registry/image@sha256:123", ToolchainRef: "toolchain:v1",
		ResourceClassID: contracts.ResourceClassAgentStandard, LifecyclePolicyID: policy,
		Metadata: map[string]string{"test": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

type fakeComputeBackend struct {
	mu               sync.Mutex
	inspectState     string
	startState       string
	startRelease     <-chan struct{}
	startCount       int
	stopCount        int
	destroyCount     int
	purgeCount       int
	executeInput     ports.ExecuteInput
	materializeCount int
}

type fakeExecutionRevoker struct {
	agentIDs []string
	err      error
}

func (revoker *fakeExecutionRevoker) RevokeEnvironmentExecutions(_ context.Context, agentID string) error {
	revoker.agentIDs = append(revoker.agentIDs, agentID)
	return revoker.err
}

func (f *fakeComputeBackend) Ready(context.Context) error { return nil }

func (f *fakeComputeBackend) Prepare(_ context.Context, request ports.ComputeRequest) (ports.PreparedCompute, error) {
	return ports.PreparedCompute{OperationRef: "prepare:" + request.Instance.ID}, nil
}

func (f *fakeComputeBackend) Start(_ context.Context, _ ports.PreparedCompute, request ports.ComputeRequest) (ports.RunningCompute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCount++
	if f.startRelease != nil {
		<-f.startRelease
	}
	return ports.RunningCompute{BackendRef: "backend:" + request.Instance.ID, State: f.startState}, nil
}

func (f *fakeComputeBackend) Inspect(context.Context, ports.ComputeIdentity) (ports.ComputeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ports.ComputeStatus{State: f.inspectState}, nil
}

func (f *fakeComputeBackend) Stop(context.Context, ports.ComputeIdentity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCount++
	return nil
}

func (f *fakeComputeBackend) Destroy(context.Context, ports.ComputeIdentity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyCount++
	return nil
}

func (f *fakeComputeBackend) Purge(context.Context, contracts.Environment, contracts.Workspace) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purgeCount++
	return nil
}

func (f *fakeComputeBackend) Checkpoint(context.Context, ports.ComputeIdentity) (ports.CheckpointResult, error) {
	return ports.CheckpointResult{
		OpaqueRef:   "snapshot:opaque",
		ContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SizeBytes:   4096,
	}, nil
}
func (f *fakeComputeBackend) CheckpointWorkspace(context.Context, contracts.Environment, contracts.Workspace) (ports.CheckpointResult, error) {
	return f.Checkpoint(context.Background(), ports.ComputeIdentity{})
}

func (f *fakeComputeBackend) MaterializeWorkspace(context.Context, contracts.Environment, contracts.Workspace, contracts.Snapshot) error {
	f.materializeCount++
	return nil
}

func (f *fakeComputeBackend) ExchangeArtifact(context.Context, ports.ArtifactExchangeInput) (ports.ArtifactExchangeResult, error) {
	return ports.ArtifactExchangeResult{
		OpaqueRef: "artifact:opaque", SizeBytes: 42,
		SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}, nil
}

func (f *fakeComputeBackend) Execute(_ context.Context, input ports.ExecuteInput) (contracts.ExecuteResult, error) {
	f.executeInput = input
	return contracts.ExecuteResult{Stdout: "ok", ExitCode: 0}, nil
}

func (*fakeComputeBackend) OpenWorkspaceFile(context.Context, ports.WorkspaceFileInput) (io.ReadCloser, int64, error) {
	content := []byte("workspace file")
	return io.NopCloser(bytes.NewReader(content)), int64(len(content)), nil
}

func (*fakeComputeBackend) PutWorkspaceFile(_ context.Context, _ ports.WorkspaceFileInput, reader io.Reader) (ports.WorkspaceFileWriteResult, error) {
	content, err := io.ReadAll(reader)
	return ports.WorkspaceFileWriteResult{
		SizeBytes: int64(len(content)),
		SHA256:    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}, err
}
