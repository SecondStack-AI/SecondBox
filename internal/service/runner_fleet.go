package service

import (
	"context"
	"errors"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type RunnerFleetStore interface {
	ReadRunnerFleet(context.Context, string, time.Time) (contracts.FleetPoolObservation, error)
	ChangeRunnerFleetState(context.Context, string, string, string, int64, time.Time) (contracts.FleetRunner, error)
}

func (service *ControlPlaneService) ReadRunnerFleet(ctx context.Context, principal contracts.Principal, pool string) (contracts.FleetPoolObservation, error) {
	if principal.Kind != "platform" {
		return contracts.FleetPoolObservation{}, ports.ErrAuthorizationDenied
	}
	if !profileNamePattern.MatchString(pool) {
		return contracts.FleetPoolObservation{}, invalidField("runnerPoolName", "must be a valid RunnerPool name")
	}
	store, ok := service.store.(RunnerFleetStore)
	if !ok {
		return contracts.FleetPoolObservation{}, errors.New("SecondBox Runner fleet storage is unavailable")
	}
	return store.ReadRunnerFleet(ctx, pool, service.now())
}

func (service *ControlPlaneService) ChangeRunnerFleetState(ctx context.Context, principal contracts.Principal, pool, runner, action string, revision int64) (contracts.FleetRunner, error) {
	if principal.Kind != "platform" {
		return contracts.FleetRunner{}, ports.ErrAuthorizationDenied
	}
	if !profileNamePattern.MatchString(pool) || !opaqueRunnerIDPattern.MatchString(runner) || revision < 1 {
		return contracts.FleetRunner{}, invalidField("runner", "pool, Runner identity and positive revision are required")
	}
	if action != "evacuate" && action != "retire" {
		return contracts.FleetRunner{}, invalidField("action", "must be evacuate or retire")
	}
	store, ok := service.store.(RunnerFleetStore)
	if !ok {
		return contracts.FleetRunner{}, errors.New("SecondBox Runner fleet storage is unavailable")
	}
	return store.ChangeRunnerFleetState(ctx, pool, runner, action, revision, service.now())
}
