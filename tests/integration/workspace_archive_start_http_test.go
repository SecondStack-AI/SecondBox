package integration_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/fleet"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestFleetArchiveUsesPlatformSubjectAuthority(t *testing.T) {
	fixture := newTeardownFixture(t)
	if err := fixture.store.ConfigureWorkspaceArchives(t.Context(), startRehomeArchiveProvider{}, map[string]int64{fixture.profileName: 3600}); err != nil {
		t.Fatal(err)
	}
	sandboxID, sandbox := createStoppedFixtureSandbox(t, fixture, "fleet-archive-http")
	control := fleet.HTTPControlPlane{BaseURL: fixture.server, Token: testPlatformToken, Pool: "fleet", Client: http.DefaultClient}
	if err := control.Archive(t.Context(), contracts.FleetWorkspace{
		SandboxID: sandboxID, TenantRef: fixture.tenantRef, SubjectRef: fixture.subjectRef, Revision: sandbox.Revision,
	}); err != nil {
		t.Fatalf("fleet platform-token archive: %v", err)
	}
	client, err := secondboxclient.NewSecondBoxSubjectClient(fixture.server, testPlatformToken, fixture.tenantRef, fixture.subjectRef, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := client.AdoptSandbox(t.Context(), sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if handle.Snapshot().Workspace.Residency != "archiving" {
		t.Fatalf("fleet request did not admit archive: %#v", handle.Snapshot().Workspace)
	}
}

func TestArchiveStartConflictExposesResidencyThenAdmitsFreshRevision(t *testing.T) {
	fixture := newTeardownFixture(t)
	policy := map[string]int64{fixture.profileName: 3600}
	if err := fixture.store.ConfigureWorkspaceArchives(t.Context(), startRehomeArchiveProvider{}, policy); err != nil {
		t.Fatal(err)
	}
	sandboxID, _ := createStoppedFixtureSandbox(t, fixture, "archive-http-start")
	client, err := secondboxclient.NewSecondBoxSubjectClient(fixture.server, testPlatformToken, fixture.tenantRef, fixture.subjectRef, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := client.AdoptSandbox(t.Context(), sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := handle.Archive(t.Context(), secondboxclient.LifecycleOptions{IdempotencyKey: "http-idle-archive"})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := handle.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Workspace.Residency != "archiving" {
		t.Fatalf("archive residency=%s", observation.Workspace.Residency)
	}
	request := secondboxclient.StartSandboxRequest{Image: testExecutionImage(), CapacityWaitMilliseconds: 60_000}
	_, err = handle.Start(t.Context(), request, secondboxclient.LifecycleOptions{IdempotencyKey: "http-archive-racing-start"})
	var conflict *secondboxclient.APIError
	if !errors.As(err, &conflict) || conflict.StatusCode != http.StatusConflict || conflict.Problem == nil || conflict.Problem.Code != "workspace_mutation_conflict" {
		t.Fatalf("archive/start conflict=%v", err)
	}
	if err := fixture.store.ReconcileWorkspaceArchives(t.Context(), time.Now().UTC(), policy); err != nil {
		t.Fatal(err)
	}
	export := pendingStartArchiveCommand(t, fixture, sandboxID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_EXPORT)
	descriptor := &runnerv1.WorkspaceArchiveDescriptor{ArchiveBytes: 4096, ArchiveChecksum: "sha256:" + strings.Repeat("a", 64), ImageChecksum: "sha256:" + strings.Repeat("b", 64)}
	recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), export, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, descriptor)
	if err := fixture.store.ReconcileWorkspaceArchives(t.Context(), time.Now().UTC(), policy); err != nil {
		t.Fatal(err)
	}
	cleanup := pendingStartArchiveCommand(t, fixture, sandboxID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_DELETE_SOURCE)
	recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), cleanup, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, nil)
	archived, err := handle.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if archived.Workspace.Residency != "archived" || archived.Revision <= observation.Revision {
		t.Fatalf("archive did not publish a fresh residency revision: %#v", archived.Workspace)
	}
	var current secondboxclient.Operation
	if err := client.RequestJSON(t.Context(), "getOperation", secondboxclient.CallOptions{PathParameters: map[string]string{"operationId": archive.ID}}, &current); err != nil || current.State != "succeeded" {
		t.Fatalf("archive operation=%#v err=%v", current, err)
	}
	if _, err := handle.Start(t.Context(), request, secondboxclient.LifecycleOptions{IdempotencyKey: "http-archive-fresh-start"}); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Stop(t.Context(), secondboxclient.LifecycleOptions{IdempotencyKey: "http-archive-stop-pending-start"}); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Delete(t.Context(), secondboxclient.LifecycleOptions{IdempotencyKey: "http-archive-delete"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.ReconcileWorkspaceArchives(t.Context(), time.Now().UTC(), policy); err != nil {
		t.Fatal(err)
	}
}
