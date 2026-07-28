package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

var (
	runnerCapabilityPattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	runnerCapacityNamePattern = regexp.MustCompile(`^[a-z][A-Za-z0-9]{0,63}$`)
	opaqueRunnerIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// CreateRunnerPool creates one explicit operator-owned scheduling boundary.
func (service *ControlPlaneService) CreateRunnerPool(
	ctx context.Context,
	principal contracts.Principal,
	request contracts.CreateRunnerPoolRequest,
) (contracts.RunnerPool, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminRunners); err != nil {
		return contracts.RunnerPool{}, err
	}
	if err := validateRunnerPoolPolicy(
		request.Name,
		request.State,
		request.Architectures,
		request.Capabilities,
		request.CapacityPolicy,
	); err != nil {
		return contracts.RunnerPool{}, err
	}
	now := service.now().UTC()
	pool := contracts.RunnerPool{
		Name:             request.Name,
		State:            request.State,
		Architectures:    sortedUnique(request.Architectures),
		Capabilities:     sortedUnique(request.Capabilities),
		CapacityPolicy:   cloneRunnerPoolCapacityPolicy(request.CapacityPolicy),
		ReadyRunnerCount: 0,
		Revision:         1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	audit := service.newAudit(ctx, principal, "runner_pool.created", "runner_pool", pool.Name, "", now)
	return service.store.CreateRunnerPool(ctx, pool, audit)
}

// UpdateRunnerPool changes explicit placement policy under optimistic concurrency.
func (service *ControlPlaneService) UpdateRunnerPool(
	ctx context.Context,
	principal contracts.Principal,
	name string,
	request contracts.UpdateRunnerPoolRequest,
	expectedRevision int64,
) (contracts.RunnerPool, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminRunners); err != nil {
		return contracts.RunnerPool{}, err
	}
	if !profileNamePattern.MatchString(name) {
		return contracts.RunnerPool{}, errors.New("SecondBox RunnerPool name is invalid")
	}
	if expectedRevision < 1 {
		return contracts.RunnerPool{}, errors.New("SecondBox RunnerPool update requires a positive revision")
	}
	if request.State == nil && request.Architectures == nil &&
		request.Capabilities == nil && request.CapacityPolicy == nil {
		return contracts.RunnerPool{}, errors.New("SecondBox RunnerPool update requires at least one field")
	}
	current, err := service.store.GetRunnerPool(ctx, name)
	if err != nil {
		return contracts.RunnerPool{}, err
	}
	state := current.State
	architectures := current.Architectures
	capabilities := current.Capabilities
	capacityPolicy := current.CapacityPolicy
	if request.State != nil {
		state = *request.State
	}
	if request.Architectures != nil {
		architectures = *request.Architectures
		normalized := sortedUnique(architectures)
		request.Architectures = &normalized
	}
	if request.Capabilities != nil {
		capabilities = *request.Capabilities
		normalized := sortedUnique(capabilities)
		request.Capabilities = &normalized
	}
	if request.CapacityPolicy != nil {
		capacityPolicy = *request.CapacityPolicy
		cloned := cloneRunnerPoolCapacityPolicy(capacityPolicy)
		request.CapacityPolicy = &cloned
	}
	if err := validateRunnerPoolPolicy(name, state, architectures, capabilities, capacityPolicy); err != nil {
		return contracts.RunnerPool{}, err
	}
	now := service.now().UTC()
	audit := service.newAudit(ctx, principal, "runner_pool.updated", "runner_pool", name, "", now)
	return service.store.UpdateRunnerPool(ctx, name, request, expectedRevision, now, audit)
}

// GetRunnerPool returns one administrative placement boundary.
func (service *ControlPlaneService) GetRunnerPool(
	ctx context.Context,
	principal contracts.Principal,
	name string,
) (contracts.RunnerPool, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminRunners); err != nil {
		return contracts.RunnerPool{}, err
	}
	if !profileNamePattern.MatchString(name) {
		return contracts.RunnerPool{}, errors.New("SecondBox RunnerPool name is invalid")
	}
	return service.store.GetRunnerPool(ctx, name)
}

// ListRunnerPools returns a bounded administrative RunnerPool page.
func (service *ControlPlaneService) ListRunnerPools(
	ctx context.Context,
	principal contracts.Principal,
	limit int,
) ([]contracts.RunnerPool, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminRunners); err != nil {
		return nil, err
	}
	return service.store.ListRunnerPools(ctx, boundedLimit(limit))
}

// GetRunner returns one enrolled execution identity and current capacity evidence.
func (service *ControlPlaneService) GetRunner(
	ctx context.Context,
	principal contracts.Principal,
	runnerID string,
) (contracts.Runner, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminRunners); err != nil {
		return contracts.Runner{}, err
	}
	if !opaqueRunnerIDPattern.MatchString(runnerID) {
		return contracts.Runner{}, errors.New("SecondBox Runner ID is invalid")
	}
	return service.store.GetRunner(ctx, runnerID)
}

// ListRunners returns a bounded administrative Runner page filtered by exact pool.
func (service *ControlPlaneService) ListRunners(
	ctx context.Context,
	principal contracts.Principal,
	poolName string,
	limit int,
) ([]contracts.Runner, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminRunners); err != nil {
		return nil, err
	}
	if poolName != "" && !profileNamePattern.MatchString(poolName) {
		return nil, errors.New("SecondBox Runner pool filter is invalid")
	}
	return service.store.ListRunners(ctx, poolName, boundedLimit(limit))
}

func validateRunnerPoolPolicy(
	name string,
	state string,
	architectures []string,
	capabilities []string,
	capacityPolicy map[string]int64,
) error {
	if !profileNamePattern.MatchString(name) {
		return errors.New("SecondBox RunnerPool name is invalid")
	}
	switch state {
	case contracts.RunnerPoolStateReady,
		contracts.RunnerPoolStateDraining,
		contracts.RunnerPoolStateOffline:
	default:
		return errors.New("SecondBox RunnerPool state is invalid")
	}
	architectures = sortedUnique(architectures)
	if len(architectures) < 1 || len(architectures) > 8 {
		return errors.New("SecondBox RunnerPool architectures must contain between 1 and 8 values")
	}
	for _, architecture := range architectures {
		if architecture != "amd64" && architecture != "arm64" {
			return fmt.Errorf("SecondBox RunnerPool architecture is unsupported: %s", architecture)
		}
	}
	capabilities = sortedUnique(capabilities)
	if len(capabilities) < 1 || len(capabilities) > 64 {
		return errors.New("SecondBox RunnerPool capabilities must contain between 1 and 64 values")
	}
	for _, capability := range capabilities {
		if !runnerCapabilityPattern.MatchString(capability) {
			return errors.New("SecondBox RunnerPool capability is invalid")
		}
	}
	if len(capacityPolicy) < 1 || len(capacityPolicy) > 32 {
		return errors.New("SecondBox RunnerPool capacity policy must contain between 1 and 32 values")
	}
	for key, value := range capacityPolicy {
		if !runnerCapacityNamePattern.MatchString(key) || value < 1 {
			return errors.New("SecondBox RunnerPool capacity policy entries must have valid names and positive values")
		}
	}
	return nil
}

func cloneRunnerPoolCapacityPolicy(source map[string]int64) map[string]int64 {
	cloned := make(map[string]int64, len(source))
	for name, value := range source {
		cloned[name] = value
	}
	return cloned
}
