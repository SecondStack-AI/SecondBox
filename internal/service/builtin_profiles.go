package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

const (
	BuiltInProfileAgentCompartment  = "agent-compartment"
	BuiltInProfileCodingEnvironment = "coding-environment"
)

var builtInProfileCreatedAt = time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)

func defaultBuiltInProfiles() []contracts.Profile {
	return []contracts.Profile{
		newBuiltInProfile(
			BuiltInProfileAgentCompartment,
			"prv_builtin_agent_compartment_v1",
			contracts.ProfileRevisionSpec{
				Pool: "default-pool", Architecture: "amd64",
				RuntimeBundleDigest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				ToolchainBundleDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				Resources: contracts.ResourcePolicy{
					CPUMillis: 1000, MemoryBytes: 1 << 30, WorkspaceBytes: 2 << 30,
					ProcessLimit: 64, ConcurrentOperations: 4,
				},
				Lifecycle: contracts.LifecyclePolicy{
					InitialState:      contracts.SandboxDesiredStateRunning,
					DrainGraceSeconds: 10, IdleSeconds: 60,
					MaximumDurationSeconds: 900, LeaseSeconds: 60,
				},
				Retention: contracts.RetentionPolicy{
					SnapshotLimit: 0, SnapshotRetentionSeconds: 3600,
					ArtifactRetentionSeconds: 86400,
				},
				Execution: contracts.ExecutionPolicy{
					MaximumDeadlineMilliseconds: 120000,
					MaximumBufferedOutputBytes:  1 << 20,
					StreamWindowBytes:           64 << 10,
					MaximumTransferBytes:        256 << 20,
					TerminalDetachSeconds:       0,
				},
				Network: contracts.NetworkPolicy{
					Mode: "deny_all", Destinations: []contracts.NetworkDestination{},
				},
				Ports: []contracts.PortPolicy{},
			},
		),
		newBuiltInProfile(
			BuiltInProfileCodingEnvironment,
			"prv_builtin_coding_environment_v1",
			contracts.ProfileRevisionSpec{
				Pool: "default-pool", Architecture: "amd64",
				RuntimeBundleDigest:   "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				ToolchainBundleDigest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
				Resources: contracts.ResourcePolicy{
					CPUMillis: 4000, MemoryBytes: 8 << 30, WorkspaceBytes: 50 << 30,
					ProcessLimit: 512, ConcurrentOperations: 16,
				},
				Lifecycle: contracts.LifecyclePolicy{
					InitialState:      contracts.SandboxDesiredStateRunning,
					DrainGraceSeconds: 120, IdleSeconds: 28800,
					MaximumDurationSeconds: 604800, LeaseSeconds: 300,
				},
				Retention: contracts.RetentionPolicy{
					SnapshotLimit: 64, SnapshotRetentionSeconds: 2592000,
					ArtifactRetentionSeconds: 2592000,
				},
				Execution: contracts.ExecutionPolicy{
					MaximumDeadlineMilliseconds: 86400000,
					MaximumBufferedOutputBytes:  16 << 20,
					StreamWindowBytes:           1 << 20,
					MaximumTransferBytes:        10 << 30,
					TerminalDetachSeconds:       86400,
				},
				Network: contracts.NetworkPolicy{
					Mode: "deny_all", Destinations: []contracts.NetworkDestination{},
				},
				Ports: []contracts.PortPolicy{{
					Name: "development-http", Port: 3000, Protocol: "http",
					MaximumSessions: 8, MaximumSessionSeconds: 86400,
				}},
			},
		),
	}
}

func newBuiltInProfile(
	name string,
	revisionID string,
	spec contracts.ProfileRevisionSpec,
) contracts.Profile {
	return contracts.Profile{
		Name: name, State: contracts.ProfileStateEnabled, Revision: 1,
		CreatedAt: builtInProfileCreatedAt, UpdatedAt: builtInProfileCreatedAt,
		CurrentRevision: contracts.ProfileRevision{
			ID: revisionID, Number: 1, Spec: spec, CreatedAt: builtInProfileCreatedAt,
		},
	}
}

func resolveBuiltInProfiles(configured []contracts.Profile) (map[string]contracts.Profile, error) {
	profiles := configured
	if profiles == nil {
		profiles = defaultBuiltInProfiles()
	}
	resolved := make(map[string]contracts.Profile, len(profiles))
	for _, profile := range profiles {
		if profile.Name != BuiltInProfileAgentCompartment &&
			profile.Name != BuiltInProfileCodingEnvironment {
			return nil, fmt.Errorf("SecondBox built-in Profile name %q is not reserved", profile.Name)
		}
		if profile.State != contracts.ProfileStateEnabled ||
			profile.CurrentRevision.ID == "" ||
			profile.CurrentRevision.Number < 1 ||
			profile.Revision < 1 ||
			profile.CreatedAt.IsZero() ||
			profile.UpdatedAt.IsZero() ||
			profile.CurrentRevision.CreatedAt.IsZero() {
			return nil, errors.New("SecondBox built-in Profile definition is incomplete")
		}
		if err := validateProfileRevisionSpec(profile.CurrentRevision.Spec); err != nil {
			return nil, fmt.Errorf("SecondBox built-in Profile %q: %w", profile.Name, err)
		}
		if _, exists := resolved[profile.Name]; exists {
			return nil, fmt.Errorf("SecondBox built-in Profile %q is duplicated", profile.Name)
		}
		resolved[profile.Name] = profile
	}
	for _, required := range []string{
		BuiltInProfileAgentCompartment,
		BuiltInProfileCodingEnvironment,
	} {
		if _, ok := resolved[required]; !ok {
			return nil, fmt.Errorf("SecondBox built-in Profile %q is required", required)
		}
	}
	return resolved, nil
}

func (service *ControlPlaneService) isBuiltInProfile(name string) bool {
	_, ok := service.builtInProfiles[name]
	return ok
}

func (service *ControlPlaneService) ensureAllBuiltInProfiles(ctx context.Context) error {
	names := make([]string, 0, len(service.builtInProfiles))
	for name := range service.builtInProfiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := service.store.EnsureBuiltInProfile(ctx, service.builtInProfiles[name]); err != nil {
			return err
		}
	}
	return nil
}
