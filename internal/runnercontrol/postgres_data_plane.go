package runnercontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	ErrDataPlaneFence        = errors.New("SecondBox data-plane fence is stale")
	ErrDataPlaneSequence     = errors.New("SecondBox data-plane sequence is invalid")
	ErrDataPlaneFrameLimit   = errors.New("SecondBox data-plane frame limit exceeded")
	ErrDataPlaneSessionLimit = errors.New("SecondBox data-plane session limit exceeded")
	ErrDataPlaneNotFound     = errors.New("SecondBox data-plane session not found")
	ErrDataPlaneDeadline     = errors.New("SecondBox data-plane operation deadline exceeded")
	ErrTerminalAttached      = errors.New("SecondBox Terminal session already has an active attachment")
	ErrTerminalDetached      = errors.New("SecondBox Terminal attachment is inactive")
	ErrTerminalReplayEvicted = errors.New("SecondBox Terminal replay sequence was evicted")
	ErrFilePermission        = errors.New("SecondBox File operation permission denied")
	ErrFileChecksum          = errors.New("SecondBox File checksum mismatch")
)

// PostgresDataPlaneStoreConfig contains explicit durability and payload bounds.
type PostgresDataPlaneStoreConfig struct {
	DatabaseURL         string
	Retention           time.Duration
	MaximumSessionBytes int64
}

// PostgresDataPlaneStore owns durable data-plane admission and outcome state.
type PostgresDataPlaneStore struct {
	pool                *pgxpool.Pool
	retention           time.Duration
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
	CredentialDigest        []byte
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
	ID                       string
	StreamID                 string
	TenantRef                string
	SubjectRef               string
	SandboxID                string
	ProfileRevisionID        string
	AssignmentID             string
	InstanceID               string
	RunnerID                 string
	Generation               int64
	FencingToken             []byte
	RequestID                string
	LeaseID                  string
	Kind                     string
	Operation                string
	State                    string
	DeadlineAt               time.Time
	MaximumResponseBytes     int64
	MaximumRequestBytes      int64
	StreamWindowBytes        int64
	ResponseCreditBytes      int64
	RequestStreamBytes       int64
	RequestStreamClosed      bool
	Detachable               bool
	TerminalDetachSeconds    int64
	AttachmentID             string
	AttachedAt               *time.Time
	DetachedAt               *time.Time
	DetachExpiresAt          *time.Time
	OutboundBytes            int64
	InboundBytes             int64
	NextClientSequence       int64
	NextInboundSequence      int64
	NextOutboundSequence     int64
	TerminalKind             string
	TerminalDetail           string
	ExitCode                 int32
	Signal                   int32
	SpawnFailureReason       string
	ElapsedMilliseconds      int64
	LimitBytes               int64
	InfrastructureReason     string
	Retryable                bool
	TerminalMessage          string
	Stdout                   []byte
	Stderr                   []byte
	Content                  []byte
	Metadata                 *runnerv1.FileMetadata
	CreatedAt                time.Time
	UpdatedAt                time.Time
	CompletedAt              *time.Time
	RetainUntil              time.Time
	RequestJSON              []byte `json:"-"`
	Transport                string
	DataPlaneAddress         string
	DataPlaneCertificateSPKI string
}

// DataPlaneCompletion is one bounded Exec or File completion persisted without
// retaining any intermediate transport frame.
type DataPlaneCompletion struct {
	TenantRef  string
	SubjectRef string
	SessionID  string
	Exec       *runnerv1.ExecBufferedResult
	File       *FileCompletion
	Now        time.Time
}

type FileCompletion struct {
	Metadata *runnerv1.FileMetadata
	Content  []byte
	Terminal *runnerv1.FileTerminal
}

type dataPlaneResult struct {
	Stdout  []byte `json:"stdout"`
	Stderr  []byte `json:"stderr"`
	Content []byte `json:"content"`
}

// DirectDataPlaneConsumption atomically spends one admitted direct credential.
type DirectDataPlaneConsumption struct {
	SessionID        string
	AssignmentID     string
	Generation       int64
	FencingToken     []byte
	CredentialDigest []byte
	Now              time.Time
}

// NewPostgresDataPlaneStore opens the durable data-plane authority.
func NewPostgresDataPlaneStore(
	ctx context.Context,
	config PostgresDataPlaneStoreConfig,
) (*PostgresDataPlaneStore, error) {
	if config.DatabaseURL == "" || config.Retention <= 0 || config.MaximumSessionBytes < 1024 {
		return nil, errors.New("SecondBox PostgreSQL data-plane store requires database, retention, and byte bounds")
	}
	pool, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox data-plane PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("SecondBox data-plane PostgreSQL readiness: %w", err)
	}
	return &PostgresDataPlaneStore{
		pool: pool, retention: config.Retention,
		maximumSessionBytes: config.MaximumSessionBytes,
	}, nil
}

// Close releases the data-plane store pool.
func (store *PostgresDataPlaneStore) Close() {
	store.pool.Close()
}

// AdmitDataPlane transactionally resolves current assignment authority and creates one session.
func (store *PostgresDataPlaneStore) AdmitDataPlane(
	ctx context.Context,
	input DataPlaneAdmission,
) (DataPlaneSession, bool, error) {
	if err := validateDataPlaneAdmission(input); err != nil {
		return DataPlaneSession{}, false, err
	}
	tx, err := store.pool.Begin(ctx)
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
			return DataPlaneSession{}, false, errors.Join(
				ports.ErrInvalidRequest,
				errors.New("SecondBox pinned Profile does not permit detached Terminal sessions"),
			)
		}
	}
	if session.Transport != contracts.DataPlaneTransportProxied &&
		session.Transport != contracts.DataPlaneTransportDirect {
		return DataPlaneSession{}, false, errors.New("SecondBox pinned Profile data-plane transport is invalid")
	}
	if session.Transport == contracts.DataPlaneTransportDirect &&
		len(input.CredentialDigest) != sha256.Size {
		return DataPlaneSession{}, false, errors.New("SecondBox direct data-plane credential digest is invalid")
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
		return DataPlaneSession{}, false, errors.Join(
			ports.ErrInvalidRequest,
			errors.New("SecondBox data-plane deadline exceeds the pinned Profile"),
		)
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
	if input.MaximumResponseBytes > store.maximumSessionBytes {
		return DataPlaneSession{}, false, ErrDataPlaneSessionLimit
	}
	requestJSON, err := json.Marshal(input.Request)
	if err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox data-plane request encoding: %w", err)
	}
	if len(requestJSON) == 0 {
		requestJSON = []byte("{}")
	}
	resultJSON, err := json.Marshal(dataPlaneResult{Stdout: []byte{}, Stderr: []byte{}, Content: []byte{}})
	if err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox data-plane result encoding: %w", err)
	}
	nextOutboundSequence := 1
	if session.Kind == "terminal" {
		nextOutboundSequence = 2
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.data_plane_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,assignment_id,instance_id,runner_id,generation,fencing_token,request_id,lease_id,kind,operation,stream_id,state,priority,idempotency_key,request_hash,deadline_at,maximum_response_bytes,maximum_request_bytes,stream_window_bytes,response_credit_bytes,request_stream_bytes,request_stream_closed,detachable,terminal_detach_seconds,attachment_id,attached_at,detached_at,detach_expires_at,outbound_bytes,inbound_bytes,next_inbound_sequence,terminal_kind,terminal_detail,exit_code,signal,spawn_failure_reason,elapsed_milliseconds,limit_bytes,infrastructure_failure_reason,retryable,terminal_message,result_json,metadata_json,request_json,created_at,updated_at,completed_at,retain_until,next_outbound_sequence
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'pending',$16,$17,$18,$19,$20,$21,$22,0,0,false,$23,$24,'',NULL,NULL,NULL,0,0,1,'','',0,0,'',0,0,'',false,'',$25,'{}',$26,$27,$27,NULL,$28,$29
		)`,
		session.ID, session.TenantRef, session.SubjectRef, session.SandboxID, session.ProfileRevisionID, session.AssignmentID, session.InstanceID, session.RunnerID, session.Generation, session.FencingToken, session.RequestID, session.LeaseID, session.Kind, session.Operation, session.StreamID, input.Priority, input.IdempotencyKey, input.RequestHash, session.DeadlineAt, session.MaximumResponseBytes, session.MaximumRequestBytes, session.StreamWindowBytes, session.Detachable, session.TerminalDetachSeconds, resultJSON, requestJSON, session.CreatedAt, session.CreatedAt.Add(store.retention), nextOutboundSequence,
	); err != nil {
		return DataPlaneSession{}, false, fmt.Errorf("SecondBox data-plane session insert: %w", err)
	}
	if session.Transport == contracts.DataPlaneTransportDirect {
		message := directDataPlaneOpenMessage(session, input.CredentialDigest)
		payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
		if err != nil {
			return DataPlaneSession{}, false, fmt.Errorf("SecondBox direct data-plane Open encoding: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO secondbox.runner_commands (
				id,runner_id,assignment_id,kind,payload,state,target_connection_id,
				delivery_count,created_at,updated_at,delivered_at
			) VALUES ($1,$2,$3,'data-plane-direct',$4,'pending','',0,$5,$5,NULL)`,
			session.ID+"_direct_open", session.RunnerID, session.AssignmentID,
			payload, session.CreatedAt,
		); err != nil {
			return DataPlaneSession{}, false, fmt.Errorf("SecondBox direct data-plane Open insert: %w", err)
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

func directDataPlaneOpenMessage(
	session DataPlaneSession,
	credentialDigest []byte,
) *runnerv1.ControlPlaneToRunner {
	kind := runnerv1.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_EXEC
	switch session.Kind {
	case "file":
		kind = runnerv1.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_FILE
	case "terminal":
		kind = runnerv1.DataPlaneSessionKind_DATA_PLANE_SESSION_KIND_PTY
	}
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
				Kind:              kind,
				DeadlineUnixMs:    uint64(session.DeadlineAt.UnixMilli()),
				CredentialDigest:  bytes.Clone(credentialDigest),
				StreamWindowBytes: uint64(session.StreamWindowBytes),
			},
		},
	}
}

func dataPlaneCorrelation(session DataPlaneSession) *runnerv1.Correlation {
	return &runnerv1.Correlation{
		RequestId: session.RequestID, OperationId: session.ID,
		SandboxId: session.SandboxID, InstanceId: session.InstanceID,
		SandboxGeneration: uint64(session.Generation), AssignmentId: session.AssignmentID,
		LeaseId: session.LeaseID, RunnerId: session.RunnerID,
	}
}

func (store *PostgresDataPlaneStore) GetDataPlaneSession(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
) (DataPlaneSession, error) {
	session, err := scanDataPlaneSession(store.pool.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`,
		tenantRef, subjectRef, sessionID))
	if err != nil {
		return DataPlaneSession{}, err
	}
	if err := hydrateDataPlaneTransport(ctx, store.pool, &session); err != nil {
		return DataPlaneSession{}, err
	}
	return session, nil
}

// dataPlaneDeadlineTerminal projects a transport deadline without synthesizing an exit code.
func dataPlaneDeadlineTerminal(session DataPlaneSession) string {
	if session.Kind == "exec" || session.Kind == "terminal" {
		return runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_DEADLINE_EXCEEDED.String()
	}
	return runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED.String()
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
	if err == nil {
		err = hydrateDataPlaneTransport(ctx, tx, &session)
	}
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
	var encodedDataPlaneEndpoint string
	var specJSON []byte
	err := tx.QueryRow(ctx, `
		SELECT sandbox.tenant_ref,sandbox.subject_ref,
		       sandbox.profile_revision_id,sandbox.generation,sandbox.state,
		       assignment.id,assignment.instance_id,assignment.runner_id,
		       assignment.fencing_token,assignment.state,revision.spec_json,
		       COALESCE(runner.data_plane_address,''),
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
		LEFT JOIN secondbox.runners AS runner ON runner.id=assignment.runner_id
		WHERE sandbox.tenant_ref=$1 AND sandbox.subject_ref=$2 AND sandbox.id=$3
		FOR UPDATE OF sandbox,assignment`,
		input.TenantRef, input.SubjectRef, input.SandboxID,
	).Scan(
		&session.TenantRef, &session.SubjectRef,
		&session.ProfileRevisionID, &session.Generation, &sandboxState,
		&session.AssignmentID, &session.InstanceID, &session.RunnerID,
		&session.FencingToken, &assignmentState, &specJSON, &encodedDataPlaneEndpoint,
		&runnerConnected,
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
	session.Transport = dataPlaneTransport(spec.Execution.DataPlaneTransport, input.Kind, input.Operation)
	if session.Transport == contracts.DataPlaneTransportDirect {
		endpoint, err := decodeDataPlaneEndpoint(encodedDataPlaneEndpoint)
		if err != nil {
			return DataPlaneSession{}, contracts.ExecutionPolicy{}, ports.ErrLifecycleUnavailable
		}
		session.DataPlaneAddress = endpoint.Address
		session.DataPlaneCertificateSPKI = endpoint.CertificateSPKISHA256
	}
	var tenantState string
	var tenantExpiresAt *time.Time
	var tenantActive, tenantMaximum int64
	if err := tx.QueryRow(ctx, `
		SELECT tenant.state,tenant.expires_at,quota.max_concurrent_operations,
		       (SELECT count(*) FROM secondbox.data_plane_sessions
		        WHERE tenant_ref=$1 AND state IN ('pending','running','cancelling'))
		FROM secondbox.tenants AS tenant
		JOIN secondbox.tenant_quotas AS quota ON quota.tenant_ref=tenant.ref
		WHERE tenant.ref=$1
		FOR UPDATE OF tenant,quota`, input.TenantRef,
	).Scan(&tenantState, &tenantExpiresAt, &tenantMaximum, &tenantActive); err != nil {
		return DataPlaneSession{}, contracts.ExecutionPolicy{}, fmt.Errorf("SecondBox Tenant operation quota lookup: %w", err)
	}
	if tenantState == contracts.TenantStateSuspended {
		return DataPlaneSession{}, contracts.ExecutionPolicy{}, ports.ErrTenantSuspended
	}
	if tenantState == contracts.TenantStateExpired ||
		tenantExpiresAt != nil && !tenantExpiresAt.After(input.Now.UTC()) {
		return DataPlaneSession{}, contracts.ExecutionPolicy{}, ports.ErrResourceExpired
	}
	if tenantState != contracts.TenantStateActive {
		return DataPlaneSession{}, contracts.ExecutionPolicy{}, ports.ErrInvalidLifecycleTransition
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
	if sandboxActive >= spec.Resources.ConcurrentOperations || subjectActive >= subjectMaximum ||
		tenantActive >= tenantMaximum {
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
	       outbound_bytes,inbound_bytes,
	       GREATEST(next_outbound_sequence-2,0) AS next_client_sequence,
	       next_inbound_sequence,next_outbound_sequence,
	       terminal_kind,
	       terminal_detail,exit_code,signal,spawn_failure_reason,elapsed_milliseconds,
	       limit_bytes,infrastructure_failure_reason,retryable,terminal_message,
	       result_json,metadata_json,
	       request_json,
	       created_at,updated_at,completed_at,retain_until
	FROM secondbox.data_plane_sessions`

type dataPlaneRow interface {
	Scan(...any) error
}

func scanDataPlaneSession(row dataPlaneRow) (DataPlaneSession, error) {
	var session DataPlaneSession
	var resultJSON, metadataJSON []byte
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
		&session.OutboundBytes, &session.InboundBytes,
		&session.NextClientSequence,
		&session.NextInboundSequence,
		&session.NextOutboundSequence,
		&session.TerminalKind, &session.TerminalDetail, &session.ExitCode,
		&session.Signal, &session.SpawnFailureReason, &session.ElapsedMilliseconds,
		&session.LimitBytes, &session.InfrastructureReason, &session.Retryable,
		&session.TerminalMessage, &resultJSON, &metadataJSON, &session.RequestJSON,
		&session.CreatedAt, &session.UpdatedAt,
		&session.CompletedAt, &session.RetainUntil,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DataPlaneSession{}, ErrDataPlaneNotFound
	}
	if err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox data-plane session lookup: %w", err)
	}
	var result dataPlaneResult
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox data-plane result decoding: %w", err)
	}
	session.Stdout, session.Stderr, session.Content = result.Stdout, result.Stderr, result.Content
	if len(metadataJSON) > 0 && string(metadataJSON) != "{}" {
		session.Metadata = &runnerv1.FileMetadata{}
		if err := protojson.Unmarshal(metadataJSON, session.Metadata); err != nil {
			return DataPlaneSession{}, fmt.Errorf("SecondBox File metadata decoding: %w", err)
		}
	}
	return session, nil
}

type dataPlaneQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func hydrateDataPlaneTransport(
	ctx context.Context,
	queryer dataPlaneQueryer,
	session *DataPlaneSession,
) error {
	var specJSON []byte
	var encodedDataPlaneEndpoint string
	if err := queryer.QueryRow(ctx, `
		SELECT revision.spec_json,COALESCE(runner.data_plane_address,'')
		FROM secondbox.profile_revisions AS revision
		LEFT JOIN secondbox.runners AS runner ON runner.id=$2
		WHERE revision.id=$1`,
		session.ProfileRevisionID, session.RunnerID,
	).Scan(&specJSON, &encodedDataPlaneEndpoint); err != nil {
		return fmt.Errorf("SecondBox data-plane transport lookup: %w", err)
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return fmt.Errorf("SecondBox data-plane transport Profile decoding: %w", err)
	}
	session.Transport = dataPlaneTransport(
		spec.Execution.DataPlaneTransport, session.Kind, session.Operation,
	)
	if session.Transport == contracts.DataPlaneTransportDirect {
		endpoint, err := decodeDataPlaneEndpoint(encodedDataPlaneEndpoint)
		if err != nil {
			return ports.ErrLifecycleUnavailable
		}
		session.DataPlaneAddress = endpoint.Address
		session.DataPlaneCertificateSPKI = endpoint.CertificateSPKISHA256
	}
	return nil
}

// Buffered Exec returns its single bounded completion on the Runner control
// connection; Profile transport selection applies to streaming sessions.
func dataPlaneTransport(profileTransport string, kind string, operation string) string {
	if kind == "exec" && operation == "exec" {
		return contracts.DataPlaneTransportProxied
	}
	return profileTransport
}

type dataPlaneProjection struct {
	execResult *runnerv1.ExecBufferedResult
	execTerm   *runnerv1.ExecTerminal
	ptyTerm    *runnerv1.ExecTerminal
	fileChunk  *runnerv1.FileChunk
	fileMeta   *runnerv1.FileMetadata
	fileTerm   *runnerv1.FileTerminal
}

func applyDataPlaneProjection(
	ctx context.Context,
	tx pgx.Tx,
	session DataPlaneSession,
	projection dataPlaneProjection,
	frameBytes int64,
	retention time.Duration,
	now time.Time,
) error {
	result := dataPlaneResult{
		Stdout: bytes.Clone(session.Stdout), Stderr: bytes.Clone(session.Stderr),
		Content: bytes.Clone(session.Content),
	}
	if projection.execResult != nil {
		result.Stdout = bytes.Clone(projection.execResult.Stdout)
		result.Stderr = bytes.Clone(projection.execResult.Stderr)
		result.Content = []byte{}
	}
	if projection.fileChunk != nil {
		if projection.fileChunk.Offset != 0 {
			return ErrDataPlaneSequence
		}
		result.Stdout, result.Stderr = []byte{}, []byte{}
		result.Content = bytes.Clone(projection.fileChunk.Data)
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("SecondBox data-plane result encoding: %w", err)
	}
	metadataJSON := []byte("{}")
	if projection.fileMeta != nil {
		var err error
		metadataJSON, err = (protojson.MarshalOptions{
			EmitUnpopulated: true,
		}).Marshal(projection.fileMeta)
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
	execTerminal := projection.execTerm
	if projection.execResult != nil {
		execTerminal = projection.execResult.Terminal
	}
	if projection.ptyTerm != nil {
		execTerminal = projection.ptyTerm
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
	if projection.fileTerm != nil {
		terminal = true
		terminalKind = projection.fileTerm.Kind.String()
		terminalDetail = projection.fileTerm.SafeDetail
		state = "completed"
		if projection.fileTerm.Kind != runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED &&
			projection.fileTerm.Kind != runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND {
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
		    result_json=$3,
		    metadata_json=CASE WHEN $4::jsonb='{}'::jsonb THEN metadata_json ELSE $4::jsonb END,
		    state=$5,terminal_kind=CASE WHEN $6='' THEN terminal_kind ELSE $6 END,
		    terminal_detail=CASE WHEN $7='' THEN terminal_detail ELSE $7 END,
		    exit_code=$8,signal=$9,
		    spawn_failure_reason=CASE WHEN $10='' THEN spawn_failure_reason ELSE $10 END,
		    elapsed_milliseconds=CASE WHEN $11=0 THEN elapsed_milliseconds ELSE $11 END,
		    limit_bytes=CASE WHEN $12=0 THEN limit_bytes ELSE $12 END,
		    infrastructure_failure_reason=CASE WHEN $13='' THEN infrastructure_failure_reason ELSE $13 END,
		    retryable=$14,
		    terminal_message=CASE WHEN $15='' THEN terminal_message ELSE $15 END,
		    completed_at=COALESCE($16,completed_at),updated_at=$17,
		    retain_until=CASE WHEN $16 IS NULL THEN retain_until ELSE $18 END
		WHERE id=$1`,
		session.ID, frameBytes, resultJSON, metadataJSON, state,
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
