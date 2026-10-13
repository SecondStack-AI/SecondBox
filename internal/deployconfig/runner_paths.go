package deployconfig

import "path/filepath"

const (
	computeBackendFirecracker = "firecracker"
	computeBackendGVisor      = "gvisor"

	// The same-host gVisor Runner joins the deployment's Compose network and
	// dials the control plane's packaged Runner listener there.
	sameHostGVisorControlPlaneAddress = "control-plane:9443"
	// The same-host gVisor Runner owns a private network namespace, so no other
	// gVisor Runner can share its network profile.
	sameHostGVisorNetworkProfile = 0
)

type packagedRunnerPath struct {
	name  string
	field *string
	value string
}

// These paths belong to the packaged image and Compose mount layout. Remote
// Runner hosts own their paths and must supply them explicitly.
func (r *Runner) packagedPaths() []packagedRunnerPath {
	paths := []packagedRunnerPath{
		{"identity_directory", &r.IdentityDirectory, "/run/secondbox-runner-identity"},
		{"workspace_root", &r.WorkspaceRoot, "/var/lib/secondbox-runner/workspaces"},
		{"egress_context_config_path", &r.EgressContextConfigPath, "/run/secondbox-runner-config/egress-contexts.json"},
		{"log_path", &r.LogPath, "/var/lib/secondbox-runner/state/logs/runner.jsonl"},
		{"log_directory", &r.LogDirectory, "/var/lib/secondbox-runner/state/logs"},
	}
	if r.ComputeBackend == computeBackendGVisor {
		return append(paths,
			packagedRunnerPath{"gvisor_runsc_path", &r.GVisorRunscPath, "/opt/secondbox-gvisor/bin/runsc"},
			packagedRunnerPath{"gvisor_agent_path", &r.GVisorAgentPath, "/opt/secondbox-gvisor/bin/secondbox-guest-agent"},
			packagedRunnerPath{"gvisor_flat_root_path", &r.GVisorFlatRootPath, "/opt/secondbox-gvisor/rootfs"},
			packagedRunnerPath{"gvisor_materialization_path", &r.GVisorMaterializationPath, "/opt/secondbox-gvisor/materialization.json"},
			packagedRunnerPath{"gvisor_runtime_directory", &r.GVisorRuntimeDirectory, "/run/secondbox-gvisor"},
		)
	}
	return append(paths,
		packagedRunnerPath{"firecracker_path", &r.FirecrackerPath, "/usr/local/bin/firecracker"},
		packagedRunnerPath{"firecracker_jailer_path", &r.FirecrackerJailerPath, "/usr/local/bin/jailer"},
		packagedRunnerPath{"firecracker_jail_root", &r.FirecrackerJailRoot, "/var/lib/secondbox-runner/jail"},
		packagedRunnerPath{"firecracker_kernel_path", &r.FirecrackerKernelPath, "/opt/secondbox-artifacts/kernel"},
		packagedRunnerPath{"firecracker_rootfs_path", &r.FirecrackerRootFSPath, "/opt/secondbox-artifacts/rootfs.ext4"},
		packagedRunnerPath{"firecracker_shared_image_path", &r.FirecrackerSharedImagePath, "/opt/secondbox-artifacts/shared.img"},
		packagedRunnerPath{"firecracker_run_directory", &r.FirecrackerRunDirectory, "/var/lib/secondbox-runner/state/run"},
		packagedRunnerPath{"firecracker_log_directory", &r.FirecrackerLogDirectory, "/var/lib/secondbox-runner/state/firecracker-logs"},
		packagedRunnerPath{"snapshot_template_cache_root", &r.SnapshotTemplateCacheRoot, "/var/lib/secondbox-runner/state/snapshot-template-cache"},
		packagedRunnerPath{"sandbox_network_state_directory", &r.SandboxNetworkStateDir, "/var/lib/secondbox-runner/state/network"},
		packagedRunnerPath{"network_policy_nft_path", &r.NetworkPolicyNFTPath, "/usr/sbin/nft"},
	)
}

// sameHostGVisorDerivedSettings are rendered by the package rather than the
// operator for a same-host gVisor Runner.
func (r *Runner) sameHostGVisorDerivedSettings() map[string]bool {
	return map[string]bool{
		"control_plane_address":  r.ControlPlaneAddress != "",
		"gvisor_network_profile": r.GVisorNetworkProfile != nil,
	}
}

func (r Runner) withPackagedPaths() Runner {
	if r.Placement == "same-host" {
		for _, path := range r.packagedPaths() {
			*path.field = path.value
		}
		if r.ComputeBackend == computeBackendGVisor {
			r.ControlPlaneAddress = sameHostGVisorControlPlaneAddress
			r.GVisorNetworkProfile = integer(sameHostGVisorNetworkProfile)
		}
	}
	return r
}

// Compose mounts the storage root once; its existing workspaces child must
// retain that mount identity for jailer hard links and reflink persistence.
func (r Runner) workspaceHostDirectory() string {
	return filepath.Join(r.StateHostDirectory, "workspaces")
}
