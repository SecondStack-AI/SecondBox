package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

type attributedAssignmentAuthority struct {
	reference *string
	expiresAt *time.Time
	sessionID *string
}

// The caller holds the Sandbox and assignment row locks. The consumed session ID
// stays on the assignment even after data-plane result retention removes it.
func (authority attributedAssignmentAuthority) admitDataPlane(ctx context.Context, tx pgx.Tx, assignmentID, desiredState string, input *DataPlaneAdmission) error {
	if authority.reference == nil && authority.expiresAt == nil && authority.sessionID == nil {
		return nil
	}
	if authority.reference == nil || *authority.reference == "" || authority.expiresAt == nil {
		return errors.New("SecondBox attributed assignment binding is incomplete")
	}
	if desiredState != contracts.SandboxDesiredStateRunning {
		return ports.ErrLifecycleUnavailable
	}
	if !input.Now.Before(*authority.expiresAt) {
		return ErrDataPlaneDeadline
	}
	if input.Kind == "exec" && input.ExecOpen != nil && !input.ExecOpen.AllocatePty {
		if input.DeadlineAt.After(*authority.expiresAt) {
			return ErrDataPlaneDeadline
		}
		if authority.sessionID != nil {
			return ports.ErrLifecycleUnavailable
		}
		if _, err := tx.Exec(ctx, `UPDATE secondbox.assignments SET execution_session_id=$2 WHERE id=$1`, assignmentID, input.ID); err != nil {
			return fmt.Errorf("SecondBox attributed exec admission failed: %w", err)
		}
		return nil
	}
	if input.Kind == "file" && input.FileOpen != nil {
		switch input.FileOpen.Operation {
		case runnerv1.FileOperation_FILE_OPERATION_READ, runnerv1.FileOperation_FILE_OPERATION_STAT,
			runnerv1.FileOperation_FILE_OPERATION_LIST, runnerv1.FileOperation_FILE_OPERATION_EXISTS:
			if input.DeadlineAt.After(*authority.expiresAt) {
				input.DeadlineAt = *authority.expiresAt
			}
			return nil
		}
	}
	return ports.ErrLifecycleUnavailable
}
