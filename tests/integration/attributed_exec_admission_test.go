package integration_test

import (
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAttributedGenerationKeepsOneExecAfterSessionCleanup(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "attributed-exec")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-attributed-exec")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(t.Context(), principal, "attributed-exec-create", contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seedDataPlaneReadyAssignment(t, sandbox, now)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `UPDATE secondbox.assignments
		SET execution_authorization_ref='command',execution_expires_at=$2 WHERE sandbox_id=$1`, sandbox.ID, now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	relay, err := runnercontrol.NewPostgresDataPlaneStore(t.Context(), runnercontrol.PostgresDataPlaneStoreConfig{
		DatabaseURL: integrationDatabaseURL, Retention: time.Hour, MaximumSessionBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	admission := func(id string) runnercontrol.DataPlaneAdmission {
		return runnercontrol.DataPlaneAdmission{
			ID: id + sandbox.ID, StreamID: "stream-" + id + sandbox.ID,
			TenantRef: principal.TenantRef, SubjectRef: principal.SubjectRef, SandboxID: sandbox.ID,
			Generation: sandbox.Generation, RequestID: id, IdempotencyKey: id, RequestHash: id,
			Kind: "exec", Operation: "exec", DeadlineAt: now.Add(10 * time.Second), MaximumResponseBytes: 1024,
			ExecOpen: &runnerv1.ExecOpen{Command: &runnerv1.ExecOpen_Shell{Shell: "true"}, DeadlineUnixMs: uint64(now.Add(10 * time.Second).UnixMilli()), OutputLimitBytes: 1024},
			Request:  map[string]string{"command": "true"}, Now: now,
		}
	}
	for _, forbidden := range []string{"deadline", "pty", "write", "output"} {
		input := admission(forbidden)
		want := ports.ErrLifecycleUnavailable
		switch forbidden {
		case "output":
			input.MaximumResponseBytes = 2 << 20
			want = runnercontrol.ErrDataPlaneOutputLimit
		case "deadline":
			input.DeadlineAt = now.Add(30 * time.Second)
			want = runnercontrol.ErrDataPlaneDeadline
		case "pty":
			input.ExecOpen.AllocatePty = true
		case "write":
			input.Kind = "file"
			input.Operation = "write"
			input.ExecOpen = nil
			input.FileOpen = &runnerv1.FileOpen{Operation: runnerv1.FileOperation_FILE_OPERATION_WRITE, WorkspaceRelativePath: "file"}
		}
		if _, _, err := relay.AdmitDataPlane(t.Context(), input); !errors.Is(err, want) {
			t.Fatalf("%s admission = %v; want %v", forbidden, err, want)
		}
	}
	lease, err := controlPlane.AcquireSandboxLease(t.Context(), principal, sandbox.ID, sandbox.Generation, "attributed-port-lease", 60)
	if err != nil {
		t.Fatal(err)
	}
	terminal := admission("terminal")
	terminal.Kind, terminal.LeaseID = "terminal", lease.ID
	terminal.ExecOpen.AllocatePty, terminal.ExecOpen.Streaming = true, true
	terminal.ExecOpen.PtyRows, terminal.ExecOpen.PtyColumns = 24, 80
	terminal.DeferResponseCredit, terminal.UseProfileStreamWindow = true, true
	if _, _, err := relay.AdmitDataPlane(t.Context(), terminal); !errors.Is(err, ports.ErrLifecycleUnavailable) {
		t.Fatalf("terminal admission = %v", err)
	}
	if _, _, err := relay.AdmitPortSession(t.Context(), runnercontrol.PortSessionAdmission{
		Session:  contracts.PortSession{ID: "port-" + sandbox.ID, SandboxID: sandbox.ID, Generation: sandbox.Generation, Name: "web", ExpiresAt: now.Add(10 * time.Second), Transport: contracts.PortTransportProxied},
		StreamID: "port-stream-" + sandbox.ID, TenantRef: principal.TenantRef, SubjectRef: principal.SubjectRef,
		RequestID: "attributed-port", LeaseID: lease.ID, IdempotencyKey: "attributed-port", RequestHash: "attributed-port",
		CredentialDigest: make([]byte, 32), Now: now,
	}); !errors.Is(err, ports.ErrPortPolicyDenied) {
		t.Fatalf("attributed port admission = %v", err)
	}
	read := admission("read-only")
	read.Kind, read.Operation, read.ExecOpen = "file", "stat", nil
	read.DeadlineAt = now.Add(30 * time.Second)
	read.FileOpen = &runnerv1.FileOpen{Operation: runnerv1.FileOperation_FILE_OPERATION_STAT, WorkspaceRelativePath: "file"}
	if session, _, err := relay.AdmitDataPlane(t.Context(), read); err != nil {
		t.Fatalf("read-only admission = %v", err)
	} else if !session.DeadlineAt.Equal(now.Add(20 * time.Second)) {
		t.Fatalf("read-only deadline = %v", session.DeadlineAt)
	}
	type result struct {
		input   runnercontrol.DataPlaneAdmission
		session runnercontrol.DataPlaneSession
		err     error
	}
	results := make(chan result, 2)
	for _, id := range []string{"first", "second"} {
		go func() {
			input := admission(id)
			session, _, err := relay.AdmitDataPlane(t.Context(), input)
			results <- result{input, session, err}
		}()
	}
	var accepted result
	acceptedCount := 0
	for range 2 {
		result := <-results
		if result.err == nil {
			accepted = result
			acceptedCount++
		} else if !errors.Is(result.err, ports.ErrLifecycleUnavailable) {
			t.Fatal(result.err)
		}
	}
	if acceptedCount != 1 {
		t.Fatalf("accepted %d commands, want one", acceptedCount)
	}
	if replay, found, err := relay.AdmitDataPlane(t.Context(), accepted.input); err != nil || !found || replay.ID != accepted.session.ID {
		t.Fatalf("replay=%+v found=%v error=%v", replay, found, err)
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM secondbox.data_plane_sessions WHERE id=$1`, accepted.session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE secondbox.sandboxes SET lifecycle_request_metadata_json=NULL WHERE id=$1`, sandbox.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := relay.AdmitDataPlane(t.Context(), admission("after-cleanup")); !errors.Is(err, ports.ErrLifecycleUnavailable) {
		t.Fatalf("admission after session cleanup = %v", err)
	}
}
