package install

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
)

const (
	ExecutionBundleEstimateBytes      = int64(11 << 30)
	MinimumWorkspaceBytes             = standardresources.DurableCodingWorkspaceBytes
	MinimumBackingReserveBytes        = int64(16 << 30)
	MinimumControlBackingBytes        = MinimumBackingReserveBytes
	MinimumDeploymentBytes            = ExecutionBundleEstimateBytes
	RunnerStorageReserveBytes         = int64(4 << 30)
	MinimumRunnerStorageBytes         = ExecutionBundleEstimateBytes + MinimumWorkspaceBytes + RunnerStorageReserveBytes
	MinimumFilesystemImageBytes       = MinimumRunnerStorageBytes
	MinimumHostMemoryBytes            = int64(12 << 30)
	HostMemoryReserveBytes            = int64(4 << 30)
	MinimumHostCPUCount               = 6
	HostVCPUReserveCount              = int64(2)
	DurableCodingVCPUCount            = standardresources.DurableCodingVCPUCount
	DurableCodingMemoryBytes          = standardresources.DurableCodingMemoryBytes
	DurableCodingConcurrentOperations = standardresources.DurableCodingConcurrentOperations
	SingleHostFirecrackerCPUTemplate  = "None"
)

type StorageOption struct {
	Choice         StorageChoice
	Label          string
	Mountpoint     string
	DeviceIdentity string
	Filesystem     string
	AvailableBytes int64
}

type NetworkOverrides struct {
	APIPort       int
	RunnerPort    int
	DataPlanePort int
	DatabasePort  int
	GuestCIDR     string
	ComposeCIDR   string
	TAPPrefix     string
	CgroupParent  string
	DNSUpstream   string
	JailerUID     UIDRange
}

type ProposalInput struct {
	OperationID              string
	CreatedAt                time.Time
	DeploymentDirectory      string
	BinaryDirectory          string
	CLIConfigPath            string
	BackingAvailableBytes    int64
	DeploymentAvailableBytes int64
	Release                  ReleasePlan
	StorageChoice            StorageChoice
	ExistingMountpoint       string
	FilesystemImageBytes     int64
	NetworkOverrides         NetworkOverrides
	StandardBundles          []string
	RetentionSeconds         int64
}

func NewOperationID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", installerError("generate operation identity", err)
	}
	return "install_" + hex.EncodeToString(value[:]), nil
}

func NewUpdateID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", installerError("generate update identity", err)
	}
	return "update_" + hex.EncodeToString(value[:]), nil
}

func StorageOptions(facts HostFacts, backingAvailableBytes, releaseDownloadBytes int64) []StorageOption {
	options := []StorageOption{}
	if releaseDownloadBytes <= 0 || backingAvailableBytes < MinimumControlBackingBytes+releaseDownloadBytes {
		return options
	}
	rootDevice := ""
	for _, device := range facts.Devices {
		if device.Mountpoint == "/" {
			rootDevice = device.Identity
			break
		}
	}
	for _, device := range facts.Devices {
		if device.Mountpoint == "" || device.Mountpoint == "/" || device.Identity == rootDevice || (device.Filesystem != "xfs" && device.Filesystem != "btrfs") || device.AvailableBytes < MinimumRunnerStorageBytes || !device.JailerCompatible {
			continue
		}
		options = append(options, StorageOption{Choice: StorageExistingMount, Label: fmt.Sprintf("%s dedicated %s mount (%s available)", device.Mountpoint, strings.ToUpper(device.Filesystem), formatBytes(device.AvailableBytes)), Mountpoint: device.Mountpoint, DeviceIdentity: device.Identity, Filesystem: device.Filesystem, AvailableBytes: device.AvailableBytes})
	}
	if size := proposedImageBytes(backingAvailableBytes, releaseDownloadBytes); facts.BtrfsSupported && size >= MinimumFilesystemImageBytes {
		options = append(options, StorageOption{Choice: StorageBtrfsImage, Label: fmt.Sprintf("Bounded Btrfs filesystem image (%s fully allocated)", formatBytes(size)), AvailableBytes: size, Filesystem: "btrfs"})
	}
	return options
}

func proposedImageBytes(available, releaseDownloadBytes int64) int64 {
	reserve := max(MinimumBackingReserveBytes, available/5)
	value := available - reserve - releaseDownloadBytes
	if value <= 0 {
		return 0
	}
	const gib = int64(1 << 30)
	return value / gib * gib
}

func ProposePlan(facts HostFacts, input ProposalInput) (InstallPlan, error) {
	if err := facts.Validate(); err != nil {
		return InstallPlan{}, err
	}
	if HasBlockingFindings(facts) {
		return InstallPlan{}, installerError("cannot propose a plan from blocked host facts", nil)
	}
	if input.OperationID == "" {
		return InstallPlan{}, installerError("proposal operation ID is required", nil)
	}
	if input.CreatedAt.IsZero() {
		return InstallPlan{}, installerError("proposal creation time is required", nil)
	}
	if input.DeploymentAvailableBytes < MinimumDeploymentBytes {
		return InstallPlan{}, installerError("deployment filesystem capacity is insufficient for verified release materialization", nil)
	}
	if input.CLIConfigPath == "" {
		return InstallPlan{}, installerError("explicit CLI configuration path is required", nil)
	}
	for name, path := range map[string]string{"deployment": input.DeploymentDirectory, "binary": input.BinaryDirectory, "CLI configuration": input.CLIConfigPath} {
		if err := validateSafePath(path); err != nil {
			return InstallPlan{}, installerError("proposal "+name+" directory", err)
		}
	}
	factsDigest, err := HostFactsDigest(facts)
	if err != nil {
		return InstallPlan{}, err
	}
	storage, workspaceBytes, storagePaths, err := proposeStorage(facts, input)
	if err != nil {
		return InstallPlan{}, err
	}
	capacity, err := proposeCapacity(facts, workspaceBytes)
	if err != nil {
		return InstallPlan{}, err
	}
	network, err := proposeNetwork(facts, input.NetworkOverrides)
	if err != nil {
		return InstallPlan{}, err
	}
	paths, secretTargets := proposePaths(facts, input, storage, storagePaths)
	if !standardBundleSelectionComplete(input.StandardBundles) {
		return InstallPlan{}, installerError("operator selection of all release-owned standard bundles is required", nil)
	}
	if input.RetentionSeconds <= 0 {
		return InstallPlan{}, installerError("operator-selected retention is required", nil)
	}
	plan := InstallPlan{SchemaVersion: PlanSchema, OperationID: input.OperationID, CreatedAt: input.CreatedAt.UTC(), HostFacts: facts, HostFactsDigest: factsDigest, Release: input.Release, Storage: storage, Capacity: capacity, Compute: ComputePlan{FirecrackerCPUTemplate: SingleHostFirecrackerCPUTemplate}, Network: network, CLI: CLIPlan{ConfigPath: input.CLIConfigPath}, Paths: paths, SecretTargets: secretTargets, GeneratedAuthorityCategories: []string{"platform-authority", "runner-enrollment", "runner-pki", "database"}, StandardBundles: slices.Clone(input.StandardBundles), RetentionSeconds: input.RetentionSeconds, PrivilegedActions: privilegedActions(storage), ReleaseHistory: []ReleaseActivation{{Release: input.Release, ActivatedAt: input.CreatedAt.UTC()}}}
	if err := plan.Validate(); err != nil {
		return InstallPlan{}, err
	}
	return plan, nil
}

func standardBundleSelectionComplete(selected []string) bool {
	want := standardresources.BundleNames()
	return len(selected) == len(want) && !slices.ContainsFunc(want, func(name string) bool {
		return !slices.Contains(selected, name)
	})
}

func proposeStorage(facts HostFacts, input ProposalInput) (StoragePlan, int64, []PlannedPath, error) {
	switch input.StorageChoice {
	case StorageExistingMount:
		for _, option := range StorageOptions(facts, input.BackingAvailableBytes, input.Release.ExpectedDownloadBytes) {
			if option.Choice == StorageExistingMount && option.Mountpoint == input.ExistingMountpoint {
				runnerRoot := filepath.Join(option.Mountpoint, "secondbox-"+input.OperationID)
				storageRoot := filepath.Join(runnerRoot, "storage")
				workspace := filepath.Join(storageRoot, "workspaces")
				workspaceBytes := option.AvailableBytes - ExecutionBundleEstimateBytes - RunnerStorageReserveBytes
				return StoragePlan{Choice: StorageExistingMount, WorkspacePath: workspace, ExistingDeviceIdentity: option.DeviceIdentity}, workspaceBytes, []PlannedPath{
					plannedPath("runner-root", runnerRoot, PathExistingWorkspace, ResourceDirectory, 0o711, 0, 0, true, true),
					plannedPath("runner-storage", storageRoot, PathExistingWorkspace, ResourceDirectory, 0o711, 0, 0, true, true),
					plannedPath("workspace", workspace, PathExistingWorkspace, ResourceDirectory, 0o750, runnerContainerUID, runnerContainerGID, true, true),
				}, nil
			}
		}
		return StoragePlan{}, 0, nil, installerError("selected workspace mount is not an observed dedicated XFS/Btrfs candidate", nil)
	case StorageBtrfsImage:
		maximum := proposedImageBytes(input.BackingAvailableBytes, input.Release.ExpectedDownloadBytes)
		size := input.FilesystemImageBytes
		if size == 0 {
			size = maximum
		}
		if !facts.BtrfsSupported || size < MinimumFilesystemImageBytes || size > maximum {
			return StoragePlan{}, 0, nil, installerError("filesystem image capacity is outside the safe fully allocated range", nil)
		}
		runnerRoot := filepath.Join("/var/lib", "secondbox-"+input.OperationID)
		storageRoot := filepath.Join(runnerRoot, "storage")
		image := filepath.Join(runnerRoot, "runner-storage.btrfs")
		workspace := filepath.Join(storageRoot, "workspaces")
		unit := filepath.Join("/etc/systemd/system", systemdMountUnitName(storageRoot))
		workspaceBytes := size - ExecutionBundleEstimateBytes - RunnerStorageReserveBytes
		return StoragePlan{Choice: StorageBtrfsImage, WorkspacePath: workspace, FilesystemImagePath: image, ImageSizeBytes: size, MountUnitPath: unit}, workspaceBytes, []PlannedPath{
			plannedPath("runner-root", runnerRoot, PathInstallerHost, ResourceDirectory, 0o711, 0, 0, true, true),
			plannedPath("runner-storage", storageRoot, PathInstallerHost, ResourceDirectory, 0o711, 0, 0, true, true),
			plannedPath("filesystem-image", image, PathFilesystemImage, ResourceFilesystemImage, 0o600, 0, 0, true, true),
			plannedPath("workspace", workspace, PathInstallerHost, ResourceDirectory, 0o750, runnerContainerUID, runnerContainerGID, true, true),
			plannedPath("workspace-mount-unit", unit, PathInstallerHost, ResourceMountUnit, 0o644, 0, 0, true, true),
		}, nil
	default:
		return StoragePlan{}, 0, nil, installerError("storage choice is unsupported", nil)
	}
}

func proposeCapacity(facts HostFacts, workspaceBytes int64) (CapacityPlan, error) {
	if facts.CPUCount < MinimumHostCPUCount || facts.MemoryBytes < MinimumHostMemoryBytes || workspaceBytes < MinimumWorkspaceBytes {
		return CapacityPlan{}, installerError("host capacity is insufficient for the durable-coding smoke Sandbox and control services", nil)
	}
	vcpuCount := int64(facts.CPUCount) - HostVCPUReserveCount
	memory := facts.MemoryBytes - HostMemoryReserveBytes
	sandboxes := min(vcpuCount/DurableCodingVCPUCount, memory/DurableCodingMemoryBytes, workspaceBytes/MinimumWorkspaceBytes)
	active := min(sandboxes, int64(4))
	runnerOperations := sandboxes * DurableCodingConcurrentOperations
	subjectOperations := active * DurableCodingConcurrentOperations
	quotas := map[string]int64{"maxSandboxes": sandboxes * 4, "maxActiveInstances": active, "maxVcpuCount": vcpuCount, "maxMemoryBytes": memory, "maxSnapshots": sandboxes * 10, "maxPortSessions": sandboxes * 4, "maxConcurrentOperations": subjectOperations}
	return CapacityPlan{MaxSandboxes: sandboxes, MaxVCPUCount: vcpuCount, MaxMemoryBytes: memory, MaxWorkspaceBytes: workspaceBytes, ConcurrentStarts: min(int64(2), active), ConcurrentOperations: runnerOperations, StoragePressurePercent: 85, SubjectQuotas: quotas}, nil
}

func proposeNetwork(facts HostFacts, overrides NetworkOverrides) (NetworkPlan, error) {
	used := map[int]bool{}
	for _, listener := range facts.ListeningPorts {
		used[listener.Port] = true
	}
	port := func(requested, fallback int) (int, error) {
		if requested != 0 {
			if requested < 1024 || requested > 65535 || used[requested] {
				return 0, installerError("advanced port is invalid or occupied", nil)
			}
			used[requested] = true
			return requested, nil
		}
		for candidate := fallback; candidate <= 65535; candidate++ {
			if !used[candidate] {
				used[candidate] = true
				return candidate, nil
			}
		}
		return 0, installerError("no collision-free loopback port is available", nil)
	}
	api, err := port(overrides.APIPort, 8080)
	if err != nil {
		return NetworkPlan{}, err
	}
	runner, err := port(overrides.RunnerPort, 9443)
	if err != nil {
		return NetworkPlan{}, err
	}
	data, err := port(overrides.DataPlanePort, 9444)
	if err != nil {
		return NetworkPlan{}, err
	}
	database, err := port(overrides.DatabasePort, 5432)
	if err != nil {
		return NetworkPlan{}, err
	}
	occupied := observedInstallIPv4Prefixes(facts)
	guestCIDR := overrides.GuestCIDR
	if guestCIDR == "" {
		guestCIDR = freeRFC1918CIDR(occupied)
		if guestCIDR == "" {
			return NetworkPlan{}, installerError("no collision-free RFC1918 guest /24 is available", nil)
		}
	}
	guestPrefix, err := validatedInstallCIDR(guestCIDR, occupied)
	if err != nil {
		return NetworkPlan{}, installerError("guest bridge CIDR is invalid or conflicts with a host route or Docker network", err)
	}
	occupied = append(occupied, guestPrefix)
	composeCIDR := overrides.ComposeCIDR
	if composeCIDR == "" {
		composeCIDR = freeRFC1918CIDR(occupied)
		if composeCIDR == "" {
			return NetworkPlan{}, installerError("no collision-free RFC1918 Compose backend /24 is available", nil)
		}
	}
	if _, err := validatedComposeCIDR(composeCIDR, occupied); err != nil {
		return NetworkPlan{}, installerError("Compose backend CIDR is invalid or conflicts with the guest network, a host route, or a Docker network", err)
	}
	tap := overrides.TAPPrefix
	if tap == "" {
		tap = "sbx"
	}
	cgroup := overrides.CgroupParent
	if cgroup == "" {
		cgroup = "secondbox"
	}
	uidRange := overrides.JailerUID
	if uidRange.Count == 0 && len(facts.CandidateUIDRanges) > 0 {
		uidRange = facts.CandidateUIDRanges[0]
	}
	maximumUID := int64(^uint32(0))
	if uidRange.Start < 10000 || uidRange.Count < 1 || uidRange.Start > maximumUID || uidRange.Count > maximumUID-uidRange.Start+1 || slices.ContainsFunc(facts.AssignedUIDs, func(uid int64) bool { return uid >= uidRange.Start && uid < uidRange.Start+uidRange.Count }) || slices.ContainsFunc(facts.ReservedIDRanges, func(reserved UIDRange) bool { return rangesOverlap(uidRange, reserved) }) {
		return NetworkPlan{}, installerError("jailer UID range is absent or assigned", nil)
	}
	dns := overrides.DNSUpstream
	if dns == "" && len(facts.DNSUpstreams) > 0 {
		dns = facts.DNSUpstreams[0]
	}
	dnsIP := net.ParseIP(dns)
	if dnsIP == nil || dnsIP.IsLoopback() || dnsIP.IsUnspecified() {
		return NetworkPlan{}, installerError("DNS upstream must be an observed or reviewed non-loopback address", nil)
	}
	return NetworkPlan{APIAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(api)), RunnerAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(runner)), DataPlaneAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(data)), DatabaseAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(database)), GuestBridgeCIDR: guestCIDR, ComposeBackendCIDR: composeCIDR, TAPPrefix: tap, CgroupParent: cgroup, JailerUIDRange: uidRange, DNSUpstream: dns, Gateways: map[string]string{"agent-compartment": "agent-gateway.secondbox.internal", "durable-coding": "platform-gateway.secondbox.internal"}}, nil
}

func freeRFC1918CIDR(observed []netip.Prefix) string {
	free := func(first, second, third byte) string {
		candidate := netip.PrefixFrom(netip.AddrFrom4([4]byte{first, second, third, 0}), 24)
		if !routePrefixCollides(candidate, observed) {
			return candidate.String()
		}
		return ""
	}

	// Preserve the historical choices when they are available, then search
	// every RFC1918 /24 deterministically. A host with many Docker networks can
	// legitimately occupy all of 172.30/31 without exhausting private space.
	for _, preferred := range [][3]byte{{172, 30, 0}, {172, 31, 0}} {
		if candidate := free(preferred[0], preferred[1], preferred[2]); candidate != "" {
			return candidate
		}
	}
	for second := 16; second <= 31; second++ {
		for third := 0; third <= 255; third++ {
			if (second == 30 || second == 31) && third == 0 {
				continue
			}
			if candidate := free(172, byte(second), byte(third)); candidate != "" {
				return candidate
			}
		}
	}
	for second := 0; second <= 255; second++ {
		for third := 0; third <= 255; third++ {
			if candidate := free(10, byte(second), byte(third)); candidate != "" {
				return candidate
			}
		}
	}
	for third := 0; third <= 255; third++ {
		if candidate := free(192, 168, byte(third)); candidate != "" {
			return candidate
		}
	}
	return ""
}

func observedInstallIPv4Prefixes(facts HostFacts) []netip.Prefix {
	prefixes := observedIPv4RoutePrefixes(facts.Routes)
	for _, subnet := range facts.DockerNetworkSubnets {
		prefix, err := netip.ParsePrefix(subnet)
		if err == nil && prefix.Addr().Is4() {
			prefixes = append(prefixes, prefix.Masked())
		}
	}
	return prefixes
}

func validatedInstallCIDR(candidate string, occupied []netip.Prefix) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(candidate)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() > 30 || prefix != prefix.Masked() || !isRFC1918Prefix(prefix) {
		return netip.Prefix{}, fmt.Errorf("must be an RFC1918 IPv4 network with usable host addresses")
	}
	if routePrefixCollides(prefix, occupied) {
		return netip.Prefix{}, fmt.Errorf("overlaps an occupied network")
	}
	return prefix, nil
}

func validatedComposeCIDR(candidate string, occupied []netip.Prefix) (netip.Prefix, error) {
	prefix, err := validatedInstallCIDR(candidate, occupied)
	if err != nil {
		return netip.Prefix{}, err
	}
	if prefix.Bits() != 24 {
		return netip.Prefix{}, fmt.Errorf("must be an RFC1918 IPv4 /24")
	}
	return prefix, nil
}

func isRFC1918Prefix(prefix netip.Prefix) bool {
	for _, private := range []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16")} {
		if prefix.Bits() >= private.Bits() && private.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

func rangesOverlap(a, b UIDRange) bool {
	return a.Count > 0 && b.Count > 0 && a.Start < b.Start+b.Count && b.Start < a.Start+a.Count
}

func observedIPv4RoutePrefixes(routes []RouteFact) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(routes))
	for _, route := range routes {
		prefix, err := netip.ParsePrefix(route.Destination)
		if err == nil && prefix.Addr().Is4() && prefix.Bits() > 0 {
			prefixes = append(prefixes, prefix.Masked())
		}
	}
	return prefixes
}

func routePrefixCollides(candidate netip.Prefix, routes []netip.Prefix) bool {
	for _, route := range routes {
		if candidate.Overlaps(route) {
			return true
		}
	}
	return false
}

func proposePaths(facts HostFacts, input ProposalInput, storagePlan StoragePlan, storage []PlannedPath) ([]PlannedPath, []SecretTarget) {
	root := input.DeploymentDirectory
	uid, gid := facts.InvokingUID, facts.InvokingGID
	runnerStorage := filepath.Dir(storagePlan.WorkspacePath)
	runnerState := filepath.Join(runnerStorage, "state")
	artifactParent := filepath.Join(runnerStorage, "release")
	paths := []PlannedPath{
		plannedPath("deployment", root, PathUserDeployment, ResourceDirectory, 0o700, uid, gid, false, true),
		plannedPath("manifest", filepath.Join(root, "secondbox.toml"), PathUserDeployment, ResourceFile, 0o600, uid, gid, false, true),
		plannedPath("artifacts-parent", artifactParent, PathInstallerHost, ResourceDirectory, 0o700, uid, gid, true, true),
		plannedPath("artifacts", filepath.Join(artifactParent, "artifacts"), PathInstallerHost, ResourceDirectory, 0o700, uid, gid, false, true),
		plannedPath("identity-parent", filepath.Join(root, "identity"), PathUserDeployment, ResourceDirectory, 0o700, uid, gid, false, true),
		plannedPath("runner-identity", filepath.Join(root, "identity", "runner-"+strings.TrimPrefix(input.OperationID, "install_")), PathUserDeployment, ResourceDirectory, 0o700, uid, gid, false, true),
		plannedPath("secrets", filepath.Join(root, "secrets"), PathUserDeployment, ResourceDirectory, 0o700, uid, gid, false, true),
		plannedPath("runner-pki", filepath.Join(root, "secrets", "runner-pki"), PathUserDeployment, ResourceDirectory, 0o700, uid, gid, false, true),
		plannedPath("signed-asset-catalog", filepath.Join(root, "secrets", "signed-assets.json"), PathUserDeployment, ResourceFile, 0o600, uid, gid, false, true),
		plannedPath("release-artifact-manifest", filepath.Join(root, "release-artifact-manifest.json"), PathUserDeployment, ResourceFile, 0o644, uid, gid, false, true),
		plannedPath("compose-environment", filepath.Join(root, ".secondbox.generated.env"), PathUserDeployment, ResourceFile, 0o600, uid, gid, false, true),
		plannedPath("compose-assets", filepath.Join(root, ".secondbox.generated.env.compose"), PathUserDeployment, ResourceDirectory, 0o700, uid, gid, false, true),
		plannedPath("binary-directory-root", filepath.Dir(input.BinaryDirectory), PathUserDeployment, ResourceDirectory, 0o755, uid, gid, false, true),
		plannedPath("binary-directory", input.BinaryDirectory, PathUserDeployment, ResourceDirectory, 0o755, uid, gid, false, true),
		plannedPath("cli-config-root", filepath.Dir(filepath.Dir(input.CLIConfigPath)), PathUserDeployment, ResourceDirectory, 0o700, uid, gid, false, true),
		plannedPath("cli-config-directory", filepath.Dir(input.CLIConfigPath), PathUserDeployment, ResourceDirectory, 0o700, uid, gid, false, true),
		plannedPath("cli-config", input.CLIConfigPath, PathUserDeployment, ResourceFile, 0o600, uid, gid, false, true),
		plannedPath("state", runnerState, PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true),
		plannedPath("jail", filepath.Join(runnerStorage, "jail"), PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true),
		plannedPath("run", filepath.Join(runnerState, "run"), PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true),
		plannedPath("network", filepath.Join(runnerState, "network"), PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true),
		plannedPath("snapshot-template-cache", filepath.Join(runnerState, "snapshot-template-cache"), PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true),
		plannedPath("firecracker-logs", filepath.Join(runnerState, "firecracker-logs"), PathInstallerHost, ResourceDirectory, 0o700, 0, 0, true, true),
		plannedPath("logs", filepath.Join(runnerState, "logs"), PathInstallerHost, ResourceDirectory, 0o750, runnerContainerUID, runnerContainerGID, true, true),
		plannedPath("secondbox-binary", filepath.Join(input.BinaryDirectory, "secondbox"), PathUserDeployment, ResourceBinary, 0o755, uid, gid, false, true),
		plannedPath("secondbox-deploy-binary", filepath.Join(input.BinaryDirectory, "secondbox-deploy"), PathUserDeployment, ResourceBinary, 0o755, uid, gid, false, true),
	}
	paths = append(paths, storage...)
	targetSpecs := []struct{ category, name, relative string }{
		{"platform-authority", "platform-token", "platform-token"},
		{"runner-enrollment", "runner-enrollment", "runner-enrollment"},
		{"runner-ca-certificate", "runner-ca-certificate", "runner-pki/runner-ca.crt"},
		{"runner-ca-private-key", "runner-ca-private-key", "runner-pki/runner-ca.key"},
		{"runner-server-certificate", "runner-server-certificate", "runner-pki/server.crt"},
		{"runner-server-private-key", "runner-server-private-key", "runner-pki/server.key"},
		{"database-password", "database-password", "postgres-password"},
	}
	targets := make([]SecretTarget, 0, len(targetSpecs))
	for _, spec := range targetSpecs {
		path := filepath.Join(root, "secrets", spec.relative)
		paths = append(paths, plannedPath(spec.name, path, PathUserDeployment, ResourceFile, 0o600, uid, gid, false, true))
		targets = append(targets, SecretTarget{Category: spec.category, Path: path})
	}
	return paths, targets
}

func plannedPath(name, path string, class PathClass, kind ResourceKind, mode uint32, uid, gid int64, sudo, create bool) PlannedPath {
	return PlannedPath{Name: name, Path: path, Class: class, Kind: kind, Mode: mode, OwnerUID: uid, OwnerGID: gid, RequiresSudo: sudo, Create: create}
}

func systemdMountUnitName(path string) string {
	trimmed := strings.Trim(filepath.Clean(path), "/")
	var escaped strings.Builder
	for _, value := range trimmed {
		switch {
		case value == '/':
			escaped.WriteByte('-')
		case value == '-':
			escaped.WriteString(`\x2d`)
		case value == '_' || value == '.' || value >= '0' && value <= '9' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z':
			escaped.WriteRune(value)
		default:
			fmt.Fprintf(&escaped, `\x%02x`, value)
		}
	}
	return escaped.String() + ".mount"
}

func privilegedActions(storage StoragePlan) []string {
	actions := []string{"create the declared Runner state, jail, run, network, log, and snapshot-cache directories", "verify /dev/kvm, /dev/net/tun, cgroup v2, jailer UID range, and workspace reflink isolation"}
	if storage.Choice == StorageBtrfsImage {
		actions = append(actions, "fully allocate and format the declared regular Btrfs image", "install, enable, and start the declared systemd workspace mount unit")
	}
	return actions
}

func RenderPlanReview(plan InstallPlan) string {
	var result strings.Builder
	fmt.Fprintf(&result, "Release %s\nArtifact manifest: %s\nManifest digest: %s\nSigning key: %s\nExpected downloads: %s\n", plan.Release.Version, plan.Release.ArtifactManifestURL, plan.Release.ArtifactManifestDigest, plan.Release.SigningKeyFingerprint, formatBytes(plan.Release.ExpectedDownloadBytes))
	fmt.Fprintf(&result, "Workspace: %s (%s, %s capacity)\nCapacity: %d Sandboxes, %d concurrent starts, %s memory\nCompute: Firecracker CPU template %s\nStandard bundles: %s\nNetwork: API %s, Runner %s, data plane %s, database %s, guests %s, Compose backend %s, DNS %s\nCLI platform authority: %s\nRetention: %s\n", plan.Storage.WorkspacePath, plan.Storage.Choice, formatBytes(plan.Capacity.MaxWorkspaceBytes), plan.Capacity.MaxSandboxes, plan.Capacity.ConcurrentStarts, formatBytes(plan.Capacity.MaxMemoryBytes), plan.Compute.FirecrackerCPUTemplate, strings.Join(plan.StandardBundles, ", "), plan.Network.APIAddress, plan.Network.RunnerAddress, plan.Network.DataPlaneAddress, plan.Network.DatabaseAddress, plan.Network.GuestBridgeCIDR, plan.Network.ComposeBackendCIDR, plan.Network.DNSUpstream, plan.CLI.ConfigPath, time.Duration(plan.RetentionSeconds)*time.Second)
	result.WriteString("Generated authority: " + strings.Join(plan.GeneratedAuthorityCategories, ", ") + "\nPersistent services: PostgreSQL, control plane, same-host Runner\nExisting SecondBox CLIs and CLI configuration at the reviewed paths are upgraded atomically; unrelated files are refused.\nOrdinary uninstall preserves workspaces, authority, manifests, execution assets, and service data.\nPaths requiring sudo:\n")
	for _, path := range plan.Paths {
		if path.RequiresSudo {
			fmt.Fprintf(&result, "  %s: %s\n", path.Name, path.Path)
		}
	}
	return result.String()
}

func formatBytes(value int64) string {
	const gib = int64(1 << 30)
	if value >= gib {
		return fmt.Sprintf("%.1f GiB", float64(value)/float64(gib))
	}
	return strconv.FormatInt(value, 10) + " bytes"
}
