package deployconfig

import "fmt"

const runnerTemplate = `# Identity and placement
[[runners]]
# Immutable opaque Runner ID; start with an ASCII letter or digit, then use at most 127 letters, digits, dots, underscores, colons, or hyphens.
runner_id = ''
# Runner location; must be same-host or remote, with at most one same-host Runner.
placement = ''
# RunnerPool name; required and must match the selected standard-resource inventory that admits this Runner.
pool_id = ''
# Runner software version reported to the control plane; required.
software_version = ''
# Authenticated control-plane Runner endpoint; required.
control_plane_address = ''
# TLS server name for the control-plane Runner endpoint; required.
control_plane_server_name = ''
# Runner identity directory; absolute on the Runner, and /run/secondbox-runner-identity for same-host placement.
identity_directory = ''
# Identity directory on the Runner host; absolute when set and required for same-host placement.
identity_host_directory = '<replace-with-absolute-runner-host-path>'

# Artifact trust
# Execution-asset directory on the Runner host; absolute when set and required for same-host placement.
artifact_host_directory = '<replace-with-absolute-runner-host-path>'
# Provisioned signed-artifact public key; an absolute Runner-host path within /opt/secondbox-artifacts for same-host placement.
artifact_public_key = ''
# Provisioned signed-artifact key fingerprint; exactly 64 lowercase hexadecimal characters and not all zeroes.
artifact_public_key_sha256 = '0000000000000000000000000000000000000000000000000000000000000000'

# Runner state
# Durable state directory on the Runner host; absolute when set and required for same-host placement.
state_host_directory = '<replace-with-absolute-runner-host-path>'
# Runner JSON log path; an absolute Runner-host path within /var/lib/secondbox-runner for same-host placement.
log_path = ''
# Runner log directory; required and absolute, and within /var/lib/secondbox-runner for same-host placement.
log_directory = ''

# Workspace persistence
# Reflink-capable workspace directory on the Runner host; absolute when set and required for same-host placement.
workspace_host_directory = '<replace-with-absolute-runner-host-path>'
# Workspace root seen by the Runner; an absolute Runner-host path.
workspace_root = ''
# Storage-pressure recovery threshold; positive and lower than warning and admission-deny thresholds.
storage_pressure_recovery_percent = 0
# Storage-pressure warning threshold; positive and between recovery and admission-deny thresholds.
storage_pressure_warning_percent = 0
# Storage-pressure admission-deny threshold; positive, above warning, and below 100.
storage_pressure_admission_deny_percent = 0

# Firecracker
# Firecracker executable; an absolute Runner-host path.
firecracker_path = ''
# Firecracker jailer executable; an absolute Runner-host path.
firecracker_jailer_path = ''
# Firecracker jail root; absolute and within /var/lib/secondbox-runner for same-host placement.
firecracker_jail_root = ''
# First per-Instance jailer user ID; must be at least 1000 unless the explicit lower-bound acknowledgement is true, and the range must not include UID 0.
firecracker_jailer_uid_start = 0
# Number of distinct jailer user IDs; positive and at least max_concurrent_global.
firecracker_jailer_uid_count = 0
# Explicit acknowledgement for a jailer UID range starting below 1000; required, so replace this string with a Boolean.
firecracker_jailer_uid_allow_below_1000 = '<replace-with-boolean>'
# Jailer group ID; must be positive.
firecracker_jailer_gid = 0
# Host cgroup version used by the jailer; must be positive.
firecracker_cgroup_version = 0
# Host cgroup parent used by the jailer; required.
firecracker_cgroup_parent = ''
# Guest kernel; absolute and within /opt/secondbox-artifacts for same-host placement.
firecracker_kernel_path = ''
# Guest root filesystem; absolute and within /opt/secondbox-artifacts for same-host placement.
firecracker_rootfs_path = ''
# Shared guest image; absolute and within /opt/secondbox-artifacts for same-host placement.
firecracker_shared_image_path = ''
# Kernel arguments; must include console=ttyS0, reboot=k, panic=1, pci=off, root=/dev/vda, rw, quiet, loglevel=1, i8042.noaux, i8042.nomux, i8042.nopnp, i8042.dumbkbd, and init=/init.
firecracker_kernel_args = ''
# Firecracker CPU template; required.
firecracker_cpu_template = ''
# Firecracker runtime directory; absolute and within /var/lib/secondbox-runner for same-host placement.
firecracker_run_directory = ''
# Firecracker log directory; absolute and within /var/lib/secondbox-runner for same-host placement.
firecracker_log_directory = ''
# Packaged Runner jail policy; must be false.
firecracker_allow_unjailed = true

# Sandbox networking
# Guest IP address assigned to Sandboxes; must be an IP address.
sandbox_guest_ip = ''
# Host bridge name used by Sandboxes; required.
sandbox_bridge_name = ''
# Host bridge network; must be a CIDR.
sandbox_bridge_cidr = ''
# Guest address range; must be a CIDR.
sandbox_guest_cidr = ''
# Prefix for per-Sandbox TAP interfaces; required.
sandbox_tap_prefix = ''
# Persisted network state; absolute and within /var/lib/secondbox-runner for same-host placement.
sandbox_network_state_directory = ''
# Bridge cleanup policy; required, so replace this string with an explicit Boolean.
sandbox_delete_bridge = '<replace-with-boolean>'
# nft executable; an absolute Runner-host path.
network_policy_nft_path = ''
# Maximum pinned DNS answers; must be positive.
network_policy_max_dns_pins = 0
# Maximum DNS TTL; must be a positive Go duration.
network_policy_max_dns_ttl = ''
# Runner-local addresses; a comma-separated list of IP addresses.
network_policy_runner_addresses = ''
# Management networks; a comma-separated list of CIDRs.
network_policy_management_cidrs = ''
# Logical gateways; unique domain=IP pairs or none, including every gateway required by selected standard bundles.
network_policy_runner_gateways = ''
# Upstream DNS resolver; must be an IP:port with a nonzero port.
network_policy_dns_upstream = ''

# Capacity
# Maximum vCPUs per Sandbox; must be positive.
sandbox_max_vcpus = 0
# Maximum memory per Sandbox in MiB; must be positive.
sandbox_max_memory_mib = 0
# Maximum disk per Sandbox in MiB; must be positive.
sandbox_max_disk_mib = 0
# Aggregate Sandbox memory budget in MiB; must be positive.
sandbox_memory_budget_mib = 0
# Maximum concurrent commands per Sandbox; must be positive.
max_concurrent_per_sandbox = 0
# Maximum concurrent commands across the Runner; must be positive.
max_concurrent_global = 0
# Maximum concurrent starts; positive and no greater than max_concurrent_global.
max_concurrent_starts = 0
# Maximum concurrent Workspace creations; must be positive.
max_concurrent_workspace_creates = 0
# Maximum concurrent operations across the Runner; must be positive.
max_concurrent_operations_global = 0
# Maximum bytes per file transfer; must be positive.
file_transfer_max_bytes = 0

# Data plane
# Guest-control vsock port; a positive integer through 65535 distinct from the guest-protocol port.
guest_control_vsock_port = 0
# Guest-protocol vsock port; a positive integer through 65535 distinct from the guest-control port.
guest_protocol_vsock_port = 0
# Guest heartbeat cadence; a Go duration from 1ms through 1m.
guest_heartbeat_interval = ''
# Runner data-plane listener; a host:port with a port from 0 through 65535.
data_plane_listen_address = ''
# Reachable Runner data-plane endpoint; host:port with an explicit host and a port from 1 through 65535.
data_plane_advertised_address = ''
`

// RunnerTemplate returns the inert, complete Runner declaration scaffold.
func RunnerTemplate() []byte {
	return []byte(runnerTemplate)
}

// WriteRunnerTemplate creates one Runner declaration scaffold without replacing
// an existing file.
func WriteRunnerTemplate(path string) error {
	if path == "" {
		return fmt.Errorf("SecondBox Runner declaration template: output path is required")
	}
	if err := writeAtomic(path, RunnerTemplate(), 0o644, false); err != nil {
		return fmt.Errorf("SecondBox Runner declaration template: write output: %w", err)
	}
	return nil
}
