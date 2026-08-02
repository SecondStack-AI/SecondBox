// Package deployconfig compiles the sole operator-edited SecondBox deployment
// manifest into explicit process environment artifacts.
package deployconfig

// ManifestV1 is the strict schema_version=1 deployment source.
type ManifestV1 struct {
	SchemaVersion int             `toml:"schema_version"`
	Deployment    Deployment      `toml:"deployment"`
	Database      Database        `toml:"database"`
	ObjectStore   ObjectStore     `toml:"object_store"`
	RunnerTrust   RunnerTrust     `toml:"runner_trust"`
	Runners       []Runner        `toml:"runners"`
	Applications  Applications    `toml:"applications"`
	Policy        Policy          `toml:"policy"`
	Overrides     TuningOverrides `toml:"overrides"`
}

type Deployment struct {
	Mode                   string `toml:"mode"`
	PublicBaseURL          string `toml:"public_base_url"`
	TLSTermination         string `toml:"tls_termination"`
	ControlPlaneImage      string `toml:"control_plane_image"`
	RunnerImage            string `toml:"runner_image"`
	PostgresImage          string `toml:"postgres_image"`
	ObjectStoreImage       string `toml:"object_store_image"`
	ObjectStoreClientImage string `toml:"object_store_client_image"`
	APIBindIP              string `toml:"api_bind_ip"`
	APIPublishedPort       *int64 `toml:"api_published_port"`
	ListenAddress          string `toml:"listen_address"`
	RunnerBindIP           string `toml:"runner_bind_ip"`
	RunnerPublishedPort    *int64 `toml:"runner_published_port"`
	RunnerListenAddress    string `toml:"runner_listen_address"`
	LogPath                string `toml:"log_path"`
	SignedAssetCatalog     string `toml:"signed_asset_catalog"`
	SignedAssetCatalogPath string `toml:"signed_asset_catalog_path"`
	DevelopmentWaitSeconds *int64 `toml:"development_prepare_wait_timeout_seconds"`
}

type Database struct {
	Mode          string `toml:"mode"`
	URLFile       string `toml:"url_file"`
	BindIP        string `toml:"bind_ip"`
	PublishedPort *int64 `toml:"published_port"`
	Name          string `toml:"name"`
	User          string `toml:"user"`
	PasswordFile  string `toml:"password_file"`
}

type ObjectStore struct {
	Mode                 string `toml:"mode"`
	Endpoint             string `toml:"endpoint"`
	Bucket               string `toml:"bucket"`
	Region               string `toml:"region"`
	UsePathStyle         *bool  `toml:"use_path_style"`
	TempDirectory        string `toml:"temp_directory"`
	AccessKeyFile        string `toml:"access_key_file"`
	SecretKeyFile        string `toml:"secret_key_file"`
	BindIP               string `toml:"bind_ip"`
	PublishedPort        *int64 `toml:"published_port"`
	ConsolePublishedPort *int64 `toml:"console_published_port"`
}

type RunnerTrust struct {
	EnrollmentCredentialFile string `toml:"enrollment_credential_file"`
	CACertificateFile        string `toml:"ca_certificate_file"`
	CAPrivateKeyFile         string `toml:"ca_private_key_file"`
	ServerCertificateFile    string `toml:"server_certificate_file"`
	ServerPrivateKeyFile     string `toml:"server_private_key_file"`
	ServerName               string `toml:"server_name"`
	CertificateLifetimeDays  *int64 `toml:"certificate_lifetime_days"`
}

type Applications struct {
	PlatformTokenFile          string `toml:"platform_token_file"`
	ApplicationAuthoritiesFile string `toml:"application_authorities_file"`
}

type Policy struct {
	DataPlaneRetentionSeconds              *int64 `toml:"data_plane_retention_seconds"`
	DataPlanePollIntervalMilliseconds      *int64 `toml:"data_plane_poll_interval_milliseconds"`
	RunnerCommandPollIntervalMilliseconds  *int64 `toml:"runner_command_poll_interval_milliseconds"`
	RunnerEnabledFeatures                  string `toml:"runner_enabled_features"`
	DefaultSubjectMaxSandboxes             *int64 `toml:"default_subject_max_sandboxes"`
	DefaultSubjectMaxActiveInstances       *int64 `toml:"default_subject_max_active_instances"`
	DefaultSubjectMaxCPUMillis             *int64 `toml:"default_subject_max_cpu_millis"`
	DefaultSubjectMaxMemoryBytes           *int64 `toml:"default_subject_max_memory_bytes"`
	DefaultSubjectMaxArtifactBytes         *int64 `toml:"default_subject_max_artifact_bytes"`
	DefaultSubjectMaxSnapshots             *int64 `toml:"default_subject_max_snapshots"`
	DefaultSubjectMaxArtifacts             *int64 `toml:"default_subject_max_artifacts"`
	DefaultSubjectMaxPortSessions          *int64 `toml:"default_subject_max_port_sessions"`
	DefaultSubjectMaxConcurrentOperations  *int64 `toml:"default_subject_max_concurrent_operations"`
	AgentCompartmentPool                   string `toml:"agent_compartment_pool"`
	AgentCompartmentRuntimeBundleDigest    string `toml:"agent_compartment_runtime_bundle_digest"`
	AgentCompartmentToolchainBundleDigest  string `toml:"agent_compartment_toolchain_bundle_digest"`
	CodingEnvironmentPool                  string `toml:"coding_environment_pool"`
	CodingEnvironmentRuntimeBundleDigest   string `toml:"coding_environment_runtime_bundle_digest"`
	CodingEnvironmentToolchainBundleDigest string `toml:"coding_environment_toolchain_bundle_digest"`
}

// TuningOverrides owns the public TOML names for all Category C overrides.
type TuningOverrides struct {
	HTTPTimeoutSeconds                          *int64 `toml:"http_timeout_seconds"`
	RunnerHeartbeatIntervalMilliseconds         *int64 `toml:"runner_heartbeat_interval_milliseconds"`
	RunnerHeartbeatTimeoutMilliseconds          *int64 `toml:"runner_heartbeat_timeout_milliseconds"`
	RunnerCommandDeliveryBatchSize              *int64 `toml:"runner_command_delivery_batch_size"`
	RunnerEventPersistenceBatchSize             *int64 `toml:"runner_event_persistence_batch_size"`
	RunnerEventPersistenceBatchWaitMilliseconds *int64 `toml:"runner_event_persistence_batch_wait_milliseconds"`
	DataPlaneClaimDurationMilliseconds          *int64 `toml:"data_plane_claim_duration_milliseconds"`
	DataPlaneMaximumFrameBytes                  *int64 `toml:"data_plane_maximum_frame_bytes"`
	DataPlaneMaximumSessionBytes                *int64 `toml:"data_plane_maximum_session_bytes"`
	LifecycleReconcileBatchSize                 *int64 `toml:"lifecycle_reconcile_batch_size"`
	LifecycleReconcilePollIntervalMilliseconds  *int64 `toml:"lifecycle_reconcile_poll_interval_milliseconds"`
	LifecycleReconcileClaimDurationMilliseconds *int64 `toml:"lifecycle_reconcile_claim_duration_milliseconds"`
	AssignmentClaimDurationMilliseconds         *int64 `toml:"assignment_claim_duration_milliseconds"`
	AssignmentDeadlineMilliseconds              *int64 `toml:"assignment_deadline_milliseconds"`
	AssignmentRetryLimit                        *int64 `toml:"assignment_retry_limit"`
	SchedulerSerializationRetryLimit            *int64 `toml:"scheduler_serialization_retry_limit"`
	ObjectStoreRetryMaxAttempts                 *int64 `toml:"object_store_retry_max_attempts"`
	ObjectStoreHTTPTimeoutMilliseconds          *int64 `toml:"object_store_http_timeout_milliseconds"`
	ObjectStoreMaxObjectBytes                   *int64 `toml:"object_store_max_object_bytes"`
}

// Runner is one immutable runner_id and its typed, placement-local runtime
// contract. Host paths remain opaque strings for remote placement.
type Runner struct {
	RunnerID                      string `toml:"runner_id"`
	Placement                     string `toml:"placement"`
	PoolID                        string `toml:"pool_id"`
	SoftwareVersion               string `toml:"software_version"`
	ControlPlaneAddress           string `toml:"control_plane_address"`
	ControlPlaneServerName        string `toml:"control_plane_server_name"`
	IdentityDirectory             string `toml:"identity_directory"`
	IdentityHostDirectory         string `toml:"identity_host_directory"`
	ArtifactHostDirectory         string `toml:"artifact_host_directory"`
	StateHostDirectory            string `toml:"state_host_directory"`
	WorkspaceHostDirectory        string `toml:"workspace_host_directory"`
	LogPath                       string `toml:"log_path"`
	LogDirectory                  string `toml:"log_directory"`
	FirecrackerPath               string `toml:"firecracker_path"`
	FirecrackerJailerPath         string `toml:"firecracker_jailer_path"`
	FirecrackerJailRoot           string `toml:"firecracker_jail_root"`
	FirecrackerJailerUID          *int64 `toml:"firecracker_jailer_uid"`
	FirecrackerJailerGID          *int64 `toml:"firecracker_jailer_gid"`
	FirecrackerCgroupVersion      *int64 `toml:"firecracker_cgroup_version"`
	FirecrackerCgroupParent       string `toml:"firecracker_cgroup_parent"`
	FirecrackerKernelPath         string `toml:"firecracker_kernel_path"`
	FirecrackerRootFSPath         string `toml:"firecracker_rootfs_path"`
	FirecrackerSharedImagePath    string `toml:"firecracker_shared_image_path"`
	FirecrackerKernelArgs         string `toml:"firecracker_kernel_args"`
	FirecrackerCPUTemplate        string `toml:"firecracker_cpu_template"`
	FirecrackerRunDirectory       string `toml:"firecracker_run_directory"`
	FirecrackerLogDirectory       string `toml:"firecracker_log_directory"`
	FirecrackerAllowUnjailed      *bool  `toml:"firecracker_allow_unjailed"`
	ArtifactPublicKey             string `toml:"artifact_public_key"`
	ArtifactPublicKeySHA256       string `toml:"artifact_public_key_sha256"`
	WorkspaceRoot                 string `toml:"workspace_root"`
	StorageRecoveryPercent        *int64 `toml:"storage_pressure_recovery_percent"`
	StorageWarningPercent         *int64 `toml:"storage_pressure_warning_percent"`
	StorageAdmissionDenyPercent   *int64 `toml:"storage_pressure_admission_deny_percent"`
	SandboxMaxVCPUs               *int64 `toml:"sandbox_max_vcpus"`
	SandboxMaxMemoryMiB           *int64 `toml:"sandbox_max_memory_mib"`
	SandboxMaxDiskMiB             *int64 `toml:"sandbox_max_disk_mib"`
	SandboxMemoryBudgetMiB        *int64 `toml:"sandbox_memory_budget_mib"`
	SandboxGuestIP                string `toml:"sandbox_guest_ip"`
	SandboxBridgeName             string `toml:"sandbox_bridge_name"`
	SandboxBridgeCIDR             string `toml:"sandbox_bridge_cidr"`
	SandboxGuestCIDR              string `toml:"sandbox_guest_cidr"`
	SandboxTapPrefix              string `toml:"sandbox_tap_prefix"`
	SandboxNetworkStateDir        string `toml:"sandbox_network_state_directory"`
	SandboxDeleteBridge           *bool  `toml:"sandbox_delete_bridge"`
	NetworkPolicyNFTPath          string `toml:"network_policy_nft_path"`
	NetworkPolicyMaxDNSPins       *int64 `toml:"network_policy_max_dns_pins"`
	NetworkPolicyMaxDNSTTL        string `toml:"network_policy_max_dns_ttl"`
	NetworkPolicyRunnerAddresses  string `toml:"network_policy_runner_addresses"`
	NetworkPolicyManagementCIDRs  string `toml:"network_policy_management_cidrs"`
	NetworkPolicyRunnerGateways   string `toml:"network_policy_runner_gateways"`
	NetworkPolicyDNSUpstream      string `toml:"network_policy_dns_upstream"`
	MaxConcurrentPerSandbox       *int64 `toml:"max_concurrent_per_sandbox"`
	MaxConcurrentGlobal           *int64 `toml:"max_concurrent_global"`
	MaxConcurrentStarts           *int64 `toml:"max_concurrent_starts"`
	MaxConcurrentWorkspaceCreates *int64 `toml:"max_concurrent_workspace_creates"`
	MaxConcurrentOperationsGlobal *int64 `toml:"max_concurrent_operations_global"`
	FileTransferMaxBytes          *int64 `toml:"file_transfer_max_bytes"`
	GuestControlVSockPort         *int64 `toml:"guest_control_vsock_port"`
	GuestProtocolVSockPort        *int64 `toml:"guest_protocol_vsock_port"`
	GuestHeartbeatInterval        string `toml:"guest_heartbeat_interval"`
	DataPlaneListenAddress        string `toml:"data_plane_listen_address"`
	DataPlaneAdvertisedAddress    string `toml:"data_plane_advertised_address"`
}

// ResolvedDeployment is the typed, validated result. Environment is the
// Compose transport; RemoteRunnerEnvironment contains isolated systemd maps.
type ResolvedDeployment struct {
	Manifest                ManifestV1
	Environment             map[string]string
	RemoteRunnerEnvironment map[string]map[string]string
	ComposeFiles            []string
	SecretPaths             map[string]string
}
