package runnercontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func (store *PostgresDataPlaneStore) StartDataPlaneSession(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sessionID string,
	now time.Time,
) (DataPlaneSession, error) {
	if tenantRef == "" || subjectRef == "" || sessionID == "" || now.IsZero() {
		return DataPlaneSession{}, errors.New("SecondBox proxied data-plane start is incomplete")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox proxied data-plane start transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, tenantRef, subjectRef); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox proxied data-plane start quota lock: %w", err)
	}
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		tenantRef, subjectRef, sessionID,
	))
	if err != nil {
		return DataPlaneSession{}, err
	}
	if err := hydrateDataPlaneTransport(ctx, tx, &session); err != nil {
		return DataPlaneSession{}, err
	}
	if session.Transport != contracts.DataPlaneTransportProxied || session.State != "pending" ||
		!now.UTC().Before(session.DeadlineAt) {
		return DataPlaneSession{}, ports.ErrAuthorizationDenied
	}
	if err := validateDirectDataPlaneAuthority(ctx, tx, session, now.UTC()); err != nil {
		return DataPlaneSession{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions SET state='running',updated_at=$2
		WHERE id=$1 AND state='pending'`, session.ID, now.UTC()); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox proxied data-plane start update: %w", err)
	}
	session.State = "running"
	session.UpdatedAt = now.UTC()
	if err := tx.Commit(ctx); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox proxied data-plane start commit: %w", err)
	}
	return session, nil
}

func (store *PostgresDataPlaneStore) CompleteDataPlaneSession(
	ctx context.Context,
	input DataPlaneCompletion,
) (DataPlaneSession, error) {
	if input.TenantRef == "" || input.SubjectRef == "" || input.SessionID == "" ||
		input.Now.IsZero() || (input.Exec == nil) == (input.File == nil) {
		return DataPlaneSession{}, errors.New("SecondBox live data-plane completion is incomplete")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox live data-plane completion transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := rowlock.TenantAndSubjectQuota(ctx, tx, input.TenantRef, input.SubjectRef); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox live data-plane completion quota lock: %w", err)
	}
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3 FOR UPDATE`,
		input.TenantRef, input.SubjectRef, input.SessionID,
	))
	if err != nil {
		return DataPlaneSession{}, err
	}
	if session.State == "completed" || session.State == "failed" ||
		session.State == "cancelled" || session.State == "expired" {
		if err := tx.Commit(ctx); err != nil {
			return DataPlaneSession{}, fmt.Errorf("SecondBox live data-plane completion replay commit: %w", err)
		}
		return session, nil
	}
	identity := dataPlaneProjection{}
	if input.Exec != nil {
		if session.Kind != "exec" || input.Exec.Terminal == nil ||
			int64(len(input.Exec.Stdout)+len(input.Exec.Stderr)) > session.MaximumResponseBytes {
			return DataPlaneSession{}, ErrDataPlaneSessionLimit
		}
		identity.execResult = proto.Clone(input.Exec).(*runnerv1.ExecBufferedResult)
		if identity.execResult.Stdout == nil {
			identity.execResult.Stdout = []byte{}
		}
		if identity.execResult.Stderr == nil {
			identity.execResult.Stderr = []byte{}
		}
	} else {
		if session.Kind != "file" || input.File.Terminal == nil ||
			int64(len(input.File.Content)) > session.MaximumResponseBytes {
			return DataPlaneSession{}, ErrDataPlaneSessionLimit
		}
		content := bytes.Clone(input.File.Content)
		if content == nil {
			content = []byte{}
		}
		identity.fileChunk = &runnerv1.FileChunk{Data: content}
		identity.fileMeta = input.File.Metadata
		identity.fileTerm = input.File.Terminal
	}
	if err := applyDataPlaneProjection(
		ctx, tx, session, identity, 0, store.retention, input.Now.UTC(),
	); err != nil {
		return DataPlaneSession{}, err
	}
	completed, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE id=$1`, session.ID))
	if err != nil {
		return DataPlaneSession{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DataPlaneSession{}, fmt.Errorf("SecondBox live data-plane completion commit: %w", err)
	}
	return completed, nil
}

func (store *PostgresDataPlaneStore) ConsumeDirectDataPlaneSession(
	ctx context.Context,
	input DirectDataPlaneConsumption,
) error {
	if input.SessionID == "" || input.AssignmentID == "" || input.Generation < 1 ||
		len(input.FencingToken) == 0 || len(input.CredentialDigest) != sha256.Size || input.Now.IsZero() {
		return errors.New("SecondBox direct data-plane consumption is incomplete")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("SecondBox direct data-plane consumption transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockDataPlaneSessionQuota(ctx, tx, input.SessionID); err != nil {
		return fmt.Errorf("SecondBox direct data-plane consumption quota lock: %w", err)
	}
	session, err := scanDataPlaneSession(tx.QueryRow(ctx, dataPlaneSessionSelect+`
		WHERE id=$1 FOR UPDATE`, input.SessionID))
	if err != nil {
		return err
	}
	if err := hydrateDataPlaneTransport(ctx, tx, &session); err != nil {
		return err
	}
	if session.Transport != contracts.DataPlaneTransportDirect ||
		session.AssignmentID != input.AssignmentID || session.Generation != input.Generation ||
		!bytes.Equal(session.FencingToken, input.FencingToken) || session.State != "pending" ||
		!input.Now.UTC().Before(session.DeadlineAt) {
		return ports.ErrAuthorizationDenied
	}
	if err := validateDirectDataPlaneAuthority(ctx, tx, session, input.Now.UTC()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.data_plane_sessions SET state='running',updated_at=$2
		WHERE id=$1 AND state='pending'`, session.ID, input.Now.UTC()); err != nil {
		return fmt.Errorf("SecondBox direct data-plane consumption update: %w", err)
	}
	return tx.Commit(ctx)
}

func validateDirectDataPlaneAuthority(
	ctx context.Context,
	tx pgx.Tx,
	session DataPlaneSession,
	now time.Time,
) error {
	var assignmentState, sandboxState string
	var generation int64
	var fencingToken []byte
	if err := tx.QueryRow(ctx, `
		SELECT assignment.state,sandbox.state,sandbox.generation,assignment.fencing_token
		FROM secondbox.assignments AS assignment
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=assignment.sandbox_id
		WHERE assignment.id=$1`, session.AssignmentID,
	).Scan(&assignmentState, &sandboxState, &generation, &fencingToken); err != nil {
		return ports.ErrAuthorizationDenied
	}
	if assignmentState != "ready" || sandboxState != contracts.SandboxStateReady ||
		generation != session.Generation || !bytes.Equal(fencingToken, session.FencingToken) {
		return ports.ErrAuthorizationDenied
	}
	if session.LeaseID != "" {
		var active bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM secondbox.leases
				WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3 AND sandbox_id=$4
				  AND generation=$5 AND state='active' AND expires_at>$6
			)`, session.LeaseID, session.TenantRef, session.SubjectRef,
			session.SandboxID, session.Generation, now,
		).Scan(&active); err != nil || !active {
			return ports.ErrLeaseInactive
		}
	}
	return nil
}
