package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

const maximumEgressContextDiagnosticItems = 1000

// ReadEgressContextPreflight compares durable Tenant/Profile requirements with
// current connected Runner advertisements and groups active assignment impact.
// It is intentionally read-only.
func (store *PostgresControlPlaneStore) ReadEgressContextPreflight(ctx context.Context) (contracts.EgressContextPreflight, error) {
	result := contracts.EgressContextPreflight{
		Ready: true, Requirements: []contracts.EgressContextRequirement{},
		Runners: []contracts.EgressContextRunner{}, ActiveAssignments: []contracts.EgressContextAssignmentGroup{},
	}
	requirementRows, err := store.pool.Query(ctx, `
		SELECT tenant.ref,tenant.egress_context,profile.name,profile.current_revision_id,
		       revision.spec_json->>'pool',
		       COALESCE(compatible.runner_ids,'[]'::jsonb)
		FROM secondbox.tenants AS tenant
		CROSS JOIN LATERAL jsonb_array_elements_text(tenant.allowed_profile_grants_json) AS profile_grant(profile_name)
		JOIN secondbox.profiles AS profile ON profile.name=profile_grant.profile_name
		JOIN secondbox.profile_revisions AS revision ON revision.id=profile.current_revision_id
		LEFT JOIN LATERAL (
			SELECT jsonb_agg(candidate.id ORDER BY candidate.id) AS runner_ids
			FROM (
				SELECT runner.id
				FROM secondbox.runners AS runner
				WHERE runner.pool_name=revision.spec_json->>'pool'
				  AND runner.state='ready'
				  AND runner.active_connection_id<>''
				  AND tenant.egress_context IS NOT NULL
				  AND runner.supported_egress_contexts_json ? tenant.egress_context
				ORDER BY runner.id
				LIMIT 1001
			) AS candidate
		) AS compatible ON true
		WHERE tenant.state='active'
		  AND profile.state='enabled'
		  AND revision.spec_json->'network'->>'requiresTenantEgressContext'='true'
		ORDER BY tenant.ref,profile.name`)
	if err != nil {
		return contracts.EgressContextPreflight{}, fmt.Errorf("SecondBox egress context preflight requirements query failed: %w", err)
	}
	for requirementRows.Next() {
		var requirement contracts.EgressContextRequirement
		var runnerIDsJSON []byte
		if err := requirementRows.Scan(&requirement.TenantRef, &requirement.EgressContext, &requirement.ProfileName, &requirement.ProfileRevisionID, &requirement.PoolName, &runnerIDsJSON); err != nil {
			requirementRows.Close()
			return contracts.EgressContextPreflight{}, fmt.Errorf("SecondBox egress context preflight requirement scan failed: %w", err)
		}
		if err := json.Unmarshal(runnerIDsJSON, &requirement.CompatibleRunnerIDs); err != nil {
			requirementRows.Close()
			return contracts.EgressContextPreflight{}, fmt.Errorf("SecondBox egress context preflight Runner list decode failed: %w", err)
		}
		if len(requirement.CompatibleRunnerIDs) > maximumEgressContextDiagnosticItems {
			requirement.CompatibleRunnerIDs = requirement.CompatibleRunnerIDs[:maximumEgressContextDiagnosticItems]
			result.Truncated = true
		}
		requirement.Status = "ready"
		if requirement.EgressContext == nil {
			requirement.Status = "tenant_context_missing"
			result.Ready = false
		} else if len(requirement.CompatibleRunnerIDs) == 0 {
			requirement.Status = "runner_context_unavailable"
			result.Ready = false
		}
		if len(result.Requirements) < maximumEgressContextDiagnosticItems {
			result.Requirements = append(result.Requirements, requirement)
		} else {
			result.Truncated = true
		}
	}
	if err := requirementRows.Err(); err != nil {
		return contracts.EgressContextPreflight{}, fmt.Errorf("SecondBox egress context preflight requirements iteration failed: %w", err)
	}

	runnerRows, err := store.pool.Query(ctx, `
		SELECT id,pool_name,state,active_connection_id<>'',supported_egress_contexts_json
		FROM secondbox.runners ORDER BY id`)
	if err != nil {
		return contracts.EgressContextPreflight{}, fmt.Errorf("SecondBox egress context preflight Runner query failed: %w", err)
	}
	for runnerRows.Next() {
		var runner contracts.EgressContextRunner
		var contextsJSON []byte
		if err := runnerRows.Scan(&runner.RunnerID, &runner.PoolName, &runner.State, &runner.Connected, &contextsJSON); err != nil {
			runnerRows.Close()
			return contracts.EgressContextPreflight{}, fmt.Errorf("SecondBox egress context preflight Runner scan failed: %w", err)
		}
		if err := json.Unmarshal(contextsJSON, &runner.AdvertisedContexts); err != nil {
			runnerRows.Close()
			return contracts.EgressContextPreflight{}, fmt.Errorf("SecondBox egress context preflight Runner contexts decode failed: %w", err)
		}
		if len(result.Runners) < maximumEgressContextDiagnosticItems {
			result.Runners = append(result.Runners, runner)
		} else {
			result.Truncated = true
		}
	}
	if err := runnerRows.Err(); err != nil {
		return contracts.EgressContextPreflight{}, fmt.Errorf("SecondBox egress context preflight Runner iteration failed: %w", err)
	}

	assignmentRows, err := store.pool.Query(ctx, `
		SELECT egress_context,runner_id,state,count(*)
		FROM secondbox.assignments
		WHERE state NOT IN ('failed','fenced','released')
		GROUP BY egress_context,runner_id,state
		ORDER BY egress_context NULLS FIRST,runner_id,state`)
	if err != nil {
		return contracts.EgressContextPreflight{}, fmt.Errorf("SecondBox egress context preflight assignments query failed: %w", err)
	}
	for assignmentRows.Next() {
		var group contracts.EgressContextAssignmentGroup
		if err := assignmentRows.Scan(&group.EgressContext, &group.RunnerID, &group.State, &group.Count); err != nil {
			assignmentRows.Close()
			return contracts.EgressContextPreflight{}, fmt.Errorf("SecondBox egress context preflight assignment scan failed: %w", err)
		}
		if len(result.ActiveAssignments) < maximumEgressContextDiagnosticItems {
			result.ActiveAssignments = append(result.ActiveAssignments, group)
		} else {
			result.Truncated = true
		}
	}
	if err := assignmentRows.Err(); err != nil {
		return contracts.EgressContextPreflight{}, fmt.Errorf("SecondBox egress context preflight assignments iteration failed: %w", err)
	}
	return result, nil
}
