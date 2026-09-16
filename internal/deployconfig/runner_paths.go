package deployconfig

import "path/filepath"

type packagedRunnerPath struct {
	name  string
	field *string
	value string
}

// These paths belong to the packaged image and Compose mount layout. Remote
// Runner hosts own their paths and must supply them explicitly.
func (r *Runner) packagedPaths() []packagedRunnerPath {
	return []packagedRunnerPath{
		{"identity_directory", &r.IdentityDirectory, "/run/secondbox-runner-identity"},
		{"workspace_root", &r.WorkspaceRoot, "/var/lib/secondbox-runner/workspaces"},
		{"egress_context_config_path", &r.EgressContextConfigPath, "/run/secondbox-runner-config/egress-contexts.json"},
		{"log_path", &r.LogPath, "/var/lib/secondbox-runner/state/logs/runner.jsonl"},
		{"log_directory", &r.LogDirectory, "/var/lib/secondbox-runner/state/logs"},
		{"firecracker_path", &r.FirecrackerPath, "/usr/local/bin/firecracker"},
		{"firecracker_jailer_path", &r.FirecrackerJailerPath, "/usr/local/bin/jailer"},
		{"firecracker_jail_root", &r.FirecrackerJailRoot, "/var/lib/secondbox-runner/jail"},
		{"firecracker_kernel_path", &r.FirecrackerKernelPath, "/opt/secondbox-artifacts/kernel"},
		{"firecracker_rootfs_path", &r.FirecrackerRootFSPath, "/opt/secondbox-artifacts/rootfs.ext4"},
		{"firecracker_shared_image_path", &r.FirecrackerSharedImagePath, "/opt/secondbox-artifacts/shared.img"},
		{"firecracker_run_directory", &r.FirecrackerRunDirectory, "/var/lib/secondbox-runner/state/run"},
		{"firecracker_log_directory", &r.FirecrackerLogDirectory, "/var/lib/secondbox-runner/state/firecracker-logs"},
		{"snapshot_template_cache_root", &r.SnapshotTemplateCacheRoot, "/var/lib/secondbox-runner/state/snapshot-template-cache"},
		{"artifact_public_key", &r.ArtifactPublicKey, "/opt/secondbox-artifacts/signing.pub"},
		{"execution_image_public_key", &r.ExecutionImagePublicKey, "/run/secondbox-image-trust/public.pem"},
		{"sandbox_network_state_directory", &r.SandboxNetworkStateDir, "/var/lib/secondbox-runner/state/network"},
		{"network_policy_nft_path", &r.NetworkPolicyNFTPath, "/usr/sbin/nft"},
	}
}

func (r Runner) withPackagedPaths() Runner {
	if r.Placement == "same-host" {
		for _, path := range r.packagedPaths() {
			*path.field = path.value
		}
	}
	return r
}

// Compose mounts the storage root once; its existing workspaces child must
// retain that mount identity for jailer hard links and reflink persistence.
func (r Runner) workspaceHostDirectory() string {
	return filepath.Join(r.StateHostDirectory, "workspaces")
}
