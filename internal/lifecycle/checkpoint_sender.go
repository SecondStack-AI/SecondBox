package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/objectstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const checkpointRestoreChunkBytes = 256 * 1024

// CheckpointRestoreSenderConfig binds published metadata to verified object bytes.
type CheckpointRestoreSenderConfig struct {
	DatabaseURL string
	ObjectStore objectstore.Store
}

// CheckpointRestoreSender streams one published checkpoint without exposing object authority.
type CheckpointRestoreSender struct {
	pool    *pgxpool.Pool
	objects objectstore.Store
}

// NewCheckpointRestoreSender validates the restore trust boundary.
func NewCheckpointRestoreSender(
	ctx context.Context,
	config CheckpointRestoreSenderConfig,
) (*CheckpointRestoreSender, error) {
	if config.DatabaseURL == "" || config.ObjectStore == nil {
		return nil, errors.New("SecondBox checkpoint restore sender requires database and object store")
	}
	pool, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox checkpoint restore PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("SecondBox checkpoint restore PostgreSQL readiness: %w", err)
	}
	return &CheckpointRestoreSender{pool: pool, objects: config.ObjectStore}, nil
}

func (sender *CheckpointRestoreSender) Close() {
	sender.pool.Close()
}

// StreamRestore verifies reachability and content before emitting provider-neutral bytes.
func (sender *CheckpointRestoreSender) StreamRestore(
	ctx context.Context,
	assignment *runnerv1.AssignmentCommand,
	emit func(*runnerv1.ControlPlaneToRunner) error,
) (resultErr error) {
	if assignment == nil || assignment.Fence == nil || assignment.Requirements == nil ||
		strings.TrimSpace(assignment.SourceCheckpointId) == "" || emit == nil {
		return errors.New("SecondBox checkpoint restore assignment and emitter are required")
	}
	var (
		checkpointID, storageKey, sha256Hex, sandboxID, state, currentCheckpointID string
		sizeBytes                                                                  int64
		compatibilityJSON                                                          []byte
	)
	err := sender.pool.QueryRow(ctx, `
		SELECT checkpoint.id,checkpoint.storage_key,checkpoint.sha256,checkpoint.size_bytes,
		       checkpoint.compatibility_json,checkpoint.sandbox_id,checkpoint.state,
		       workspace.current_checkpoint_id
		FROM secondbox.workspace_checkpoints AS checkpoint
		JOIN secondbox.workspaces AS workspace ON workspace.id=checkpoint.workspace_id
		WHERE checkpoint.id=$1`,
		assignment.SourceCheckpointId,
	).Scan(
		&checkpointID, &storageKey, &sha256Hex, &sizeBytes, &compatibilityJSON,
		&sandboxID, &state, &currentCheckpointID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("SecondBox checkpoint restore source is not published")
	}
	if err != nil {
		return fmt.Errorf("SecondBox checkpoint restore metadata lookup: %w", err)
	}
	if state != "published" || currentCheckpointID != checkpointID ||
		sandboxID != assignment.Fence.SandboxId || storageKey == "" ||
		sizeBytes <= 0 || !catalogDigestPattern.MatchString("sha256:"+sha256Hex) {
		return errors.New("SecondBox checkpoint restore source is not current verified authority")
	}
	var compatibility map[string]string
	if err := json.Unmarshal(compatibilityJSON, &compatibility); err != nil {
		return fmt.Errorf("SecondBox checkpoint restore compatibility decoding: %w", err)
	}
	if compatibility["architecture"] != assignment.Requirements.Architecture ||
		compatibility["backend"] != "firecracker" ||
		compatibility["profileRevisionId"] != assignment.ProfileRevisionId ||
		compatibility["workspaceFormat"] != "ext4" {
		return errors.New("SecondBox checkpoint restore compatibility does not match assignment")
	}
	body, evidence, err := sender.objects.GetVerified(ctx, storageKey, objectstore.Evidence{
		SHA256: sha256Hex, SizeBytes: sizeBytes,
	})
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, body.Close())
	}()
	begin := &runnerv1.RestoreBegin{
		Fence: assignment.Fence, CheckpointId: checkpointID, StorageObjectId: storageKey,
		Sha256: sha256Hex, SizeBytes: uint64(sizeBytes), Compatibility: compatibility,
		DeadlineUnixMs: assignment.DeadlineUnixMs, Correlation: assignment.Correlation,
	}
	if err := emit(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_RestoreBegin{RestoreBegin: begin},
	}); err != nil {
		return err
	}
	buffer := make([]byte, checkpointRestoreChunkBytes)
	var offset uint64
	for {
		count, readErr := body.Read(buffer)
		if count != 0 {
			chunk := &runnerv1.RestoreChunk{
				Fence: assignment.Fence, CheckpointId: checkpointID,
				StorageObjectId: storageKey, Offset: offset,
				Data: append([]byte(nil), buffer[:count]...),
			}
			if err := emit(&runnerv1.ControlPlaneToRunner{
				Message: &runnerv1.ControlPlaneToRunner_RestoreChunk{RestoreChunk: chunk},
			}); err != nil {
				return err
			}
			offset += uint64(count)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("SecondBox checkpoint restore object read: %w", readErr)
		}
	}
	if offset != uint64(evidence.SizeBytes) || offset != uint64(sizeBytes) {
		return errors.New("SecondBox checkpoint restore object size changed during streaming")
	}
	return emit(&runnerv1.ControlPlaneToRunner{
		Message: &runnerv1.ControlPlaneToRunner_RestoreChunk{
			RestoreChunk: &runnerv1.RestoreChunk{
				Fence: assignment.Fence, CheckpointId: checkpointID,
				StorageObjectId: storageKey, Offset: offset, EndOfObject: true,
			},
		},
	})
}
