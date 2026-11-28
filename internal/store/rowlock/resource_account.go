package rowlock

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SandboxResourceAccountPolicy reads account attribution and live admission
// governance after the caller locks the Sandbox. Only this requirement can
// tighten with a later Profile revision. Sizing always uses the pinned row.
func SandboxResourceAccountPolicy(ctx context.Context, tx pgx.Tx, sandboxID string) (previousKey string, required bool, err error) {
	err = tx.QueryRow(ctx, `
  SELECT COALESCE(sandbox.resource_account_key,''),
      COALESCE((pinned.spec_json->>'requireResourceAccount')::boolean,false)
      OR COALESCE((current_revision.spec_json->>'requireResourceAccount')::boolean,false)
  FROM secondbox.sandboxes AS sandbox
  LEFT JOIN secondbox.profile_revisions AS pinned ON pinned.id=sandbox.profile_revision_id
  LEFT JOIN secondbox.profiles AS profile ON profile.name=sandbox.profile_name
  LEFT JOIN secondbox.profile_revisions AS current_revision ON current_revision.id=profile.current_revision_id
  WHERE sandbox.id=$1`, sandboxID).Scan(&previousKey, &required)
	if err != nil {
		return "", false, fmt.Errorf("SecondBox resource account policy lookup failed: %w", err)
	}
	return previousKey, required, nil
}
