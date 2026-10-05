package runnercontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func (store *PostgresDataPlaneStore) AdmitPortSession(
	ctx context.Context,
	input PortSessionAdmission,
) (PortTunnel, bool, error) {
	if input.Session.ID == "" || input.StreamID == "" || input.TenantRef == "" ||
		input.Session.SandboxID == "" || input.SubjectRef == "" ||
		input.RequestID == "" || input.LeaseID == "" || input.IdempotencyKey == "" ||
		input.RequestHash == "" || input.Session.Generation < 1 ||
		input.Session.Name == "" || input.Session.ExpiresAt.IsZero() ||
		!input.Now.Before(input.Session.ExpiresAt) ||
		len(input.CredentialDigest) != sha256.Size {
		return PortTunnel{}, false, errors.New("SecondBox PortSession admission is incomplete")
	}
	if input.Session.Transport != contracts.PortTransportProxied &&
		input.Session.Transport != contracts.PortTransportDirect {
		return PortTunnel{}, false, errors.New("SecondBox PortSession transport is invalid")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, input.TenantRef, input.SubjectRef); err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession quota lock: %w", err)
	}
	lockKey := input.TenantRef + "\x1f" + input.SubjectRef +
		"\x1fport-session\x1f" + input.Session.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession idempotency lock: %w", err)
	}
	var replayHash, replayID string
	err = tx.QueryRow(ctx, `
		SELECT request_hash,id FROM secondbox.port_sessions
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3 AND idempotency_key=$4`,
		input.TenantRef, input.SubjectRef, input.Session.SandboxID, input.IdempotencyKey,
	).Scan(&replayHash, &replayID)
	if err == nil {
		if replayHash != input.RequestHash {
			return PortTunnel{}, false, ports.ErrIdempotencyConflict
		}
		tunnel, err := scanPortTunnel(tx.QueryRow(ctx, portTunnelSelect+`
			WHERE port.tenant_ref=$1 AND port.subject_ref=$2 AND port.id=$3`,
			input.TenantRef, input.SubjectRef, replayID))
		if err != nil {
			return PortTunnel{}, false, err
		}
		if err := authorizeReplayProfile(ctx, tx, input.ProfileGrants, tunnel.ProfileRevisionID); err != nil {
			return PortTunnel{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession replay commit: %w", err)
		}
		return tunnel, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession replay lookup: %w", err)
	}
	if err := rowlock.ActiveSubject(ctx, tx, input.TenantRef, input.SubjectRef, input.Now); err != nil {
		return PortTunnel{}, false, err
	}
	for _, capacityKey := range []string{
		input.TenantRef + "\x1f" + input.SubjectRef + "\x1fport-session-capacity",
		input.TenantRef + "\x1f" + input.SubjectRef +
			"\x1fport-session-capacity\x1f" + input.Session.SandboxID,
	} {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, capacityKey); err != nil {
			return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession capacity lock: %w", err)
		}
	}
	tunnel, spec, policy, err := lockPortAdmissionAuthority(ctx, tx, input)
	if err != nil {
		return PortTunnel{}, false, err
	}
	tunnel.Session = input.Session
	tunnel.Session.Protocol = policy.Protocol
	tunnel.TenantRef, tunnel.SubjectRef = input.TenantRef, input.SubjectRef
	tunnel.RequestID, tunnel.LeaseID, tunnel.StreamID = input.RequestID, input.LeaseID, input.StreamID
	tunnel.GuestPort, tunnel.StreamWindowBytes = policy.Port, spec.Execution.StreamWindowBytes
	if input.Session.ExpiresAt.After(input.Now.Add(time.Duration(policy.MaximumSessionSeconds) * time.Second)) {
		return PortTunnel{}, false, ports.ErrPortPolicyDenied
	}
	if err := enforcePortSessionCapacity(ctx, tx, input, tunnel.ProfileRevisionID, policy); err != nil {
		return PortTunnel{}, false, err
	}
	if tunnel.StreamWindowBytes < 1 {
		return PortTunnel{}, false, ports.ErrPortPolicyDenied
	}
	session := DataPlaneSession{
		ID: input.Session.ID, StreamID: input.StreamID,
		TenantRef: tunnel.TenantRef, SubjectRef: tunnel.SubjectRef,
		SandboxID:         input.Session.SandboxID,
		ProfileRevisionID: tunnel.ProfileRevisionID, AssignmentID: tunnel.AssignmentID,
		InstanceID: tunnel.InstanceID, RunnerID: tunnel.RunnerID,
		Generation: input.Session.Generation, FencingToken: bytes.Clone(tunnel.FencingToken),
		RequestID: input.RequestID, LeaseID: input.LeaseID,
		Kind: "port", Operation: "port:" + input.Session.Name, State: "pending",
		// A Port session relays a byte stream without buffering or storing it:
		// the stream window bounds its memory and its duration bounds its life,
		// so it carries no byte limit.
		DeadlineAt: input.Session.ExpiresAt, StreamWindowBytes: tunnel.StreamWindowBytes,
		CreatedAt: input.Now.UTC(), UpdatedAt: input.Now.UTC(),
	}
	// The direct transport hands the caller a Runner address, so a Runner that
	// has advertised none cannot serve it.
	if tunnel.Session.Transport == contracts.PortTransportDirect &&
		(tunnel.DataPlaneAddress == "" || tunnel.DataPlaneCertificateSPKISHA256 == "") {
		return PortTunnel{}, false, ports.ErrLifecycleUnavailable
	}
	requestJSON, err := json.Marshal(struct {
		Name            string `json:"name"`
		DurationSeconds int64  `json:"durationSeconds"`
	}{input.Session.Name, int64(input.Session.ExpiresAt.Sub(input.Now).Seconds())})
	if err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession request encoding: %w", err)
	}
	resultJSON, err := json.Marshal(dataPlaneResult{Stdout: []byte{}, Stderr: []byte{}, Content: []byte{}})
	if err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession result encoding: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.data_plane_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,assignment_id,instance_id,runner_id,generation,fencing_token,request_id,lease_id,kind,operation,stream_id,state,priority,idempotency_key,request_hash,deadline_at,maximum_response_bytes,maximum_request_bytes,stream_window_bytes,response_credit_bytes,request_stream_bytes,request_stream_closed,detachable,terminal_detach_seconds,attachment_id,attached_at,detached_at,detach_expires_at,outbound_bytes,inbound_bytes,next_inbound_sequence,terminal_kind,terminal_detail,exit_code,signal,spawn_failure_reason,elapsed_milliseconds,limit_bytes,infrastructure_failure_reason,retryable,terminal_message,result_json,metadata_json,request_json,created_at,updated_at,completed_at,retain_until,next_outbound_sequence
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'port',$13,$14,'pending',0,$15,$16,$17,$18,$18,$19,0,0,false,false,0,'',NULL,NULL,NULL,0,0,1,'','',0,0,'',0,0,'',false,'',$20,'{}',$21,$22,$22,NULL,$23,1
		)`,
		session.ID, session.TenantRef, session.SubjectRef, session.SandboxID, session.ProfileRevisionID, session.AssignmentID, session.InstanceID, session.RunnerID, session.Generation, session.FencingToken, input.RequestID, input.LeaseID, session.Operation, session.StreamID, input.IdempotencyKey, input.RequestHash, session.DeadlineAt, session.MaximumResponseBytes, tunnel.StreamWindowBytes, resultJSON, requestJSON, input.Now.UTC(), input.Now.UTC().Add(store.retention),
	); err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox Port data-plane insert: %w", err)
	}
	if tunnel.Session.Transport == contracts.PortTransportDirect {
		payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(
			portDirectDataPlaneOpenMessage(session, input, policy),
		)
		if err != nil {
			return PortTunnel{}, false, fmt.Errorf("SecondBox direct Port Open encoding: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.runner_commands (
				id,runner_id,assignment_id,kind,payload,state,target_connection_id,
				delivery_count,created_at,updated_at,delivered_at
			) VALUES ($1,$2,$3,'data-plane-direct',$4,'pending','',0,$5,$5,NULL)`,
			session.ID+"_direct_open", session.RunnerID, session.AssignmentID,
			payload, session.CreatedAt,
		); err != nil {
			return PortTunnel{}, false, fmt.Errorf("SecondBox direct Port Open insert: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.port_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,data_plane_session_id,lease_id,generation,name,guest_port,protocol,transport,credential_digest,stream_window_bytes,state,idempotency_key,request_hash,expires_at,created_at,updated_at,connected_at,closed_at
		) VALUES (
			$1,$2,$3,$4,$5,$1,$6,$7,$8,$9,$10,$16,$17,$11,'open',$12,$13,$14,$15,$15,NULL,NULL
		)`,
		input.Session.ID, tunnel.TenantRef, tunnel.SubjectRef, input.Session.SandboxID, tunnel.ProfileRevisionID, input.LeaseID, input.Session.Generation, input.Session.Name, policy.Port, policy.Protocol, tunnel.StreamWindowBytes, input.IdempotencyKey, input.RequestHash, input.Session.ExpiresAt.UTC(), input.Now.UTC(), tunnel.Session.Transport, input.CredentialDigest,
	); err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession commit: %w", err)
	}
	return tunnel, false, nil
}

// directPortAdmission returns the assignment-bound state the Runner needs to
// admit one direct caller connection locally.
func directPortAdmission(
	input PortSessionAdmission,
	policy contracts.PortPolicy,
) *runnerv1.PortDirectOpen {
	return &runnerv1.PortDirectOpen{
		GuestPort: uint32(policy.Port), Protocol: policy.Protocol,
		PortName:         input.Session.Name,
		DeadlineUnixMs:   uint64(input.Session.ExpiresAt.UTC().UnixMilli()),
		CredentialDigest: bytes.Clone(input.CredentialDigest),
		LeaseId:          input.LeaseID,
	}
}

func portDirectDataPlaneOpenMessage(
	session DataPlaneSession,
	input PortSessionAdmission,
	policy contracts.PortPolicy,
) *runnerv1.ControlPlaneToRunner {
	open := directPortAdmission(input, policy)
	return &runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_DataPlaneDirectOpen{
			DataPlaneDirectOpen: &runnerv1.DataPlaneDirectOpen{
				Fence: &runnerv1.AssignmentFence{
					AssignmentId: session.AssignmentID, SandboxId: session.SandboxID,
					InstanceId: session.InstanceID, SandboxGeneration: uint64(session.Generation),
					FencingToken: bytes.Clone(session.FencingToken),
				},
				OperationId: session.ID, StreamId: session.StreamID,
				Correlation:       dataPlaneCorrelation(session),
				Kind:              runnerv1.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PORT,
				DeadlineUnixMs:    uint64(session.DeadlineAt.UnixMilli()),
				CredentialDigest:  bytes.Clone(input.CredentialDigest),
				StreamWindowBytes: uint64(session.StreamWindowBytes),
				Port:              open,
			},
		},
	}
}

// GetPortTunnel returns the assignment-bound projection so the caller-facing
// endpoint can be rebuilt for whichever transport admitted the session.
func (store *PostgresDataPlaneStore) GetPortTunnel(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sandboxID string,
	sessionID string,
	now time.Time,
) (PortTunnel, error) {
	tunnel, err := scanPortTunnel(store.pool.QueryRow(ctx, portTunnelSelect+`
		WHERE port.tenant_ref=$1 AND port.subject_ref=$2
		  AND port.sandbox_id=$3 AND port.id=$4`,
		tenantRef, subjectRef, sandboxID, sessionID,
	))
	if err != nil {
		return PortTunnel{}, err
	}
	if tunnel.Session.State == contracts.PortSessionStateOpen &&
		!now.UTC().Before(tunnel.Session.ExpiresAt) {
		if err := store.terminatePortSession(
			ctx, tunnel, contracts.PortSessionStateExpired, "completed",
			runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED,
			"port session expired", true, now.UTC(),
		); err != nil {
			return PortTunnel{}, err
		}
		tunnel.Session.State = contracts.PortSessionStateExpired
	}
	return tunnel, nil
}

func (store *PostgresDataPlaneStore) ConsumePortSession(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	now time.Time,
) (PortTunnel, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox Port tunnel consume transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, tenantRef, subjectRef); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox Port tunnel consume quota lock: %w", err)
	}
	tunnel, err := lockPortTunnel(ctx, tx, tenantRef, subjectRef, "", sessionID)
	if err != nil {
		return PortTunnel{}, err
	}
	// A direct session's credential is spent by its home Runner, never by the
	// proxied WebSocket, so the proxied transport refuses it outright.
	if tunnel.Session.Transport != contracts.PortTransportProxied {
		return PortTunnel{}, ports.ErrPortTokenInvalid
	}
	var connectedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT connected_at FROM secondbox.port_sessions WHERE id=$1`, sessionID).Scan(&connectedAt); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox Port tunnel consumption lookup: %w", err)
	}
	if connectedAt != nil {
		return PortTunnel{}, ports.ErrPortTokenConsumed
	}
	if tunnel.Session.State != contracts.PortSessionStateOpen || !now.UTC().Before(tunnel.Session.ExpiresAt) {
		return PortTunnel{}, ports.ErrPortTokenInvalid
	}
	if err := validateLivePortAuthority(ctx, tx, tunnel, now.UTC()); err != nil {
		return PortTunnel{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.port_sessions SET connected_at=$2,updated_at=$2 WHERE id=$1`,
		sessionID, now.UTC(),
	); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox Port tunnel consumption update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.activity_sessions (
			id,tenant_ref,subject_ref,sandbox_id,generation,kind,state,lease_id,last_activity_at,
			created_at,updated_at,closed_at
		) VALUES ($1,$2,$3,$4,$5,'port','active',$6,$7,$7,$7,NULL)`,
		tunnel.Session.ID, tunnel.TenantRef, tunnel.SubjectRef,
		tunnel.Session.SandboxID, tunnel.Session.Generation, tunnel.LeaseID, now.UTC(),
	); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox Port activity insert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions SET state='running',updated_at=$2
		WHERE id=$1 AND state='pending'`, sessionID, now.UTC()); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox Port data-plane consumption update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox Port tunnel consume commit: %w", err)
	}
	return tunnel, nil
}

// ConsumeDirectPortSession spends one single-use credential for the direct
// transport. PostgreSQL remains the single consumption authority: the Runner's
// local checks reduce work, they never replace this write.
func (store *PostgresDataPlaneStore) ConsumeDirectPortSession(
	ctx context.Context,
	input DirectPortConsumption,
) (PortTunnel, error) {
	if input.RunnerID == "" || input.SessionID == "" || input.AssignmentID == "" ||
		input.Generation < 1 || len(input.FencingToken) == 0 ||
		len(input.CredentialDigest) != sha256.Size {
		return PortTunnel{}, errors.New("SecondBox direct PortSession consumption authority is incomplete")
	}
	now := input.Now.UTC()
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox direct Port consume transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockDataPlaneSessionQuota(ctx, tx, input.SessionID); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox direct Port consume quota lock: %w", err)
	}
	tunnel, err := lockDirectPortTunnel(ctx, tx, input.RunnerID, input.SessionID)
	if err != nil {
		return PortTunnel{}, err
	}
	if tunnel.Session.Transport != contracts.PortTransportDirect ||
		tunnel.AssignmentID != input.AssignmentID ||
		tunnel.Session.Generation != input.Generation ||
		!bytes.Equal(tunnel.FencingToken, input.FencingToken) {
		return PortTunnel{}, ports.ErrPortTokenInvalid
	}
	var storedDigest []byte
	var connectedAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT credential_digest,connected_at FROM secondbox.port_sessions WHERE id=$1`,
		input.SessionID,
	).Scan(&storedDigest, &connectedAt); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox direct Port consumption lookup: %w", err)
	}
	if subtle.ConstantTimeCompare(storedDigest, input.CredentialDigest) != 1 {
		return PortTunnel{}, ports.ErrPortTokenInvalid
	}
	if connectedAt != nil {
		return PortTunnel{}, ports.ErrPortTokenConsumed
	}
	if tunnel.Session.State != contracts.PortSessionStateOpen ||
		!now.Before(tunnel.Session.ExpiresAt) {
		return PortTunnel{}, ports.ErrPortTokenInvalid
	}
	if err := validateLivePortAuthority(ctx, tx, tunnel, now); err != nil {
		return PortTunnel{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.port_sessions SET connected_at=$2,updated_at=$2 WHERE id=$1`,
		input.SessionID, now,
	); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox direct Port consumption update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions SET state='running',updated_at=$2 WHERE id=$1`,
		input.SessionID, now,
	); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox direct Port data-plane update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.activity_sessions (
			id,tenant_ref,subject_ref,sandbox_id,generation,kind,state,lease_id,last_activity_at,
			created_at,updated_at,closed_at
		) VALUES ($1,$2,$3,$4,$5,'port','active',$6,$7,$7,$7,NULL)
		ON CONFLICT (id) DO NOTHING`,
		tunnel.Session.ID, tunnel.TenantRef, tunnel.SubjectRef,
		tunnel.Session.SandboxID, tunnel.Session.Generation, tunnel.LeaseID, now,
	); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox direct Port activity insert: %w", err)
	}
	// Data-plane activity is not a change to the Sandbox resource, so it leaves
	// the resource revision alone.
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET last_activity_at=$1,updated_at=$1
		WHERE id=$2 AND generation=$3`,
		now, tunnel.Session.SandboxID, tunnel.Session.Generation,
	); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox direct Port Sandbox activity update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox direct Port consume commit: %w", err)
	}
	return tunnel, nil
}

// lockDirectPortTunnel locks one PortSession by its home Runner rather than by a
// caller's ownership refs, because the authenticated Runner is the requester.
func lockDirectPortTunnel(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	sessionID string,
) (PortTunnel, error) {
	var lockedSessionID string
	err := tx.QueryRow(ctx, `
		SELECT session.id
		FROM secondbox.data_plane_sessions AS session
		JOIN secondbox.port_sessions AS port ON port.data_plane_session_id=session.id
		WHERE port.id=$1 AND session.runner_id=$2
		FOR UPDATE OF session`,
		sessionID, runnerID,
	).Scan(&lockedSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortTunnel{}, ports.ErrPortSessionNotFound
	}
	if err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox direct PortSession data-plane lock: %w", err)
	}
	// The data-plane session is locked; the PortSession is still addressed by its
	// own identifier rather than by the locked session identifier, which happens
	// to equal it today only because admission derives one from the other.
	return scanPortTunnel(tx.QueryRow(ctx, portTunnelSelect+`
		WHERE port.id=$1 AND session.runner_id=$2
		FOR UPDATE OF port`,
		sessionID, runnerID,
	))
}

func (store *PostgresDataPlaneStore) ClosePortSession(
	ctx context.Context,
	input PortTunnelClose,
) (contracts.PortSession, error) {
	if input.TenantRef == "" || input.SubjectRef == "" ||
		input.SandboxID == "" || input.SessionID == "" || input.Reason == "" {
		return contracts.PortSession{}, errors.New("SecondBox PortSession close authority is incomplete")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockDataPlaneSessionQuota(ctx, tx, input.SessionID); errors.Is(err, pgx.ErrNoRows) {
		return contracts.PortSession{}, ports.ErrPortSessionNotFound
	} else if err != nil {
		return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close quota lock: %w", err)
	}
	if input.IdempotencyKey != "" {
		lockKey := input.TenantRef + "\x1f" + input.SubjectRef +
			"\x1fport-session-close\x1f" + input.SessionID + "\x1f" + input.IdempotencyKey
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
			return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close lock: %w", err)
		}
		var priorHash string
		var expiresAt time.Time
		err := tx.QueryRow(ctx, `
			SELECT request_hash,expires_at FROM secondbox.idempotency_records
			WHERE tenant_ref=$1 AND subject_ref=$2
			  AND operation='port_session.close' AND target_id=$3 AND idempotency_key=$4`,
			input.TenantRef, input.SubjectRef, input.SessionID, input.IdempotencyKey,
		).Scan(&priorHash, &expiresAt)
		if err == nil && !expiresAt.After(input.Now.UTC()) {
			if _, deleteErr := tx.Exec(ctx, `
				DELETE FROM secondbox.idempotency_records
				WHERE tenant_ref=$1 AND subject_ref=$2
				  AND operation='port_session.close' AND target_id=$3 AND idempotency_key=$4`,
				input.TenantRef, input.SubjectRef, input.SessionID, input.IdempotencyKey,
			); deleteErr != nil {
				return contracts.PortSession{}, fmt.Errorf("SecondBox expired PortSession close idempotency cleanup: %w", deleteErr)
			}
			err = pgx.ErrNoRows
		}
		if err == nil && priorHash != input.RequestHash {
			return contracts.PortSession{}, ports.ErrIdempotencyConflict
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close replay lookup: %w", err)
		}
	}
	tunnel, err := lockPortTunnel(
		ctx, tx, input.TenantRef, input.SubjectRef, input.SandboxID, input.SessionID,
	)
	if err != nil {
		return contracts.PortSession{}, err
	}
	if tunnel.Session.State == contracts.PortSessionStateOpen {
		var dataPlaneState string
		if err := tx.QueryRow(ctx, `
			SELECT state FROM secondbox.data_plane_sessions WHERE id=$1`, input.SessionID,
		).Scan(&dataPlaneState); err != nil {
			return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close state lookup: %w", err)
		}
		if tunnel.Session.Transport == contracts.PortTransportProxied && dataPlaneState == "pending" {
			// A Runner learns of a proxied session only when its tunnel opens, so
			// an unconsumed session has no Runner state to cancel.
			if _, err := tx.Exec(ctx, `
				UPDATE secondbox.data_plane_sessions SET terminal_kind=$2,terminal_detail=$3 WHERE id=$1`,
				input.SessionID, runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED.String(), input.Reason,
			); err != nil {
				return contracts.PortSession{}, fmt.Errorf("SecondBox unconsumed PortSession close: %w", err)
			}
			if err := completeCancelledSession(ctx, tx, input.SessionID, input.Now.UTC(), store.retention); err != nil {
				return contracts.PortSession{}, err
			}
		} else if err := store.enqueueCancellation(
			ctx, tx, portDataPlaneSession(tunnel),
			runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED.String(),
			input.Reason, input.Now.UTC(),
		); err != nil {
			return contracts.PortSession{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.port_sessions SET state='closed',closed_at=$2,updated_at=$2 WHERE id=$1`,
			input.SessionID, input.Now.UTC(),
		); err != nil {
			return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close update: %w", err)
		}
		if err := closeActivitySession(ctx, tx, input.SessionID, input.Now.UTC()); err != nil {
			return contracts.PortSession{}, err
		}
		tunnel.Session.State = contracts.PortSessionStateClosed
	}
	if input.IdempotencyKey != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.idempotency_records (
				tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,response_resource_id,
				created_at,expires_at
			) VALUES ($1,$2,'port_session.close',$3,$4,$5,$3,$6,$7)
			ON CONFLICT (tenant_ref,subject_ref,operation,target_id,idempotency_key) DO NOTHING`,
			input.TenantRef, input.SubjectRef,
			input.SessionID, input.IdempotencyKey, input.RequestHash,
			input.Now.UTC(), input.Now.UTC().Add(store.retention),
		); err != nil {
			return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close idempotency insert: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close commit: %w", err)
	}
	return tunnel.Session, nil
}

func lockPortAdmissionAuthority(
	ctx context.Context,
	tx pgx.Tx,
	input PortSessionAdmission,
) (PortTunnel, contracts.ProfileRevisionSpec, contracts.PortPolicy, error) {
	var tunnel PortTunnel
	var encodedDataPlaneEndpoint string
	var sandboxState, assignmentState string
	var specJSON []byte
	var profileName string
	err := tx.QueryRow(ctx, `
		SELECT sandbox.tenant_ref,sandbox.subject_ref,
		       sandbox.profile_name,sandbox.profile_revision_id,sandbox.generation,sandbox.state,
		       assignment.id,assignment.instance_id,assignment.runner_id,
		       assignment.fencing_token,assignment.state,revision.spec_json,
		       COALESCE(runner.data_plane_address,'')
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.assignments AS assignment
		  ON assignment.instance_id=sandbox.current_instance_id
		  AND assignment.sandbox_id=sandbox.id
		  AND assignment.generation=sandbox.generation
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		LEFT JOIN secondbox.runners AS runner ON runner.id=assignment.runner_id
		WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2 AND sandbox.id=$3
		FOR UPDATE OF sandbox,assignment`,
		input.TenantRef, input.SubjectRef, input.Session.SandboxID,
	).Scan(
		&tunnel.TenantRef, &tunnel.SubjectRef,
		&profileName, &tunnel.ProfileRevisionID, &tunnel.Session.Generation, &sandboxState,
		&tunnel.AssignmentID, &tunnel.InstanceID, &tunnel.RunnerID,
		&tunnel.FencingToken, &assignmentState, &specJSON, &encodedDataPlaneEndpoint,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, ports.ErrSandboxNotFound
	}
	if err != nil {
		return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, fmt.Errorf("SecondBox Port authority lookup: %w", err)
	}
	if input.ProfileGrants != nil && !slices.Contains(input.ProfileGrants, profileName) {
		return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, ports.ErrAuthorizationDenied
	}
	if input.Session.Transport == contracts.PortTransportDirect {
		endpoint, err := decodeDataPlaneEndpoint(encodedDataPlaneEndpoint)
		if err != nil {
			return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, ports.ErrLifecycleUnavailable
		}
		tunnel.DataPlaneAddress = endpoint.Address
		tunnel.DataPlaneCertificateSPKISHA256 = endpoint.CertificateSPKISHA256
	}
	if tunnel.Session.Generation != input.Session.Generation {
		return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, ports.ErrGenerationFenced
	}
	if sandboxState != contracts.SandboxStateReady || assignmentState != "ready" {
		return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, ports.ErrLifecycleUnavailable
	}
	var leaseGeneration int64
	var leaseAccount, leaseState string
	var leaseExpiry time.Time
	if err := tx.QueryRow(ctx, `
		SELECT generation,subject_ref,state,expires_at FROM secondbox.leases
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3 AND id=$4 FOR UPDATE`,
		input.TenantRef, input.SubjectRef, input.Session.SandboxID, input.LeaseID,
	).Scan(&leaseGeneration, &leaseAccount, &leaseState, &leaseExpiry); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, ports.ErrLeaseNotFound
		}
		return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, fmt.Errorf("SecondBox Port Lease lookup: %w", err)
	}
	// A session is admitted under an active Lease and lives while that Lease
	// is renewed: its own expiry bounds it by the Profile's maximum session
	// duration, not by the Lease's current expiry. The data-plane sweep ends it
	// when the Lease is released, lapses, or is fenced.
	if leaseGeneration != input.Session.Generation || leaseAccount != input.SubjectRef ||
		leaseState != contracts.LeaseStateActive || !input.Now.Before(leaseExpiry) {
		return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, ports.ErrLeaseInactive
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, fmt.Errorf("SecondBox Port Profile decoding: %w", err)
	}
	for _, policy := range spec.Ports {
		if policy.Name == input.Session.Name && (policy.Protocol == "tcp" || policy.Protocol == "http") {
			return tunnel, spec, policy, nil
		}
	}
	return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, ports.ErrPortPolicyDenied
}

func enforcePortSessionCapacity(
	ctx context.Context,
	tx pgx.Tx,
	input PortSessionAdmission,
	_ string,
	policy contracts.PortPolicy,
) error {
	var tenantState string
	var tenantExpiresAt *time.Time
	var tenantActive int64
	var tenantMaximum contracts.PolicyLimit
	if err := tx.QueryRow(ctx, `
		SELECT tenant.state,tenant.expires_at,quota.max_port_sessions,
		       (SELECT count(*) FROM secondbox.port_sessions
		        WHERE tenant_ref=$1 AND state IN ('open','closing') AND expires_at>$2)
		FROM secondbox.tenants AS tenant
		JOIN secondbox.tenant_quotas AS quota ON quota.tenant_ref=tenant.ref
		WHERE tenant.ref=$1
		FOR UPDATE OF tenant,quota`, input.TenantRef, input.Now.UTC(),
	).Scan(&tenantState, &tenantExpiresAt, &tenantMaximum, &tenantActive); err != nil {
		return fmt.Errorf("SecondBox Tenant PortSession quota lookup: %w", err)
	}
	if tenantState == contracts.TenantStateSuspended {
		return ports.ErrTenantSuspended
	}
	if tenantState == contracts.TenantStateExpired ||
		tenantExpiresAt != nil && !tenantExpiresAt.After(input.Now.UTC()) {
		return ports.ErrResourceExpired
	}
	if tenantState != contracts.TenantStateActive {
		return ports.ErrInvalidLifecycleTransition
	}
	var subjectMaximum contracts.PolicyLimit
	if err := tx.QueryRow(ctx, `
		SELECT max_port_sessions FROM secondbox.subject_quotas
		WHERE tenant_ref=$1 AND subject_ref=$2 FOR UPDATE`,
		input.TenantRef, input.SubjectRef,
	).Scan(&subjectMaximum); err != nil {
		return fmt.Errorf("SecondBox subject PortSession quota lookup: %w", err)
	}
	var subjectActive, namedActive int64
	if err := tx.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE tenant_ref=$1 AND subject_ref=$2),
		  count(*) FILTER (WHERE sandbox_id=$3 AND name=$4)
		FROM secondbox.port_sessions
		WHERE state IN ('open','closing') AND expires_at>$5`,
		input.TenantRef, input.SubjectRef,
		input.Session.SandboxID, input.Session.Name, input.Now.UTC(),
	).Scan(&subjectActive, &namedActive); err != nil {
		return fmt.Errorf("SecondBox PortSession usage lookup: %w", err)
	}
	if !tenantMaximum.Allows(tenantActive+1) || !subjectMaximum.Allows(subjectActive+1) || !policy.MaximumSessions.Allows(namedActive+1) {
		return ports.ErrQuotaExceeded
	}
	return nil
}

func validateLivePortAuthority(ctx context.Context, tx pgx.Tx, tunnel PortTunnel, now time.Time) error {
	var sandboxGeneration, leaseGeneration int64
	var sandboxState, assignmentState, leaseState, leaseAccount string
	var fence []byte
	var leaseExpiry time.Time
	var activeRunner bool
	err := tx.QueryRow(ctx, `
		SELECT sandbox.generation,sandbox.state,assignment.state,assignment.fencing_token,
		       lease.generation,lease.state,lease.subject_ref,lease.expires_at,
		       EXISTS (
		         SELECT 1
		         FROM secondbox.runner_connections AS connection
		         WHERE connection.runner_id=assignment.runner_id
		           AND connection.state='active'
		       )
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.assignments AS assignment ON assignment.id=$3
		JOIN secondbox.leases AS lease
		  ON lease.tenant_ref=$1 AND lease.subject_ref=$2
		  AND lease.sandbox_id=sandbox.id AND lease.id=$5
		WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2 AND sandbox.id=$4`,
		tunnel.TenantRef, tunnel.SubjectRef, tunnel.AssignmentID,
		tunnel.Session.SandboxID, tunnel.LeaseID,
	).Scan(
		&sandboxGeneration, &sandboxState, &assignmentState, &fence,
		&leaseGeneration, &leaseState, &leaseAccount, &leaseExpiry, &activeRunner,
	)
	if err != nil || sandboxGeneration != tunnel.Session.Generation ||
		leaseGeneration != tunnel.Session.Generation || sandboxState != contracts.SandboxStateReady ||
		assignmentState != "ready" || !bytes.Equal(fence, tunnel.FencingToken) ||
		leaseState != contracts.LeaseStateActive || leaseAccount != tunnel.SubjectRef ||
		!now.Before(leaseExpiry) || !now.Before(tunnel.Session.ExpiresAt) || !activeRunner {
		return ports.ErrLeaseInactive
	}
	return nil
}

func (store *PostgresDataPlaneStore) terminatePortSession(
	ctx context.Context,
	expected PortTunnel,
	portState string,
	dataPlaneState string,
	terminalKind runnerv1.PortTerminalKind,
	reason string,
	sendCancel bool,
	now time.Time,
) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Port terminal projection transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, expected.TenantRef, expected.SubjectRef); err != nil {
		return fmt.Errorf("SecondBox Port terminal projection quota lock: %w", err)
	}
	tunnel, err := lockPortTunnel(
		ctx, tx, expected.TenantRef, expected.SubjectRef, "", expected.Session.ID,
	)
	if err != nil {
		return err
	}
	if tunnel.Session.State != contracts.PortSessionStateOpen {
		return tx.Commit(ctx)
	}
	if sendCancel {
		if err := store.enqueueCancellation(
			ctx, tx, portDataPlaneSession(tunnel), terminalKind.String(), reason, now.UTC(),
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET state=$2,terminal_kind=$3,terminal_detail=$4,
		    completed_at=COALESCE(completed_at,$5),
		    updated_at=$5 WHERE id=$1`,
		tunnel.Session.ID, dataPlaneState, terminalKind.String(), reason, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Port terminal data-plane update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.port_sessions
		SET state=$2,closed_at=COALESCE(closed_at,$3),updated_at=$3 WHERE id=$1`,
		tunnel.Session.ID, portState, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Port terminal projection update: %w", err)
	}
	if err := closeActivitySession(ctx, tx, tunnel.Session.ID, now.UTC()); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox Port terminal projection commit: %w", err)
	}
	return nil
}

func portDataPlaneSession(tunnel PortTunnel) DataPlaneSession {
	return DataPlaneSession{
		ID: tunnel.Session.ID, StreamID: tunnel.StreamID,
		TenantRef: tunnel.TenantRef, SubjectRef: tunnel.SubjectRef,
		SandboxID: tunnel.Session.SandboxID, ProfileRevisionID: tunnel.ProfileRevisionID,
		AssignmentID: tunnel.AssignmentID, InstanceID: tunnel.InstanceID,
		RunnerID: tunnel.RunnerID, Generation: tunnel.Session.Generation,
		FencingToken: bytes.Clone(tunnel.FencingToken), RequestID: tunnel.RequestID,
		LeaseID: tunnel.LeaseID, Kind: "port", Operation: "port:" + tunnel.Session.Name,
		State: "running", DeadlineAt: tunnel.Session.ExpiresAt,
		StreamWindowBytes: tunnel.StreamWindowBytes,
		CreatedAt:         tunnel.Session.CreatedAt, UpdatedAt: tunnel.Session.CreatedAt,
	}
}

const portTunnelSelect = `
	SELECT
	  port.id,port.sandbox_id,port.generation,port.name,port.protocol,port.transport,port.state,
	  port.created_at,port.expires_at,port.lease_id,
	  port.profile_revision_id,session.assignment_id,session.instance_id,session.runner_id,
	  session.request_id,
	  session.stream_id,session.fencing_token,port.guest_port,port.stream_window_bytes,
	  sandbox.tenant_ref,sandbox.subject_ref,COALESCE(runner.data_plane_address,'')
	FROM secondbox.port_sessions AS port
	JOIN secondbox.data_plane_sessions AS session ON session.id=port.data_plane_session_id
	JOIN secondbox.sandboxes AS sandbox ON sandbox.id=port.sandbox_id
	LEFT JOIN secondbox.runners AS runner ON runner.id=session.runner_id`

// lockPortTunnel follows the data-plane session-then-port lock order.
func lockPortTunnel(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	sandboxID string,
	sessionID string,
) (PortTunnel, error) {
	var lockedSessionID string
	err := tx.QueryRow(ctx, `
		SELECT session.id
		FROM secondbox.data_plane_sessions AS session
		JOIN secondbox.port_sessions AS port ON port.data_plane_session_id=session.id
		WHERE port.tenant_ref=$1 AND port.subject_ref=$2
		  AND ($3='' OR port.sandbox_id=$3) AND port.id=$4
		FOR UPDATE OF session`,
		tenantRef, subjectRef, sandboxID, sessionID,
	).Scan(&lockedSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortTunnel{}, ports.ErrPortSessionNotFound
	}
	if err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox PortSession data-plane lock: %w", err)
	}
	return scanPortTunnel(tx.QueryRow(ctx, portTunnelSelect+`
		WHERE port.tenant_ref=$1 AND port.subject_ref=$2
		  AND ($3='' OR port.sandbox_id=$3) AND port.id=$4
		FOR UPDATE OF port`,
		tenantRef, subjectRef, sandboxID, lockedSessionID,
	))
}

func scanPortTunnel(row dataPlaneRow) (PortTunnel, error) {
	var tunnel PortTunnel
	var encodedDataPlaneEndpoint string
	err := row.Scan(
		&tunnel.Session.ID, &tunnel.Session.SandboxID, &tunnel.Session.Generation,
		&tunnel.Session.Name, &tunnel.Session.Protocol, &tunnel.Session.Transport,
		&tunnel.Session.State,
		&tunnel.Session.CreatedAt, &tunnel.Session.ExpiresAt,
		&tunnel.LeaseID, &tunnel.ProfileRevisionID,
		&tunnel.AssignmentID, &tunnel.InstanceID, &tunnel.RunnerID, &tunnel.RequestID, &tunnel.StreamID,
		&tunnel.FencingToken, &tunnel.GuestPort, &tunnel.StreamWindowBytes,
		&tunnel.TenantRef, &tunnel.SubjectRef, &encodedDataPlaneEndpoint,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortTunnel{}, ports.ErrPortSessionNotFound
	}
	if err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox PortSession lookup: %w", err)
	}
	if tunnel.Session.Transport == contracts.PortTransportDirect {
		endpoint, err := decodeDataPlaneEndpoint(encodedDataPlaneEndpoint)
		if err != nil {
			return PortTunnel{}, ports.ErrLifecycleUnavailable
		}
		tunnel.DataPlaneAddress = endpoint.Address
		tunnel.DataPlaneCertificateSPKISHA256 = endpoint.CertificateSPKISHA256
	}
	return tunnel, nil
}

// RecordPortSessionTerminal projects the terminal outcome of one Port session
// without retaining the authenticated Runner message. Port bytes and credit
// frames are routed in memory by the live data plane and never reach this
// store; their sequence numbers therefore advance past the recorded one, and
// only a sequence at or beyond it is a new terminal.
func (store *PostgresDataPlaneStore) RecordPortSessionTerminal(
	ctx context.Context,
	input RunnerDataPlaneFrame,
	now time.Time,
) (bool, error) {
	now = now.UTC()
	frame := input.Message.GetPort()
	if frame == nil || frame.Fence == nil || frame.OperationId == "" ||
		frame.StreamId == "" || frame.Sequence == 0 {
		return false, errors.New("SecondBox inbound Port frame is incomplete")
	}
	terminal := frame.GetTerminal()
	if terminal == nil {
		return false, errors.New("SecondBox inbound Port frame is not a terminal")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox inbound Port terminal transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockDataPlaneSessionQuota(ctx, tx, frame.OperationId); err != nil {
		return false, fmt.Errorf("SecondBox inbound Port terminal quota lock: %w", err)
	}
	tunnel, err := lockDirectPortTunnel(ctx, tx, input.RunnerID, frame.OperationId)
	if err != nil {
		return false, err
	}
	var sessionState, assignmentState, sandboxState, connectionState string
	var nextSequence, generation int64
	var fencingToken []byte
	if err := tx.QueryRow(ctx, `
		SELECT session.state,session.next_inbound_sequence,
		       assignment.state,sandbox.state,sandbox.generation,assignment.fencing_token,
		       connection.state
		FROM secondbox.data_plane_sessions AS session
		JOIN secondbox.assignments AS assignment ON assignment.id=session.assignment_id
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=assignment.sandbox_id
		JOIN secondbox.runner_connections AS connection
		  ON connection.id=$2 AND connection.runner_id=$3
		WHERE session.id=$1`,
		frame.OperationId, input.ConnectionID, input.RunnerID,
	).Scan(
		&sessionState, &nextSequence, &assignmentState, &sandboxState,
		&generation, &fencingToken, &connectionState,
	); err != nil {
		return false, ErrDataPlaneFence
	}
	session := portDataPlaneSession(tunnel)
	session.State = sessionState
	assignmentAcceptsTerminal := assignmentState == "ready" || assignmentState == "fencing"
	sandboxAcceptsTerminal := sandboxState == contracts.SandboxStateReady ||
		sandboxState == contracts.SandboxStateDraining ||
		sandboxState == contracts.SandboxStateStopping
	if frame.StreamId != tunnel.StreamID || !assignmentAcceptsTerminal ||
		!sandboxAcceptsTerminal || connectionState != "active" || generation != session.Generation ||
		frame.Fence.AssignmentId != session.AssignmentID ||
		frame.Fence.SandboxId != session.SandboxID || frame.Fence.InstanceId != session.InstanceID ||
		int64(frame.Fence.SandboxGeneration) != session.Generation ||
		!bytes.Equal(frame.Fence.FencingToken, session.FencingToken) ||
		!bytes.Equal(fencingToken, session.FencingToken) {
		return false, ErrDataPlaneFence
	}
	if !proto.Equal(frame.Correlation, dataPlaneCorrelation(session)) {
		return false, ErrDataPlaneFence
	}
	sequence := int64(frame.Sequence)
	if sequence < nextSequence ||
		(session.State != "pending" && session.State != "running" && session.State != "cancelling") {
		// A replayed terminal, or one that follows a session the control plane
		// already completed, changes nothing.
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("SecondBox inbound Port duplicate commit: %w", err)
		}
		return false, nil
	}
	var transport string
	if err := tx.QueryRow(ctx, `
		SELECT transport FROM secondbox.port_sessions WHERE id=$1`, session.ID,
	).Scan(&transport); err != nil {
		return false, fmt.Errorf("SecondBox inbound Port transport lookup: %w", err)
	}
	state, portState := "completed", contracts.PortSessionStateClosed
	if terminal.Kind == runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_FENCED {
		state, portState = "failed", contracts.PortSessionStateFenced
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET state=$2,next_inbound_sequence=$3,
		    terminal_kind=$4,terminal_detail=$5,
		    updated_at=$6,completed_at=COALESCE(completed_at,$6),retain_until=$7
		WHERE id=$1`,
		session.ID, state, sequence+1, terminal.Kind.String(), terminal.SafeDetail,
		now, now.Add(store.retention),
	); err != nil {
		return false, fmt.Errorf("SecondBox inbound Port terminal update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.port_sessions
		SET state=CASE WHEN state IN ('open','closing') THEN $2 ELSE state END,
		    closed_at=COALESCE(closed_at,$3),updated_at=$3
		WHERE id=$1`,
		session.ID, portState, now,
	); err != nil {
		return false, fmt.Errorf("SecondBox inbound Port terminal projection update: %w", err)
	}
	if err := closeActivitySession(ctx, tx, session.ID, now); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox inbound Port terminal commit: %w", err)
	}
	return transport == contracts.PortTransportProxied, nil
}

var _ PortSessionStore = (*PostgresDataPlaneStore)(nil)
var _ PortSessionTerminalRecorder = (*PostgresDataPlaneStore)(nil)

// closeActivitySession closes one active useful-activity session and stamps
// its Sandbox's last activity at the close. Streams write nothing while they
// run, because an active session already holds the Sandbox out of idle, so the
// close is the instant idle time is measured from.
func closeActivitySession(ctx context.Context, tx pgx.Tx, sessionID string, now time.Time) error {
	var sandboxID string
	var generation int64
	err := tx.QueryRow(ctx, `
		UPDATE secondbox.activity_sessions
		SET state='closed',closed_at=$2,last_activity_at=$2,updated_at=$2
		WHERE id=$1 AND state='active'
		RETURNING sandbox_id,generation`,
		sessionID, now.UTC(),
	).Scan(&sandboxID, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("SecondBox activity session close: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET last_activity_at=$3,updated_at=$3
		WHERE id=$1 AND generation=$2`,
		sandboxID, generation, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox activity session Sandbox stamp: %w", err)
	}
	return nil
}
