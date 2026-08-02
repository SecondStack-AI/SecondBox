package integration_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresTerminalRelayOwnsAttachmentDetachReplayAndFenceAuthority(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "terminal-relay")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-terminal-relay")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "terminal-relay-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "terminal-relay-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: time.Second,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	session, replayed, err := relay.AdmitDataPlane(t.Context(), runnercontrol.DataPlaneAdmission{
		ID: "dps_terminal_relay", StreamID: "stream_terminal_relay",
		TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
		SubjectRef: principal.SubjectRef, LeaseID: lease.ID,
		Generation: sandbox.Generation, RequestID: "request-terminal-relay",
		Kind: "terminal", Operation: "terminal", IdempotencyKey: "terminal-relay",
		RequestHash: "terminal-request-hash", DeadlineAt: now.Add(time.Minute),
		MaximumResponseBytes: 1024, MaximumRequestBytes: 1024, StreamWindowBytes: 64,
		DeferResponseCredit: true, Detachable: true,
		ExecOpen: &runnerv1.ExecOpen{
			Command:        &runnerv1.ExecOpen_Shell{Shell: "cat"},
			DeadlineUnixMs: uint64(now.Add(time.Minute).UnixMilli()), OutputLimitBytes: 1024,
			AllocatePty: true, PtyRows: 24, PtyColumns: 80, Streaming: true,
		},
		Request: map[string]any{"detachable": true}, Now: now,
	})
	if err != nil || replayed {
		t.Fatalf("terminal admission = %#v replayed=%t error=%v", session, replayed, err)
	}
	if session.TerminalDetachSeconds != 30 || !session.Detachable {
		t.Fatalf("terminal detach policy = %#v", session)
	}
	delivery, found, err := relay.ClaimOutboundFrame(t.Context(), seed.RunnerID, seed.ConnectionOne, now)
	if err != nil || !found || !delivery.Message.GetExec().GetOpen().GetAllocatePty() {
		t.Fatalf("terminal Open delivery = %#v found=%t error=%v", delivery, found, err)
	}
	if err := relay.MarkOutboundFrameDelivered(t.Context(), delivery.ID, seed.ConnectionOne, delivery.ClaimAttempt, now); err != nil {
		t.Fatal(err)
	}

	attached, err := relay.AcquireTerminalAttachment(
		t.Context(), principal.TenantRef, principal.SubjectRef,
		sandbox.ID, session.ID, sandbox.Generation, "attachment-one", now,
	)
	if err != nil || attached.AttachmentID != "attachment-one" {
		t.Fatalf("first terminal attachment = %#v error=%v", attached, err)
	}
	if _, err := relay.AcquireTerminalAttachment(
		t.Context(), principal.TenantRef, principal.SubjectRef,
		sandbox.ID, session.ID, sandbox.Generation, "attachment-two", now,
	); !errors.Is(err, runnercontrol.ErrTerminalAttached) {
		t.Fatalf("parallel terminal attachment error = %v", err)
	}
	inserted, err := relay.AppendTerminalClientFrame(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID, "attachment-one",
		runnercontrol.TerminalClientFrame{Sequence: 0, Credit: 4}, now,
	)
	if err != nil || !inserted {
		t.Fatalf("terminal credit append = %t, %v", inserted, err)
	}
	inserted, err = relay.AppendTerminalClientFrame(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID, "attachment-one",
		runnercontrol.TerminalClientFrame{Sequence: 1, ResizeRows: 40, ResizeColumns: 120}, now,
	)
	if err != nil || !inserted {
		t.Fatalf("terminal resize append = %t, %v", inserted, err)
	}
	inserted, err = relay.AppendTerminalClientFrame(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID, "attachment-one",
		runnercontrol.TerminalClientFrame{Sequence: 2, Input: []byte{0, 1, 0xfe, 0xff}}, now,
	)
	if err != nil || !inserted {
		t.Fatalf("terminal binary input append = %t, %v", inserted, err)
	}
	if detached, err := relay.DetachTerminalAttachment(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID, "attachment-one", now.Add(time.Second),
	); err != nil || !detached {
		t.Fatalf("terminal detach = %t, %v", detached, err)
	}
	reattached, err := relay.AcquireTerminalAttachment(
		t.Context(), principal.TenantRef, principal.SubjectRef,
		sandbox.ID, session.ID, sandbox.Generation, "attachment-two", now.Add(29*time.Second),
	)
	if err != nil {
		t.Fatalf("terminal reconnect within bound: %v", err)
	}
	if reattached.NextClientSequence != 3 {
		t.Fatalf("terminal reconnect next client sequence = %d", reattached.NextClientSequence)
	}

	output := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Pty{Pty: &runnerv1.PtyFrame{
			Fence: seed.Fence, OperationId: session.ID, StreamId: session.StreamID, Sequence: 1,
			Payload: &runnerv1.PtyFrame_Output{Output: &runnerv1.ExecOutput{
				Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
				Data:    []byte{0, 1, 0xfe, 0xff},
			}},
		}},
	}
	if inserted, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionOne, Message: output,
	}, now.Add(29*time.Second)); err != nil || !inserted {
		t.Fatalf("terminal output persistence = %t, %v", inserted, err)
	}
	frames, err := relay.ListTerminalServerFrames(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID, -1, 64,
	)
	if err != nil || len(frames) != 1 || !bytes.Equal(frames[0].Output, []byte{0, 1, 0xfe, 0xff}) {
		t.Fatalf("replayed terminal frames = %#v error=%v", frames, err)
	}
	if detached, err := relay.DetachTerminalAttachment(
		t.Context(), principal.TenantRef, principal.SubjectRef, session.ID, "attachment-two", now.Add(29*time.Second),
	); err != nil || !detached {
		t.Fatalf("second terminal detach = %t, %v", detached, err)
	}
	if changed, err := relay.SweepDataPlane(t.Context(), now.Add(59*time.Second), 100); err != nil || !changed {
		t.Fatalf("detached Terminal sweep = %t, %v", changed, err)
	}
	var cancelDelivery runnercontrol.RelayDelivery
	for {
		delivery, found, err := relay.ClaimOutboundFrame(
			t.Context(), seed.RunnerID, seed.ConnectionOne, now.Add(59*time.Second),
		)
		if err != nil || !found {
			t.Fatalf("detached Terminal cancellation claim = %#v found=%t error=%v", delivery, found, err)
		}
		if err := relay.MarkOutboundFrameDelivered(
			t.Context(), delivery.ID, seed.ConnectionOne, delivery.ClaimAttempt, now.Add(59*time.Second),
		); err != nil {
			t.Fatal(err)
		}
		if delivery.Message.GetExec().GetCancel() != nil {
			cancelDelivery = delivery
			break
		}
	}
	if cancelDelivery.Message.GetExec().GetCancel().GetReason() != "Terminal detach interval expired" {
		t.Fatalf("detached Terminal cancellation = %#v", cancelDelivery.Message.GetExec().GetCancel())
	}
	if changed, err := relay.SweepDataPlane(t.Context(), now.Add(59*time.Second), 100); err != nil || changed {
		t.Fatalf("duplicate detached Terminal sweep = %t, %v", changed, err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var activityState string
	if err := pool.QueryRow(t.Context(), `
		SELECT state FROM secondbox.activity_sessions WHERE id=$1`, session.ID,
	).Scan(&activityState); err != nil {
		t.Fatal(err)
	}
	if activityState != contracts.ActivitySessionStateActive {
		t.Fatalf("Terminal activity closed before terminal acknowledgement: %q", activityState)
	}
	terminal := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Pty{Pty: &runnerv1.PtyFrame{
			Fence: seed.Fence, OperationId: session.ID, StreamId: session.StreamID, Sequence: 2,
			Payload: &runnerv1.PtyFrame_Terminal{Terminal: &runnerv1.ExecTerminal{
				Kind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED, ExitCode: -1,
			}},
		}},
	}
	if inserted, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: seed.RunnerID, ConnectionID: seed.ConnectionOne, Message: terminal,
	}, now.Add(59*time.Second)); err != nil || !inserted {
		t.Fatalf("detached Terminal acknowledgement = %t, %v", inserted, err)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT state FROM secondbox.activity_sessions WHERE id=$1`, session.ID,
	).Scan(&activityState); err != nil {
		t.Fatal(err)
	}
	if activityState != contracts.ActivitySessionStateClosed {
		t.Fatalf("Terminal activity remained open after terminal acknowledgement: %q", activityState)
	}
	if _, err := controlPlane.ReleaseSandboxLease(
		t.Context(), principal, lease.ID, "terminal-relay-initial-lease-release",
	); err != nil {
		t.Fatal(err)
	}

	admitTerminal := func(id string, leaseID string, detachable bool) runnercontrol.DataPlaneSession {
		t.Helper()
		admitted, _, err := relay.AdmitDataPlane(t.Context(), runnercontrol.DataPlaneAdmission{
			ID: "dps_" + id, StreamID: "stream_" + id,
			TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
			SubjectRef: principal.SubjectRef, LeaseID: leaseID,
			Generation: sandbox.Generation, RequestID: "request-" + id,
			Kind: "terminal", Operation: "terminal", IdempotencyKey: id,
			RequestHash: id + "-hash", DeadlineAt: now.Add(time.Minute),
			MaximumResponseBytes: 1024, MaximumRequestBytes: 1024, StreamWindowBytes: 64,
			DeferResponseCredit: true, Detachable: detachable,
			ExecOpen: &runnerv1.ExecOpen{
				Command:        &runnerv1.ExecOpen_Shell{Shell: "sleep 60"},
				DeadlineUnixMs: uint64(now.Add(time.Minute).UnixMilli()), OutputLimitBytes: 1024,
				AllocatePty: true, PtyRows: 24, PtyColumns: 80, Streaming: true,
			},
			Request: map[string]any{"detachable": detachable}, Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return admitted
	}
	claimCancellation := func(expectedReason string) {
		t.Helper()
		for {
			delivery, found, err := relay.ClaimOutboundFrame(
				t.Context(), seed.RunnerID, seed.ConnectionOne, now.Add(time.Second),
			)
			if err != nil || !found {
				t.Fatalf("Terminal cancellation claim = %#v found=%t error=%v", delivery, found, err)
			}
			if err := relay.MarkOutboundFrameDelivered(
				t.Context(), delivery.ID, seed.ConnectionOne, delivery.ClaimAttempt, now.Add(time.Second),
			); err != nil {
				t.Fatal(err)
			}
			if cancel := delivery.Message.GetExec().GetCancel(); cancel != nil {
				if cancel.Reason != expectedReason {
					t.Fatalf("Terminal cancellation reason = %q, want %q", cancel.Reason, expectedReason)
				}
				return
			}
		}
	}

	leaseExpired, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "terminal-expired-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = admitTerminal("terminal-lease-expired", leaseExpired.ID, true)
	if _, err := controlPlane.ReleaseSandboxLease(
		t.Context(), principal, leaseExpired.ID, "terminal-expired-lease-release",
	); err != nil {
		t.Fatal(err)
	}
	if changed, err := relay.SweepDataPlane(t.Context(), now.Add(time.Second), 100); err != nil || !changed {
		t.Fatalf("inactive Lease Terminal sweep = %t, %v", changed, err)
	}
	claimCancellation("operation Lease is inactive")

	leaseAttached, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, sandbox.Generation, "terminal-attached-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	nonDetachable := admitTerminal("terminal-nondetachable", leaseAttached.ID, false)
	if _, err := relay.AcquireTerminalAttachment(
		t.Context(), principal.TenantRef, principal.SubjectRef,
		sandbox.ID, nonDetachable.ID, sandbox.Generation, "attachment-nondetachable", now,
	); err != nil {
		t.Fatal(err)
	}
	if detached, err := relay.DetachTerminalAttachment(
		t.Context(), principal.TenantRef, principal.SubjectRef,
		nonDetachable.ID, "attachment-nondetachable", now,
	); err != nil || !detached {
		t.Fatalf("non-detachable Terminal disconnect = %t, %v", detached, err)
	}
	claimCancellation("public Terminal client disconnected")
}
