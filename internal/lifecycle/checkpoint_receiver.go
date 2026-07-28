package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/objectstore"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var checkpointIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// CheckpointReceiverConfig binds restart-safe spool and immutable publication authority.
type CheckpointReceiverConfig struct {
	DatabaseURL    string
	SpoolDirectory string
	ObjectStore    objectstore.Store
	LifecycleStore ports.LifecycleStore
}

// CheckpointReceiver ingests opaque runner bytes and publishes verified checkpoints.
type CheckpointReceiver struct {
	pool      *pgxpool.Pool
	spool     string
	objects   objectstore.Store
	lifecycle ports.LifecycleStore
}

// NewCheckpointReceiver validates the checkpoint ingestion trust boundary.
func NewCheckpointReceiver(
	ctx context.Context,
	config CheckpointReceiverConfig,
) (*CheckpointReceiver, error) {
	if config.DatabaseURL == "" || !filepath.IsAbs(config.SpoolDirectory) ||
		config.ObjectStore == nil || config.LifecycleStore == nil {
		return nil, errors.New("SecondBox checkpoint receiver requires database, absolute spool, object store, and lifecycle store")
	}
	info, err := os.Stat(config.SpoolDirectory)
	if err != nil {
		return nil, fmt.Errorf("SecondBox checkpoint spool is unavailable: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("SecondBox checkpoint spool path is not a directory")
	}
	pool, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox checkpoint receiver PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("SecondBox checkpoint receiver PostgreSQL readiness: %w", err)
	}
	return &CheckpointReceiver{
		pool: pool, spool: config.SpoolDirectory,
		objects: config.ObjectStore, lifecycle: config.LifecycleStore,
	}, nil
}

func (receiver *CheckpointReceiver) Close() {
	receiver.pool.Close()
}

// ReceiveCheckpoint validates one fenced chunk or terminal result.
func (receiver *CheckpointReceiver) ReceiveCheckpoint(
	ctx context.Context,
	event runnercontrol.Event,
	now time.Time,
) error {
	if event.Kind != runnercontrol.EventCheckpoint || event.Message == nil {
		return errors.New("SecondBox checkpoint receiver requires a checkpoint event")
	}
	if chunk := event.Message.GetCheckpointChunk(); chunk != nil {
		if err := receiver.validateAuthority(
			ctx, event.RunnerID, chunk.Fence, chunk.CheckpointId, chunk.StorageObjectId,
		); err != nil {
			return err
		}
		return receiver.writeChunk(chunk.CheckpointId, chunk.Offset, chunk.Data)
	}
	if result := event.Message.GetCheckpointResult(); result != nil {
		if err := receiver.validateAuthority(
			ctx, event.RunnerID, result.Fence, result.CheckpointId, result.StorageObjectId,
		); err != nil {
			return err
		}
		if result.Terminal != runnerv1.CheckpointTerminalKind_CHECKPOINT_TERMINAL_KIND_CREATED {
			return nil
		}
		return receiver.publish(ctx, result, now.UTC())
	}
	return errors.New("SecondBox checkpoint receiver event has no checkpoint frame")
}

func (receiver *CheckpointReceiver) validateAuthority(
	ctx context.Context,
	runnerID string,
	fence interface {
		GetAssignmentId() string
		GetSandboxId() string
		GetInstanceId() string
		GetSandboxGeneration() uint64
		GetFencingToken() []byte
	},
	checkpointID string,
	storageObjectID string,
) error {
	if fence == nil || !checkpointIDPattern.MatchString(checkpointID) || storageObjectID == "" {
		return errors.New("SecondBox checkpoint frame identity is invalid")
	}
	var storedRunnerID, assignmentID, instanceID, sandboxID, storedCheckpointID, storedObjectID string
	var generation int64
	var token []byte
	err := receiver.pool.QueryRow(ctx, `
		SELECT runner_id,assignment_id,instance_id,sandbox_id,generation,fencing_token,
		       checkpoint_id,storage_object_id
		FROM secondbox.lifecycle_effects
		WHERE checkpoint_id=$1 AND kind='checkpoint'`,
		checkpointID,
	).Scan(
		&storedRunnerID, &assignmentID, &instanceID, &sandboxID, &generation, &token,
		&storedCheckpointID, &storedObjectID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return runnercontrol.ErrStaleAssignmentEvidence
	}
	if err != nil {
		return fmt.Errorf("SecondBox checkpoint effect authority lookup: %w", err)
	}
	if storedRunnerID != runnerID || assignmentID != fence.GetAssignmentId() ||
		instanceID != fence.GetInstanceId() || sandboxID != fence.GetSandboxId() ||
		generation != int64(fence.GetSandboxGeneration()) ||
		!bytes.Equal(token, fence.GetFencingToken()) ||
		storedCheckpointID != checkpointID || storedObjectID != storageObjectID {
		return runnercontrol.ErrStaleAssignmentEvidence
	}
	return nil
}

func (receiver *CheckpointReceiver) writeChunk(
	checkpointID string,
	offset uint64,
	data []byte,
) error {
	path := filepath.Join(receiver.spool, checkpointID+".partial")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("SecondBox checkpoint spool open failed: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("SecondBox checkpoint spool stat failed: %w", err)
	}
	size := uint64(info.Size())
	if offset > size {
		return errors.New("SecondBox checkpoint chunk offset has a gap")
	}
	if offset < size {
		existing := make([]byte, len(data))
		count, err := file.ReadAt(existing, int64(offset))
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("SecondBox checkpoint replay read failed: %w", err)
		}
		if count != len(data) || !bytes.Equal(existing, data) {
			return errors.New("SecondBox checkpoint chunk replay content differs")
		}
		return nil
	}
	if _, err := file.WriteAt(data, int64(offset)); err != nil {
		return fmt.Errorf("SecondBox checkpoint spool append failed: %w", err)
	}
	return file.Sync()
}

func (receiver *CheckpointReceiver) publish(
	ctx context.Context,
	result *runnerv1.CheckpointResult,
	now time.Time,
) error {
	if !catalogDigestPattern.MatchString("sha256:"+result.Sha256) ||
		result.SizeBytes == 0 || len(result.Compatibility) == 0 {
		return errors.New("SecondBox checkpoint result integrity evidence is invalid")
	}
	var state, workspaceID string
	var generation int64
	var payloadJSON []byte
	if err := receiver.pool.QueryRow(ctx, `
		SELECT state,generation,payload_json,payload_json->>'workspaceId'
		FROM secondbox.lifecycle_effects WHERE checkpoint_id=$1`,
		result.CheckpointId,
	).Scan(&state, &generation, &payloadJSON, &workspaceID); err != nil {
		return fmt.Errorf("SecondBox checkpoint publication effect lookup: %w", err)
	}
	if state == "published" {
		return nil
	}
	var payload struct {
		RetainUntil      time.Time `json:"retainUntil"`
		MaximumSizeBytes int64     `json:"maximumSizeBytes"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return fmt.Errorf("SecondBox checkpoint effect payload decoding: %w", err)
	}
	if result.SizeBytes > uint64(payload.MaximumSizeBytes) ||
		int64(result.SizeBytes) < 0 {
		return errors.New("SecondBox checkpoint result exceeds effect size bound")
	}
	path := filepath.Join(receiver.spool, result.CheckpointId+".partial")
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("SecondBox checkpoint spool open for publication failed: %w", err)
	}
	hasher := sha256.New()
	size, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if size != int64(result.SizeBytes) || actualSHA256 != result.Sha256 {
		return errors.New("SecondBox checkpoint spool does not match runner integrity evidence")
	}
	file, err = os.Open(path)
	if err != nil {
		return fmt.Errorf("SecondBox checkpoint spool reopen failed: %w", err)
	}
	_, putErr := receiver.objects.PutImmutable(
		ctx, result.StorageObjectId, file, size, actualSHA256,
	)
	closeErr = file.Close()
	if putErr != nil || closeErr != nil {
		return errors.Join(putErr, closeErr)
	}
	checkpoint := contracts.WorkspaceCheckpoint{
		ID: result.CheckpointId, SandboxID: result.Fence.SandboxId,
		WorkspaceID: workspaceID, SourceGeneration: generation,
		SHA256: actualSHA256, SizeBytes: size,
		Compatibility: result.Compatibility, RetainUntil: payload.RetainUntil, CreatedAt: now,
	}
	publication := ports.CheckpointPublicationInput{
		Checkpoint: checkpoint, StorageKey: result.StorageObjectId,
		ExpectedWorkspaceGeneration: generation,
	}
	if _, err := receiver.lifecycle.StageCheckpoint(ctx, publication); err != nil {
		return err
	}
	if _, err := receiver.lifecycle.VerifyCheckpoint(ctx, publication, now); err != nil {
		return err
	}
	if _, err := receiver.lifecycle.PublishCheckpoint(ctx, publication, now); err != nil {
		return err
	}
	evidenceJSON, err := json.Marshal(map[string]any{
		"sha256": actualSHA256, "sizeBytes": size,
		"compatibility":             result.Compatibility,
		"terminationEvidenceDigest": result.TerminationEvidenceDigest,
	})
	if err != nil {
		return fmt.Errorf("SecondBox checkpoint publication evidence encoding: %w", err)
	}
	if _, err := receiver.pool.Exec(ctx, `
		UPDATE secondbox.lifecycle_effects
		SET state='published',evidence_json=$2,updated_at=$3
		WHERE checkpoint_id=$1 AND state='queued'`,
		result.CheckpointId, evidenceJSON, now,
	); err != nil {
		return fmt.Errorf("SecondBox checkpoint effect publication update: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("SecondBox checkpoint spool cleanup failed: %w", err)
	}
	return nil
}
