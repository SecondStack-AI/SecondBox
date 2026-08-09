package install

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
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
	MinimumObjectStoreBytes           = int64(4 << 30)
	MinimumControlBackingBytes        = MinimumBackingReserveBytes
	MinimumDeploymentBytes            = ExecutionBundleEstimateBytes
	RunnerStorageReserveBytes         = int64(4 << 30)
	MinimumRunnerStorageBytes         = ExecutionBundleEstimateBytes + MinimumWorkspaceBytes + RunnerStorageReserveBytes
	MinimumFilesystemImageBytes       = MinimumRunnerStorageBytes
	MinimumHostMemoryBytes            = int64(12 << 30)
	HostMemoryReserveBytes            = int64(4 << 30)
	MinimumHostCPUCount               = 6
	HostCPUReserveMillis              = int64(2000)
	DurableCodingCPUMillis            = standardresources.DurableCodingCPUMillis
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
	APIPort                int
	RunnerPort             int
	DataPlanePort          int
	DatabasePort           int
	ObjectStorePort        int
	ObjectStoreConsolePort int
	GuestCIDR              string
	TAPPrefix              string
	CgroupParent           string
	DNSUpstream            string
	JailerUID              UIDRange
}

type ProposalInput struct {
	OperationID              string
	CreatedAt                time.Time
	DeploymentDirectory      string
	BinaryDirectory          string
	CLIConfigPath            string
	CLITenantRef             string
	CLISubjectRef            string
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

func StorageOptions(facts HostFacts, backingAvailableBytes, releaseDownloadBytes int64) []StorageOption {
	options := []StorageOption{}
	if releaseDownloadBytes <= 0 || backingAvailableBytes < MinimumControlBackingBytes+releaseDownloadBytes+MinimumObjectStoreBytes {
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
	value := available - reserve - releaseDownloadBytes - MinimumObjectStoreBytes
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
	if input.CLIConfigPath == "" || input.CLITenantRef == "" || input.CLISubjectRef == "" {
		return InstallPlan{}, installerError("explicit CLI configuration path, tenant reference, and subject reference are required", nil)
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
	objectStoreBytes := input.BackingAvailableBytes - backingReserveBytes(input.BackingAvailableBytes) - input.Release.ExpectedDownloadBytes - storage.ImageSizeBytes
	capacity, err := proposeCapacity(facts, workspaceBytes, objectStoreBytes)
	if err != nil {
		return InstallPlan{}, err
	}
	network, err := proposeNetwork(facts, input.NetworkOverrides)
	if err != nil {
		return InstallPlan{}, err
	}
	paths, secretTargets := proposePaths(facts, input, storage, storagePaths)
	if len(input.StandardBundles) != 2 || !slices.Contains(input.StandardBundles, "agent-compartment") || !slices.Contains(input.StandardBundles, "durable-coding") {
		return InstallPlan{}, installerError("operator selection of both release-owned standard bundles is required", nil)
	}
	if input.RetentionSeconds <= 0 {
		return InstallPlan{}, installerError("operator-selected retention is required", nil)
	}
	plan := InstallPlan{SchemaVersion: PlanSchema, OperationID: input.OperationID, CreatedAt: input.CreatedAt.UTC(), HostFacts: facts, HostFactsDigest: factsDigest, Release: input.Release, Storage: storage, Capacity: capacity, Compute: ComputePlan{FirecrackerCPUTemplate: SingleHostFirecrackerCPUTemplate}, Network: network, CLI: CLIPlan{ConfigPath: input.CLIConfigPath, TenantRef: input.CLITenantRef, SubjectRef: input.CLISubjectRef}, Paths: paths, SecretTargets: secretTargets, GeneratedAuthorityCategories: []string{"application-authority", "platform-authority", "runner-enrollment", "runner-pki", "database", "object-storage"}, StandardBundles: slices.Clone(input.StandardBundles), RetentionSeconds: input.RetentionSeconds, PrivilegedActions: privilegedActions(storage)}
	if err := plan.Validate(); err != nil {
		return InstallPlan{}, err
	}
	return plan, nil
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

func proposeCapacity(facts HostFacts, workspaceBytes, objectStoreBytes int64) (CapacityPlan, error) {
	if facts.CPUCount < MinimumHostCPUCount || facts.MemoryBytes < MinimumHostMemoryBytes || workspaceBytes < MinimumWorkspaceBytes || objectStoreBytes < MinimumObjectStoreBytes {
		return CapacityPlan{}, installerError("host capacity is insufficient for the durable-coding smoke Sandbox and control services", nil)
	}
	cpuMillis := int64(facts.CPUCount)*1000 - HostCPUReserveMillis
	memory := facts.MemoryBytes - HostMemoryReserveBytes
	sandboxes := min(cpuMillis/DurableCodingCPUMillis, memory/DurableCodingMemoryBytes, workspaceBytes/MinimumWorkspaceBytes)
	active := min(sandboxes, int64(4))
	runnerOperations := sandboxes * DurableCodingConcurrentOperations
	subjectOperations := active * DurableCodingConcurrentOperations
	quotas := map[string]int64{"maxSandboxes": sandboxes * 4, "maxActiveInstances": active, "maxCpuMillis": cpuMillis, "maxMemoryBytes": memory, "maxArtifactBytes": objectStoreBytes / 2, "maxSnapshots": sandboxes * 10, "maxArtifacts": sandboxes * 100, "maxPortSessions": sandboxes * 4, "maxConcurrentOperations": subjectOperations}
	return CapacityPlan{MaxSandboxes: sandboxes, MaxCPUMillis: cpuMillis, MaxMemoryBytes: memory, MaxWorkspaceBytes: workspaceBytes, ConcurrentStarts: min(int64(2), active), ConcurrentOperations: runnerOperations, StoragePressurePercent: 85, SubjectQuotas: quotas}, nil
}

func backingReserveBytes(available int64) int64 {
	return max(MinimumBackingReserveBytes, available/5)
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
	objectStore, err := port(overrides.ObjectStorePort, 9000)
	if err != nil {
		return NetworkPlan{}, err
	}
	objectStoreConsole, err := port(overrides.ObjectStoreConsolePort, 9001)
	if err != nil {
		return NetworkPlan{}, err
	}
	cidr := overrides.GuestCIDR
	if cidr == "" {
		cidr = freeGuestCIDR(facts.Routes)
	}
	ip, network, cidrErr := net.ParseCIDR(cidr)
	ones, bits := 0, 0
	if cidrErr == nil {
		ones, bits = network.Mask.Size()
	}
	if cidr == "" || cidrErr != nil || ip.To4() == nil || bits != 32 || ones > 30 || !network.IP.Equal(ip) || routeCollides(cidr, facts.Routes) {
		return NetworkPlan{}, installerError("guest bridge CIDR is invalid or conflicts with a host route", nil)
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
	return NetworkPlan{APIAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(api)), RunnerAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(runner)), DataPlaneAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(data)), DatabaseAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(database)), ObjectStoreAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(objectStore)), ObjectStoreConsoleAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(objectStoreConsole)), GuestBridgeCIDR: cidr, TAPPrefix: tap, CgroupParent: cgroup, JailerUIDRange: uidRange, DNSUpstream: dns, Gateways: map[string]string{"agent-compartment": "agent-gateway.secondbox.internal", "durable-coding": "platform-gateway.secondbox.internal"}}, nil
}

func freeGuestCIDR(routes []RouteFact) string {
	for third := 30; third <= 31; third++ {
		candidate := fmt.Sprintf("172.%d.0.0/24", third)
		if !routeCollides(candidate, routes) {
			return candidate
		}
	}
	return ""
}

func rangesOverlap(a, b UIDRange) bool {
	return a.Count > 0 && b.Count > 0 && a.Start < b.Start+b.Count && b.Start < a.Start+a.Count
}

func routeCollides(candidate string, routes []RouteFact) bool {
	_, candidateNetwork, err := net.ParseCIDR(candidate)
	if err != nil {
		return true
	}
	for _, route := range routes {
		_, routeNetwork, err := net.ParseCIDR(route.Destination)
		if err == nil && (routeNetwork.Contains(candidateNetwork.IP) || candidateNetwork.Contains(routeNetwork.IP)) {
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
		plannedPath("logs", filepath.Join(runnerState, "logs"), PathInstallerHost, ResourceDirectory, 0o750, runnerContainerUID, runnerContainerGID, true, true),
		plannedPath("secondbox-binary", filepath.Join(input.BinaryDirectory, "secondbox"), PathUserDeployment, ResourceBinary, 0o755, uid, gid, false, true),
		plannedPath("secondbox-deploy-binary", filepath.Join(input.BinaryDirectory, "secondbox-deploy"), PathUserDeployment, ResourceBinary, 0o755, uid, gid, false, true),
	}
	paths = append(paths, storage...)
	targetSpecs := []struct{ category, name, relative string }{
		{"application-authority", "application-authorities", "application-authorities.json"},
		{"platform-authority", "platform-token", "platform-token"},
		{"runner-enrollment", "runner-enrollment", "runner-enrollment"},
		{"runner-ca-certificate", "runner-ca-certificate", "runner-pki/runner-ca.crt"},
		{"runner-ca-private-key", "runner-ca-private-key", "runner-pki/runner-ca.key"},
		{"runner-server-certificate", "runner-server-certificate", "runner-pki/server.crt"},
		{"runner-server-private-key", "runner-server-private-key", "runner-pki/server.key"},
		{"database-password", "database-password", "postgres-password"},
		{"object-access-key", "object-access-key", "object-access-key"},
		{"object-secret-key", "object-secret-key", "object-secret-key"},
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
	fmt.Fprintf(&result, "Workspace: %s (%s, %s capacity)\nCapacity: %d Sandboxes, %d concurrent starts, %s memory\nCompute: Firecracker CPU template %s\nStandard bundles: %s\nNetwork: API %s, Runner %s, data plane %s, database %s, object store %s, object console %s, guests %s, DNS %s\nCLI: %s as %s/%s\nRetention: %s\n", plan.Storage.WorkspacePath, plan.Storage.Choice, formatBytes(plan.Capacity.MaxWorkspaceBytes), plan.Capacity.MaxSandboxes, plan.Capacity.ConcurrentStarts, formatBytes(plan.Capacity.MaxMemoryBytes), plan.Compute.FirecrackerCPUTemplate, strings.Join(plan.StandardBundles, ", "), plan.Network.APIAddress, plan.Network.RunnerAddress, plan.Network.DataPlaneAddress, plan.Network.DatabaseAddress, plan.Network.ObjectStoreAddress, plan.Network.ObjectStoreConsoleAddress, plan.Network.GuestBridgeCIDR, plan.Network.DNSUpstream, plan.CLI.ConfigPath, plan.CLI.TenantRef, plan.CLI.SubjectRef, time.Duration(plan.RetentionSeconds)*time.Second)
	result.WriteString("Generated authority: " + strings.Join(plan.GeneratedAuthorityCategories, ", ") + "\nPersistent services: PostgreSQL, object storage, control plane, same-host Runner\nOrdinary uninstall preserves workspaces, authority, manifests, artifacts, and service data.\nPaths requiring sudo:\n")
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
