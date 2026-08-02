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
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	ErrRelayFence          = errors.New("SecondBox data-plane relay fence is stale")
	ErrRelaySequence       = errors.New("SecondBox data-plane relay sequence is invalid")
	ErrRelayFrameLimit     = errors.New("SecondBox data-plane relay frame limit exceeded")
	ErrRelaySessionLimit   = errors.New("SecondBox data-plane relay session limit exceeded")
	ErrRelayDeliveryClaim  = errors.New("SecondBox data-plane relay delivery claim is inactive")
	ErrDataPlaneNotFound   = errors.New("SecondBox data-plane session not found")
	ErrDataPlaneIncomplete = errors.New("SecondBox data-plane session is not terminal")
	ErrDataPlaneDeadline   = errors.New("SecondBox data-plane operation deadline exceeded")
	ErrTerminalAttached    = errors.New("SecondBox Terminal session already has an active attachment")
	ErrTerminalDetached    = errors.New("SecondBox Terminal attachment is inactive")
	ErrFilePermission      = errors.New("SecondBox File operation permission denied")
	ErrFileChecksum        = errors.New("SecondBox File checksum mismatch")
)

// PostgresFrameRelayConfig contains explicit durability and memory bounds.
type PostgresFrameRelayConfig struct {
	DatabaseURL         string
	ClaimDuration       time.Duration
	Retention           time.Duration
	MaximumFrameBytes   int64
	MaximumSessionBytes int64
}

// PostgresFrameRelay is the durable root data-plane outbox and inbox.
type PostgresFrameRelay struct {
	pool                *pgxpool.Pool
	claimDuration       time.Duration
	retention           time.Duration
	maximumFrameBytes   int64
	maximumSessionBytes int64
}

// DataPlaneAdmission is one authenticated request translated to runner frames.
type DataPlaneAdmission struct {
	ID                      string
	StreamID                string
	TenantRef               string
	SubjectRef              string
	SandboxID               string
	LeaseID                 string
	Generation              int64
	Kind                    string
	Operation               string
	RequestID               string
	IdempotencyKey          string
	RequestHash             string
	DeadlineAt              time.Time
	MaximumResponseBytes    int64
	MaximumRequestBytes     int64
	StreamWindowBytes       int64
	Priority                int64
	ExecOpen                *runnerv1.ExecOpen
	DeferResponseCredit     bool
	UseProfileRequestLimit  bool
	UseProfileResponseLimit bool
	UseProfileStreamWindow  bool
	Detachable              bool
	FileOpen                *runnerv1.FileOpen
	FileContent             []byte
	Request                 any
	Now                     time.Time
}

// PublicDataPlaneCancellation binds one HTTP cancellation key to an exact session response.
type PublicDataPlaneCancellation struct {
	TenantRef        string
	SubjectRef       string
	SandboxID        string
	SessionID        string
	SessionKind      string
	SessionOperation string
	IdempotencyKey   string
	RequestHash      string
	Reason           string
	Generation       int64
	Now              time.Time
	IdempotencyEnds  time.Time
}

// DataPlaneSession is the durable public-operation projection.
type DataPlaneSession struct {
	ID                    string
	StreamID              string
	TenantRef             string
	SubjectRef            string
	SandboxID             string
	ProfileRevisionID     string
	AssignmentID          string
	InstanceID            string
	RunnerID              string
	Generation            int64
	FencingToken          []byte
	RequestID             string
	LeaseID               string
	Kind                  string
	Operation             string
	State                 string
	DeadlineAt            time.Time
	MaximumResponseBytes  int64
	MaximumRequestBytes   int64
	StreamWindowBytes     int64
	ResponseCreditBytes   int64
	RequestStreamBytes    int64
	RequestStreamClosed   bool
	Detachable            bool
	TerminalDetachSeconds int64
	AttachmentID          string
	AttachedAt            *time.Time
	DetachedAt            *time.Time
	DetachExpiresAt       *time.Time
	NextClientSequence    int64
	TerminalKind          string
	TerminalDetail        string
	ExitCode              int32
	Signal                int32
	SpawnFailureReason    string
	ElapsedMilliseconds   int64
	LimitBytes            int64
	InfrastructureReason  string
	Retryable             bool
	TerminalMessage       string
	Stdout                []byte
	Stderr                []byte
	Content               []byte
	Metadata              *runnerv1.FileMetadata
	CreatedAt             time.Time
	UpdatedAt             time.Time
	CompletedAt           *time.Time
}

// NewPostgresFrameRelay opens the durable relay authority.
func NewPostgresFrameRelay(
	ctx context.Context,
	config PostgresFrameRelayConfig,
) (*PostgresFrameRelay, error) {
	if config.DatabaseURL == "" || config.ClaimDuration <= 0 || config.Retention <= 0 ||
		config.MaximumFrameBytes < 1024 || config.MaximumSessionBytes < config.MaximumFrameBytes {
		return nil, errors.New("SecondBox PostgreSQL frame relay requires database, claim, retention, and byte bounds")
	}
	pool, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox frame relay PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("SecondBox frame relay PostgreSQL readiness: %w", err)
	}
	return &PostgresFrameRelay{
		pool: pool, claimDuration: config.ClaimDuration, retention: config.Retention,
		maximumFrameBytes:   config.MaximumFrameBytes,
		maximumSessionBytes: config.MaximumSessionBytes,
	}, nil
}

// Close releases the relay pool.
func (relay *PostgresFrameRelay) Close() {
	relay.pool.Close()
}

// AdmitDataPlane transactionally resolves current assignment authority and creates one outbox.
func (relay *PostgresFrameRelay) AdmitDataPlane(
	ctx context.Context,
	input DataPlaneAdmission,
) (DataPlaneSession, bool, error) {
	if input.SubjectRef == "" {
	}
	if err := validateDataPlaneAdmission(input); err != nil {
		return DataPlaneSession{}, false, err
	}
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox data-plane admission transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey := input.TenantRef + "\x1f" + input.SubjectRef +
		"\x1fdata-plane\x1f" + input.Operation + "\x1f" +
		input.SandboxID + "\x1f" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox data-plane idempotency lock: %w", err)
	}
	if input.IdempotencyKey != "" {
		session, found, err := lookupDataPlaneReplay(
			ctx, tx, input.TenantRef, input.SubjectRef, input.SandboxID, input.Operation,
			input.IdempotencyKey, input.RequestHash,
		)
		if err != nil {
			return DataPlaneSession{}, false, err
		}
		if found {
			if err := tx.Commit(ctx); err != nil {
				return DataPlaneSession{}, false, fmt.Errorf("SecondBox data-plane replay commit: %w", err)
			}
			return session, true, nil
		}
	}
	projectCapacityKey := input.TenantRef + "\x1f" + input.SubjectRef + "\x1fdata-plane-capacity"
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, projectCapacityKey); err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox data-plane capacity lock: %w", err)
	}
	sandboxCapacityKey := projectCapacityKey + "\x1f" + input.SandboxID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, sandboxCapacityKey); err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox Sandbox data-plane capacity lock: %w", err)
	}
	session, policy, err := lockDataPlaneAuthority(ctx, tx, input)
	if err != nil {
		return DataPlaneSession{}, false, err
	}
	if input.UseProfileRequestLimit {
		input.MaximumRequestBytes = policy.MaximumTransferBytes
	}
	if input.UseProfileResponseLimit {
		input.MaximumResponseBytes = policy.MaximumBufferedOutputBytes
	}
	if input.UseProfileStreamWindow {
		input.StreamWindowBytes = policy.StreamWindowBytes
	}
	if input.Kind == "terminal" {
		input.ExecOpen.OutputLimitBytes = uint64(input.MaximumResponseBytes)
		session.Detachable = input.Detachable
		session.TerminalDetachSeconds = policy.TerminalDetachSeconds
		if input.Detachable && policy.TerminalDetachSeconds == 0 {
			return DataPlaneSession{}, false, errors.New("SecondBox pinned Profile does not permit detached Terminal sessions")
		}
	}
	if input.Kind == "file" && input.FileOpen != nil &&
		input.FileOpen.Operation == runnerv1.FileOperation_FILE_OPERATION_READ {
		if input.MaximumResponseBytes == 0 {
			input.MaximumResponseBytes = policy.MaximumTransferBytes
		}
		input.FileOpen = proto.Clone(input.FileOpen).(*runnerv1.FileOpen)
		input.FileOpen.ExpectedSize = uint64(input.MaximumResponseBytes)
	}
	if input.DeadlineAt.After(input.Now.Add(
		time.Duration(policy.MaximumDeadlineMilliseconds) * time.Millisecond,
	)) {
		return DataPlaneSession{}, false, errors.New("SecondBox data-plane deadline exceeds the pinned Profile")
	}
	if input.DeferResponseCredit && input.StreamWindowBytes > policy.StreamWindowBytes {
		return DataPlaneSession{}, false, ports.ErrQuotaExceeded
	}
	responseLimit := policy.MaximumBufferedOutputBytes
	if input.Kind == "file" {
		responseLimit = policy.MaximumTransferBytes
	}
	if input.MaximumResponseBytes > responseLimit ||
		input.MaximumRequestBytes > policy.MaximumTransferBytes {
		return DataPlaneSession{}, false, ports.ErrQuotaExceeded
	}
	session.ID, session.StreamID = input.ID, input.StreamID
	session.TenantRef, session.SandboxID = input.TenantRef, input.SandboxID
	session.TenantRef, session.SubjectRef = input.TenantRef, input.SubjectRef
	session.RequestID, session.LeaseID = input.RequestID, input.LeaseID
	session.Kind, session.Operation = input.Kind, input.Operation
	session.State, session.DeadlineAt = "pending", input.DeadlineAt.UTC()
	session.MaximumResponseBytes = input.MaximumResponseBytes
	session.MaximumRequestBytes = input.MaximumRequestBytes
	session.StreamWindowBytes = input.StreamWindowBytes
	session.CreatedAt, session.UpdatedAt = input.Now.UTC(), input.Now.UTC()
	frames, outboundBytes, err := relay.buildOutboundFrames(session, input)
	if err != nil {
		return DataPlaneSession{}, false, err
	}
	if outboundBytes+input.MaximumResponseBytes > relay.maximumSessionBytes {
		return DataPlaneSession{}, false, ErrRelaySessionLimit
	}
	requestJSON, err := json.Marshal(input.Request)
	if err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox data-plane request encoding: %w", err)
	}
	if len(requestJSON) == 0 {
		requestJSON = []byte("{}")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.data_plane_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,assignment_id,instance_id,runner_id,generation,fencing_token,request_id,lease_id,kind,operation,stream_id,state,priority,idempotency_key,request_hash,deadline_at,maximum_response_bytes,maximum_request_bytes,stream_window_bytes,response_credit_bytes,request_stream_bytes,request_stream_closed,detachable,terminal_detach_seconds,attachment_id,attached_at,detached_at,detach_expires_at,outbound_bytes,inbound_bytes,next_inbound_sequence,terminal_kind,terminal_detail,exit_code,signal,spawn_failure_reason,elapsed_milliseconds,limit_bytes,infrastructure_failure_reason,retryable,terminal_message,stdout_bytes,stderr_bytes,content_bytes,metadata_json,request_json,created_at,updated_at,completed_at,retain_until
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'pending',$16,$17,$18,$19,$20,$21,$22,0,0,false,$23,$24,'',NULL,NULL,NULL,$25,0,1,'','',0,0,'',0,0,'',false,'',$26,$26,$26,'{}',$27,$28,$28,NULL,$29
		)`,
		session.ID, session.TenantRef, session.SubjectRef, session.SandboxID, session.ProfileRevisionID, session.AssignmentID, session.InstanceID, session.RunnerID, session.Generation, session.FencingToken, session.RequestID, session.LeaseID, session.Kind, session.Operation, session.StreamID, input.Priority, input.IdempotencyKey, input.RequestHash, session.DeadlineAt, session.MaximumResponseBytes, session.MaximumRequestBytes, session.StreamWindowBytes, session.Detachable, session.TerminalDetachSeconds, outboundBytes, []byte{}, requestJSON, session.CreatedAt, session.CreatedAt.Add(relay.retention),
	); err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox data-plane session insert: %w", err)
	}
	for _, frame := range frames {
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.data_plane_frames (
				id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
				priority,state,claim_owner,claim_expires_at,delivery_count,
				created_at,updated_at,delivered_at
			) VALUES ($1,$2,'outbound',$3,$4,$5,$6,$7,'pending','',NULL,0,$8,$8,NULL)`,
			frame.id, session.ID, frame.sequence, frame.hash, frame.payload,
			len(frame.payload), frame.priority, session.CreatedAt,
		); err != nil {
			return DataPlaneSession{}, false, fmt.Errorf("SecondBox outbound relay frame insert: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.activity_sessions (
			id,tenant_ref,subject_ref,sandbox_id,generation,kind,state,lease_id,last_activity_at,
			created_at,updated_at,closed_at
		) VALUES ($1,$2,$3,$4,$5,$6,'active',$7,$8,$8,$8,NULL)`,
		session.ID, session.TenantRef, session.SubjectRef,
		session.SandboxID, session.Generation, session.Kind, input.LeaseID, session.CreatedAt,
	); err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox data-plane activity insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox data-plane admission commit: %w", err)
	}
	return session, false, nil
}

type encodedRelayFrame struct {
	id       string
	sequence int64
	priority int64
	hash     string
	payload  []byte
}

func (relay *PostgresFrameRelay) buildOutboundFrames(
	session DataPlaneSession,
	input DataPlaneAdmission,
) ([]encodedRelayFrame, int64, error) {
	fence := &runnerv1.AssignmentFence{
		AssignmentId: session.AssignmentID, SandboxId: session.SandboxID,
		InstanceId: session.InstanceID, SandboxGeneration: uint64(session.Generation),
		FencingToken: append([]byte(nil), session.FencingToken...),
	}
	correlation := dataPlaneCorrelation(session)
	messages := make([]*runnerv1.ControlPlaneToRunner, 0, 4)
	if input.Kind == "exec" || input.Kind == "terminal" {
		messages = append(messages, &runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
				Fence: fence, OperationId: session.ID, StreamId: session.StreamID, Sequence: 1,
				Correlation: proto.Clone(correlation).(*runnerv1.Correlation),
				Payload:     &runnerv1.ExecFrame_Open{Open: proto.Clone(input.ExecOpen).(*runnerv1.ExecOpen)},
			}},
		})
		if !input.DeferResponseCredit {
			messages = append(messages, &runnerv1.ControlPlaneToRunner{
				Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
					Fence: fence, OperationId: session.ID, StreamId: session.StreamID, Sequence: 2,
					Correlation: proto.Clone(correlation).(*runnerv1.Correlation),
					Payload: &runnerv1.ExecFrame_Credit{Credit: &runnerv1.StreamCredit{
						ByteCount: uint64(input.MaximumResponseBytes),
					}},
				}},
			})
		}
	} else {
		sequence := uint64(1)
		messages = append(messages, &runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_File{File: &runnerv1.FileFrame{
				Fence: fence, OperationId: session.ID, StreamId: session.StreamID, Sequence: sequence,
				Correlation: proto.Clone(correlation).(*runnerv1.Correlation),
				Payload:     &runnerv1.FileFrame_Open{Open: proto.Clone(input.FileOpen).(*runnerv1.FileOpen)},
			}},
		})
		sequence++
		for offset := 0; offset < len(input.FileContent); {
			size := int(relay.maximumFrameBytes / 2)
			if remaining := len(input.FileContent) - offset; size > remaining {
				size = remaining
			}
			messages = append(messages, &runnerv1.ControlPlaneToRunner{
				Message: &runnerv1.ControlPlaneToRunner_File{File: &runnerv1.FileFrame{
					Fence: fence, OperationId: session.ID, StreamId: session.StreamID, Sequence: sequence,
					Correlation: proto.Clone(correlation).(*runnerv1.Correlation),
					Payload: &runnerv1.FileFrame_Chunk{Chunk: &runnerv1.FileChunk{
						Offset: uint64(offset), Data: bytes.Clone(input.FileContent[offset : offset+size]),
					}},
				}},
			})
			offset += size
			sequence++
		}
		if input.FileOpen.Operation == runnerv1.FileOperation_FILE_OPERATION_READ {
			messages = append(messages, &runnerv1.ControlPlaneToRunner{
				Message: &runnerv1.ControlPlaneToRunner_File{File: &runnerv1.FileFrame{
					Fence: fence, OperationId: session.ID, StreamId: session.StreamID, Sequence: sequence,
					Correlation: proto.Clone(correlation).(*runnerv1.Correlation),
					Payload: &runnerv1.FileFrame_Credit{Credit: &runnerv1.StreamCredit{
						ByteCount: uint64(input.MaximumResponseBytes),
					}},
				}},
			})
		}
	}
	frames := make([]encodedRelayFrame, 0, len(messages))
	var total int64
	for index, message := range messages {
		payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
		if err != nil {
			return nil, 0, fmt.Errorf("SecondBox outbound relay frame encoding: %w", err)
		}
		if int64(len(payload)) > relay.maximumFrameBytes {
			return nil, 0, ErrRelayFrameLimit
		}
		hash := sha256.Sum256(payload)
		priority := input.Priority
		frames = append(frames, encodedRelayFrame{
			id: fmt.Sprintf("%s_out_%d", session.ID, index+1), sequence: int64(index + 1),
			priority: priority, hash: hex.EncodeToString(hash[:]), payload: payload,
		})
		total += int64(len(payload))
	}
	return frames, total, nil
}

func dataPlaneCorrelation(session DataPlaneSession) *runnerv1.Correlation {
	return &runnerv1.Correlation{
		RequestId: session.RequestID, OperationId: session.ID,
		SandboxId: session.SandboxID, InstanceId: session.InstanceID,
		SandboxGeneration: uint64(session.Generation), AssignmentId: session.AssignmentID,
		LeaseId: session.LeaseID, RunnerId: session.RunnerID,
	}
}

// ClaimOutboundFrame claims the highest-priority contiguous frame for one runner.
func (relay *PostgresFrameRelay) ClaimOutboundFrame(
	ctx context.Context,
	runnerID string,
	connectionID string,
	now time.Time,
) (RelayDelivery, bool, error) {
	var id string
	var payload []byte
	var claimAttempt int64
	err := relay.pool.QueryRow(ctx, `
		WITH active_connection AS MATERIALIZED (
			SELECT connection.id
			FROM secondbox.runner_connections AS connection
			WHERE connection.id=$2
			  AND connection.runner_id=$1
			  AND connection.state='active'
		),
		candidate_frame AS MATERIALIZED (
			SELECT frame.id
			FROM secondbox.data_plane_frames AS frame
			JOIN secondbox.data_plane_sessions AS session ON session.id=frame.session_id
			CROSS JOIN active_connection
			WHERE frame.direction='outbound'
			  AND frame.state IN ('pending','claimed')
			  AND (frame.state='pending' OR frame.claim_expires_at<=$3)
			  AND session.runner_id=$1
			  AND session.state IN ('pending','running','cancelling')
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.data_plane_frames AS prior
			    WHERE prior.session_id=frame.session_id AND prior.direction='outbound'
			      AND prior.sequence<frame.sequence AND prior.state<>'delivered'
			  )
			ORDER BY frame.priority,frame.created_at,frame.id
			LIMIT 1
		),
		locked_connection AS MATERIALIZED (
			SELECT connection.id
			FROM secondbox.runner_connections AS connection
			CROSS JOIN candidate_frame
			WHERE connection.id=$2
			  AND connection.runner_id=$1
			  AND connection.state='active'
			FOR SHARE OF connection
		),
		chosen AS MATERIALIZED (
			SELECT frame.id,frame.payload
			FROM secondbox.data_plane_frames AS frame
			JOIN secondbox.data_plane_sessions AS session ON session.id=frame.session_id
			CROSS JOIN locked_connection
			WHERE frame.direction='outbound'
			  AND frame.state IN ('pending','claimed')
			  AND (frame.state='pending' OR frame.claim_expires_at<=$3)
			  AND session.runner_id=$1
			  AND session.state IN ('pending','running','cancelling')
			  AND NOT EXISTS (
			    SELECT 1 FROM secondbox.data_plane_frames AS prior
			    WHERE prior.session_id=frame.session_id AND prior.direction='outbound'
			      AND prior.sequence<frame.sequence AND prior.state<>'delivered'
			  )
			ORDER BY frame.priority,frame.created_at,frame.id
			FOR UPDATE OF frame SKIP LOCKED
			LIMIT 1
		),
		claimed AS (
			UPDATE secondbox.data_plane_frames AS frame
			SET state='claimed',
			    claim_owner=$2 || chr(31) || (frame.delivery_count+1)::text,
			    claim_expires_at=$4,
			    delivery_count=frame.delivery_count+1,
			    updated_at=$3
			FROM chosen
			WHERE frame.id=chosen.id
			RETURNING frame.id,chosen.payload,frame.delivery_count
		)
		SELECT id,payload,delivery_count
		FROM claimed`,
		runnerID,
		connectionID,
		now.UTC(),
		now.UTC().Add(relay.claimDuration),
	).Scan(&id, &payload, &claimAttempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RelayDelivery{}, false, nil
	}
	if err != nil {
		return RelayDelivery{}, false, fmt.Errorf("SecondBox outbound relay claim lookup: %w", err)
	}
	message := &runnerv1.ControlPlaneToRunner{}
	if err := proto.Unmarshal(payload, message); err != nil {
		return RelayDelivery{}, false, fmt.Errorf("SecondBox outbound relay frame decoding: %w", err)
	}
	return RelayDelivery{ID: id, ClaimAttempt: claimAttempt, Message: message}, true, nil
}

// MarkOutboundFrameDelivered commits transport success for the exact connection claim.
func (relay *PostgresFrameRelay) MarkOutboundFrameDelivered(
	ctx context.Context,
	deliveryID string,
	connectionID string,
	claimAttempt int64,
	now time.Time,
) error {
	if claimAttempt <= 0 {
		return ErrRelayDeliveryClaim
	}
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox outbound relay delivery transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	liveCredential, err := lockLiveRelayConnectionCredential(
		ctx, tx, "", connectionID,
	)
	if err != nil {
		return err
	}
	if !liveCredential {
		return ErrRelayDeliveryClaim
	}
	tag, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_frames
		SET state='delivered',delivered_at=$3,updated_at=$3
		WHERE id=$1 AND state='claimed' AND claim_owner=$2`,
		deliveryID, relayClaimOwner(connectionID, claimAttempt), now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("SecondBox outbound relay delivery update: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrRelayDeliveryClaim
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions AS session
		SET state=CASE WHEN session.state='pending' THEN 'running' ELSE session.state END,updated_at=$2
		FROM secondbox.data_plane_frames AS frame
		WHERE frame.id=$1 AND session.id=frame.session_id`,
		deliveryID, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox data-plane running state update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("SecondBox outbound relay delivery commit: %w", err)
	}
	return nil
}

func relayClaimOwner(connectionID string, claimAttempt int64) string {
	return fmt.Sprintf("%s\x1f%d", connectionID, claimAttempt)
}

// PersistInboundFrame commits one contiguous, assignment-fenced runner frame.
func (relay *PostgresFrameRelay) PersistInboundFrame(
	ctx context.Context,
	input InboundRelayFrame,
	now time.Time,
) (bool, error) {
	if input.Message != nil && input.Message.GetPort() != nil {
		return relay.persistInboundPortFrame(ctx, input, now.UTC())
	}
	identity, err := inboundFrameIdentity(input.Message)
	if err != nil {
		return false, err
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(input.Message)
	if err != nil {
		return false, fmt.Errorf("SecondBox inbound relay frame encoding: %w", err)
	}
	if int64(len(payload)) > relay.maximumFrameBytes {
		return false, ErrRelayFrameLimit
	}
	hash := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(hash[:])
	tx, err := relay.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("SecondBox inbound relay transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	liveCredential, err := lockLiveRelayConnectionCredential(
		ctx, tx, input.RunnerID, input.ConnectionID,
	)
	if err != nil {
		return false, err
	}
	if !liveCredential {
		return false, ErrRelayFence
	}
	session, nextSequence, inboundBytes, err := lockInboundSession(
		ctx, tx, input.RunnerID, input.ConnectionID, identity,
	)
	if err != nil {
		return false, err
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
				return false, fmt.Errorf("SecondBox inbound duplicate commit: %w", err)
			}
			return false, nil
		}
		return false, ErrRelaySequence
	}
	if session.State == "completed" || session.State == "failed" ||
		session.State == "cancelled" || session.State == "expired" {
		return false, ErrRelaySequence
	}
	if identity.sequence != nextSequence {
		return false, ErrRelaySequence
	}
	streamOutput := identity.execOutput
	if identity.ptyOutput != nil {
		streamOutput = identity.ptyOutput
	}
	if (session.Operation == "exec-stream" || session.Operation == "terminal") &&
		streamOutput != nil &&
		int64(len(session.Stdout)+len(session.Stderr)+len(streamOutput.Data)) > session.ResponseCreditBytes {
		return false, ErrRelaySequence
	}
	if inboundBytes+int64(len(payload)) > relay.maximumSessionBytes ||
		inboundPayloadBytes(identity)+sessionContentBytes(session) > session.MaximumResponseBytes {
		if err := relay.enqueueCancellation(
			ctx, tx, session, relayLimitTerminal(session), "operation response exceeded its byte limit", now.UTC(),
		); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("SecondBox response-limit cancellation commit: %w", err)
		}
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.data_plane_frames (
			id,session_id,direction,sequence,payload_hash,payload,payload_bytes,
			priority,state,claim_owner,claim_expires_at,delivery_count,
			created_at,updated_at,delivered_at
		) VALUES ($1,$2,'inbound',$3,$4,$5,$6,0,'delivered',$7,NULL,1,$8,$8,$8)`,
		fmt.Sprintf("%s_in_%d", session.ID, identity.sequence), session.ID,
		identity.sequence, payloadHash, payload, len(payload), input.ConnectionID, now.UTC(),
	); err != nil {
		return false, fmt.Errorf("SecondBox inbound relay frame insert: %w", err)
	}
	if err := applyInboundPayload(ctx, tx, session, identity, int64(len(payload)), relay.retention, now.UTC()); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("SecondBox inbound relay commit: %w", err)
	}
	return true, nil
}

// lockLiveRelayConnectionCredential linearizes data-plane access with connection replacement.
func lockLiveRelayConnectionCredential(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	connectionID string,
) (bool, error) {
	var connectionState string
	err := tx.QueryRow(ctx, `
		SELECT connection.state
		FROM secondbox.runner_connections AS connection
		WHERE connection.id=$1 AND ($2='' OR connection.runner_id=$2)
		FOR SHARE OF connection`,
		connectionID, runnerID,
	).Scan(&connectionState)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("SecondBox relay connection authority lookup: %w", err)
	}
	return connectionState == "active", nil
}

type inboundIdentity struct {
	fence      *runnerv1.AssignmentFence
	operation  string
	stream     string
	sequence   int64
	execOutput *runnerv1.ExecOutput
	execTerm   *runnerv1.ExecTerminal
	ptyOutput  *runnerv1.ExecOutput
	ptyTerm    *runnerv1.ExecTerminal
	fileChunk  *runnerv1.FileChunk
	fileMeta   *runnerv1.FileMetadata
	fileTerm   *runnerv1.FileTerminal
}

func inboundFrameIdentity(message *runnerv1.RunnerToControlPlane) (inboundIdentity, error) {
	if message == nil {
		return inboundIdentity{}, errors.New("SecondBox inbound relay message is required")
	}
	if frame := message.GetExec(); frame != nil {
		if frame.Sequence == 0 || frame.Fence == nil || (frame.GetOutput() == nil && frame.GetTerminal() == nil) {
			return inboundIdentity{}, errors.New("SecondBox inbound Exec frame is incomplete")
		}
		return inboundIdentity{
			fence: frame.Fence, operation: frame.OperationId, stream: frame.StreamId,
			sequence: int64(frame.Sequence), execOutput: frame.GetOutput(), execTerm: frame.GetTerminal(),
		}, nil
	}
	if frame := message.GetPty(); frame != nil {
		if frame.Sequence == 0 || frame.Fence == nil ||
			(frame.GetOutput() == nil && frame.GetTerminal() == nil) {
			return inboundIdentity{}, errors.New("SecondBox inbound Terminal frame is incomplete")
		}
		return inboundIdentity{
			fence: frame.Fence, operation: frame.OperationId, stream: frame.StreamId,
			sequence: int64(frame.Sequence), ptyOutput: frame.GetOutput(), ptyTerm: frame.GetTerminal(),
		}, nil
	}
	if frame := message.GetFile(); frame != nil {
		if frame.Sequence == 0 || frame.Fence == nil ||
			(frame.GetChunk() == nil && frame.GetMetadata() == nil && frame.GetTerminal() == nil) {
			return inboundIdentity{}, errors.New("SecondBox inbound File frame is incomplete")
		}
		return inboundIdentity{
			fence: frame.Fence, operation: frame.OperationId, stream: frame.StreamId,
			sequence: int64(frame.Sequence), fileChunk: frame.GetChunk(),
			fileMeta: frame.GetMetadata(), fileTerm: frame.GetTerminal(),
		}, nil
	}
	return inboundIdentity{}, errors.New("SecondBox inbound relay frame is not Exec, Terminal, or File")
}

func (relay *PostgresFrameRelay) GetDataPlaneSession(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
) (DataPlaneSession, error) {
	return scanDataPlaneSession(relay.pool.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`,
		tenantRef, subjectRef, sessionID))
}

// ExecClientFrame is one ordered public WebSocket control translated into the Runner stream.
func relayDeadlineTerminal(session DataPlaneSession) string {
	if session.Kind == "exec" || session.Kind == "terminal" {
		return runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED.String()
	}
	return runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED.String()
}

func relayLimitTerminal(session DataPlaneSession) string {
	if session.Kind == "exec" || session.Kind == "terminal" {
		return runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED.String()
	}
	return runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED.String()
}

func cancellationMessage(
	session DataPlaneSession,
	sequence uint64,
	reason string,
) *runnerv1.ControlPlaneToRunner {
	fence := &runnerv1.AssignmentFence{
		AssignmentId: session.AssignmentID, SandboxId: session.SandboxID,
		InstanceId: session.InstanceID, SandboxGeneration: uint64(session.Generation),
		FencingToken: bytes.Clone(session.FencingToken),
	}
	if session.Kind == "exec" || session.Kind == "terminal" {
		return &runnerv1.ControlPlaneToRunner{
			Message: &runnerv1.ControlPlaneToRunner_Exec{Exec: &runnerv1.ExecFrame{
				Fence: fence, OperationId: session.ID, StreamId: session.StreamID, Sequence: sequence,
				Correlation: dataPlaneCorrelation(session),
				Payload:     &runnerv1.ExecFrame_Cancel{Cancel: &runnerv1.ExecCancel{Reason: reason}},
			}},
		}
	}
	return &runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_File{File: &runnerv1.FileFrame{
			Fence: fence, OperationId: session.ID, StreamId: session.StreamID, Sequence: sequence,
			Correlation: dataPlaneCorrelation(session),
			Payload:     &runnerv1.FileFrame_Cancel{Cancel: &runnerv1.ExecCancel{Reason: reason}},
		}},
	}
}

func validateDataPlaneAdmission(input DataPlaneAdmission) error {
	if input.ID == "" || input.StreamID == "" || input.TenantRef == "" || input.SandboxID == "" ||
		input.SubjectRef == "" || input.RequestID == "" ||
		input.Generation < 1 || input.DeadlineAt.IsZero() ||
		input.MaximumResponseBytes < 0 || input.MaximumRequestBytes < 0 || input.RequestHash == "" {
		return errors.New("SecondBox data-plane admission identity and bounds are required")
	}
	if input.Kind == "exec" {
		if input.ExecOpen == nil || input.FileOpen != nil {
			return errors.New("SecondBox Exec admission requires exactly one Exec Open")
		}
		if input.DeferResponseCredit && input.StreamWindowBytes < 1 {
			return errors.New("SecondBox streaming Exec admission requires an output window")
		}
	} else if input.Kind == "terminal" {
		if input.ExecOpen == nil || input.FileOpen != nil ||
			!input.ExecOpen.AllocatePty || !input.ExecOpen.Streaming ||
			input.ExecOpen.PtyRows == 0 || input.ExecOpen.PtyColumns == 0 ||
			!input.DeferResponseCredit ||
			(input.StreamWindowBytes < 1 && !input.UseProfileStreamWindow) ||
			input.LeaseID == "" {
			return errors.New("SecondBox Terminal admission requires one leased streaming PTY Open")
		}
	} else if input.Kind == "file" {
		if input.FileOpen == nil || input.ExecOpen != nil {
			return errors.New("SecondBox File admission requires exactly one File Open")
		}
	} else {
		return errors.New("SecondBox data-plane admission kind is invalid")
	}
	return nil
}

func lookupDataPlaneReplay(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	sandboxID string,
	operation string,
	idempotencyKey string,
	requestHash string,
) (DataPlaneSession, bool, error) {
	var priorHash, sessionID string
	err := tx.QueryRow(ctx, `
		SELECT request_hash,id FROM secondbox.data_plane_sessions
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3
		  AND operation=$4 AND idempotency_key=$5`,
		tenantRef, subjectRef, sandboxID, operation, idempotencyKey,
	).Scan(&priorHash, &sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DataPlaneSession{}, false, nil
	}
	if err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox data-plane replay lookup: %w", err)
	}
	if priorHash != requestHash {
		return DataPlaneSession{}, false, ports.ErrIdempotencyConflict
	}
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`,
		tenantRef, subjectRef, sessionID))
	return session, true, err
}

func lockDataPlaneAuthority(
	ctx context.Context,
	tx pgx.Tx,
	input DataPlaneAdmission,
) (DataPlaneSession, contracts.ExecutionPolicy, error) {
	var session DataPlaneSession
	var sandboxState, assignmentState string
	var runnerConnected bool
	var specJSON []byte
	err := tx.QueryRow(ctx, `
		SELECT sandbox.tenant_ref,sandbox.subject_ref,
		       sandbox.profile_revision_id,sandbox.generation,sandbox.state,
		       assignment.id,assignment.instance_id,assignment.runner_id,
		       assignment.fencing_token,assignment.state,revision.spec_json,
		       EXISTS (
		         SELECT 1
		         FROM secondbox.runner_connections AS connection
		         WHERE connection.runner_id=assignment.runner_id
		           AND connection.state='active'
		       )
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.assignments AS assignment
		  ON assignment.instance_id=sandbox.current_instance_id
		  AND assignment.sandbox_id=sandbox.id
		  AND assignment.generation=sandbox.generation
		JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
		WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2 AND sandbox.id=$3
		FOR UPDATE OF sandbox,assignment`,
		input.TenantRef, input.SubjectRef, input.SandboxID,
	).Scan(
		&session.TenantRef, &session.SubjectRef,
		&session.ProfileRevisionID, &session.Generation, &sandboxState,
		&session.AssignmentID, &session.InstanceID, &session.RunnerID,
		&session.FencingToken, &assignmentState, &specJSON, &runnerConnected,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DataPlaneSession{}, contracts.ExecutionPolicy{}, ports.ErrSandboxNotFound
	}
	if err != nil {
		return DataPlaneSession{}, contracts.ExecutionPolicy{}, fmt.Errorf("SecondBox data-plane authority lookup: %w", err)
	}
	if session.Generation != input.Generation {
		return DataPlaneSession{}, contracts.ExecutionPolicy{}, ports.ErrGenerationFenced
	}
	if sandboxState != contracts.SandboxStateReady ||
		assignmentState != "ready" ||
		!runnerConnected {
		return DataPlaneSession{}, contracts.ExecutionPolicy{}, ports.ErrLifecycleUnavailable
	}
	if input.LeaseID != "" {
		var leaseGeneration int64
		var leaseAccount, leaseState string
		var leaseExpiry time.Time
		if err := tx.QueryRow(ctx, `
			SELECT generation,subject_ref,state,expires_at
			FROM secondbox.leases
			WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3 AND id=$4 FOR UPDATE`,
			input.TenantRef, input.SubjectRef, input.SandboxID, input.LeaseID,
		).Scan(&leaseGeneration, &leaseAccount, &leaseState, &leaseExpiry); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return DataPlaneSession{}, contracts.ExecutionPolicy{}, ports.ErrLeaseNotFound
			}
			return DataPlaneSession{}, contracts.ExecutionPolicy{}, fmt.Errorf("SecondBox data-plane Lease lookup: %w", err)
		}
		if leaseGeneration != input.Generation || leaseAccount != input.SubjectRef ||
			leaseState != contracts.LeaseStateActive || !input.Now.Before(leaseExpiry) {
			return DataPlaneSession{}, contracts.ExecutionPolicy{}, ports.ErrLeaseInactive
		}
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return DataPlaneSession{}, contracts.ExecutionPolicy{}, fmt.Errorf("SecondBox data-plane Profile policy decoding: %w", err)
	}
	var sandboxActive, subjectActive, subjectMaximum int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM secondbox.data_plane_sessions
		WHERE sandbox_id=$1 AND state IN ('pending','running','cancelling')`,
		input.SandboxID,
	).Scan(&sandboxActive); err != nil {
		return DataPlaneSession{}, contracts.ExecutionPolicy{}, fmt.Errorf("SecondBox Sandbox operation usage lookup: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT quota.max_concurrent_operations,
		       (SELECT count(*) FROM secondbox.data_plane_sessions
		        WHERE tenant_ref=$1 AND subject_ref=$2
		          AND state IN ('pending','running','cancelling'))
		FROM secondbox.subject_quotas AS quota
		WHERE quota.tenant_ref=$1 AND quota.subject_ref=$2
		FOR UPDATE`,
		input.TenantRef, input.SubjectRef,
	).Scan(&subjectMaximum, &subjectActive); err != nil {
		return DataPlaneSession{}, contracts.ExecutionPolicy{}, fmt.Errorf("SecondBox subject operation quota lookup: %w", err)
	}
	if sandboxActive >= spec.Resources.ConcurrentOperations || subjectActive >= subjectMaximum {
		return DataPlaneSession{}, contracts.ExecutionPolicy{}, ports.ErrQuotaExceeded
	}
	return session, spec.Execution, nil
}

const dataPlaneSessionSelect = `
	SELECT id,stream_id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,assignment_id,
	       instance_id,runner_id,generation,fencing_token,request_id,lease_id,kind,operation,state,
	       deadline_at,maximum_response_bytes,maximum_request_bytes,stream_window_bytes,
	       response_credit_bytes,request_stream_bytes,request_stream_closed,detachable,
	       terminal_detach_seconds,attachment_id,attached_at,detached_at,detach_expires_at,
	       GREATEST((
	           SELECT COALESCE(max(frame.sequence),1)-1
	           FROM secondbox.data_plane_frames AS frame
	           WHERE frame.session_id=secondbox.data_plane_sessions.id
	             AND frame.direction='outbound'
	       ),0) AS next_client_sequence,
	       terminal_kind,
	       terminal_detail,exit_code,signal,spawn_failure_reason,elapsed_milliseconds,
	       limit_bytes,infrastructure_failure_reason,retryable,terminal_message,
	       stdout_bytes,stderr_bytes,content_bytes,metadata_json,
	       created_at,updated_at,completed_at
	FROM secondbox.data_plane_sessions`

type relayRow interface {
	Scan(...any) error
}

func scanDataPlaneSession(row relayRow) (DataPlaneSession, error) {
	var session DataPlaneSession
	var metadataJSON []byte
	err := row.Scan(
		&session.ID, &session.StreamID, &session.TenantRef,
		&session.SubjectRef, &session.SandboxID,
		&session.ProfileRevisionID, &session.AssignmentID, &session.InstanceID,
		&session.RunnerID, &session.Generation, &session.FencingToken,
		&session.RequestID, &session.LeaseID, &session.Kind, &session.Operation,
		&session.State, &session.DeadlineAt,
		&session.MaximumResponseBytes, &session.MaximumRequestBytes, &session.StreamWindowBytes,
		&session.ResponseCreditBytes, &session.RequestStreamBytes, &session.RequestStreamClosed,
		&session.Detachable, &session.TerminalDetachSeconds, &session.AttachmentID,
		&session.AttachedAt, &session.DetachedAt, &session.DetachExpiresAt,
		&session.NextClientSequence,
		&session.TerminalKind, &session.TerminalDetail, &session.ExitCode,
		&session.Signal, &session.SpawnFailureReason, &session.ElapsedMilliseconds,
		&session.LimitBytes, &session.InfrastructureReason, &session.Retryable,
		&session.TerminalMessage, &session.Stdout, &session.Stderr,
		&session.Content, &metadataJSON, &session.CreatedAt, &session.UpdatedAt,
		&session.CompletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DataPlaneSession{}, ErrDataPlaneNotFound
	}
	if err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox data-plane session lookup: %w", err)
	}
	if len(metadataJSON) > 0 && string(metadataJSON) != "{}" {
		session.Metadata = &runnerv1.FileMetadata{}
		if err := protojson.Unmarshal(metadataJSON, session.Metadata); err != nil {
			return DataPlaneSession{}, fmt.Errorf("SecondBox File metadata decoding: %w", err)
		}
	}
	return session, nil
}

func lockInboundSession(
	ctx context.Context,
	tx pgx.Tx,
	runnerID string,
	connectionID string,
	identity inboundIdentity,
) (DataPlaneSession, int64, int64, error) {
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE id=$1 AND stream_id=$2 FOR UPDATE`, identity.operation, identity.stream))
	if err != nil {
		return DataPlaneSession{}, 0, 0, err
	}
	var assignmentState, sandboxState, activeConnection string
	var generation int64
	var fence []byte
	err = tx.QueryRow(ctx, `
		SELECT assignment.state,sandbox.state,sandbox.generation,assignment.fencing_token,
		       connection.state
		FROM secondbox.assignments AS assignment
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=assignment.sandbox_id
		JOIN secondbox.runner_connections AS connection
		  ON connection.id=$2 AND connection.runner_id=$3
		WHERE assignment.id=$1`,
		session.AssignmentID, connectionID, runnerID,
	).Scan(&assignmentState, &sandboxState, &generation, &fence, &activeConnection)
	if err != nil {
		return DataPlaneSession{}, 0, 0, ErrRelayFence
	}
	assignmentAcceptsTerminal := assignmentState == "ready" || assignmentState == "fencing"
	sandboxAcceptsTerminal := sandboxState == contracts.SandboxStateReady ||
		sandboxState == contracts.SandboxStateDraining ||
		sandboxState == contracts.SandboxStateStopping
	if runnerID != session.RunnerID || !assignmentAcceptsTerminal ||
		!sandboxAcceptsTerminal || activeConnection != "active" ||
		generation != session.Generation || identity.fence.AssignmentId != session.AssignmentID ||
		identity.fence.SandboxId != session.SandboxID ||
		identity.fence.InstanceId != session.InstanceID ||
		int64(identity.fence.SandboxGeneration) != session.Generation ||
		!bytes.Equal(identity.fence.FencingToken, session.FencingToken) ||
		!bytes.Equal(fence, session.FencingToken) {
		return DataPlaneSession{}, 0, 0, ErrRelayFence
	}
	var nextSequence, inboundBytes int64
	if err := tx.QueryRow(ctx, `
		SELECT next_inbound_sequence,inbound_bytes
		FROM secondbox.data_plane_sessions WHERE id=$1`,
		session.ID,
	).Scan(&nextSequence, &inboundBytes); err != nil {
		return DataPlaneSession{}, 0, 0, fmt.Errorf("SecondBox inbound sequence lookup: %w", err)
	}
	return session, nextSequence, inboundBytes, nil
}

func applyInboundPayload(
	ctx context.Context,
	tx pgx.Tx,
	session DataPlaneSession,
	identity inboundIdentity,
	frameBytes int64,
	retention time.Duration,
	now time.Time,
) error {
	stdout, stderr, content := []byte{}, []byte{}, []byte{}
	streamOutput := identity.execOutput
	if identity.ptyOutput != nil {
		streamOutput = identity.ptyOutput
	}
	if streamOutput != nil {
		if streamOutput.Channel == runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT {
			stdout = streamOutput.Data
		} else if streamOutput.Channel == runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR {
			stderr = streamOutput.Data
		} else {
			return errors.New("SecondBox Exec output channel is invalid")
		}
	}
	if identity.fileChunk != nil {
		content = identity.fileChunk.Data
		if identity.fileChunk.Offset != uint64(len(session.Content)) {
			return ErrRelaySequence
		}
	}
	metadataJSON := []byte("{}")
	if identity.fileMeta != nil {
		var err error
		metadataJSON, err = (protojson.MarshalOptions{
			EmitUnpopulated: true,
		}).Marshal(identity.fileMeta)
		if err != nil {
			return fmt.Errorf("SecondBox File metadata encoding: %w", err)
		}
	}
	terminalKind, terminalDetail := "", ""
	exitCode, signal := int32(0), int32(0)
	spawnFailureReason, infrastructureReason, terminalMessage := "", "", ""
	elapsedMilliseconds, limitBytes := int64(0), int64(0)
	retryable := false
	terminal := false
	state := session.State
	execTerminal := identity.execTerm
	if identity.ptyTerm != nil {
		execTerminal = identity.ptyTerm
	}
	if execTerminal != nil {
		terminal = true
		terminalKind = execTerminal.Kind.String()
		terminalDetail = execTerminal.SafeDetail
		exitCode = execTerminal.ExitCode
		signal = execTerminal.Signal
		spawnFailureReason = execTerminal.SpawnFailureReason.String()
		elapsedMilliseconds = int64(execTerminal.ElapsedMilliseconds)
		limitBytes = int64(execTerminal.LimitBytes)
		infrastructureReason = execTerminal.InfrastructureFailureReason.String()
		retryable = execTerminal.Retryable
		terminalMessage = execTerminal.Message
		if session.State == "cancelling" && session.TerminalKind != "" {
			terminalKind = session.TerminalKind
			terminalDetail = session.TerminalDetail
			elapsedMilliseconds = session.ElapsedMilliseconds
			limitBytes = session.LimitBytes
		}
		state = "completed"
		if execTerminal.Kind != runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED &&
			execTerminal.Kind != runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED &&
			execTerminal.Kind != runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_CANCELLED &&
			execTerminal.Kind != runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_OUTPUT_EXHAUSTED {
			state = "failed"
		}
	}
	if identity.fileTerm != nil {
		terminal = true
		terminalKind = identity.fileTerm.Kind.String()
		terminalDetail = identity.fileTerm.SafeDetail
		state = "completed"
		if identity.fileTerm.Kind != runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED &&
			identity.fileTerm.Kind != runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND {
			state = "failed"
		}
		if session.State == "cancelling" && session.TerminalKind != "" {
			terminalKind = session.TerminalKind
			terminalDetail = session.TerminalDetail
			state = "completed"
		}
	}
	var completedAt *time.Time
	if terminal {
		value := now.UTC()
		completedAt = &value
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions
		SET next_inbound_sequence=next_inbound_sequence+1,
		    inbound_bytes=inbound_bytes+$2,
		    stdout_bytes=stdout_bytes||$3::bytea,
		    stderr_bytes=stderr_bytes||$4::bytea,
		    content_bytes=content_bytes||$5::bytea,
		    metadata_json=CASE WHEN $6::jsonb='{}'::jsonb THEN metadata_json ELSE $6::jsonb END,
		    state=$7,terminal_kind=CASE WHEN $8='' THEN terminal_kind ELSE $8 END,
		    terminal_detail=CASE WHEN $9='' THEN terminal_detail ELSE $9 END,
		    exit_code=$10,signal=$11,
		    spawn_failure_reason=CASE WHEN $12='' THEN spawn_failure_reason ELSE $12 END,
		    elapsed_milliseconds=CASE WHEN $13=0 THEN elapsed_milliseconds ELSE $13 END,
		    limit_bytes=CASE WHEN $14=0 THEN limit_bytes ELSE $14 END,
		    infrastructure_failure_reason=CASE WHEN $15='' THEN infrastructure_failure_reason ELSE $15 END,
		    retryable=$16,
		    terminal_message=CASE WHEN $17='' THEN terminal_message ELSE $17 END,
		    completed_at=COALESCE($18,completed_at),updated_at=$19,
		    retain_until=CASE WHEN $18 IS NULL THEN retain_until ELSE $20 END
		WHERE id=$1`,
		session.ID, frameBytes, stdout, stderr, content, metadataJSON, state,
		terminalKind, terminalDetail, exitCode, signal, spawnFailureReason,
		elapsedMilliseconds, limitBytes, infrastructureReason, retryable,
		terminalMessage, completedAt, now.UTC(), now.UTC().Add(retention),
	); err != nil {
		return fmt.Errorf("SecondBox inbound session update: %w", err)
	}
	if terminal {
		return closeDataPlaneActivity(ctx, tx, session, now.UTC())
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.activity_sessions SET last_activity_at=$2,updated_at=$2
		WHERE id=$1 AND state='active'`,
		session.ID, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox data-plane activity update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET last_activity_at=$2,updated_at=$2
		WHERE id=$1 AND generation=$3`,
		session.SandboxID, now.UTC(), session.Generation,
	); err != nil {
		return fmt.Errorf("SecondBox sandbox activity update: %w", err)
	}
	return nil
}

func closeDataPlaneActivity(
	ctx context.Context,
	tx pgx.Tx,
	session DataPlaneSession,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.activity_sessions
		SET state='closed',last_activity_at=$2,updated_at=$2,closed_at=$2
		WHERE id=$1 AND state='active'`,
		session.ID, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox data-plane activity close: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET last_activity_at=$2,updated_at=$2
		WHERE id=$1 AND generation=$3`,
		session.SandboxID, now.UTC(), session.Generation,
	); err != nil {
		return fmt.Errorf("SecondBox sandbox activity close: %w", err)
	}
	return nil
}

func touchDataPlaneActivity(
	ctx context.Context,
	tx pgx.Tx,
	session DataPlaneSession,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.activity_sessions SET last_activity_at=$2,updated_at=$2
		WHERE id=$1 AND state='active'`,
		session.ID, now.UTC(),
	); err != nil {
		return fmt.Errorf("SecondBox data-plane activity update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.sandboxes SET last_activity_at=$2,updated_at=$2
		WHERE id=$1 AND generation=$3`,
		session.SandboxID, now.UTC(), session.Generation,
	); err != nil {
		return fmt.Errorf("SecondBox sandbox activity update: %w", err)
	}
	return nil
}

func inboundPayloadBytes(identity inboundIdentity) int64 {
	switch {
	case identity.execOutput != nil:
		return int64(len(identity.execOutput.Data))
	case identity.ptyOutput != nil:
		return int64(len(identity.ptyOutput.Data))
	case identity.fileChunk != nil:
		return int64(len(identity.fileChunk.Data))
	default:
		return 0
	}
}

func sessionContentBytes(session DataPlaneSession) int64 {
	return int64(len(session.Stdout) + len(session.Stderr) + len(session.Content))
}
