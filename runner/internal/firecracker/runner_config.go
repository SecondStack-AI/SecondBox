package firecracker

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
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
	requiredAddresses := func(name string) ([]netip.Addr, error) {
		raw, err := required(name)
		if err != nil {
			return nil, err
		}
		parts := strings.Split(raw, ",")
		addresses := make([]netip.Addr, 0, len(parts))
		for _, part := range parts {
			address, parseErr := netip.ParseAddr(strings.TrimSpace(part))
			if parseErr != nil {
				return nil, fmt.Errorf("SecondBox Firecracker config %s contains invalid address %q", name, part)
			}
			addresses = append(addresses, address)
		}
		return addresses, nil
	}
	requiredPrefixes := func(name string) ([]netip.Prefix, error) {
		raw, err := required(name)
		if err != nil {
			return nil, err
		}
		parts := strings.Split(raw, ",")
		prefixes := make([]netip.Prefix, 0, len(parts))
		for _, part := range parts {
			prefix, parseErr := netip.ParsePrefix(strings.TrimSpace(part))
			if parseErr != nil {
				return nil, fmt.Errorf("SecondBox Firecracker config %s contains invalid CIDR %q", name, part)
			}
			prefixes = append(prefixes, prefix.Masked())
		}
		return prefixes, nil
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
	jailerUID, err := requiredInt("SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID")
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
	networkPolicyNFTPath, err := required("SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH")
	if err != nil {
		return nil, err
	}
	networkPolicyMaximumDNSPins, err := requiredInt("SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS")
	if err != nil {
		return nil, err
	}
	networkPolicyMaximumDNSTTLRaw, err := required("SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL")
	if err != nil {
		return nil, err
	}
	networkPolicyMaximumDNSTTL, err := time.ParseDuration(networkPolicyMaximumDNSTTLRaw)
	if err != nil || networkPolicyMaximumDNSTTL <= 0 {
		return nil, fmt.Errorf("SecondBox Firecracker config requires positive duration SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL")
	}
	networkPolicyRunnerAddresses, err := requiredAddresses("SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES")
	if err != nil {
		return nil, err
	}
	networkPolicyManagementCIDRs, err := requiredPrefixes("SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS")
	if err != nil {
		return nil, err
	}
	networkPolicyDNSUpstreamRaw, err := required("SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM")
	if err != nil {
		return nil, err
	}
	networkPolicyDNSUpstream, err := netip.ParseAddrPort(networkPolicyDNSUpstreamRaw)
	if err != nil || networkPolicyDNSUpstream.Port() == 0 {
		return nil, fmt.Errorf("SecondBox Firecracker config SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM must be an IP:port")
	}

	return &config.Config{
		FirecrackerPath:                            firecrackerPath,
		JailerPath:                                 jailerPath,
		MicroVMJailerChrootBaseDir:                 jailRoot,
		MicroVMJailerUID:                           jailerUID,
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
		MicroVMGuestIP:                             guestIP,
		MicroVMBridgeName:                          bridgeName,
		MicroVMBridgeCIDR:                          bridgeCIDR,
		MicroVMTapPrefix:                           tapPrefix,
		MicroVMMaxConcurrentPerSandbox:             maxPerSandbox,
		MicroVMMaxConcurrentGlobal:                 maxGlobal,
		MicroVMMemoryBudgetMiB:                     memoryBudgetMiB,
		MicroVMToolVMReuseEnabled:                  false,
		MicroVMToolVMIdleTTL:                       time.Duration(0),
		FileTransferMaxBytes:                       int64(fileTransferMaxBytes),
		NetworkPolicyNFTPath:                       networkPolicyNFTPath,
		NetworkPolicyMaximumDNSPins:                networkPolicyMaximumDNSPins,
		NetworkPolicyMaximumDNSTTL:                 networkPolicyMaximumDNSTTL,
		NetworkPolicyRunnerAddresses:               networkPolicyRunnerAddresses,
		NetworkPolicyManagementCIDRs:               networkPolicyManagementCIDRs,
		NetworkPolicyDNSUpstream:                   networkPolicyDNSUpstream,
	}, nil
}
