package runnercontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func (relay *PostgresFrameRelay) AdmitPortSession(
	ctx context.Context,
	input PortSessionAdmission,
) (PortTunnel, bool, error) {
	if input.Session.ID == "" || input.StreamID == "" || input.ProjectID == "" ||
		input.Session.SandboxID == "" || input.ServiceAccountID == "" ||
		input.RequestID == "" || input.LeaseID == "" || input.IdempotencyKey == "" ||
		input.RequestHash == "" || input.Session.Generation < 1 ||
		input.Session.Name == "" || input.Session.ExpiresAt.IsZero() ||
		!input.Now.Before(input.Session.ExpiresAt) {
		return PortTunnel{}, false, errors.New("SecondBox PortSession admission is incomplete")
	}
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.ProjectID + "\x1fport-session\x1f" + input.Session.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession idempotency lock: %w", err)
	}
	var replayHash, replayID string
	err = tx.QueryRow(ctx, `
		SELECT request_hash,id FROM secondbox.port_sessions
		WHERE project_id=$1 AND sandbox_id=$2 AND idempotency_key=$3`,
		input.ProjectID, input.Session.SandboxID, input.IdempotencyKey,
	).Scan(&replayHash, &replayID)
	if err == nil {
		if replayHash != input.RequestHash {
			return PortTunnel{}, false, ports.ErrIdempotencyConflict
		}
		tunnel, err := scanPortTunnel(tx.QueryRow(ctx, portTunnelSelect+`
			WHERE port.project_id=$1 AND port.id=$2`, input.ProjectID, replayID))
		if err != nil {
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
	for _, capacityKey := range []string{
		input.ProjectID + "\x1fport-session-capacity",
		input.ProjectID + "\x1fport-session-capacity\x1f" + input.Session.SandboxID,
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
	tunnel.ProjectID, tunnel.ServiceAccountID = input.ProjectID, input.ServiceAccountID
	tunnel.RequestID, tunnel.LeaseID, tunnel.StreamID = input.RequestID, input.LeaseID, input.StreamID
	tunnel.GuestPort, tunnel.StreamWindowBytes = policy.Port, spec.Execution.StreamWindowBytes
	if input.Session.ExpiresAt.After(input.Now.Add(time.Duration(policy.MaximumSessionSeconds) * time.Second)) {
		return PortTunnel{}, false, ports.ErrPortPolicyDenied
	}
	if err := enforcePortSessionCapacity(ctx, tx, input, tunnel.ProfileRevisionID, policy); err != nil {
		return PortTunnel{}, false, err
	}
	maximumPayloadBytes := min(spec.Execution.MaximumTransferBytes, relay.maximumSessionBytes)
	if maximumPayloadBytes < 1 || tunnel.StreamWindowBytes < 1 ||
		tunnel.StreamWindowBytes > maximumPayloadBytes {
		return PortTunnel{}, false, ports.ErrQuotaExceeded
	}
	session := DataPlaneSession{
		ID: input.Session.ID, StreamID: input.StreamID,
		ProjectID: input.ProjectID, SandboxID: input.Session.SandboxID,
		ProfileRevisionID: tunnel.ProfileRevisionID, AssignmentID: tunnel.AssignmentID,
		InstanceID: tunnel.InstanceID, RunnerID: tunnel.RunnerID,
		Generation: input.Session.Generation, FencingToken: bytes.Clone(tunnel.FencingToken),
		RequestID: input.RequestID, LeaseID: input.LeaseID,
		Kind: "port", Operation: "port:" + input.Session.Name, State: "pending",
		DeadlineAt: input.Session.ExpiresAt, MaximumResponseBytes: maximumPayloadBytes,
		MaximumRequestBytes: maximumPayloadBytes, StreamWindowBytes: tunnel.StreamWindowBytes,
		CreatedAt: input.Now.UTC(), UpdatedAt: input.Now.UTC(),
	}
	openMessage := portRelayMessage(session, 1, &runnerv1.PortFrame_Open{Open: &runnerv1.PortOpen{
		GuestPort: uint32(policy.Port), Protocol: policy.Protocol,
		IdleTimeoutMs: uint64(input.Session.ExpiresAt.Sub(input.Now).Milliseconds()),
	}})
	openPayload, err := proto.MarshalOptions{Deterministic: true}.Marshal(openMessage)
	if err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox Port Open encoding: %w", err)
	}
	if int64(len(openPayload)) > relay.maximumFrameBytes {
		return PortTunnel{}, false, ErrRelayFrameLimit
	}
	openHash := sha256.Sum256(openPayload)
	requestJSON, err := json.Marshal(struct {
		Name            string `json:"name"`
		DurationSeconds int64  `json:"durationSeconds"`
	}{input.Session.Name, int64(input.Session.ExpiresAt.Sub(input.Now).Seconds())})
	if err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession request encoding: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.data_plane_sessions (
			id,project_id,sandbox_id,profile_revision_id,assignment_id,instance_id,
			runner_id,generation,fencing_token,service_account_id,request_id,lease_id,kind,operation,
			stream_id,state,priority,idempotency_key,request_hash,deadline_at,
			maximum_response_bytes,maximum_request_bytes,stream_window_bytes,response_credit_bytes,
			request_stream_bytes,request_stream_closed,detachable,terminal_detach_seconds,
			attachment_id,attached_at,detached_at,detach_expires_at,
			outbound_bytes,inbound_bytes,next_inbound_sequence,
			terminal_kind,terminal_detail,exit_code,signal,spawn_failure_reason,
			elapsed_milliseconds,limit_bytes,infrastructure_failure_reason,retryable,terminal_message,
			stdout_bytes,stderr_bytes,content_bytes,metadata_json,request_json,
			created_at,updated_at,completed_at,retain_until
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'port',$13,$14,'pending',0,$15,$16,$17,
			$18,$18,$19,0,0,false,false,0,'',NULL,NULL,NULL,$20,0,1,'','',0,0,'',0,0,'',false,'',
			$21,$21,$21,'{}',$22,$23,$23,NULL,$24
		)`,
		session.ID, session.ProjectID, session.SandboxID, session.ProfileRevisionID,
		session.AssignmentID, session.InstanceID, session.RunnerID, session.Generation,
		session.FencingToken, input.ServiceAccountID, input.RequestID, input.LeaseID,
		session.Operation, session.StreamID, input.IdempotencyKey, input.RequestHash,
		session.DeadlineAt, maximumPayloadBytes, tunnel.StreamWindowBytes,
		len(openPayload), []byte{}, requestJSON, input.Now.UTC(), input.Now.UTC().Add(relay.retention),
	); err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox Port data-plane insert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
			priority,state,claim_owner,claim_expires_at,delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,'outbound',1,$3,$4,$5,0,'pending','',NULL,0,$6,$6,NULL)`,
		session.ID+"_port_open", session.ID, hex.EncodeToString(openHash[:]),
		openPayload, len(openPayload), input.Now.UTC(),
	); err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox Port Open insert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.port_sessions (
			id,project_id,sandbox_id,profile_revision_id,data_plane_session_id,
			service_account_id,lease_id,generation,name,guest_port,protocol,stream_window_bytes,
			client_credit_bytes,client_bytes,runner_bytes,state,idempotency_key,request_hash,
			expires_at,created_at,updated_at,connected_at,closed_at
		) VALUES ($1,$2,$3,$4,$1,$5,$6,$7,$8,$9,$10,$11,0,0,0,'open',$12,$13,$14,$15,$15,NULL,NULL)`,
		input.Session.ID, input.ProjectID, input.Session.SandboxID, tunnel.ProfileRevisionID,
		input.ServiceAccountID, input.LeaseID, input.Session.Generation, input.Session.Name,
		policy.Port, policy.Protocol, tunnel.StreamWindowBytes, input.IdempotencyKey,
		input.RequestHash, input.Session.ExpiresAt.UTC(), input.Now.UTC(),
	); err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PortTunnel{}, false, fmt.Errorf("SecondBox PortSession commit: %w", err)
	}
	return tunnel, false, nil
}

func (relay *PostgresFrameRelay) GetPortSession(
	ctx context.Context,
	projectID string,
	sandboxID string,
	sessionID string,
	now time.Time,
) (contracts.PortSession, error) {
	tunnel, err := scanPortTunnel(relay.pool.QueryRow(ctx, portTunnelSelect+`
		WHERE port.project_id=$1 AND port.sandbox_id=$2 AND port.id=$3`,
		projectID, sandboxID, sessionID,
	))
	if err != nil {
		return contracts.PortSession{}, err
	}
	if tunnel.Session.State == contracts.PortSessionStateOpen &&
		!now.UTC().Before(tunnel.Session.ExpiresAt) {
		if err := relay.terminatePortSession(
			ctx, tunnel, contracts.PortSessionStateExpired, "completed",
			runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED,
			"port session expired", true, now.UTC(),
		); err != nil {
			return contracts.PortSession{}, err
		}
		tunnel.Session.State = contracts.PortSessionStateExpired
	}
	return tunnel.Session, err
}

func (relay *PostgresFrameRelay) ConsumePortSession(
	ctx context.Context,
	projectID string,
	sessionID string,
	now time.Time,
) (PortTunnel, error) {
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox Port tunnel consume transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	tunnel, err := lockPortTunnel(ctx, tx, projectID, "", sessionID)
	if err != nil {
		return PortTunnel{}, err
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
			id,project_id,sandbox_id,generation,kind,state,lease_id,last_activity_at,
			created_at,updated_at,closed_at
		) VALUES ($1,$2,$3,$4,'port','active',$5,$6,$6,$6,NULL)`,
		tunnel.Session.ID, tunnel.ProjectID, tunnel.Session.SandboxID,
		tunnel.Session.Generation, tunnel.LeaseID, now.UTC(),
	); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox Port activity insert: %w", err)
	}
	if err := relay.enqueuePortFrame(ctx, tx, tunnel, &runnerv1.PortFrame_Credit{
		Credit: &runnerv1.StreamCredit{ByteCount: uint64(tunnel.StreamWindowBytes)},
	}, 0, now.UTC()); err != nil {
		return PortTunnel{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox Port tunnel consume commit: %w", err)
	}
	return tunnel, nil
}

func (relay *PostgresFrameRelay) ClosePortSession(
	ctx context.Context,
	input PortTunnelClose,
) (contracts.PortSession, error) {
	if input.ProjectID == "" || input.SandboxID == "" || input.SessionID == "" || input.Reason == "" {
		return contracts.PortSession{}, errors.New("SecondBox PortSession close authority is incomplete")
	}
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if input.IdempotencyKey != "" {
		lockKey := input.ProjectID + "\x1fport-session-close\x1f" + input.SessionID + "\x1f" + input.IdempotencyKey
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
			return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close lock: %w", err)
		}
		var priorHash string
		err := tx.QueryRow(ctx, `
			SELECT request_hash FROM secondbox.idempotency_records
			WHERE project_id=$1 AND operation='port_session.close' AND target_id=$2 AND idempotency_key=$3`,
			input.ProjectID, input.SessionID, input.IdempotencyKey,
		).Scan(&priorHash)
		if err == nil && priorHash != input.RequestHash {
			return contracts.PortSession{}, ports.ErrIdempotencyConflict
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close replay lookup: %w", err)
		}
	}
	tunnel, err := lockPortTunnel(
		ctx, tx, input.ProjectID, input.SandboxID, input.SessionID,
	)
	if err != nil {
		return contracts.PortSession{}, err
	}
	if tunnel.Session.State == contracts.PortSessionStateOpen {
		if err := relay.enqueuePortFrame(ctx, tx, tunnel, &runnerv1.PortFrame_Cancel{
			Cancel: &runnerv1.ExecCancel{Reason: input.Reason},
		}, -100, input.Now.UTC()); err != nil {
			return contracts.PortSession{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.port_sessions SET state='closed',closed_at=$2,updated_at=$2 WHERE id=$1`,
			input.SessionID, input.Now.UTC(),
		); err != nil {
			return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close update: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.data_plane_sessions
			SET state='cancelling',terminal_kind=$3,terminal_detail=$4,updated_at=$2 WHERE id=$1`,
			input.SessionID, input.Now.UTC(),
			runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED.String(), input.Reason,
		); err != nil {
			return contracts.PortSession{}, fmt.Errorf("SecondBox Port data-plane close update: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.activity_sessions
			SET state='closed',closed_at=$2,updated_at=$2 WHERE id=$1 AND state='active'`,
			input.SessionID, input.Now.UTC(),
		); err != nil {
			return contracts.PortSession{}, fmt.Errorf("SecondBox Port activity close update: %w", err)
		}
		tunnel.Session.State = contracts.PortSessionStateClosed
	}
	if input.IdempotencyKey != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.idempotency_records (
				project_id,operation,target_id,idempotency_key,request_hash,response_resource_id,
				created_at,expires_at
			) VALUES ($1,'port_session.close',$2,$3,$4,$2,$5,$6)
			ON CONFLICT (project_id,operation,target_id,idempotency_key) DO NOTHING`,
			input.ProjectID, input.SessionID, input.IdempotencyKey, input.RequestHash,
			input.Now.UTC(), input.Now.UTC().Add(24*time.Hour),
		); err != nil {
			return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close idempotency insert: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.PortSession{}, fmt.Errorf("SecondBox PortSession close commit: %w", err)
	}
	return tunnel.Session, nil
}

func (relay *PostgresFrameRelay) QueuePortClientBytes(
	ctx context.Context,
	projectID string,
	sessionID string,
	data []byte,
	now time.Time,
) error {
	if len(data) == 0 || int64(len(data)) > relay.maximumFrameBytes/2 {
		return ErrRelayFrameLimit
	}
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Port client-byte transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	tunnel, err := lockPortTunnel(ctx, tx, projectID, "", sessionID)
	if err != nil {
		return err
	}
	if err := validateLivePortAuthority(ctx, tx, tunnel, now.UTC()); err != nil {
		return err
	}
	var credit, sent, maximum int64
	if err := tx.QueryRow(ctx, `
		SELECT port.client_credit_bytes,port.client_bytes,session.maximum_request_bytes
		FROM secondbox.port_sessions AS port
		JOIN secondbox.data_plane_sessions AS session ON session.id=port.data_plane_session_id
		WHERE port.id=$1`,
		sessionID,
	).Scan(&credit, &sent, &maximum); err != nil {
		return fmt.Errorf("SecondBox Port client credit lookup: %w", err)
	}
	if int64(len(data)) > credit {
		return ports.ErrPortBackpressure
	}
	if sent+int64(len(data)) > maximum {
		return ErrRelaySessionLimit
	}
	if err := relay.enqueuePortFrame(ctx, tx, tunnel, &runnerv1.PortFrame_Bytes{
		Bytes: &runnerv1.PortBytes{Data: bytes.Clone(data)},
	}, 0, now.UTC()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.port_sessions
		SET client_credit_bytes=client_credit_bytes-$2,client_bytes=client_bytes+$2,
		    updated_at=$3 WHERE id=$1`,
		sessionID, len(data), now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Port client credit update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.activity_sessions
		SET last_activity_at=$2,updated_at=$2 WHERE id=$1 AND state='active'`,
		sessionID, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Port client activity update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes
		SET last_activity_at=$1,revision=revision+1,updated_at=$1
		WHERE id=$2 AND generation=$3`,
		now.UTC(), tunnel.Session.SandboxID, tunnel.Session.Generation,
	); err != nil {
		return fmt.Errorf("SecondBox Port client Sandbox activity update: %w", err)
	}
	return tx.Commit(ctx)
}

func (relay *PostgresFrameRelay) NextPortTunnelEvent(
	ctx context.Context,
	projectID string,
	sessionID string,
	afterSequence int64,
	now time.Time,
) (PortTunnelEvent, bool, error) {
	if afterSequence < -1 {
		return PortTunnelEvent{}, false, errors.New("SecondBox Port event sequence is invalid")
	}
	tunnel, err := scanPortTunnel(relay.pool.QueryRow(ctx, portTunnelSelect+`
		WHERE port.project_id=$1 AND port.id=$2`, projectID, sessionID,
	))
	if err != nil {
		return PortTunnelEvent{}, false, err
	}
	if !now.UTC().Before(tunnel.Session.ExpiresAt) {
		if err := relay.terminatePortSession(
			ctx, tunnel, contracts.PortSessionStateExpired, "completed",
			runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_CANCELLED,
			"port session expired", true, now.UTC(),
		); err != nil {
			return PortTunnelEvent{}, false, err
		}
		return PortTunnelEvent{}, false, ports.ErrPortTokenInvalid
	}
	for {
		var sequence int64
		var payload []byte
		err = relay.pool.QueryRow(ctx, `
			SELECT frame.sequence,frame.payload
			FROM secondbox.data_plane_frames AS frame
			WHERE frame.session_id=$1 AND frame.direction='inbound' AND frame.sequence>$2
			  AND frame.consumed_at IS NULL
			ORDER BY frame.sequence LIMIT 1`,
			sessionID, afterSequence,
		).Scan(&sequence, &payload)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := relay.ensurePortRunnerConnected(ctx, tunnel.RunnerID); err != nil {
				if terminateErr := relay.terminatePortSession(
					ctx, tunnel, contracts.PortSessionStateFenced, "failed",
					runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_FENCED,
					"runner disconnected", false, now.UTC(),
				); terminateErr != nil {
					return PortTunnelEvent{}, false, terminateErr
				}
				return PortTunnelEvent{}, false, err
			}
			return PortTunnelEvent{}, false, nil
		}
		if err != nil {
			return PortTunnelEvent{}, false, fmt.Errorf("SecondBox Port event lookup: %w", err)
		}
		message := &runnerv1.RunnerToControlPlane{}
		if err := proto.Unmarshal(payload, message); err != nil {
			return PortTunnelEvent{}, false, fmt.Errorf("SecondBox Port event decoding: %w", err)
		}
		frame := message.GetPort()
		if frame == nil {
			return PortTunnelEvent{}, false, ErrRelaySequence
		}
		event := PortTunnelEvent{Sequence: sequence}
		if value := frame.GetBytes(); value != nil {
			event.Bytes = bytes.Clone(value.Data)
			return event, true, nil
		}
		if terminal := frame.GetTerminal(); terminal != nil {
			event.TerminalKind, event.TerminalDetail = terminal.Kind.String(), terminal.SafeDetail
			return event, true, nil
		}
		if err := relay.AcknowledgePortTunnelEvent(
			ctx, projectID, sessionID, sequence, now.UTC(),
		); err != nil {
			return PortTunnelEvent{}, false, err
		}
		afterSequence = sequence
	}
}

func (relay *PostgresFrameRelay) AcknowledgePortTunnelEvent(
	ctx context.Context,
	projectID string,
	sessionID string,
	sequence int64,
	now time.Time,
) error {
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Port event acknowledgement transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	tunnel, err := lockPortTunnel(ctx, tx, projectID, "", sessionID)
	if err != nil {
		return err
	}
	var payload []byte
	var consumedAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT payload,consumed_at FROM secondbox.data_plane_frames
		WHERE session_id=$1 AND direction='inbound' AND sequence=$2 FOR UPDATE`,
		sessionID, sequence,
	).Scan(&payload, &consumedAt); err != nil {
		return fmt.Errorf("SecondBox Port event acknowledgement lookup: %w", err)
	}
	if consumedAt != nil {
		return tx.Commit(ctx)
	}
	message := &runnerv1.RunnerToControlPlane{}
	if err := proto.Unmarshal(payload, message); err != nil || message.GetPort() == nil {
		return ErrRelaySequence
	}
	if frameBytes := message.GetPort().GetBytes(); frameBytes != nil && len(frameBytes.Data) > 0 {
		if err := relay.enqueuePortFrame(ctx, tx, tunnel, &runnerv1.PortFrame_Credit{
			Credit: &runnerv1.StreamCredit{ByteCount: uint64(len(frameBytes.Data))},
		}, 0, now.UTC()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_frames SET consumed_at=$3,updated_at=$3
		WHERE session_id=$1 AND direction='inbound' AND sequence=$2`,
		sessionID, sequence, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Port event acknowledgement update: %w", err)
	}
	return tx.Commit(ctx)
}

func lockPortAdmissionAuthority(
	ctx context.Context,
	tx pgx.Tx,
	input PortSessionAdmission,
) (PortTunnel, contracts.ProfileRevisionSpec, contracts.PortPolicy, error) {
	var tunnel PortTunnel
	var sandboxState, assignmentState string
	var specJSON []byte
	err := tx.QueryRow(ctx, `
		SELECT sandbox.profile_revision_id,sandbox.generation,sandbox.state,
		       assignment.id,assignment.instance_id,assignment.runner_id,
		       assignment.fencing_token,assignment.state,revision.spec_json
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.assignments AS assignment
		  ON assignment.instance_id=sandbox.current_instance_id
		  AND assignment.sandbox_id=sandbox.id
		  AND assignment.generation=sandbox.generation
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		WHERE sandbox.project_id=$1 AND sandbox.id=$2
		FOR UPDATE OF sandbox,assignment`,
		input.ProjectID, input.Session.SandboxID,
	).Scan(
		&tunnel.ProfileRevisionID, &tunnel.Session.Generation, &sandboxState,
		&tunnel.AssignmentID, &tunnel.InstanceID, &tunnel.RunnerID,
		&tunnel.FencingToken, &assignmentState, &specJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, ports.ErrSandboxNotFound
	}
	if err != nil {
		return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, fmt.Errorf("SecondBox Port authority lookup: %w", err)
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
		SELECT generation,service_account_id,state,expires_at FROM secondbox.leases
		WHERE project_id=$1 AND sandbox_id=$2 AND id=$3 FOR UPDATE`,
		input.ProjectID, input.Session.SandboxID, input.LeaseID,
	).Scan(&leaseGeneration, &leaseAccount, &leaseState, &leaseExpiry); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, ports.ErrLeaseNotFound
		}
		return PortTunnel{}, contracts.ProfileRevisionSpec{}, contracts.PortPolicy{}, fmt.Errorf("SecondBox Port Lease lookup: %w", err)
	}
	if leaseGeneration != input.Session.Generation || leaseAccount != input.ServiceAccountID ||
		leaseState != contracts.LeaseStateActive || !input.Now.Before(leaseExpiry) ||
		input.Session.ExpiresAt.After(leaseExpiry) {
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
	profileRevisionID string,
	policy contracts.PortPolicy,
) error {
	var projectMaximum, profileMaximum int64
	if err := tx.QueryRow(ctx, `
		SELECT max_port_sessions FROM secondbox.project_quotas WHERE project_id=$1`,
		input.ProjectID,
	).Scan(&projectMaximum); err != nil {
		return fmt.Errorf("SecondBox Project PortSession quota lookup: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT quota.max_port_sessions
		FROM secondbox.profile_quotas AS quota
		JOIN secondbox.profile_revisions AS revision ON revision.profile_name=quota.profile_name
		WHERE revision.id=$1`,
		profileRevisionID,
	).Scan(&profileMaximum); err != nil {
		return fmt.Errorf("SecondBox Profile PortSession quota lookup: %w", err)
	}
	var projectActive, profileActive, namedActive int64
	if err := tx.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE project_id=$1),
		  count(*) FILTER (WHERE profile_revision_id=$2),
		  count(*) FILTER (WHERE sandbox_id=$3 AND name=$4)
		FROM secondbox.port_sessions
		WHERE state IN ('open','closing') AND expires_at>$5`,
		input.ProjectID, profileRevisionID, input.Session.SandboxID, input.Session.Name, input.Now.UTC(),
	).Scan(&projectActive, &profileActive, &namedActive); err != nil {
		return fmt.Errorf("SecondBox PortSession usage lookup: %w", err)
	}
	if projectActive >= projectMaximum || profileActive >= profileMaximum ||
		namedActive >= policy.MaximumSessions {
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
		       lease.generation,lease.state,lease.service_account_id,lease.expires_at,
		       EXISTS (
		         SELECT 1
		         FROM secondbox.runner_connections AS connection
		         JOIN secondbox.runner_credentials AS credential
		           ON credential.serial_number=connection.credential_serial
		         WHERE connection.runner_id=assignment.runner_id
		           AND connection.state='active'
		           AND credential.state IN ('active','retiring')
		       )
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.assignments AS assignment ON assignment.id=$3
		JOIN secondbox.leases AS lease
		  ON lease.project_id=$1 AND lease.sandbox_id=sandbox.id AND lease.id=$4
		WHERE sandbox.project_id=$1 AND sandbox.id=$2`,
		tunnel.ProjectID, tunnel.Session.SandboxID, tunnel.AssignmentID, tunnel.LeaseID,
	).Scan(
		&sandboxGeneration, &sandboxState, &assignmentState, &fence,
		&leaseGeneration, &leaseState, &leaseAccount, &leaseExpiry, &activeRunner,
	)
	if err != nil || sandboxGeneration != tunnel.Session.Generation ||
		leaseGeneration != tunnel.Session.Generation || sandboxState != contracts.SandboxStateReady ||
		assignmentState != "ready" || !bytes.Equal(fence, tunnel.FencingToken) ||
		leaseState != contracts.LeaseStateActive || leaseAccount != tunnel.ServiceAccountID ||
		!now.Before(leaseExpiry) || !now.Before(tunnel.Session.ExpiresAt) || !activeRunner {
		return ports.ErrLeaseInactive
	}
	return nil
}

func (relay *PostgresFrameRelay) ensurePortRunnerConnected(ctx context.Context, runnerID string) error {
	var active bool
	if err := relay.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1
		  FROM secondbox.runner_connections AS connection
		  JOIN secondbox.runner_credentials AS credential
		    ON credential.serial_number=connection.credential_serial
		  WHERE connection.runner_id=$1
		    AND connection.state='active'
		    AND credential.state IN ('active','retiring')
		)`, runnerID,
	).Scan(&active); err != nil {
		return fmt.Errorf("SecondBox Port runner connection lookup: %w", err)
	}
	if !active {
		return ports.ErrLifecycleUnavailable
	}
	return nil
}

func (relay *PostgresFrameRelay) terminatePortSession(
	ctx context.Context,
	expected PortTunnel,
	portState string,
	dataPlaneState string,
	terminalKind runnerv1.PortTerminalKind,
	reason string,
	sendCancel bool,
	now time.Time,
) error {
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox Port terminal projection transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	tunnel, err := lockPortTunnel(
		ctx, tx, expected.ProjectID, "", expected.Session.ID,
	)
	if err != nil {
		return err
	}
	if tunnel.Session.State != contracts.PortSessionStateOpen {
		return tx.Commit(ctx)
	}
	if sendCancel {
		if err := relay.enqueuePortFrame(ctx, tx, tunnel, &runnerv1.PortFrame_Cancel{
			Cancel: &runnerv1.ExecCancel{Reason: reason},
		}, -100, now.UTC()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.port_sessions
		SET state=$2,closed_at=COALESCE(closed_at,$3),updated_at=$3 WHERE id=$1`,
		tunnel.Session.ID, portState, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Port terminal projection update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET state=$2,terminal_kind=$3,terminal_detail=$4,
		    completed_at=COALESCE(completed_at,$5),updated_at=$5 WHERE id=$1`,
		tunnel.Session.ID, dataPlaneState, terminalKind.String(), reason, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Port terminal data-plane update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.activity_sessions
		SET state='closed',closed_at=$2,updated_at=$2 WHERE id=$1 AND state='active'`,
		tunnel.Session.ID, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Port terminal activity close: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox Port terminal projection commit: %w", err)
	}
	return nil
}

func (relay *PostgresFrameRelay) enqueuePortFrame(
	ctx context.Context,
	tx pgx.Tx,
	tunnel PortTunnel,
	payload any,
	priority int64,
	now time.Time,
) error {
	var sequence int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(max(sequence),0)+1 FROM secondbox.data_plane_frames
		WHERE session_id=$1 AND direction='outbound'`,
		tunnel.Session.ID,
	).Scan(&sequence); err != nil {
		return fmt.Errorf("SecondBox Port outbound sequence lookup: %w", err)
	}
	session := DataPlaneSession{
		ID: tunnel.Session.ID, StreamID: tunnel.StreamID, SandboxID: tunnel.Session.SandboxID,
		AssignmentID: tunnel.AssignmentID, InstanceID: tunnel.InstanceID,
		RunnerID: tunnel.RunnerID, Generation: tunnel.Session.Generation,
		FencingToken: tunnel.FencingToken, RequestID: tunnel.RequestID, LeaseID: tunnel.LeaseID,
	}
	message := portRelayMessage(session, uint64(sequence), payload)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return fmt.Errorf("SecondBox Port frame encoding: %w", err)
	}
	if int64(len(encoded)) > relay.maximumFrameBytes {
		return ErrRelayFrameLimit
	}
	hash := sha256.Sum256(encoded)
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
			priority,state,claim_owner,claim_expires_at,delivery_count,created_at,updated_at,delivered_at
		) VALUES ($1,$2,'outbound',$3,$4,$5,$6,$7,'pending','',NULL,0,$8,$8,NULL)`,
		fmt.Sprintf("%s_port_out_%d", tunnel.Session.ID, sequence), tunnel.Session.ID,
		sequence, hex.EncodeToString(hash[:]), encoded, len(encoded), priority, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox Port frame insert: %w", err)
	}
	return nil
}

func portRelayMessage(
	session DataPlaneSession,
	sequence uint64,
	payload any,
) *runnerv1.ControlPlaneToRunner {
	frame := &runnerv1.PortFrame{
		Fence: &runnerv1.AssignmentFence{
			AssignmentId: session.AssignmentID, SandboxId: session.SandboxID,
			InstanceId: session.InstanceID, SandboxGeneration: uint64(session.Generation),
			FencingToken: bytes.Clone(session.FencingToken),
		},
		OperationId: session.ID, StreamId: session.StreamID, Sequence: sequence,
		Correlation: dataPlaneCorrelation(session),
	}
	switch value := payload.(type) {
	case *runnerv1.PortFrame_Open:
		frame.Payload = value
	case *runnerv1.PortFrame_Bytes:
		frame.Payload = value
	case *runnerv1.PortFrame_Credit:
		frame.Payload = value
	case *runnerv1.PortFrame_Cancel:
		frame.Payload = value
	}
	return &runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Port{Port: frame},
	}
}

const portTunnelSelect = `
	SELECT
	  port.id,port.sandbox_id,port.generation,port.name,port.protocol,port.state,
	  port.created_at,port.expires_at,port.project_id,port.service_account_id,port.lease_id,
	  port.profile_revision_id,session.assignment_id,session.instance_id,session.runner_id,
	  session.request_id,
	  session.stream_id,session.fencing_token,port.guest_port,port.stream_window_bytes
	FROM secondbox.port_sessions AS port
	JOIN secondbox.data_plane_sessions AS session ON session.id=port.data_plane_session_id`

// lockPortTunnel follows the inbound relay's session-then-port lock order.
func lockPortTunnel(
	ctx context.Context,
	tx pgx.Tx,
	projectID string,
	sandboxID string,
	sessionID string,
) (PortTunnel, error) {
	var lockedSessionID string
	err := tx.QueryRow(ctx, `
		SELECT session.id
		FROM secondbox.data_plane_sessions AS session
		JOIN secondbox.port_sessions AS port ON port.data_plane_session_id=session.id
		WHERE port.project_id=$1 AND ($2='' OR port.sandbox_id=$2) AND port.id=$3
		FOR UPDATE OF session`,
		projectID, sandboxID, sessionID,
	).Scan(&lockedSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortTunnel{}, ports.ErrPortSessionNotFound
	}
	if err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox PortSession data-plane lock: %w", err)
	}
	return scanPortTunnel(tx.QueryRow(ctx, portTunnelSelect+`
		WHERE port.project_id=$1 AND ($2='' OR port.sandbox_id=$2) AND port.id=$3
		FOR UPDATE OF port`,
		projectID, sandboxID, lockedSessionID,
	))
}

func scanPortTunnel(row relayRow) (PortTunnel, error) {
	var tunnel PortTunnel
	err := row.Scan(
		&tunnel.Session.ID, &tunnel.Session.SandboxID, &tunnel.Session.Generation,
		&tunnel.Session.Name, &tunnel.Session.Protocol, &tunnel.Session.State,
		&tunnel.Session.CreatedAt, &tunnel.Session.ExpiresAt, &tunnel.ProjectID,
		&tunnel.ServiceAccountID, &tunnel.LeaseID, &tunnel.ProfileRevisionID,
		&tunnel.AssignmentID, &tunnel.InstanceID, &tunnel.RunnerID, &tunnel.RequestID, &tunnel.StreamID,
		&tunnel.FencingToken, &tunnel.GuestPort, &tunnel.StreamWindowBytes,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortTunnel{}, ports.ErrPortSessionNotFound
	}
	if err != nil {
		return PortTunnel{}, fmt.Errorf("SecondBox PortSession lookup: %w", err)
	}
	return tunnel, nil
}

func (relay *PostgresFrameRelay) persistInboundPortFrame(
	ctx context.Context,
	input InboundRelayFrame,
	now time.Time,
) (bool, error) {
	frame := input.Message.GetPort()
	if frame == nil || frame.Fence == nil || frame.OperationId == "" ||
		frame.StreamId == "" || frame.Sequence == 0 {
		return false, errors.New("SecondBox inbound Port frame is incomplete")
	}
	payloadCount := 0
	if frame.GetBytes() != nil {
		payloadCount++
	}
	if frame.GetCredit() != nil {
		payloadCount++
	}
	if frame.GetTerminal() != nil {
		payloadCount++
	}
	if payloadCount != 1 || frame.GetOpen() != nil || frame.GetCancel() != nil {
		return false, errors.New("SecondBox inbound Port payload is invalid")
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(input.Message)
	if err != nil {
		return false, fmt.Errorf("SecondBox inbound Port encoding: %w", err)
	}
	if int64(len(encoded)) > relay.maximumFrameBytes {
		return false, ErrRelayFrameLimit
	}
	digest := sha256.Sum256(encoded)
	payloadHash := hex.EncodeToString(digest[:])
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox inbound Port transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	identity := inboundIdentity{
		fence: frame.Fence, operation: frame.OperationId,
		stream: frame.StreamId, sequence: int64(frame.Sequence),
	}
	session, nextSequence, inboundBytes, err := lockInboundSession(
		ctx, tx, input.RunnerID, input.ConnectionID, identity,
	)
	if err != nil {
		return false, err
	}
	if session.Kind != "port" {
		return false, ErrRelaySequence
	}
	if !proto.Equal(frame.Correlation, dataPlaneCorrelation(session)) {
		return false, ErrRelayFence
	}
	if identity.sequence < nextSequence {
		var priorHash string
		err := tx.QueryRow(ctx, `
			SELECT payload_hash FROM secondbox.data_plane_frames
			WHERE session_id=$1 AND direction='inbound' AND sequence=$2`,
			session.ID, identity.sequence,
		).Scan(&priorHash)
		if err == nil && priorHash == payloadHash {
			if err := tx.Commit(ctx); err != nil {
				return false, fmt.Errorf("SecondBox inbound Port duplicate commit: %w", err)
			}
			return false, nil
		}
		return false, ErrRelaySequence
	}
	if identity.sequence != nextSequence ||
		(session.State != "pending" && session.State != "running" && session.State != "cancelling") {
		return false, ErrRelaySequence
	}
	if inboundBytes+int64(len(encoded)) > relay.maximumSessionBytes {
		return false, ErrRelaySessionLimit
	}
	var credit, clientCredit, runnerBytes int64
	if value := frame.GetCredit(); value != nil {
		if value.ByteCount == 0 || value.ByteCount > uint64(session.StreamWindowBytes) {
			return false, ErrRelayFrameLimit
		}
		credit = int64(value.ByteCount)
	}
	if err := tx.QueryRow(ctx, `
		SELECT client_credit_bytes,runner_bytes FROM secondbox.port_sessions
		WHERE id=$1 FOR UPDATE`,
		session.ID,
	).Scan(&clientCredit, &runnerBytes); err != nil {
		return false, fmt.Errorf("SecondBox inbound Port usage lookup: %w", err)
	}
	if credit > 0 && clientCredit+credit > session.StreamWindowBytes {
		return false, ErrRelayFrameLimit
	}
	if value := frame.GetBytes(); value != nil {
		if len(value.Data) == 0 || int64(len(value.Data)) > relay.maximumFrameBytes/2 ||
			runnerBytes+int64(len(value.Data)) > session.MaximumResponseBytes {
			return false, ErrRelaySessionLimit
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
			priority,state,claim_owner,claim_expires_at,delivery_count,
			created_at,updated_at,delivered_at,consumed_at
		) VALUES ($1,$2,'inbound',$3,$4,$5,$6,0,'delivered',$7,NULL,1,$8,$8,$8,NULL)`,
		fmt.Sprintf("%s_port_in_%d", session.ID, identity.sequence), session.ID,
		identity.sequence, payloadHash, encoded, len(encoded), input.ConnectionID, now.UTC(),
	); err != nil {
		return false, fmt.Errorf("SecondBox inbound Port frame insert: %w", err)
	}
	state, terminalKind, terminalDetail := session.State, "", ""
	portState := contracts.PortSessionStateOpen
	var completedAt *time.Time
	if terminal := frame.GetTerminal(); terminal != nil {
		terminalKind, terminalDetail = terminal.Kind.String(), terminal.SafeDetail
		state = "completed"
		if terminal.Kind == runnerv1.PortTerminalKind_PORT_TERMINAL_KIND_FENCED {
			state, portState = "failed", contracts.PortSessionStateFenced
		} else {
			portState = contracts.PortSessionStateClosed
		}
		finished := now.UTC()
		completedAt = &finished
	}
	frameDataBytes := int64(0)
	if value := frame.GetBytes(); value != nil {
		frameDataBytes = int64(len(value.Data))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET state=$2,next_inbound_sequence=$3,inbound_bytes=inbound_bytes+$4,
		    terminal_kind=CASE WHEN $5='' THEN terminal_kind ELSE $5 END,
		    terminal_detail=CASE WHEN $5='' THEN terminal_detail ELSE $6 END,
		    updated_at=$7,completed_at=COALESCE($8,completed_at)
		WHERE id=$1`,
		session.ID, state, identity.sequence+1, len(encoded),
		terminalKind, terminalDetail, now.UTC(), completedAt,
	); err != nil {
		return false, fmt.Errorf("SecondBox inbound Port session update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.port_sessions
		SET client_credit_bytes=client_credit_bytes+$2,runner_bytes=runner_bytes+$3,
		    state=CASE WHEN $4='' THEN state ELSE $4 END,
		    closed_at=CASE WHEN $4='' THEN closed_at ELSE COALESCE(closed_at,$5) END,
		    updated_at=$5
		WHERE id=$1`,
		session.ID, credit, frameDataBytes,
		map[bool]string{true: portState, false: ""}[completedAt != nil], now.UTC(),
	); err != nil {
		return false, fmt.Errorf("SecondBox inbound Port projection update: %w", err)
	}
	if frameDataBytes > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.activity_sessions
			SET last_activity_at=$2,updated_at=$2 WHERE id=$1 AND state='active'`,
			session.ID, now.UTC(),
		); err != nil {
			return false, fmt.Errorf("SecondBox inbound Port activity update: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.sandboxes
			SET last_activity_at=$1,revision=revision+1,updated_at=$1
			WHERE id=$2 AND generation=$3`,
			now.UTC(), session.SandboxID, session.Generation,
		); err != nil {
			return false, fmt.Errorf("SecondBox inbound Port activity update: %w", err)
		}
	}
	if completedAt != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE secondbox.activity_sessions
			SET state='closed',closed_at=$2,updated_at=$2 WHERE id=$1 AND state='active'`,
			session.ID, now.UTC(),
		); err != nil {
			return false, fmt.Errorf("SecondBox inbound Port activity close: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox inbound Port commit: %w", err)
	}
	return true, nil
}

var _ PortSessionRelay = (*PostgresFrameRelay)(nil)
