package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

// resolveAttributedConnections runs only for a new Assignment, inside its
// serializable transaction. Policy reads participate in dependency tracking and
// use Schedule's existing serialization retry, without changing row-lock order.
func resolveAttributedConnections(ctx context.Context, tx pgx.Tx, locked rowlock.SandboxWorkspace, attribution *runnerv1.AttributedExecution) error {
	var pinnedJSON, currentJSON, selectionJSON []byte
	var profileName string
	if err := tx.QueryRow(ctx, `
		SELECT pinned.profile_name,pinned.spec_json,current.spec_json,subject.sandbox_policy_json
		FROM secondbox.profile_revisions AS pinned
		JOIN secondbox.profiles AS profile ON profile.name=pinned.profile_name
		JOIN secondbox.profile_revisions AS current ON current.id=profile.current_revision_id
		JOIN secondbox.subjects AS subject ON subject.tenant_ref=$2 AND subject.ref=$3
		WHERE pinned.id=$1`, locked.ProfileRevisionID, locked.TenantRef, locked.SubjectRef,
	).Scan(&profileName, &pinnedJSON, &currentJSON, &selectionJSON); err != nil {
		return fmt.Errorf("SecondBox attributed connection policy lookup failed: %w", err)
	}
	var pinned, current contracts.ProfileRevisionSpec
	if err := json.Unmarshal(pinnedJSON, &pinned); err != nil {
		return fmt.Errorf("SecondBox attributed pinned Profile decoding failed: %w", err)
	}
	if pinned.AttributedExecution == nil || attribution.Gateway != pinned.AttributedExecution.Gateway {
		return errors.New("SecondBox attributed Assignment requires pinned gateway permission")
	}
	if err := json.Unmarshal(currentJSON, &current); err != nil {
		return fmt.Errorf("SecondBox attributed current Profile decoding failed: %w", err)
	}
	grant, err := current.AttributedConnectionGrant()
	if err != nil {
		return err
	}
	if grant == nil {
		// Removing permission from the head does not revoke an existing pin.
		// Its numeric default supplies both axes of this generation's grant.
		limit := contracts.AttributedExecutionConnectionLimits{MaximumConnections: pinned.AttributedExecution.MaximumConnections}
		if err := limit.Validate(); err != nil {
			return err
		}
		grant = &contracts.AttributedExecutionConnectionObservation{
			DefaultMaximumConnections: limit.MaximumConnections,
			MaximumConnectionsCeiling: limit.MaximumConnections,
		}
	}
	var selection contracts.SubjectSandboxPolicy
	if len(selectionJSON) != 0 {
		if err := json.Unmarshal(selectionJSON, &selection); err != nil {
			return fmt.Errorf("SecondBox attributed Subject policy decoding failed: %w", err)
		}
	}
	var requested *contracts.AttributedExecutionConnectionLimits
	if selection.Profile == profileName {
		requested = selection.AttributedExecution
	}
	resolved, err := grant.Resolve(requested)
	if err != nil {
		return err
	}
	attribution.MaximumConnections = uint32(resolved.MaximumConnections)
	return nil
}
