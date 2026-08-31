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
	PoolAMD64                = "standard-amd64"

	DurableCodingVCPUCount            = int64(4)
	DurableCodingMemoryBytes          = int64(8 << 30)
	DurableCodingWorkspaceBytes       = int64(50 << 30)
	DurableCodingConcurrentOperations = int64(16)

	AgentGateway    = "agent-gateway.secondbox.internal"
	PlatformGateway = "platform-gateway.secondbox.internal"

	v030RuntimeBundleDigest   = "sha256:9279ca3f8bc3eac4adcd1953926a33fc42da99641d60af042eea12eb12ba0335"
	v030ToolchainBundleDigest = "sha256:cd859a7b0ef9849cc842c8b9c4d0b3b21340e50bed1ac712126585a9fa5553b4"
	developmentVersion        = "0.0.0-development"
	developmentTag            = "v0.0.0-development"
	developmentSourceCommit   = "dddddddddddddddddddddddddddddddddddddddd"
)

type PoolBinding struct {
	Name           string
	Architectures  []string
	Capabilities   []string
	CapacityPolicy map[string]int64
	State          string
}

type Selection struct {
	Bundles []string
	Pools   map[string]PoolBinding
}

// BundleNames returns every release-owned standard bundle in artifact order.
func BundleNames() []string {
	return []string{AgentCompartment, DurableCoding, AgentCompartmentIsolated}
}

// Build resolves execution asset digests only from the already-validated
// artifact manifest. Consumer repositories never copy release digests.
func Build(manifest releasecontract.ArtifactManifest, selection Selection) (resourceapply.Document, error) {
	if err := manifest.Validate(); err != nil {
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
		if binding.Name != PoolAMD64 || !slices.Contains(binding.Architectures, ArchitectureAMD64) {
			return resourceapply.Document{}, fmt.Errorf("SecondBox standard bundle %q requires an amd64 RunnerPool", name)
		}
		updatedPools, err := appendOrValidatePool(document.RunnerPools, binding)
		if err != nil {
			return resourceapply.Document{}, err
		}
		document.RunnerPools = updatedPools
		profile, err := profileLineageForManifest(manifest, name)
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

func profileLineageForManifest(manifest releasecontract.ArtifactManifest, name string) (resourceapply.Profile, error) {
	if manifest.Version == developmentVersion && manifest.Tag == developmentTag && manifest.SourceCommit == developmentSourceCommit {
		return DevelopmentProfileLineage(name, manifest.MicroVM.RuntimeBundle.ManifestDigest, manifest.MicroVM.ToolchainBundle.ManifestDigest)
	}
	return ProfileLineage(name, manifest.MicroVM.RuntimeBundle.ManifestDigest, manifest.MicroVM.ToolchainBundle.ManifestDigest)
}

// ProfileLineage returns the complete ordered lineage for one architecture-qualified standard Profile.
func ProfileLineage(name, runtimeDigest, toolchainDigest string) (resourceapply.Profile, error) {
	var specs []secondboxclient.ProfileRevisionSpec
	switch name {
	case AgentCompartment:
		// Standard Profile history is append-only. Keep every shipped spec here in
		// revision order so an existing deployment can prove its immutable prefix.
		specs = []secondboxclient.ProfileRevisionSpec{
			agentSpec(PoolAMD64, v030RuntimeBundleDigest, v030ToolchainBundleDigest, 120000),
			// Callers may request a command deadline up to the Sandbox's outer lifetime.
			agentSpec(PoolAMD64, v030RuntimeBundleDigest, v030ToolchainBundleDigest, 900000),
		}
		if runtimeDigest != v030RuntimeBundleDigest || toolchainDigest != v030ToolchainBundleDigest {
			specs = append(specs, agentSpec(PoolAMD64, runtimeDigest, toolchainDigest, 900000))
		}
	case DurableCoding:
		specs = []secondboxclient.ProfileRevisionSpec{codingSpec(PoolAMD64, v030RuntimeBundleDigest, v030ToolchainBundleDigest)}
		if runtimeDigest != v030RuntimeBundleDigest || toolchainDigest != v030ToolchainBundleDigest {
			specs = append(specs, codingSpec(PoolAMD64, runtimeDigest, toolchainDigest))
		}
	case AgentCompartmentIsolated:
		specs = []secondboxclient.ProfileRevisionSpec{isolatedAgentSpec(PoolAMD64, v030RuntimeBundleDigest, v030ToolchainBundleDigest)}
		if runtimeDigest != v030RuntimeBundleDigest || toolchainDigest != v030ToolchainBundleDigest {
			specs = append(specs, isolatedAgentSpec(PoolAMD64, runtimeDigest, toolchainDigest))
		}
	default:
		return resourceapply.Profile{}, fmt.Errorf("SecondBox standard bundle %q is unknown", name)
	}
	return profileFromSpecs(name, specs)
}

// DevelopmentProfileLineage returns a synthetic, current-only lineage for the
// explicit local development release identity. It never imports published
// history whose signed component assets are absent from the development catalog.
func DevelopmentProfileLineage(name, runtimeDigest, toolchainDigest string) (resourceapply.Profile, error) {
	var specs []secondboxclient.ProfileRevisionSpec
	switch name {
	case AgentCompartment:
		specs = []secondboxclient.ProfileRevisionSpec{agentSpec(PoolAMD64, runtimeDigest, toolchainDigest, 900000)}
	case DurableCoding:
		specs = []secondboxclient.ProfileRevisionSpec{codingSpec(PoolAMD64, runtimeDigest, toolchainDigest)}
	case AgentCompartmentIsolated:
		specs = []secondboxclient.ProfileRevisionSpec{isolatedAgentSpec(PoolAMD64, runtimeDigest, toolchainDigest)}
	default:
		return resourceapply.Profile{}, fmt.Errorf("SecondBox standard bundle %q is unknown", name)
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
			if !reflect.DeepEqual(pool.Architectures, binding.Architectures) || !reflect.DeepEqual(pool.Capabilities, binding.Capabilities) || !reflect.DeepEqual(pool.CapacityPolicy, binding.CapacityPolicy) || pool.State != binding.State {
				return nil, fmt.Errorf("SecondBox standard bundles bind RunnerPool %q inconsistently", binding.Name)
			}
			return pools, nil
		}
	}
	return append(pools, resourceapply.RunnerPool{Name: binding.Name, Architectures: binding.Architectures, Capabilities: binding.Capabilities, CapacityPolicy: binding.CapacityPolicy, State: binding.State, MutableFields: []string{"capacityPolicy", "state"}}), nil
}

func agentSpec(pool, runtimeDigest, toolchainDigest string, maximumDeadlineMilliseconds int64) secondboxclient.ProfileRevisionSpec {
	requiresTenantEgressContext := true
	return secondboxclient.ProfileRevisionSpec{
		Pool: pool, Architecture: ArchitectureAMD64, RuntimeBundleDigest: runtimeDigest, ToolchainBundleDigest: toolchainDigest,
		Resources: secondboxclient.ResourcePolicy{VCPUCount: 1, MemoryBytes: 1 << 30, WorkspaceBytes: 2 << 30, ConcurrentOperations: 4},
		Startup:   secondboxclient.StartupPolicy{Mode: secondboxclient.StartupModeColdBoot},
		Lifecycle: secondboxclient.LifecyclePolicy{InitialState: secondboxclient.SandboxDesiredStateRunning, DrainGraceSeconds: 10, IdleSeconds: 60, MaximumDurationSeconds: 900, LeaseSeconds: 60},
		Retention: secondboxclient.RetentionPolicy{SnapshotLimit: 0, SnapshotRetentionSeconds: 3600},
		Execution: secondboxclient.ExecutionPolicy{MaximumDeadlineMilliseconds: maximumDeadlineMilliseconds, MaximumBufferedOutputBytes: 1 << 20, StreamWindowBytes: 64 << 10, MaximumTransferBytes: 256 << 20, TerminalDetachSeconds: 0, DataPlaneTransport: "proxied"},
		Network:   secondboxclient.NetworkPolicy{Mode: "allow_list", Destinations: []secondboxclient.NetworkDestination{{Protocol: "https", Domain: AgentGateway, Port: 443}}, RequiresTenantEgressContext: &requiresTenantEgressContext},
		Ports:     []secondboxclient.PortPolicy{},
	}
}

func isolatedAgentSpec(pool, runtimeDigest, toolchainDigest string) secondboxclient.ProfileRevisionSpec {
	spec := agentSpec(pool, runtimeDigest, toolchainDigest, 900000)
	requiresTenantEgressContext := false
	spec.Network = secondboxclient.NetworkPolicy{Mode: "deny_all", Destinations: []secondboxclient.NetworkDestination{}, RequiresTenantEgressContext: &requiresTenantEgressContext}
	return spec
}

func codingSpec(pool, runtimeDigest, toolchainDigest string) secondboxclient.ProfileRevisionSpec {
	requiresTenantEgressContext := true
	return secondboxclient.ProfileRevisionSpec{
		Pool: pool, Architecture: ArchitectureAMD64, RuntimeBundleDigest: runtimeDigest, ToolchainBundleDigest: toolchainDigest,
		Resources: secondboxclient.ResourcePolicy{VCPUCount: DurableCodingVCPUCount, MemoryBytes: DurableCodingMemoryBytes, WorkspaceBytes: DurableCodingWorkspaceBytes, ConcurrentOperations: DurableCodingConcurrentOperations},
		Startup:   secondboxclient.StartupPolicy{Mode: secondboxclient.StartupModeColdBoot},
		Lifecycle: secondboxclient.LifecyclePolicy{InitialState: secondboxclient.SandboxDesiredStateRunning, DrainGraceSeconds: 120, IdleSeconds: 28800, MaximumDurationSeconds: 604800, LeaseSeconds: 300},
		Retention: secondboxclient.RetentionPolicy{SnapshotLimit: 64, SnapshotRetentionSeconds: 2592000},
		Execution: secondboxclient.ExecutionPolicy{MaximumDeadlineMilliseconds: 86400000, MaximumBufferedOutputBytes: 16 << 20, StreamWindowBytes: 1 << 20, MaximumTransferBytes: 10 << 30, TerminalDetachSeconds: 86400, DataPlaneTransport: "proxied"},
		Network:   secondboxclient.NetworkPolicy{Mode: "allow_list", Destinations: []secondboxclient.NetworkDestination{{Protocol: "https", Domain: PlatformGateway, Port: 443}}, RequiresTenantEgressContext: &requiresTenantEgressContext},
		Ports:     []secondboxclient.PortPolicy{{Name: "development-http", Port: 3000, Protocol: "http", MaximumSessions: 8, MaximumSessionSeconds: 86400}},
	}
}
