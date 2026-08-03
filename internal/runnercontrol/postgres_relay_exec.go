package runnercontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func (relay *PostgresFrameRelay) AppendExecClientFrame(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	frame ExecClientFrame,
	now time.Time,
) (bool, error) {
	if tenantRef == "" || subjectRef == "" || sessionID == "" || frame.Sequence < 0 {
		return false, errors.New("SecondBox public Exec frame identity is incomplete")
	}
	isInput := frame.Input != nil || frame.EndInput
	selected := 0
	if isInput {
		selected++
	}
	if frame.Credit > 0 {
		selected++
	}
	if frame.Cancel {
		selected++
	}
	if selected != 1 {
		return false, errors.New("SecondBox public Exec frame requires exactly one payload")
	}
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox public Exec frame transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		tenantRef, subjectRef, sessionID))
	if err != nil {
		return false, err
	}
	if session.Kind != "exec" || session.Operation != "exec-stream" {
		return false, ErrDataPlaneNotFound
	}
	runnerSequence := frame.Sequence + 2
	message := &runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
			Fence: &runnerv1.AssignmentFence{
				AssignmentId: session.AssignmentID, SandboxId: session.SandboxID,
				InstanceId: session.InstanceID, SandboxGeneration: uint64(session.Generation),
				FencingToken: bytes.Clone(session.FencingToken),
			},
			OperationId: session.ID, StreamId: session.StreamID, Sequence: uint64(runnerSequence),
			Correlation: dataPlaneCorrelation(session),
		}},
	}
	execFrame := message.GetExec()
	switch {
	case isInput:
		execFrame.Payload = &runnerv1.ExecFrame_Input{Input: &runnerv1.ExecInput{
			Data: bytes.Clone(frame.Input), EndOfInput: frame.EndInput,
		}}
	case frame.Credit > 0:
		execFrame.Payload = &runnerv1.ExecFrame_Credit{Credit: &runnerv1.StreamCredit{
			ByteCount: uint64(frame.Credit),
		}}
	case frame.Cancel:
		execFrame.Payload = &runnerv1.ExecFrame_Cancel{Cancel: &runnerv1.ExecCancel{
			Reason: "public streaming client cancellation",
		}}
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return false, fmt.Errorf("SecondBox public Exec frame encoding: %w", err)
	}
	if int64(len(payload)) > relay.maximumFrameBytes {
		return false, ErrRelayFrameLimit
	}
	hash := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(hash[:])
	var priorHash string
	err = tx.QueryRow(ctx, `
		SELECT payload_hash FROM secondbox.data_plane_frames
		WHERE session_id=$1 AND direction='outbound' AND sequence=$2`,
		session.ID, runnerSequence,
	).Scan(&priorHash)
	if err == nil {
		if priorHash != payloadHash {
			return false, ErrRelaySequence
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("SecondBox public Exec duplicate commit: %w", err)
		}
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("SecondBox public Exec sequence lookup: %w", err)
	}
	if isInput && session.RequestStreamClosed {
		return false, ErrRelaySequence
	}
	if isInput && len(frame.Input) == 0 && !frame.EndInput {
		return false, errors.New("SecondBox public Exec stdin frame is empty")
	}
	if isInput &&
		session.RequestStreamBytes+int64(len(frame.Input)) > session.MaximumRequestBytes {
		return false, ErrRelaySessionLimit
	}
	if frame.Credit > 0 {
		emittedBytes := int64(len(session.Stdout) + len(session.Stderr))
		if session.ResponseCreditBytes-emittedBytes+frame.Credit > session.StreamWindowBytes {
			return false, ErrRelayFrameLimit
		}
	}
	if runnerSequence != session.NextOutboundSequence {
		return false, ErrRelaySequence
	}
	if session.State != "pending" && session.State != "running" {
		switch session.State {
		case "completed", "failed", "cancelled", "expired":
			if _, err := tx.Exec(ctx, `
				INSERT INTO secondbox.data_plane_frames (
					id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
					priority,state,claim_owner,claim_expires_at,delivery_count,
					created_at,updated_at,delivered_at
				) VALUES ($1,$2,'outbound',$3,$4,$5,$6,0,'discarded','',NULL,0,$7,$7,NULL)`,
				fmt.Sprintf("%s_client_%d", session.ID, frame.Sequence), session.ID,
				runnerSequence, payloadHash, payload, len(payload), now.UTC(),
			); err != nil {
				return false, fmt.Errorf("SecondBox terminal public Exec frame insert: %w", err)
			}
			requestBytes := int64(0)
			if isInput {
				requestBytes = int64(len(frame.Input))
			}
			if _, err := tx.Exec(ctx, `
				UPDATE secondbox.data_plane_sessions
				SET request_stream_bytes=request_stream_bytes+$2,
				    request_stream_closed=request_stream_closed OR $3,
					outbound_bytes=outbound_bytes+$4,
					response_credit_bytes=response_credit_bytes+$5,
					next_outbound_sequence=next_outbound_sequence+1,
					frames_retain_until=retain_until,frame_cleanup_completed_at=NULL,
					updated_at=$6
				WHERE id=$1`,
				session.ID, requestBytes, frame.EndInput, len(payload), frame.Credit, now.UTC(),
			); err != nil {
				return false, fmt.Errorf("SecondBox terminal public Exec session update: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return false, fmt.Errorf("SecondBox terminal public Exec frame commit: %w", err)
			}
			return true, nil
		default:
			return false, ErrRelaySequence
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
			priority,state,claim_owner,claim_expires_at,delivery_count,
			created_at,updated_at,delivered_at
		) VALUES ($1,$2,'outbound',$3,$4,$5,$6,0,'pending','',NULL,0,$7,$7,NULL)`,
		fmt.Sprintf("%s_client_%d", session.ID, frame.Sequence), session.ID,
		runnerSequence, payloadHash, payload, len(payload), now.UTC(),
	); err != nil {
		return false, fmt.Errorf("SecondBox public Exec frame insert: %w", err)
	}
	requestBytes := int64(0)
	if isInput {
		requestBytes = int64(len(frame.Input))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET request_stream_bytes=request_stream_bytes+$2,
		    request_stream_closed=request_stream_closed OR $8,
		    outbound_bytes=outbound_bytes+$3,
		    response_credit_bytes=response_credit_bytes+$7,
		    next_outbound_sequence=next_outbound_sequence+1,
		    frames_retain_until=retain_until,frame_cleanup_completed_at=NULL,
		    state=CASE WHEN $4 THEN 'cancelling' ELSE state END,
		    terminal_kind=CASE WHEN $4 THEN $5 ELSE terminal_kind END,
		    terminal_detail=CASE WHEN $4 THEN 'public streaming client cancellation' ELSE terminal_detail END,
		    updated_at=$6
		WHERE id=$1`,
		session.ID, requestBytes, len(payload), frame.Cancel,
		runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED.String(), now.UTC(), frame.Credit,
		frame.EndInput,
	); err != nil {
		return false, fmt.Errorf("SecondBox public Exec session update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox public Exec frame commit: %w", err)
	}
	return true, nil
}

// ListExecServerFrames returns retained Runner frames after one public sequence.
func (relay *PostgresFrameRelay) ListExecServerFrames(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	afterSequence int64,
	limit int,
) ([]ExecServerFrame, error) {
	if tenantRef == "" || subjectRef == "" ||
		sessionID == "" || afterSequence < -1 || limit < 1 || limit > 256 {
		return nil, errors.New("SecondBox public Exec frame query is invalid")
	}
	var kind, operation string
	if err := relay.pool.QueryRow(ctx, `
		SELECT kind,operation FROM secondbox.data_plane_sessions
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`,
		tenantRef, subjectRef, sessionID,
	).Scan(&kind, &operation); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrDataPlaneNotFound
		}
		return nil, fmt.Errorf("SecondBox public Exec session lookup: %w", err)
	}
	if kind != "exec" || operation != "exec-stream" {
		return nil, ErrDataPlaneNotFound
	}
	rows, err := relay.pool.Query(ctx, `
		SELECT sequence,payload FROM secondbox.data_plane_frames
		WHERE session_id=$1 AND direction='inbound' AND sequence>$2
		ORDER BY sequence
		LIMIT $3`, sessionID, afterSequence+1, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("SecondBox public Exec frame lookup: %w", err)
	}
	defer rows.Close()
	result := make([]ExecServerFrame, 0, limit)
	for rows.Next() {
		var runnerSequence int64
		var payload []byte
		if err := rows.Scan(&runnerSequence, &payload); err != nil {
			return nil, fmt.Errorf("SecondBox public Exec frame scan: %w", err)
		}
		var message runnerv1.RunnerToControlPlane
		if err := proto.Unmarshal(payload, &message); err != nil {
			return nil, fmt.Errorf("SecondBox public Exec frame decoding: %w", err)
		}
		execFrame := message.GetExec()
		if execFrame == nil || execFrame.GetOutput() == nil && execFrame.GetTerminal() == nil {
			return nil, errors.New("SecondBox public Exec retained frame is invalid")
		}
		result = append(result, ExecServerFrame{
			Sequence: runnerSequence - 1,
			Output:   execFrame.GetOutput(),
			Terminal: execFrame.GetTerminal(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SecondBox public Exec frame rows: %w", err)
	}
	return result, nil
}

// CancelDataPlaneSession durably requests guest cancellation for one public session.
