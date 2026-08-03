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

// BuiltInProfileBinding supplies the deployment-specific RunnerPool and signed
// execution assets that one built-in Profile pins. Everything else about a
// built-in Profile is fixed by SecondBox.
type BuiltInProfileBinding struct {
	Pool                  string
	RuntimeBundleDigest   string
	ToolchainBundleDigest string
}

// BuiltInProfileBindings names the deployment values both built-in Profiles need.
type BuiltInProfileBindings struct {
	AgentCompartment  BuiltInProfileBinding
	CodingEnvironment BuiltInProfileBinding
}

// BuildBuiltInProfiles applies deployment bindings to the fixed built-in specs.
//
// There is no default binding. A deployment that does not name a RunnerPool and
// its verified bundle digests has no bootable built-in Profile, and saying so at
// startup is better than admitting Sandboxes that can never be placed.
func BuildBuiltInProfiles(bindings BuiltInProfileBindings) ([]contracts.Profile, error) {
	profiles := builtInProfiles(bindings)
	if _, err := resolveBuiltInProfiles(profiles); err != nil {
		return nil, err
	}
	return profiles, nil
}

func builtInProfiles(bindings BuiltInProfileBindings) []contracts.Profile {
	return []contracts.Profile{
		newBuiltInProfile(
			BuiltInProfileAgentCompartment,
			"prv_builtin_agent_compartment_v1",
			contracts.ProfileRevisionSpec{
				Pool: bindings.AgentCompartment.Pool, Architecture: "amd64",
				RuntimeBundleDigest:   bindings.AgentCompartment.RuntimeBundleDigest,
				ToolchainBundleDigest: bindings.AgentCompartment.ToolchainBundleDigest,
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
					DataPlaneTransport:          contracts.DataPlaneTransportProxied,
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
				Pool: bindings.CodingEnvironment.Pool, Architecture: "amd64",
				RuntimeBundleDigest:   bindings.CodingEnvironment.RuntimeBundleDigest,
				ToolchainBundleDigest: bindings.CodingEnvironment.ToolchainBundleDigest,
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
					DataPlaneTransport:          contracts.DataPlaneTransportProxied,
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
	if len(configured) == 0 {
		return nil, errors.New("SecondBox built-in Profiles must be configured explicitly")
	}
	resolved := make(map[string]contracts.Profile, len(configured))
	for _, profile := range configured {
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
