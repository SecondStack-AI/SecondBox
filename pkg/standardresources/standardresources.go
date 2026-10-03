// Package standardresources materializes release-owned resource bundles from a
// verified release artifact manifest and explicit deployment bindings.
package standardresources

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const (
	AgentCompartment         = "agent-compartment"
	DurableCoding            = "durable-coding"
	AgentCompartmentIsolated = "agent-compartment-isolated"
	ArchitectureAMD64        = "amd64"
	ArchitectureARM64        = "arm64"
	PoolAMD64                = "standard-amd64"
	PoolARM64                = "standard-arm64"

	DurableCodingVCPUCount            = int64(4)
	DurableCodingMemoryBytes          = int64(8 << 30)
	DurableCodingWorkspaceBytes       = int64(50 << 30)
	DurableCodingConcurrentOperations = int64(16)

	AgentGateway    = "agent-gateway.secondbox.internal"
	PlatformGateway = "platform-gateway.secondbox.internal"

	developmentVersion      = "0.0.0-development"
	developmentTag          = "v0.0.0-development"
	developmentSourceCommit = "dddddddddddddddddddddddddddddddddddddddd"
)

type PoolBinding struct {
	Name          string
	Architectures []string
	Capabilities  []string
	State         string
}

type Selection struct {
	Bundles []string
	Pools   map[string]PoolBinding
}

// StandardPool returns the release-owned RunnerPool that serves one guest
// architecture. A release targets exactly one guest architecture, so its
// standard Profiles bind to exactly one of these pools.
func StandardPool(architecture string) (string, error) {
	switch architecture {
	case ArchitectureAMD64:
		return PoolAMD64, nil
	case ArchitectureARM64:
		return PoolARM64, nil
	}
	return "", fmt.Errorf("SecondBox standard resources do not support guest architecture %q", architecture)
}

// BundleNames returns every release-owned standard bundle in artifact order.
func BundleNames() []string {
	return []string{AgentCompartment, DurableCoding, AgentCompartmentIsolated}
}

// Build returns the code-owned Profile lineages of the selected bundles after
// proving they are the lineages the validated artifact manifest released.
func Build(manifest releasecontract.ArtifactManifest, selection Selection) (resourceapply.Document, error) {
	if err := manifest.Validate(); err != nil {
		return resourceapply.Document{}, err
	}
	architecture, err := manifest.GuestArchitecture()
	if err != nil {
		return resourceapply.Document{}, err
	}
	pool, err := StandardPool(architecture)
	if err != nil {
		return resourceapply.Document{}, err
	}
	document := resourceapply.Document{SchemaVersion: resourceapply.SchemaVersion, RunnerPools: []resourceapply.RunnerPool{}, Profiles: []resourceapply.Profile{}}
	seen := map[string]bool{}
	for _, name := range selection.Bundles {
		if seen[name] {
			return resourceapply.Document{}, fmt.Errorf("SecondBox standard bundle %q is selected more than once", name)
		}
		seen[name] = true
		binding, ok := selection.Pools[name]
		if !ok {
			return resourceapply.Document{}, fmt.Errorf("SecondBox standard bundle %q has no RunnerPool binding", name)
		}
		if binding.Name != pool || !slices.Contains(binding.Architectures, architecture) {
			return resourceapply.Document{}, fmt.Errorf("SecondBox standard bundle %q requires the %s RunnerPool %s", name, architecture, pool)
		}
		updatedPools, err := appendOrValidatePool(document.RunnerPools, binding)
		if err != nil {
			return resourceapply.Document{}, err
		}
		document.RunnerPools = updatedPools
		profile, err := profileLineageForManifest(manifest, name, architecture)
		if err != nil {
			return resourceapply.Document{}, err
		}
		if err := validateManifestIdentity(manifest, profile); err != nil {
			return resourceapply.Document{}, err
		}
		document.Profiles = append(document.Profiles, profile)
	}
	if err := document.Validate(); err != nil {
		return resourceapply.Document{}, err
	}
	return document, nil
}

func profileLineageForManifest(manifest releasecontract.ArtifactManifest, name, architecture string) (resourceapply.Profile, error) {
	if manifest.Version == developmentVersion && manifest.Tag == developmentTag && manifest.SourceCommit == developmentSourceCommit {
		return DevelopmentProfileLineage(name, architecture)
	}
	return ProfileLineage(name, architecture)
}

// ProfileLineage returns the complete ordered lineage for one architecture-qualified standard Profile.
func ProfileLineage(name, architecture string) (resourceapply.Profile, error) {
	pool, err := StandardPool(architecture)
	if err != nil {
		return resourceapply.Profile{}, err
	}
	var specs []secondboxclient.ProfileRevisionSpec
	// Standard Profile history is append-only. Keep every shipped spec here in
	// revision order so an existing deployment can prove its immutable prefix.
	// The revision that follows each bundle's first spec once moved it off the
	// v0.3.0 execution bundle; Profiles no longer name bundles, so it repeats
	// its predecessor and remains only to keep installed revision numbers.
	switch name {
	case AgentCompartment:
		specs = []secondboxclient.ProfileRevisionSpec{
			agentSpec(pool, architecture, 120000),
			// Callers may request a command deadline up to the Sandbox's outer lifetime.
			agentSpec(pool, architecture, 900000),
			agentSpec(pool, architecture, 900000),
			attributedAgentSpec(pool, architecture),
		}
	case DurableCoding:
		specs = []secondboxclient.ProfileRevisionSpec{codingSpec(pool, architecture), codingSpec(pool, architecture)}
	case AgentCompartmentIsolated:
		specs = []secondboxclient.ProfileRevisionSpec{isolatedAgentSpec(pool, architecture), isolatedAgentSpec(pool, architecture)}
	default:
		return resourceapply.Profile{}, fmt.Errorf("SecondBox standard bundle %q is unknown", name)
	}
	if name == AgentCompartment || name == AgentCompartmentIsolated {
		current := specs[len(specs)-1]
		current.Lifecycle.MaximumDurationSeconds = secondboxclient.Unlimited
		current.LifecycleCeiling = &secondboxclient.SandboxLifecycleLimits{IdleSeconds: secondboxclient.Unlimited, MaximumDurationSeconds: secondboxclient.Unlimited}
		specs = append(specs, current)
	}
	if name == AgentCompartment {
		current := specs[len(specs)-1]
		attributed := *current.AttributedExecution
		attributed.MaximumConnections = 128
		current.AttributedExecution = &attributed
		current.AttributedExecutionCeiling = secondboxclient.AttributedExecutionConnectionLimits{MaximumConnections: 4096}
		specs = append(specs, current)
	}
	return profileFromSpecs(name, specs)
}

// DevelopmentProfileLineage returns the shorter lineage the explicit local
// development release identity has always recorded, without published history.
func DevelopmentProfileLineage(name, architecture string) (resourceapply.Profile, error) {
	pool, err := StandardPool(architecture)
	if err != nil {
		return resourceapply.Profile{}, err
	}
	var specs []secondboxclient.ProfileRevisionSpec
	switch name {
	case AgentCompartment:
		specs = []secondboxclient.ProfileRevisionSpec{
			agentSpec(pool, architecture, 900000),
			attributedAgentSpec(pool, architecture),
		}
	case DurableCoding:
		specs = []secondboxclient.ProfileRevisionSpec{codingSpec(pool, architecture)}
	case AgentCompartmentIsolated:
		specs = []secondboxclient.ProfileRevisionSpec{isolatedAgentSpec(pool, architecture)}
	default:
		return resourceapply.Profile{}, fmt.Errorf("SecondBox standard bundle %q is unknown", name)
	}
	if name == AgentCompartment || name == AgentCompartmentIsolated {
		current := specs[len(specs)-1]
		current.Lifecycle.MaximumDurationSeconds = secondboxclient.Unlimited
		current.LifecycleCeiling = &secondboxclient.SandboxLifecycleLimits{IdleSeconds: secondboxclient.Unlimited, MaximumDurationSeconds: secondboxclient.Unlimited}
		specs = append(specs, current)
	}
	if name == AgentCompartment {
		current := specs[len(specs)-1]
		attributed := *current.AttributedExecution
		attributed.MaximumConnections = 128
		current.AttributedExecution = &attributed
		current.AttributedExecutionCeiling = secondboxclient.AttributedExecutionConnectionLimits{MaximumConnections: 4096}
		specs = append(specs, current)
	}
	return profileFromSpecs(name, specs)
}

func profileFromSpecs(name string, specs []secondboxclient.ProfileRevisionSpec) (resourceapply.Profile, error) {
	revisions := make([]resourceapply.ProfileRevision, 0, len(specs))
	for index, spec := range specs {
		digest, err := resourceapply.SpecDigest(spec)
		if err != nil {
			return resourceapply.Profile{}, err
		}
		revisions = append(revisions, resourceapply.ProfileRevision{Number: int64(index + 1), SpecDigest: digest, Spec: spec})
	}
	return resourceapply.Profile{Name: name, Revisions: revisions}, nil
}

func validateManifestIdentity(manifest releasecontract.ArtifactManifest, profile resourceapply.Profile) error {
	for _, bundle := range manifest.StandardBundles {
		if bundle.Name != profile.Name {
			continue
		}
		if len(bundle.Profiles) != len(profile.Revisions) {
			return fmt.Errorf("SecondBox standard bundle %q lineage differs from the artifact manifest", profile.Name)
		}
		for index, revision := range profile.Revisions {
			identity := bundle.Profiles[index]
			if identity.Name != profile.Name || identity.Revision != revision.Number || identity.SpecDigest != revision.SpecDigest {
				return fmt.Errorf("SecondBox standard bundle %q revision %d identity differs from the artifact manifest", profile.Name, revision.Number)
			}
		}
		return nil
	}
	return fmt.Errorf("SecondBox standard bundle %q is absent from the artifact manifest", profile.Name)
}

func appendOrValidatePool(pools []resourceapply.RunnerPool, binding PoolBinding) ([]resourceapply.RunnerPool, error) {
	for _, pool := range pools {
		if pool.Name == binding.Name {
			if !reflect.DeepEqual(pool.Architectures, binding.Architectures) || !reflect.DeepEqual(pool.Capabilities, binding.Capabilities) || pool.State != binding.State {
				return nil, fmt.Errorf("SecondBox standard bundles bind RunnerPool %q inconsistently", binding.Name)
			}
			return pools, nil
		}
	}
	return append(pools, resourceapply.RunnerPool{Name: binding.Name, Architectures: binding.Architectures, Capabilities: binding.Capabilities, State: binding.State, MutableFields: []string{"state"}}), nil
}

func agentSpec(pool, architecture string, maximumDeadlineMilliseconds int64) secondboxclient.ProfileRevisionSpec {
	requiresTenantEgressContext := true
	return secondboxclient.ProfileRevisionSpec{
		Pool: pool, Architecture: architecture,
		Resources: secondboxclient.ResourcePolicy{VCPUCount: 1, MemoryBytes: 1 << 30, WorkspaceBytes: 2 << 30, ConcurrentOperations: 4},
		Startup:   secondboxclient.StartupPolicy{Mode: secondboxclient.StartupModeColdBoot},
		Lifecycle: secondboxclient.LifecyclePolicy{InitialState: secondboxclient.SandboxDesiredStateRunning, DrainGraceSeconds: 10, IdleSeconds: 60, MaximumDurationSeconds: 900, LeaseSeconds: 60},
		Retention: secondboxclient.RetentionPolicy{SnapshotLimit: 0, SnapshotRetentionSeconds: 3600},
		Execution: secondboxclient.ExecutionPolicy{MaximumDeadlineMilliseconds: secondboxclient.PolicyLimit(maximumDeadlineMilliseconds), MaximumBufferedOutputBytes: 1 << 20, StreamWindowBytes: 64 << 10, MaximumTransferBytes: 256 << 20, TerminalDetachSeconds: 0, DataPlaneTransport: "proxied"},
		Network:   secondboxclient.NetworkPolicy{Mode: "allow_list", Destinations: []secondboxclient.NetworkDestination{{Protocol: "https", Domain: AgentGateway, Port: 443}}, RequiresTenantEgressContext: &requiresTenantEgressContext},
		Ports:     []secondboxclient.PortPolicy{},
	}
}

func attributedAgentSpec(pool, architecture string) secondboxclient.ProfileRevisionSpec {
	spec := agentSpec(pool, architecture, 900000)
	spec.AttributedExecution = &secondboxclient.AttributedExecutionPolicy{Gateway: AgentGateway, MaximumConnections: 2}
	return spec
}

func isolatedAgentSpec(pool, architecture string) secondboxclient.ProfileRevisionSpec {
	spec := agentSpec(pool, architecture, 900000)
	requiresTenantEgressContext := false
	spec.Network = secondboxclient.NetworkPolicy{Mode: "deny_all", Destinations: []secondboxclient.NetworkDestination{}, RequiresTenantEgressContext: &requiresTenantEgressContext}
	return spec
}

func codingSpec(pool, architecture string) secondboxclient.ProfileRevisionSpec {
	requiresTenantEgressContext := true
	return secondboxclient.ProfileRevisionSpec{
		Pool: pool, Architecture: architecture,
		Resources: secondboxclient.ResourcePolicy{VCPUCount: DurableCodingVCPUCount, MemoryBytes: DurableCodingMemoryBytes, WorkspaceBytes: DurableCodingWorkspaceBytes, ConcurrentOperations: DurableCodingConcurrentOperations},
		Startup:   secondboxclient.StartupPolicy{Mode: secondboxclient.StartupModeColdBoot},
		Lifecycle: secondboxclient.LifecyclePolicy{InitialState: secondboxclient.SandboxDesiredStateRunning, DrainGraceSeconds: 120, IdleSeconds: 28800, MaximumDurationSeconds: 604800, LeaseSeconds: 300},
		Retention: secondboxclient.RetentionPolicy{SnapshotLimit: 64, SnapshotRetentionSeconds: 2592000},
		Execution: secondboxclient.ExecutionPolicy{MaximumDeadlineMilliseconds: 86400000, MaximumBufferedOutputBytes: 16 << 20, StreamWindowBytes: 1 << 20, MaximumTransferBytes: 10 << 30, TerminalDetachSeconds: 86400, DataPlaneTransport: "proxied"},
		Network:   secondboxclient.NetworkPolicy{Mode: "allow_list", Destinations: []secondboxclient.NetworkDestination{{Protocol: "https", Domain: PlatformGateway, Port: 443}}, RequiresTenantEgressContext: &requiresTenantEgressContext},
		Ports:     []secondboxclient.PortPolicy{{Name: "development-http", Port: 3000, Protocol: "http", MaximumSessions: 8, MaximumSessionSeconds: 86400}},
	}
}
