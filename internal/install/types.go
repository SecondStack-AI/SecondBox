// Package install owns the durable authority and orchestration contracts for
// the guided single-host installer. It deliberately does not import terminal
// presentation or deployment-manifest packages.
package install

import "time"

const (
	runnerContainerUID int64 = 10001
	runnerContainerGID int64 = 10001
)

const (
	HostFactsSchema = "secondbox.install.host-facts/v1"
	PlanSchemaV1    = "secondbox.install.plan/v1"
	PlanSchema      = "secondbox.install.plan/v2"
	ReceiptSchemaV1 = "secondbox.install.receipt/v1"
	ReceiptSchema   = "secondbox.install.receipt/v2"
)

type FindingClass string

const (
	FindingPass        FindingClass = "pass"
	FindingWarning     FindingClass = "warning"
	FindingRemediable  FindingClass = "remediable"
	FindingNeedsAction FindingClass = "needs_action"
	FindingBlocked     FindingClass = "blocked"
)

type FailureClass string

const (
	FailureBlocked     FailureClass = "blocked"
	FailureNeedsAction FailureClass = "needs_action"
	FailureRetryable   FailureClass = "retryable"
	FailureInternal    FailureClass = "internal"
)

type OperationStatus string

const (
	OperationPlanned      OperationStatus = "planned"
	OperationRunning      OperationStatus = "running"
	OperationFailed       OperationStatus = "failed"
	OperationSucceeded    OperationStatus = "succeeded"
	OperationUninstalling OperationStatus = "uninstalling"
	OperationUninstalled  OperationStatus = "uninstalled"
	OperationPurging      OperationStatus = "purging"
	OperationPurged       OperationStatus = "purged"
)

type Stage string

const (
	StagePreflight              Stage = "preflight"
	StagePlanAccepted           Stage = "plan_accepted"
	StageHostApply              Stage = "host_apply"
	StageReleaseVerified        Stage = "release_verified"
	StageAssetsMaterialized     Stage = "assets_materialized"
	StageDeploymentMaterialized Stage = "deployment_materialized"
	StageRunnerEnrolled         Stage = "runner_enrolled"
	StageComposeStarted         Stage = "compose_started"
	StageCLILogin               Stage = "cli_login"
	StageReadiness              Stage = "readiness"
	StageSmokeExecution         Stage = "smoke_execution"
)

var StageSequence = []Stage{StagePreflight, StagePlanAccepted, StageHostApply, StageReleaseVerified, StageAssetsMaterialized, StageDeploymentMaterialized, StageRunnerEnrolled, StageComposeStarted, StageCLILogin, StageReadiness, StageSmokeExecution}

type UpdateStatus string

const (
	UpdateRunning   UpdateStatus = "running"
	UpdateFailed    UpdateStatus = "failed"
	UpdateSucceeded UpdateStatus = "succeeded"
)

type UpdateStage string

const (
	UpdateStagePreflight           UpdateStage = "preflight"
	UpdateStageReleaseVerified     UpdateStage = "release_verified"
	UpdateStageAssetsStaged        UpdateStage = "assets_staged"
	UpdateStageActivationStarted   UpdateStage = "activation_started"
	UpdateStageDeploymentPublished UpdateStage = "deployment_published"
	UpdateStageComposeStarted      UpdateStage = "compose_started"
	UpdateStageResourcesApplied    UpdateStage = "resources_applied"
	UpdateStageReadiness           UpdateStage = "readiness"
	UpdateStageSmokeExecution      UpdateStage = "smoke_execution"
)

var UpdateStageSequence = []UpdateStage{UpdateStagePreflight, UpdateStageReleaseVerified, UpdateStageAssetsStaged, UpdateStageActivationStarted, UpdateStageDeploymentPublished, UpdateStageComposeStarted, UpdateStageResourcesApplied, UpdateStageReadiness, UpdateStageSmokeExecution}

type StorageChoice string

const (
	StorageExistingMount StorageChoice = "existing_mount"
	StorageBtrfsImage    StorageChoice = "btrfs_image"
)

type PathClass string

const (
	PathUserDeployment    PathClass = "user_deployment"
	PathInstallerHost     PathClass = "installer_host"
	PathExistingWorkspace PathClass = "existing_workspace"
	PathFilesystemImage   PathClass = "filesystem_image"
)

type Finding struct {
	ID      string       `json:"id"`
	Class   FindingClass `json:"class"`
	Summary string       `json:"summary"`
	Detail  string       `json:"detail,omitempty"`
	Remedy  string       `json:"remedy,omitempty"`
}
type DeviceFact struct {
	Path             string `json:"path"`
	Identity         string `json:"identity"`
	Filesystem       string `json:"filesystem,omitempty"`
	SizeBytes        int64  `json:"sizeBytes"`
	AvailableBytes   int64  `json:"availableBytes"`
	Mountpoint       string `json:"mountpoint,omitempty"`
	JailerCompatible bool   `json:"jailerCompatible"`
}
type PortFact struct {
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Process  string `json:"process,omitempty"`
}
type RouteFact struct {
	Destination string `json:"destination"`
	Interface   string `json:"interface"`
	Gateway     string `json:"gateway,omitempty"`
}
type UIDRange struct {
	Start int64 `json:"start"`
	Count int64 `json:"count"`
}

type HostFacts struct {
	SchemaVersion        string            `json:"schemaVersion"`
	ObservedAt           time.Time         `json:"observedAt"`
	HostIdentity         string            `json:"hostIdentity"`
	OS                   string            `json:"os"`
	Architecture         string            `json:"architecture"`
	InvokingUID          int64             `json:"invokingUid"`
	InvokingGID          int64             `json:"invokingGid"`
	KernelVersion        string            `json:"kernelVersion"`
	SystemdVersion       string            `json:"systemdVersion,omitempty"`
	DockerVersion        string            `json:"dockerVersion,omitempty"`
	ComposeVersion       string            `json:"composeVersion,omitempty"`
	CgroupVersion        int               `json:"cgroupVersion"`
	CgroupControllers    []string          `json:"cgroupControllers"`
	CPUCount             int               `json:"cpuCount"`
	MemoryBytes          int64             `json:"memoryBytes"`
	Virtualization       string            `json:"virtualization,omitempty"`
	BtrfsSupported       bool              `json:"btrfsSupported"`
	KVMAccessible        bool              `json:"kvmAccessible"`
	TUNAccessible        bool              `json:"tunAccessible"`
	Devices              []DeviceFact      `json:"devices"`
	ListeningPorts       []PortFact        `json:"listeningPorts"`
	Routes               []RouteFact       `json:"routes"`
	DockerNetworkSubnets []string          `json:"dockerNetworkSubnets,omitempty"`
	DNSUpstreams         []string          `json:"dnsUpstreams"`
	AssignedUIDs         []int64           `json:"assignedUIDs"`
	ReservedIDRanges     []UIDRange        `json:"reservedIdRanges"`
	CandidateUIDRanges   []UIDRange        `json:"candidateUIDRanges"`
	Utilities            map[string]string `json:"utilities"`
	Findings             []Finding         `json:"findings"`
}

type PlannedPath struct {
	Name         string       `json:"name"`
	Path         string       `json:"path"`
	Class        PathClass    `json:"class"`
	Kind         ResourceKind `json:"kind"`
	Mode         uint32       `json:"mode"`
	OwnerUID     int64        `json:"ownerUid"`
	OwnerGID     int64        `json:"ownerGid"`
	RequiresSudo bool         `json:"requiresSudo"`
	Create       bool         `json:"create"`
}
type SecretTarget struct {
	Category string `json:"category"`
	Path     string `json:"path"`
}
type StoragePlan struct {
	Choice                 StorageChoice `json:"choice"`
	WorkspacePath          string        `json:"workspacePath"`
	ExistingDeviceIdentity string        `json:"existingDeviceIdentity,omitempty"`
	FilesystemImagePath    string        `json:"filesystemImagePath,omitempty"`
	ImageSizeBytes         int64         `json:"imageSizeBytes,omitempty"`
	MountUnitPath          string        `json:"mountUnitPath,omitempty"`
}
type CapacityPlan struct {
	MaxSandboxes           int64            `json:"maxSandboxes"`
	MaxVCPUCount           int64            `json:"maxVcpuCount"`
	MaxMemoryBytes         int64            `json:"maxMemoryBytes"`
	MaxWorkspaceBytes      int64            `json:"maxWorkspaceBytes"`
	ConcurrentStarts       int64            `json:"concurrentStarts"`
	ConcurrentOperations   int64            `json:"concurrentOperations"`
	StoragePressurePercent int64            `json:"storagePressurePercent"`
	SubjectQuotas          map[string]int64 `json:"subjectQuotas"`
}
type ComputePlan struct {
	FirecrackerCPUTemplate string `json:"firecrackerCpuTemplate"`
}
type NetworkPlan struct {
	APIAddress         string            `json:"apiAddress"`
	RunnerAddress      string            `json:"runnerAddress"`
	DataPlaneAddress   string            `json:"dataPlaneAddress"`
	DatabaseAddress    string            `json:"databaseAddress"`
	GuestBridgeCIDR    string            `json:"guestBridgeCidr"`
	ComposeBackendCIDR string            `json:"composeBackendCidr,omitempty"`
	TAPPrefix          string            `json:"tapPrefix"`
	CgroupParent       string            `json:"cgroupParent"`
	JailerUIDRange     UIDRange          `json:"jailerUidRange"`
	DNSUpstream        string            `json:"dnsUpstream"`
	Gateways           map[string]string `json:"gateways"`
}
type ReleasePlan struct {
	Version                string            `json:"version"`
	ArtifactManifestURL    string            `json:"artifactManifestUrl"`
	ArtifactManifestDigest string            `json:"artifactManifestDigest"`
	SigningKeyFingerprint  string            `json:"signingKeyFingerprint"`
	Images                 map[string]string `json:"images"`
	BinaryDigests          map[string]string `json:"binaryDigests"`
	ExpectedDownloadBytes  int64             `json:"expectedDownloadBytes"`
}

// ReleaseActivation is the append-only audit history for the release that an
// installation operation made active. The initial entry is derived from the
// original immutable install plan when a v1 operation is first read.
type ReleaseActivation struct {
	Release     ReleasePlan `json:"release"`
	ActivatedAt time.Time   `json:"activatedAt"`
	UpdateID    string      `json:"updateId,omitempty"`
}
type CLIPlan struct {
	ConfigPath string `json:"configPath"`
}

type InstallPlan struct {
	SchemaVersion                string              `json:"schemaVersion"`
	OperationID                  string              `json:"operationId"`
	CreatedAt                    time.Time           `json:"createdAt"`
	HostFacts                    HostFacts           `json:"hostFacts"`
	HostFactsDigest              string              `json:"hostFactsDigest"`
	Release                      ReleasePlan         `json:"release"`
	Storage                      StoragePlan         `json:"storage"`
	Capacity                     CapacityPlan        `json:"capacity"`
	Compute                      ComputePlan         `json:"compute"`
	Network                      NetworkPlan         `json:"network"`
	CLI                          CLIPlan             `json:"cli"`
	Paths                        []PlannedPath       `json:"paths"`
	SecretTargets                []SecretTarget      `json:"secretTargets"`
	GeneratedAuthorityCategories []string            `json:"generatedAuthorityCategories"`
	StandardBundles              []string            `json:"standardBundles"`
	RetentionSeconds             int64               `json:"retentionSeconds"`
	PrivilegedActions            []string            `json:"privilegedActions"`
	ReleaseHistory               []ReleaseActivation `json:"releaseHistory,omitempty"`
}

type ResourceKind string

const (
	ResourceDirectory       ResourceKind = "directory"
	ResourceFile            ResourceKind = "file"
	ResourceFilesystemImage ResourceKind = "filesystem_image"
	ResourceMountUnit       ResourceKind = "mount_unit"
	ResourceBinary          ResourceKind = "binary"
	ResourceComposeProject  ResourceKind = "compose_project"
)

type CreatedResource struct {
	ID       string       `json:"id"`
	Kind     ResourceKind `json:"kind"`
	Path     string       `json:"path,omitempty"`
	Class    PathClass    `json:"class,omitempty"`
	Stage    Stage        `json:"stage"`
	Mode     uint32       `json:"mode,omitempty"`
	OwnerUID int64        `json:"ownerUid,omitempty"`
	OwnerGID int64        `json:"ownerGid,omitempty"`
	Digest   string       `json:"digest,omitempty"`
	Identity string       `json:"identity,omitempty"`
}
type StageRecord struct {
	Stage       Stage             `json:"stage"`
	CompletedAt time.Time         `json:"completedAt"`
	Evidence    map[string]string `json:"evidence"`
}

type UpdateStageRecord struct {
	Stage       UpdateStage       `json:"stage"`
	CompletedAt time.Time         `json:"completedAt"`
	Evidence    map[string]string `json:"evidence"`
}

// UpdateRecord journals one forward-only release transition inside the
// installation receipt. TargetRelease remains durable while an interrupted
// activation is resumed by a later target-release bootstrap.
type UpdateRecord struct {
	ID              string              `json:"id"`
	SourceRelease   ReleasePlan         `json:"sourceRelease"`
	TargetRelease   ReleasePlan         `json:"targetRelease"`
	Status          UpdateStatus        `json:"status"`
	FailureClass    FailureClass        `json:"failureClass,omitempty"`
	FailureStage    UpdateStage         `json:"failureStage,omitempty"`
	CompletedStages []UpdateStageRecord `json:"completedStages"`
	StartedAt       time.Time           `json:"startedAt"`
	UpdatedAt       time.Time           `json:"updatedAt"`
}

type InstallReceipt struct {
	SchemaVersion       string            `json:"schemaVersion"`
	OperationID         string            `json:"operationId"`
	PlanDigest          string            `json:"planDigest"`
	HostIdentity        string            `json:"hostIdentity"`
	Status              OperationStatus   `json:"status"`
	FailureClass        FailureClass      `json:"failureClass,omitempty"`
	FailureStage        Stage             `json:"failureStage,omitempty"`
	CompletedStages     []StageRecord     `json:"completedStages"`
	CreatedResources    []CreatedResource `json:"createdResources"`
	PendingResourceIDs  []string          `json:"pendingResourceIds"`
	RemovedResourceIDs  []string          `json:"removedResourceIds"`
	CompletedPurgeSteps []string          `json:"completedPurgeSteps"`
	Updates             []UpdateRecord    `json:"updates,omitempty"`
	UpdatedAt           time.Time         `json:"updatedAt"`
}
