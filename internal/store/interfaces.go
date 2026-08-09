package store

import (
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/service"
)

var (
	_ service.HealthStore              = (*PostgresControlPlaneStore)(nil)
	_ service.ProfileStore             = (*PostgresControlPlaneStore)(nil)
	_ service.RunnerAdminStore         = (*PostgresControlPlaneStore)(nil)
	_ service.SandboxStore             = (*PostgresControlPlaneStore)(nil)
	_ service.ActivityStore            = (*PostgresControlPlaneStore)(nil)
	_ service.SnapshotStore            = (*PostgresControlPlaneStore)(nil)
	_ service.ObservabilityStore       = (*PostgresControlPlaneStore)(nil)
	_ lifecycle.ReconcileStore         = (*PostgresControlPlaneStore)(nil)
	_ lifecycle.SnapshotRetentionStore = (*PostgresControlPlaneStore)(nil)
)
