package deployconfig

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func MigrateLegacyEnvironment(sourcePath, targetDirectory string) (string, error) {
	sourceInfo, err := os.Lstat(sourcePath)
	if err != nil || !sourceInfo.Mode().IsRegular() || sourceInfo.Mode()&os.ModeSymlink != 0 {
		return "", manifestError("legacy migration source must be a regular non-symbolic-link file", err)
	}
	if sourceInfo.Mode().Perm()&0o077 != 0 {
		return "", manifestError("legacy migration source must not grant group or other access", nil)
	}
	known := legacyEnvironmentNames()
	values := make(map[string]string, len(known))
	file, err := os.Open(sourcePath)
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || name == "" || strings.TrimSpace(name) != name {
			file.Close()
			return "", manifestError(fmt.Sprintf("legacy line %d is invalid", lineNumber), nil)
		}
		if !known[name] {
			file.Close()
			return "", manifestError("legacy environment contains unknown name "+name, nil)
		}
		if _, exists := values[name]; exists {
			file.Close()
			return "", manifestError("legacy environment contains duplicate name "+name, nil)
		}
		if value == "" || strings.Contains(value, "REPLACE_WITH_") || strings.Contains(value, "GENERATE_") || strings.ContainsAny(value, "\x00\r") {
			file.Close()
			return "", manifestError("legacy environment contains an empty, placeholder, or invalid value for "+name, nil)
		}
		values[name] = value
	}
	closeErr := file.Close()
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	for name := range known {
		if _, exists := values[name]; !exists {
			return "", manifestError("legacy environment missing "+name, nil)
		}
	}
	absoluteTarget, err := filepath.Abs(targetDirectory)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(absoluteTarget); err == nil {
		return "", manifestError("legacy migration refuses an existing target", nil)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(absoluteTarget)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", manifestError("legacy migration target parent must be an existing non-symbolic-link directory", err)
	}
	if err := os.Mkdir(absoluteTarget, 0o700); err != nil {
		return "", err
	}
	created := true
	defer func() {
		if created {
			_ = os.RemoveAll(absoluteTarget)
		}
	}()
	secrets := filepath.Join(absoluteTarget, "secrets")
	if err := os.Mkdir(secrets, 0o700); err != nil {
		return "", err
	}
	writeSecret := func(name, value string) (string, error) {
		if strings.Contains(value, "\n") {
			return "", manifestError("legacy secret "+name+" contains a line break", nil)
		}
		if err := writeAtomic(filepath.Join(secrets, name), []byte(value+"\n"), 0o600, false); err != nil {
			return "", err
		}
		return "secrets/" + name, nil
	}
	postgresPassword, err := writeSecret("postgres-password", values["SECONDBOX_POSTGRES_PASSWORD"])
	if err != nil {
		return "", err
	}
	databaseURL, err := writeSecret("database-url", values["SECONDBOX_DATABASE_URL"])
	if err != nil {
		return "", err
	}
	objectAccess, err := writeSecret("object-store-access-key", values["SECONDBOX_OBJECT_STORE_ROOT_USER"])
	if err != nil {
		return "", err
	}
	objectSecret, err := writeSecret("object-store-secret-key", values["SECONDBOX_OBJECT_STORE_ROOT_PASSWORD"])
	if err != nil {
		return "", err
	}
	platformToken, err := writeSecret("platform-token", values["SECONDBOX_PLATFORM_TOKEN"])
	if err != nil {
		return "", err
	}
	runnerCredential, err := writeSecret("runner-enrollment-credential", values["SECONDBOX_RUNNER_CREDENTIAL"])
	if err != nil {
		return "", err
	}
	authorities, err := writeSecret("application-authorities.json", values["SECONDBOX_APPLICATION_AUTHORITIES_JSON"])
	if err != nil {
		return "", err
	}
	parseInt := func(name string) *int64 {
		value, parseErr := strconv.ParseInt(values[name], 10, 64)
		if parseErr != nil {
			return nil
		}
		return integer(value)
	}
	parseBool := func(name string) *bool {
		value, parseErr := strconv.ParseBool(values[name])
		if parseErr != nil {
			return nil
		}
		return boolean(value)
	}
	mode := values["SECONDBOX_DEPLOYMENT_MODE"]
	databaseMode := "external"
	objectMode := "external"
	if mode == "development" {
		databaseMode = "bundled"
		objectMode = "bundled"
	}
	manifest := ManifestV1{SchemaVersion: 1,
		Deployment:   Deployment{Mode: mode, PublicBaseURL: values["SECONDBOX_PUBLIC_BASE_URL"], TLSTermination: values["SECONDBOX_TLS_TERMINATION"], ControlPlaneImage: values["SECONDBOX_CONTROL_PLANE_IMAGE"], RunnerImage: values["SECONDBOX_RUNNER_IMAGE"], PostgresImage: values["SECONDBOX_POSTGRES_IMAGE"], ObjectStoreImage: values["SECONDBOX_OBJECT_STORE_IMAGE"], ObjectStoreClientImage: values["SECONDBOX_OBJECT_STORE_CLIENT_IMAGE"], APIBindIP: values["SECONDBOX_API_BIND_IP"], APIPublishedPort: parseInt("SECONDBOX_API_PUBLISHED_PORT"), ListenAddress: values["SECONDBOX_LISTEN_ADDR"], RunnerBindIP: values["SECONDBOX_RUNNER_BIND_IP"], RunnerPublishedPort: parseInt("SECONDBOX_RUNNER_PUBLISHED_PORT"), RunnerListenAddress: values["SECONDBOX_RUNNER_LISTEN_ADDR"], LogPath: values["SECONDBOX_LOG_PATH"], SignedAssetCatalog: values["SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH"], SignedAssetCatalogPath: values["SECONDBOX_SIGNED_ASSET_CATALOG_PATH"], DevelopmentWaitSeconds: parseInt("SECONDBOX_DEVELOPMENT_PREPARE_WAIT_TIMEOUT_SECONDS")},
		Database:     Database{Mode: databaseMode, URLFile: databaseURL, BindIP: values["SECONDBOX_POSTGRES_BIND_IP"], PublishedPort: parseInt("SECONDBOX_POSTGRES_PUBLISHED_PORT"), Name: values["SECONDBOX_POSTGRES_DATABASE"], User: values["SECONDBOX_POSTGRES_USER"], PasswordFile: postgresPassword},
		ObjectStore:  ObjectStore{Mode: objectMode, Endpoint: values["SECONDBOX_OBJECT_STORE_ENDPOINT"], Bucket: values["SECONDBOX_OBJECT_STORE_BUCKET"], Region: values["SECONDBOX_OBJECT_STORE_REGION"], UsePathStyle: parseBool("SECONDBOX_OBJECT_STORE_USE_PATH_STYLE"), TempDirectory: values["SECONDBOX_OBJECT_STORE_TEMP_DIRECTORY"], AccessKeyFile: objectAccess, SecretKeyFile: objectSecret, BindIP: values["SECONDBOX_OBJECT_STORE_BIND_IP"], PublishedPort: parseInt("SECONDBOX_OBJECT_STORE_PUBLISHED_PORT"), ConsolePublishedPort: parseInt("SECONDBOX_OBJECT_STORE_CONSOLE_PUBLISHED_PORT")},
		RunnerTrust:  RunnerTrust{EnrollmentCredentialFile: runnerCredential, CACertificateFile: filepath.Join(values["SECONDBOX_RUNNER_PKI_HOST_DIR"], "runner-ca.crt"), CAPrivateKeyFile: values["SECONDBOX_RUNNER_CA_PRIVATE_KEY"], ServerCertificateFile: filepath.Join(values["SECONDBOX_RUNNER_PKI_HOST_DIR"], "server.crt"), ServerPrivateKeyFile: filepath.Join(values["SECONDBOX_RUNNER_PKI_HOST_DIR"], "server.key"), ServerName: values["SECONDBOX_RUNNER_SERVER_NAME"], CertificateLifetimeDays: parseInt("SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS")},
		Applications: Applications{PlatformTokenFile: platformToken, ApplicationAuthoritiesFile: authorities},
		Policy:       Policy{DataPlaneRetentionSeconds: parseInt("SECONDBOX_DATA_PLANE_RETENTION_SECONDS"), DataPlanePollIntervalMilliseconds: parseInt("SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS"), RunnerCommandPollIntervalMilliseconds: parseInt("SECONDBOX_RUNNER_COMMAND_POLL_INTERVAL_MILLISECONDS"), RunnerEnabledFeatures: values["SECONDBOX_RUNNER_ENABLED_FEATURES"], DefaultSubjectMaxSandboxes: parseInt("SECONDBOX_DEFAULT_SUBJECT_MAX_SANDBOXES"), DefaultSubjectMaxActiveInstances: parseInt("SECONDBOX_DEFAULT_SUBJECT_MAX_ACTIVE_INSTANCES"), DefaultSubjectMaxCPUMillis: parseInt("SECONDBOX_DEFAULT_SUBJECT_MAX_CPU_MILLIS"), DefaultSubjectMaxMemoryBytes: parseInt("SECONDBOX_DEFAULT_SUBJECT_MAX_MEMORY_BYTES"), DefaultSubjectMaxArtifactBytes: parseInt("SECONDBOX_DEFAULT_SUBJECT_MAX_ARTIFACT_BYTES"), DefaultSubjectMaxSnapshots: parseInt("SECONDBOX_DEFAULT_SUBJECT_MAX_SNAPSHOTS"), DefaultSubjectMaxArtifacts: parseInt("SECONDBOX_DEFAULT_SUBJECT_MAX_ARTIFACTS"), DefaultSubjectMaxPortSessions: parseInt("SECONDBOX_DEFAULT_SUBJECT_MAX_PORT_SESSIONS"), DefaultSubjectMaxConcurrentOperations: parseInt("SECONDBOX_DEFAULT_SUBJECT_MAX_CONCURRENT_OPERATIONS"), AgentCompartmentPool: values["SECONDBOX_BUILTIN_AGENT_COMPARTMENT_POOL"], AgentCompartmentRuntimeBundleDigest: values["SECONDBOX_BUILTIN_AGENT_COMPARTMENT_RUNTIME_BUNDLE_DIGEST"], AgentCompartmentToolchainBundleDigest: values["SECONDBOX_BUILTIN_AGENT_COMPARTMENT_TOOLCHAIN_BUNDLE_DIGEST"], CodingEnvironmentPool: values["SECONDBOX_BUILTIN_CODING_ENVIRONMENT_POOL"], CodingEnvironmentRuntimeBundleDigest: values["SECONDBOX_BUILTIN_CODING_ENVIRONMENT_RUNTIME_BUNDLE_DIGEST"], CodingEnvironmentToolchainBundleDigest: values["SECONDBOX_BUILTIN_CODING_ENVIRONMENT_TOOLCHAIN_BUNDLE_DIGEST"]},
		Overrides:    TuningOverrides{HTTPTimeoutSeconds: parseInt("SECONDBOX_HTTP_TIMEOUT_SECONDS"), RunnerHeartbeatIntervalMilliseconds: parseInt("SECONDBOX_RUNNER_HEARTBEAT_INTERVAL_MILLISECONDS"), RunnerHeartbeatTimeoutMilliseconds: parseInt("SECONDBOX_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS"), RunnerCommandDeliveryBatchSize: parseInt("SECONDBOX_RUNNER_COMMAND_DELIVERY_BATCH_SIZE"), RunnerEventPersistenceBatchSize: parseInt("SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_SIZE"), RunnerEventPersistenceBatchWaitMilliseconds: parseInt("SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_WAIT_MILLISECONDS"), DataPlaneClaimDurationMilliseconds: parseInt("SECONDBOX_DATA_PLANE_CLAIM_DURATION_MILLISECONDS"), DataPlaneMaximumFrameBytes: parseInt("SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES"), DataPlaneMaximumSessionBytes: parseInt("SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES"), LifecycleReconcileBatchSize: parseInt("SECONDBOX_LIFECYCLE_RECONCILE_BATCH_SIZE"), LifecycleReconcilePollIntervalMilliseconds: parseInt("SECONDBOX_LIFECYCLE_RECONCILE_POLL_INTERVAL_MILLISECONDS"), LifecycleReconcileClaimDurationMilliseconds: parseInt("SECONDBOX_LIFECYCLE_RECONCILE_CLAIM_DURATION_MILLISECONDS"), AssignmentClaimDurationMilliseconds: parseInt("SECONDBOX_ASSIGNMENT_CLAIM_DURATION_MILLISECONDS"), AssignmentDeadlineMilliseconds: parseInt("SECONDBOX_ASSIGNMENT_DEADLINE_MILLISECONDS"), AssignmentRetryLimit: parseInt("SECONDBOX_ASSIGNMENT_RETRY_LIMIT"), SchedulerSerializationRetryLimit: parseInt("SECONDBOX_SCHEDULER_SERIALIZATION_RETRY_LIMIT"), ObjectStoreRetryMaxAttempts: parseInt("SECONDBOX_OBJECT_STORE_RETRY_MAX_ATTEMPTS"), ObjectStoreHTTPTimeoutMilliseconds: parseInt("SECONDBOX_OBJECT_STORE_HTTP_TIMEOUT_MILLISECONDS"), ObjectStoreMaxObjectBytes: parseInt("SECONDBOX_OBJECT_STORE_MAX_OBJECT_BYTES")},
	}
	if databaseMode == "bundled" {
		manifest.Database.URLFile = ""
	} else {
		manifest.Database.BindIP = ""
		manifest.Database.PublishedPort = nil
		manifest.Database.Name = ""
		manifest.Database.User = ""
		manifest.Database.PasswordFile = ""
	}
	if objectMode == "external" {
		manifest.ObjectStore.BindIP = ""
		manifest.ObjectStore.PublishedPort = nil
		manifest.ObjectStore.ConsolePublishedPort = nil
	}
	if values["SECONDBOX_SAME_HOST_RUNNER_ENABLED"] == "true" {
		manifest.Runners = []Runner{legacyRunner(values, parseInt, parseBool)}
	}
	encoded, err := encodeManifest(manifest)
	if err != nil {
		return "", err
	}
	manifestPath := filepath.Join(absoluteTarget, "secondbox.toml")
	if err := writeAtomic(manifestPath, encoded, 0o600, false); err != nil {
		return "", err
	}
	if _, err := Resolve(manifestPath); err != nil {
		return "", err
	}
	created = false
	return manifestPath, nil
}

func legacyRunner(values map[string]string, parseInt func(string) *int64, parseBool func(string) *bool) Runner {
	return Runner{RunnerID: values["SECONDBOX_RUNNER_ID"], Placement: "same-host", PoolID: values["SECONDBOX_RUNNER_POOL_ID"], SoftwareVersion: values["SECONDBOX_RUNNER_SOFTWARE_VERSION"], ControlPlaneAddress: values["SECONDBOX_RUNNER_CONTROL_PLANE_ADDRESS"], ControlPlaneServerName: values["SECONDBOX_RUNNER_CONTROL_PLANE_SERVER_NAME"], IdentityDirectory: "/run/secondbox-runner-identity", IdentityHostDirectory: values["SECONDBOX_RUNNER_IDENTITY_HOST_DIR"], ArtifactHostDirectory: values["SECONDBOX_RUNNER_ARTIFACT_HOST_DIR"], StateHostDirectory: values["SECONDBOX_RUNNER_STATE_HOST_DIR"], WorkspaceHostDirectory: values["SECONDBOX_RUNNER_WORKSPACE_HOST_DIR"], LogPath: values["SECONDBOX_RUNNER_LOG_PATH"], LogDirectory: values["SECONDBOX_RUNNER_LOG_DIR"], FirecrackerPath: values["SECONDBOX_RUNNER_FIRECRACKER_PATH"], FirecrackerJailerPath: values["SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH"], FirecrackerJailRoot: values["SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"], FirecrackerJailerUID: parseInt("SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID"), FirecrackerJailerGID: parseInt("SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID"), FirecrackerCgroupVersion: parseInt("SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION"), FirecrackerCgroupParent: values["SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT"], FirecrackerKernelPath: values["SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"], FirecrackerRootFSPath: values["SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"], FirecrackerSharedImagePath: values["SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"], FirecrackerKernelArgs: values["SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"], FirecrackerCPUTemplate: values["SECONDBOX_RUNNER_FIRECRACKER_CPU_TEMPLATE"], FirecrackerRunDirectory: values["SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR"], FirecrackerLogDirectory: values["SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR"], FirecrackerAllowUnjailed: parseBool("SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED"), ArtifactPublicKey: values["SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"], ArtifactPublicKeySHA256: values["SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"], WorkspaceRoot: values["SECONDBOX_RUNNER_WORKSPACE_ROOT"], StorageRecoveryPercent: parseInt("SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT"), StorageWarningPercent: parseInt("SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT"), StorageAdmissionDenyPercent: parseInt("SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT"), SandboxMaxVCPUs: parseInt("SECONDBOX_RUNNER_SANDBOX_MAX_VCPUS"), SandboxMaxMemoryMiB: parseInt("SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB"), SandboxMaxDiskMiB: parseInt("SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB"), SandboxMemoryBudgetMiB: parseInt("SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB"), SandboxGuestIP: values["SECONDBOX_RUNNER_SANDBOX_GUEST_IP"], SandboxBridgeName: values["SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME"], SandboxBridgeCIDR: values["SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR"], SandboxGuestCIDR: values["SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR"], SandboxTapPrefix: values["SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX"], SandboxNetworkStateDir: values["SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR"], SandboxDeleteBridge: parseBool("SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE"), NetworkPolicyNFTPath: values["SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH"], NetworkPolicyMaxDNSPins: parseInt("SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS"), NetworkPolicyMaxDNSTTL: values["SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL"], NetworkPolicyRunnerAddresses: values["SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES"], NetworkPolicyManagementCIDRs: values["SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS"], NetworkPolicyRunnerGateways: values["SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_GATEWAYS"], NetworkPolicyDNSUpstream: values["SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM"], MaxConcurrentPerSandbox: parseInt("SECONDBOX_RUNNER_MAX_CONCURRENT_PER_SANDBOX"), MaxConcurrentGlobal: parseInt("SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL"), MaxConcurrentStarts: parseInt("SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS"), MaxConcurrentWorkspaceCreates: parseInt("SECONDBOX_RUNNER_MAX_CONCURRENT_WORKSPACE_CREATES"), MaxConcurrentOperationsGlobal: parseInt("SECONDBOX_RUNNER_MAX_CONCURRENT_OPERATIONS_GLOBAL"), FileTransferMaxBytes: parseInt("SECONDBOX_RUNNER_FILE_TRANSFER_MAX_BYTES"), GuestControlVSockPort: parseInt("SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT"), GuestProtocolVSockPort: parseInt("SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT"), GuestHeartbeatInterval: values["SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL"], DataPlaneListenAddress: values["SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS"], DataPlaneAdvertisedAddress: values["SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS"]}
}

func legacyEnvironmentNames() map[string]bool {
	names := []string{
		"SECONDBOX_DEPLOYMENT_MODE",
		"SECONDBOX_PUBLIC_BASE_URL",
		"SECONDBOX_TLS_TERMINATION",
		"SECONDBOX_CONTROL_PLANE_IMAGE",
		"SECONDBOX_RUNNER_IMAGE",
		"SECONDBOX_POSTGRES_IMAGE",
		"SECONDBOX_OBJECT_STORE_IMAGE",
		"SECONDBOX_OBJECT_STORE_CLIENT_IMAGE",
		"SECONDBOX_API_BIND_IP",
		"SECONDBOX_API_PUBLISHED_PORT",
		"SECONDBOX_LISTEN_ADDR",
		"SECONDBOX_LOG_PATH",
		"SECONDBOX_HTTP_TIMEOUT_SECONDS",
		"SECONDBOX_RUNNER_BIND_IP",
		"SECONDBOX_RUNNER_PUBLISHED_PORT",
		"SECONDBOX_RUNNER_LISTEN_ADDR",
		"SECONDBOX_RUNNER_SERVER_NAME",
		"SECONDBOX_RUNNER_PKI_HOST_DIR",
		"SECONDBOX_RUNNER_SERVER_CERTIFICATE",
		"SECONDBOX_RUNNER_SERVER_PRIVATE_KEY",
		"SECONDBOX_RUNNER_CA_CERTIFICATE",
		"SECONDBOX_RUNNER_CA_PRIVATE_KEY",
		"SECONDBOX_RUNNER_CREDENTIAL",
		"SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS",
		"SECONDBOX_RUNNER_HEARTBEAT_INTERVAL_MILLISECONDS",
		"SECONDBOX_RUNNER_COMMAND_POLL_INTERVAL_MILLISECONDS",
		"SECONDBOX_RUNNER_COMMAND_DELIVERY_BATCH_SIZE",
		"SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_SIZE",
		"SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_WAIT_MILLISECONDS",
		"SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS",
		"SECONDBOX_DATA_PLANE_CLAIM_DURATION_MILLISECONDS",
		"SECONDBOX_DATA_PLANE_RETENTION_SECONDS",
		"SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES",
		"SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES",
		"SECONDBOX_LIFECYCLE_RECONCILE_POLL_INTERVAL_MILLISECONDS",
		"SECONDBOX_LIFECYCLE_RECONCILE_CLAIM_DURATION_MILLISECONDS",
		"SECONDBOX_LIFECYCLE_RECONCILE_BATCH_SIZE",
		"SECONDBOX_ASSIGNMENT_CLAIM_DURATION_MILLISECONDS",
		"SECONDBOX_ASSIGNMENT_DEADLINE_MILLISECONDS",
		"SECONDBOX_ASSIGNMENT_RETRY_LIMIT",
		"SECONDBOX_SCHEDULER_SERIALIZATION_RETRY_LIMIT",
		"SECONDBOX_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS",
		"SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH",
		"SECONDBOX_SIGNED_ASSET_CATALOG_PATH",
		"SECONDBOX_RUNNER_PROTOCOL_MINIMUM",
		"SECONDBOX_RUNNER_PROTOCOL_MAXIMUM",
		"SECONDBOX_RUNNER_ENABLED_FEATURES",
		"SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT",
		"SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT",
		"SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL",
		"SECONDBOX_BUILTIN_AGENT_COMPARTMENT_POOL",
		"SECONDBOX_BUILTIN_AGENT_COMPARTMENT_RUNTIME_BUNDLE_DIGEST",
		"SECONDBOX_BUILTIN_AGENT_COMPARTMENT_TOOLCHAIN_BUNDLE_DIGEST",
		"SECONDBOX_BUILTIN_CODING_ENVIRONMENT_POOL",
		"SECONDBOX_BUILTIN_CODING_ENVIRONMENT_RUNTIME_BUNDLE_DIGEST",
		"SECONDBOX_BUILTIN_CODING_ENVIRONMENT_TOOLCHAIN_BUNDLE_DIGEST",
		"SECONDBOX_SAME_HOST_RUNNER_ENABLED",
		"SECONDBOX_RUNNER_ID",
		"SECONDBOX_RUNNER_POOL_ID",
		"SECONDBOX_RUNNER_SOFTWARE_VERSION",
		"SECONDBOX_RUNNER_CONTROL_PLANE_ADDRESS",
		"SECONDBOX_RUNNER_CONTROL_PLANE_SERVER_NAME",
		"SECONDBOX_RUNNER_IDENTITY_HOST_DIR",
		"SECONDBOX_RUNNER_ARTIFACT_HOST_DIR",
		"SECONDBOX_RUNNER_STATE_HOST_DIR",
		"SECONDBOX_RUNNER_WORKSPACE_HOST_DIR",
		"SECONDBOX_RUNNER_LOG_PATH",
		"SECONDBOX_RUNNER_LOG_DIR",
		"SECONDBOX_RUNNER_FIRECRACKER_PATH",
		"SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH",
		"SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT",
		"SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID",
		"SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID",
		"SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION",
		"SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT",
		"SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH",
		"SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH",
		"SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH",
		"SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS",
		"SECONDBOX_RUNNER_FIRECRACKER_CPU_TEMPLATE",
		"SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR",
		"SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR",
		"SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED",
		"SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY",
		"SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256",
		"SECONDBOX_RUNNER_WORKSPACE_ROOT",
		"SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT",
		"SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT",
		"SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT",
		"SECONDBOX_RUNNER_SANDBOX_MAX_VCPUS",
		"SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB",
		"SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB",
		"SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB",
		"SECONDBOX_RUNNER_SANDBOX_GUEST_IP",
		"SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME",
		"SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR",
		"SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR",
		"SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX",
		"SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR",
		"SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE",
		"SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL",
		"SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS",
		"SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_GATEWAYS",
		"SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM",
		"SECONDBOX_RUNNER_MAX_CONCURRENT_PER_SANDBOX",
		"SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL",
		"SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS",
		"SECONDBOX_RUNNER_MAX_CONCURRENT_WORKSPACE_CREATES",
		"SECONDBOX_RUNNER_MAX_CONCURRENT_OPERATIONS_GLOBAL",
		"SECONDBOX_RUNNER_FILE_TRANSFER_MAX_BYTES",
		"SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS",
		"SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS",
		"SECONDBOX_POSTGRES_BIND_IP",
		"SECONDBOX_POSTGRES_PUBLISHED_PORT",
		"SECONDBOX_POSTGRES_DATABASE",
		"SECONDBOX_POSTGRES_USER",
		"SECONDBOX_POSTGRES_PASSWORD",
		"SECONDBOX_DATABASE_URL",
		"SECONDBOX_OBJECT_STORE_BIND_IP",
		"SECONDBOX_OBJECT_STORE_PUBLISHED_PORT",
		"SECONDBOX_OBJECT_STORE_CONSOLE_PUBLISHED_PORT",
		"SECONDBOX_OBJECT_STORE_ENDPOINT",
		"SECONDBOX_OBJECT_STORE_BUCKET",
		"SECONDBOX_OBJECT_STORE_REGION",
		"SECONDBOX_OBJECT_STORE_ROOT_USER",
		"SECONDBOX_OBJECT_STORE_ROOT_PASSWORD",
		"SECONDBOX_OBJECT_STORE_USE_PATH_STYLE",
		"SECONDBOX_OBJECT_STORE_RETRY_MAX_ATTEMPTS",
		"SECONDBOX_OBJECT_STORE_HTTP_TIMEOUT_MILLISECONDS",
		"SECONDBOX_DEVELOPMENT_PREPARE_WAIT_TIMEOUT_SECONDS",
		"SECONDBOX_OBJECT_STORE_TEMP_DIRECTORY",
		"SECONDBOX_OBJECT_STORE_MAX_OBJECT_BYTES",
		"SECONDBOX_PLATFORM_TOKEN",
		"SECONDBOX_APPLICATION_AUTHORITIES_JSON",
		"SECONDBOX_DEFAULT_SUBJECT_MAX_SANDBOXES",
		"SECONDBOX_DEFAULT_SUBJECT_MAX_ACTIVE_INSTANCES",
		"SECONDBOX_DEFAULT_SUBJECT_MAX_CPU_MILLIS",
		"SECONDBOX_DEFAULT_SUBJECT_MAX_MEMORY_BYTES",
		"SECONDBOX_DEFAULT_SUBJECT_MAX_ARTIFACT_BYTES",
		"SECONDBOX_DEFAULT_SUBJECT_MAX_SNAPSHOTS",
		"SECONDBOX_DEFAULT_SUBJECT_MAX_ARTIFACTS",
		"SECONDBOX_DEFAULT_SUBJECT_MAX_PORT_SESSIONS",
		"SECONDBOX_DEFAULT_SUBJECT_MAX_CONCURRENT_OPERATIONS",
	}
	known := make(map[string]bool, len(names))
	for _, name := range names {
		known[name] = true
	}
	return known
}
