package firecracker

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
)

// LoadRunnerFirecrackerConfigFromEnv loads explicit standalone runner settings.
func LoadRunnerFirecrackerConfigFromEnv() (*config.Config, error) {
	required := func(name string) (string, error) {
		value, present := os.LookupEnv(name)
		value = strings.TrimSpace(value)
		if !present || value == "" {
			return "", fmt.Errorf("SecondBox Firecracker config missing required %s", name)
		}
		return value, nil
	}
	requiredInt := func(name string) (int, error) {
		raw, err := required(name)
		if err != nil {
			return 0, err
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return 0, fmt.Errorf("SecondBox Firecracker config requires positive integer %s", name)
		}
		return value, nil
	}
	requiredBool := func(name string) (bool, error) {
		raw, err := required(name)
		if err != nil {
			return false, err
		}
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return false, fmt.Errorf("SecondBox Firecracker config requires boolean %s", name)
		}
		return value, nil
	}
	firecrackerPath, err := required("SECONDBOX_RUNNER_FIRECRACKER_PATH")
	if err != nil {
		return nil, err
	}
	jailerPath, err := required("SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH")
	if err != nil {
		return nil, err
	}
	jailRoot, err := required("SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT")
	if err != nil {
		return nil, err
	}
	jailerUIDStart, err := requiredInt("SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_START")
	if err != nil {
		return nil, err
	}
	jailerUIDCount, err := requiredInt("SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_COUNT")
	if err != nil {
		return nil, err
	}
	jailerUIDAllowBelow1000, err := requiredBool("SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_ALLOW_BELOW_1000")
	if err != nil {
		return nil, err
	}
	jailerGID, err := requiredInt("SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID")
	if err != nil {
		return nil, err
	}
	cgroupVersion, err := requiredInt("SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION")
	if err != nil {
		return nil, err
	}
	cgroupParent, err := required("SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT")
	if err != nil {
		return nil, err
	}
	kernelPath, err := required("SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH")
	if err != nil {
		return nil, err
	}
	rootfsPath, err := required("SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH")
	if err != nil {
		return nil, err
	}
	sharedImagePath, err := required("SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH")
	if err != nil {
		return nil, err
	}
	publicKeyPath, err := required("SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY")
	if err != nil {
		return nil, err
	}
	publicKeySHA256, err := required("SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256")
	if err != nil {
		return nil, err
	}
	runnerWorkspaceRoot, err := required("SECONDBOX_RUNNER_WORKSPACE_ROOT")
	if err != nil {
		return nil, err
	}
	runDir, err := required("SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR")
	if err != nil {
		return nil, err
	}
	logDir, err := required("SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR")
	if err != nil {
		return nil, err
	}
	kernelArgs, err := required("SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS")
	if err != nil {
		return nil, err
	}
	guestControlPort, err := requiredInt("SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT")
	if err != nil {
		return nil, err
	}
	guestProtocolPort, err := requiredInt("SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT")
	if err != nil {
		return nil, err
	}
	if guestControlPort > 65535 || guestProtocolPort > 65535 {
		return nil, fmt.Errorf("SecondBox guest vsock ports must be at most 65535")
	}
	if guestControlPort == guestProtocolPort {
		return nil, fmt.Errorf("SecondBox guest control and protocol vsock ports must be distinct")
	}
	guestHeartbeatRaw, err := required("SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL")
	if err != nil {
		return nil, err
	}
	guestHeartbeatInterval, err := time.ParseDuration(guestHeartbeatRaw)
	if err != nil || guestHeartbeatInterval <= 0 {
		return nil, fmt.Errorf("SecondBox Firecracker config requires positive duration SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL")
	}
	memoryMiB, err := requiredInt("SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB")
	if err != nil {
		return nil, err
	}
	vcpus, err := requiredInt("SECONDBOX_RUNNER_SANDBOX_MAX_VCPUS")
	if err != nil {
		return nil, err
	}
	cpuTemplate, err := required("SECONDBOX_RUNNER_FIRECRACKER_CPU_TEMPLATE")
	if err != nil {
		return nil, err
	}
	workspaceSizeMiB, err := requiredInt("SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB")
	if err != nil {
		return nil, err
	}
	storagePressureRecoveryPercent, err := requiredInt("SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT")
	if err != nil {
		return nil, err
	}
	storagePressureWarningPercent, err := requiredInt("SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT")
	if err != nil {
		return nil, err
	}
	storagePressureAdmissionDenyPercent, err := requiredInt("SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT")
	if err != nil {
		return nil, err
	}
	if err := (storagePressurePolicy{
		RecoveryPercent:      storagePressureRecoveryPercent,
		WarningPercent:       storagePressureWarningPercent,
		AdmissionDenyPercent: storagePressureAdmissionDenyPercent,
	}).Validate(); err != nil {
		return nil, fmt.Errorf("SecondBox Firecracker config: %w", err)
	}
	guestIP, err := required("SECONDBOX_RUNNER_SANDBOX_GUEST_IP")
	if err != nil {
		return nil, err
	}
	bridgeName, err := required("SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME")
	if err != nil {
		return nil, err
	}
	bridgeCIDR, err := required("SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR")
	if err != nil {
		return nil, err
	}
	tapPrefix, err := required("SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX")
	if err != nil {
		return nil, err
	}
	maxPerSandbox, err := requiredInt("SECONDBOX_RUNNER_MAX_CONCURRENT_PER_SANDBOX")
	if err != nil {
		return nil, err
	}
	maxGlobal, err := requiredInt("SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL")
	if err != nil {
		return nil, err
	}
	if jailerUIDCount < maxGlobal {
		return nil, fmt.Errorf("SecondBox Firecracker config requires SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_COUNT to be at least SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL")
	}
	if jailerUIDStart < 1000 && !jailerUIDAllowBelow1000 {
		return nil, fmt.Errorf("SecondBox Firecracker config requires SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_ALLOW_BELOW_1000=true when SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_START is below 1000")
	}
	if uint64(jailerUIDStart)+uint64(jailerUIDCount)-1 > uint64(^uint32(0)) {
		return nil, fmt.Errorf("SecondBox Firecracker config jailer UID range must fit within unsigned 32-bit user IDs")
	}
	maxOperationsGlobal, err := requiredInt(
		"SECONDBOX_RUNNER_MAX_CONCURRENT_OPERATIONS_GLOBAL",
	)
	if err != nil {
		return nil, err
	}
	memoryBudgetMiB, err := requiredInt("SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB")
	if err != nil {
		return nil, err
	}
	fileTransferMaxBytes, err := requiredInt("SECONDBOX_RUNNER_FILE_TRANSFER_MAX_BYTES")
	if err != nil {
		return nil, err
	}
	allowUnjailedRaw, err := required("SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED")
	if err != nil {
		return nil, err
	}
	allowUnjailed, err := strconv.ParseBool(allowUnjailedRaw)
	if err != nil {
		return nil, fmt.Errorf("SecondBox Firecracker config requires boolean SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED")
	}
	snapshotTemplateCacheRoot, err := required("SECONDBOX_RUNNER_SNAPSHOT_TEMPLATE_CACHE_ROOT")
	if err != nil {
		return nil, err
	}
	networkPolicyNFTPath, err := required("SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH")
	if err != nil {
		return nil, err
	}
	networkPolicyConfig, err := networkpolicy.LoadRunnerConfigFromEnvironment()
	if err != nil {
		return nil, fmt.Errorf("SecondBox Firecracker config: %w", err)
	}

	return &config.Config{
		FirecrackerPath:                            firecrackerPath,
		JailerPath:                                 jailerPath,
		MicroVMJailerChrootBaseDir:                 jailRoot,
		MicroVMJailerUIDStart:                      jailerUIDStart,
		MicroVMJailerUIDCount:                      jailerUIDCount,
		MicroVMJailerUIDAllowBelow1000:             jailerUIDAllowBelow1000,
		MicroVMJailerGID:                           jailerGID,
		MicroVMJailerCgroupVersion:                 cgroupVersion,
		MicroVMJailerParentCgroup:                  cgroupParent,
		MicroVMKernelPath:                          kernelPath,
		MicroVMRootfsPath:                          rootfsPath,
		MicroVMToolRootfsPath:                      rootfsPath,
		MicroVMSharedImagePath:                     sharedImagePath,
		MicroVMToolSharedImagePath:                 sharedImagePath,
		MicroVMPublicKeyPath:                       publicKeyPath,
		MicroVMPublicKeySHA256:                     publicKeySHA256,
		RunnerWorkspaceRoot:                        runnerWorkspaceRoot,
		MicroVMRunDir:                              runDir,
		MicroVMLogDir:                              logDir,
		MicroVMKernelArgs:                          kernelArgs,
		MicroVMGuestControlVsockPort:               uint32(guestControlPort),
		MicroVMGuestProtocolVsockPort:              uint32(guestProtocolPort),
		MicroVMGuestHeartbeatInterval:              guestHeartbeatInterval,
		MicroVMMemoryMiB:                           memoryMiB,
		MicroVMVCPUs:                               vcpus,
		MicroVMCPUTemplate:                         cpuTemplate,
		MicroVMWorkspaceSizeMiB:                    workspaceSizeMiB,
		MicroVMStoragePressureRecoveryPercent:      storagePressureRecoveryPercent,
		MicroVMStoragePressureWarningPercent:       storagePressureWarningPercent,
		MicroVMStoragePressureAdmissionDenyPercent: storagePressureAdmissionDenyPercent,
		MicroVMAllowUnjailed:                       allowUnjailed,
		MicroVMSnapshotTemplateCacheRoot:           snapshotTemplateCacheRoot,
		MicroVMGuestIP:                             guestIP,
		MicroVMBridgeName:                          bridgeName,
		MicroVMBridgeCIDR:                          bridgeCIDR,
		MicroVMTapPrefix:                           tapPrefix,
		MicroVMMaxConcurrentPerSandbox:             maxPerSandbox,
		MicroVMMaxConcurrentGlobal:                 maxGlobal,
		MicroVMMaxConcurrentOperationsGlobal:       maxOperationsGlobal,
		MicroVMMemoryBudgetMiB:                     memoryBudgetMiB,
		MicroVMToolVMReuseEnabled:                  false,
		MicroVMToolVMIdleTTL:                       time.Duration(0),
		FileTransferMaxBytes:                       int64(fileTransferMaxBytes),
		NetworkPolicyNFTPath:                       networkPolicyNFTPath,
		NetworkPolicyMaximumDNSPins:                networkPolicyConfig.CompileOptions.MaximumPins,
		NetworkPolicyMaximumDNSTTL:                 networkPolicyConfig.CompileOptions.MaximumTTL,
		NetworkPolicyRunnerAddresses:               networkPolicyConfig.CompileOptions.RunnerAddresses,
		NetworkPolicyManagementCIDRs:               networkPolicyConfig.CompileOptions.ManagementPrefixes,
		NetworkPolicyEgressContexts:                networkPolicyConfig.EgressContexts,
		NetworkPolicyDNSUpstream:                   networkPolicyConfig.DNSUpstream,
	}, nil
}
