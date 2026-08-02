package service

import (
	"context"
	"errors"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

const (
	minimumTimingWindowSeconds = int64(60)
	maximumTimingWindowSeconds = int64(3600)
)

// GetSandboxTiming returns bounded timing evidence owned by one caller-visible Sandbox.
func (service *ControlPlaneService) GetSandboxTiming(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	limit int,
) (contracts.SandboxTiming, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.SandboxTiming{}, ports.ErrAuthorizationDenied
	}
	if sandboxID == "" {
		return contracts.SandboxTiming{}, errors.New("SecondBox timing Sandbox ID is required")
	}
	if limit < 1 || limit > 200 {
		return contracts.SandboxTiming{}, errors.New("SecondBox timing limit must be from 1 through 200")
	}
	return service.store.ReadSandboxTiming(
		ctx, principal.TenantRef, principal.SubjectRef, sandboxID, limit,
	)
}

// GetOperationTiming returns queue, execution, and boot-stage evidence for one Operation.
func (service *ControlPlaneService) GetOperationTiming(
	ctx context.Context,
	principal contracts.Principal,
	operationID string,
) (contracts.OperationTiming, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.OperationTiming{}, ports.ErrAuthorizationDenied
	}
	if operationID == "" {
		return contracts.OperationTiming{}, errors.New("SecondBox timing Operation ID is required")
	}
	return service.store.ReadOperationTiming(
		ctx, principal.TenantRef, principal.SubjectRef, operationID,
	)
}

// GetDeploymentTiming returns a bounded current-deployment timing projection.
func (service *ControlPlaneService) GetDeploymentTiming(
	ctx context.Context,
	principal contracts.Principal,
	windowSeconds int64,
) (contracts.DeploymentTimingSummary, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.DeploymentTimingSummary{}, ports.ErrAuthorizationDenied
	}
	if windowSeconds < minimumTimingWindowSeconds ||
		windowSeconds > maximumTimingWindowSeconds {
		return contracts.DeploymentTimingSummary{}, errors.New(
			"SecondBox timing windowSeconds must be from 60 through 3600",
		)
	}
	observedAt := service.now().UTC()
	summary, err := service.store.ReadDeploymentTiming(
		ctx,
		observedAt.Add(-time.Duration(windowSeconds)*time.Second),
		observedAt,
	)
	if err != nil {
		return contracts.DeploymentTimingSummary{}, err
	}
	summary.WindowSeconds = windowSeconds
	summary.ObservedAt = observedAt
	return summary, nil
}
