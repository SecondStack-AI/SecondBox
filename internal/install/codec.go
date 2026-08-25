package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var checksumPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var keyPattern = regexp.MustCompile(`^SHA256:[0-9A-F]{64}$`)
var operationPattern = regexp.MustCompile(`^install_[a-z0-9]{16,64}$`)
var updatePattern = regexp.MustCompile(`^update_[a-z0-9]{16,64}$`)

func Canonical(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("SecondBox installer canonical JSON: %w", err)
	}
	return encoded, nil
}
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func PlanDigest(plan InstallPlan) (string, error) {
	encoded, err := Canonical(plan)
	if err != nil {
		return "", err
	}
	return Digest(encoded), nil
}
func HostFactsDigest(facts HostFacts) (string, error) {
	encoded, err := Canonical(facts)
	if err != nil {
		return "", err
	}
	return Digest(encoded), nil
}

func DecodeHostFacts(content []byte) (HostFacts, error) {
	var value HostFacts
	if err := decodeStrict(content, &value); err != nil {
		return HostFacts{}, installerError("decode host facts", err)
	}
	if err := value.Validate(); err != nil {
		return HostFacts{}, err
	}
	return value, nil
}
func DecodePlan(content []byte) (InstallPlan, error) {
	var value InstallPlan
	if err := decodeStrict(content, &value); err != nil {
		return InstallPlan{}, installerError("decode plan", err)
	}
	if value.SchemaVersion == PlanSchemaV1 {
		if len(value.ReleaseHistory) != 0 {
			return InstallPlan{}, installerError("v1 plan must not contain release history", nil)
		}
		value.SchemaVersion = PlanSchema
		value.ReleaseHistory = []ReleaseActivation{{Release: value.Release, ActivatedAt: value.CreatedAt.UTC()}}
	}
	if err := value.Validate(); err != nil {
		return InstallPlan{}, err
	}
	return value, nil
}
func DecodeReceipt(content []byte, plan InstallPlan) (InstallReceipt, error) {
	var value InstallReceipt
	if err := decodeStrict(content, &value); err != nil {
		return InstallReceipt{}, installerError("decode receipt", err)
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		return InstallReceipt{}, err
	}
	if value.SchemaVersion == ReceiptSchemaV1 {
		if len(value.Updates) != 0 {
			return InstallReceipt{}, installerError("v1 receipt must not contain update history", nil)
		}
		legacyPlan := plan
		legacyPlan.SchemaVersion = PlanSchemaV1
		legacyPlan.ReleaseHistory = nil
		legacyDigest, legacyErr := PlanDigest(legacyPlan)
		if legacyErr != nil {
			return InstallReceipt{}, legacyErr
		}
		if value.PlanDigest != legacyDigest {
			return InstallReceipt{}, installerError("v1 receipt plan identity does not match", nil)
		}
		value.SchemaVersion = ReceiptSchema
		value.PlanDigest = digest
		value.Updates = []UpdateRecord{}
	}
	if err := value.Validate(digest, plan.HostFacts.HostIdentity, plan.OperationID); err != nil {
		return InstallReceipt{}, err
	}
	for _, id := range value.PendingResourceIDs {
		planned, found := plannedPathByName(plan.Paths, id)
		if !found || !planned.Create {
			return InstallReceipt{}, installerError("pending resource is absent from accepted plan: "+id, nil)
		}
	}
	if err := validateUpdateHistory(plan, value); err != nil {
		return InstallReceipt{}, err
	}
	return value, nil
}
func decodeStrict(content []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("document contains more than one JSON value")
		}
		return err
	}
	return nil
}

func (facts HostFacts) Validate() error {
	if facts.SchemaVersion != HostFactsSchema {
		return installerError("host facts schema is unsupported", nil)
	}
	if facts.ObservedAt.IsZero() || facts.HostIdentity == "" || facts.OS == "" || facts.Architecture == "" || facts.KernelVersion == "" || facts.InvokingUID < 0 || facts.InvokingGID < 0 {
		return installerError("host facts identity is incomplete", nil)
	}
	for _, subnet := range facts.DockerNetworkSubnets {
		prefix, err := netip.ParsePrefix(subnet)
		if err != nil || prefix != prefix.Masked() {
			return installerError("Docker network subnet fact is invalid", err)
		}
	}
	classes := map[FindingClass]bool{FindingPass: true, FindingWarning: true, FindingRemediable: true, FindingNeedsAction: true, FindingBlocked: true}
	ids := map[string]bool{}
	for _, finding := range facts.Findings {
		if finding.ID == "" || finding.Summary == "" || !classes[finding.Class] {
			return installerError("host finding is invalid", nil)
		}
		if ids[finding.ID] {
			return installerError("host finding ID is duplicated", nil)
		}
		ids[finding.ID] = true
	}
	return nil
}

func validateReleasePlan(release ReleasePlan) error {
	if !digestPattern.MatchString(release.ArtifactManifestDigest) {
		return installerError("artifact manifest digest is invalid", nil)
	}
	if !strings.HasPrefix(release.ArtifactManifestURL, "https://") {
		return installerError("artifact manifest URL must use HTTPS", nil)
	}
	if release.Version == "" || !keyPattern.MatchString(release.SigningKeyFingerprint) || release.ExpectedDownloadBytes <= 0 {
		return installerError("release identity, signing key, or expected download size is invalid", nil)
	}
	wantImages := []string{"control-plane", "runner", "microvm-artifacts", "installer-tools", "postgres"}
	if len(release.Images) != len(wantImages) {
		return installerError("release plan must contain exactly five immutable images", nil)
	}
	for _, name := range wantImages {
		_, digest, found := strings.Cut(release.Images[name], "@")
		if !found || !digestPattern.MatchString(digest) {
			return installerError("release image "+name+" is not digest-pinned", nil)
		}
	}
	if len(release.BinaryDigests) != 2 {
		return installerError("release plan must contain exactly the secondbox and secondbox-deploy binary digests", nil)
	}
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		if !checksumPattern.MatchString(release.BinaryDigests[name]) {
			return installerError("release binary "+name+" digest is invalid", nil)
		}
	}
	return nil
}

func (plan InstallPlan) Validate() error {
	if plan.SchemaVersion != PlanSchema {
		return installerError("plan schema is unsupported", nil)
	}
	if !operationPattern.MatchString(plan.OperationID) {
		return installerError("operation ID is invalid", nil)
	}
	if plan.CreatedAt.IsZero() {
		return installerError("plan creation time is required", nil)
	}
	if err := plan.HostFacts.Validate(); err != nil {
		return err
	}
	factsDigest, err := HostFactsDigest(plan.HostFacts)
	if err != nil {
		return err
	}
	if plan.HostFactsDigest != factsDigest {
		return installerError("host facts digest does not match", nil)
	}
	if err := validateReleasePlan(plan.Release); err != nil {
		return err
	}
	if len(plan.ReleaseHistory) == 0 {
		return installerError("release activation history is required", nil)
	}
	seenUpdateIDs := map[string]bool{}
	for index, activation := range plan.ReleaseHistory {
		if err := validateReleasePlan(activation.Release); err != nil {
			return installerError("release activation history is invalid", err)
		}
		if activation.ActivatedAt.IsZero() || (index == 0 && activation.UpdateID != "") || (index > 0 && !updatePattern.MatchString(activation.UpdateID)) || (activation.UpdateID != "" && seenUpdateIDs[activation.UpdateID]) {
			return installerError("release activation identity is invalid or duplicated", nil)
		}
		if index > 0 && !activation.ActivatedAt.After(plan.ReleaseHistory[index-1].ActivatedAt) {
			return installerError("release activation times must increase", nil)
		}
		if activation.UpdateID != "" {
			seenUpdateIDs[activation.UpdateID] = true
		}
	}
	active := plan.ReleaseHistory[len(plan.ReleaseHistory)-1].Release
	if active.Version != plan.Release.Version || active.ArtifactManifestDigest != plan.Release.ArtifactManifestDigest {
		return installerError("active release differs from release activation history", nil)
	}
	if plan.Storage.Choice != StorageExistingMount && plan.Storage.Choice != StorageBtrfsImage {
		return installerError("storage choice is invalid", nil)
	}
	if err := validateSafePath(plan.Storage.WorkspacePath); err != nil {
		return installerError("workspace path", err)
	}
	if plan.Storage.Choice == StorageExistingMount {
		if plan.Storage.ExistingDeviceIdentity == "" || plan.Storage.FilesystemImagePath != "" || plan.Storage.ImageSizeBytes != 0 || plan.Storage.MountUnitPath != "" {
			return installerError("existing workspace storage is inconsistent", nil)
		}
	}
	if plan.Storage.Choice == StorageBtrfsImage {
		if plan.Storage.ExistingDeviceIdentity != "" || plan.Storage.ImageSizeBytes < MinimumFilesystemImageBytes {
			return installerError("filesystem-image storage is inconsistent", nil)
		}
		for _, path := range []string{plan.Storage.FilesystemImagePath, plan.Storage.MountUnitPath} {
			if err := validateSafePath(path); err != nil {
				return installerError("filesystem-image path", err)
			}
		}
	}
	if len(plan.SecretTargets) == 0 {
		return installerError("secret targets are required", nil)
	}
	names := map[string]bool{}
	paths := map[string]bool{}
	validClasses := map[PathClass]bool{PathUserDeployment: true, PathInstallerHost: true, PathExistingWorkspace: true, PathFilesystemImage: true}
	validKinds := map[ResourceKind]bool{ResourceDirectory: true, ResourceFile: true, ResourceFilesystemImage: true, ResourceMountUnit: true, ResourceBinary: true}
	for _, planned := range plan.Paths {
		if planned.Name == "" || names[planned.Name] || !validClasses[planned.Class] || !validKinds[planned.Kind] || planned.Mode == 0 || planned.Mode > 0o777 || planned.OwnerUID < 0 || planned.OwnerGID < 0 {
			return installerError("planned path is invalid or duplicated", nil)
		}
		names[planned.Name] = true
		if err := validateSafePath(planned.Path); err != nil {
			return installerError("planned path "+planned.Name, err)
		}
		if paths[planned.Path] {
			return installerError("planned path target is duplicated", nil)
		}
		paths[planned.Path] = true
		if strings.HasPrefix(planned.Path, "/dev/") || planned.Path == "/dev" {
			return installerError("planned paths must not target physical or virtual device nodes", nil)
		}
		if planned.RequiresSudo {
			wantUID, wantGID := int64(0), int64(0)
			if planned.Name == "workspace" || planned.Name == "logs" {
				wantUID, wantGID = runnerContainerUID, runnerContainerGID
			} else if planned.Name == "artifacts-parent" {
				wantUID, wantGID = plan.HostFacts.InvokingUID, plan.HostFacts.InvokingGID
			}
			if planned.OwnerUID != wantUID || planned.OwnerGID != wantGID {
				return installerError("privileged planned path has unexpected explicit ownership: "+planned.Name, nil)
			}
		}
	}
	workspaceIndex := slices.IndexFunc(plan.Paths, func(path PlannedPath) bool { return path.Name == "workspace" })
	if workspaceIndex < 0 || plan.Paths[workspaceIndex].Path != plan.Storage.WorkspacePath || plan.Paths[workspaceIndex].Kind != ResourceDirectory || !plan.Paths[workspaceIndex].RequiresSudo {
		return installerError("workspace storage does not match its privileged planned resource", nil)
	}
	runnerRoot, hasRunnerRoot := plannedPathByName(plan.Paths, "runner-root")
	runnerStorage, hasRunnerStorage := plannedPathByName(plan.Paths, "runner-storage")
	artifactParent, hasArtifactParent := plannedPathByName(plan.Paths, "artifacts-parent")
	artifacts, hasArtifacts := plannedPathByName(plan.Paths, "artifacts")
	state, hasState := plannedPathByName(plan.Paths, "state")
	jail, hasJail := plannedPathByName(plan.Paths, "jail")
	run, hasRun := plannedPathByName(plan.Paths, "run")
	logs, hasLogs := plannedPathByName(plan.Paths, "logs")
	firecrackerLogs, hasFirecrackerLogs := plannedPathByName(plan.Paths, "firecracker-logs")
	if !hasRunnerRoot || !hasRunnerStorage || !hasArtifactParent || !hasArtifacts || !hasState || !hasJail || !hasRun || !hasLogs || !hasFirecrackerLogs ||
		!runnerRoot.RequiresSudo || runnerRoot.Kind != ResourceDirectory || runnerRoot.Mode != 0o711 ||
		!runnerStorage.RequiresSudo || runnerStorage.Kind != ResourceDirectory || runnerStorage.Mode != 0o711 || runnerStorage.Path != filepath.Dir(plan.Storage.WorkspacePath) || filepath.Dir(runnerStorage.Path) != runnerRoot.Path ||
		!artifactParent.RequiresSudo || artifactParent.Kind != ResourceDirectory || artifactParent.Mode != 0o700 || artifactParent.Path != filepath.Join(runnerStorage.Path, "release") ||
		artifacts.RequiresSudo || artifacts.Kind != ResourceDirectory || artifacts.Path != filepath.Join(artifactParent.Path, "artifacts") ||
		!state.RequiresSudo || state.Kind != ResourceDirectory || state.Path != filepath.Join(runnerStorage.Path, "state") ||
		!jail.RequiresSudo || jail.Kind != ResourceDirectory || jail.Mode != 0o700 || jail.Path != filepath.Join(runnerStorage.Path, "jail") ||
		!run.RequiresSudo || run.Kind != ResourceDirectory || run.Path != filepath.Join(state.Path, "run") ||
		!logs.RequiresSudo || logs.Kind != ResourceDirectory || logs.Mode != 0o750 || logs.Path != filepath.Join(state.Path, "logs") ||
		!firecrackerLogs.RequiresSudo || firecrackerLogs.Kind != ResourceDirectory || firecrackerLogs.Mode != 0o700 || firecrackerLogs.Path != filepath.Join(state.Path, "firecracker-logs") {
		return installerError("runner storage topology must colocate release assets, run state, and Workspaces", nil)
	}
	if plan.Storage.Choice == StorageBtrfsImage {
		imageIndex := slices.IndexFunc(plan.Paths, func(path PlannedPath) bool { return path.Name == "filesystem-image" })
		unitIndex := slices.IndexFunc(plan.Paths, func(path PlannedPath) bool { return path.Name == "workspace-mount-unit" })
		if imageIndex < 0 || plan.Paths[imageIndex].Path != plan.Storage.FilesystemImagePath || plan.Paths[imageIndex].Kind != ResourceFilesystemImage || filepath.Dir(plan.Storage.FilesystemImagePath) != runnerRoot.Path || unitIndex < 0 || plan.Paths[unitIndex].Path != plan.Storage.MountUnitPath || plan.Paths[unitIndex].Kind != ResourceMountUnit || filepath.Base(plan.Storage.MountUnitPath) != systemdMountUnitName(runnerStorage.Path) {
			return installerError("filesystem-image storage does not match its planned resources", nil)
		}
	}
	secretCategories := map[string]bool{}
	for _, target := range plan.SecretTargets {
		if target.Category == "" || secretCategories[target.Category] {
			return installerError("secret category is invalid or duplicated", nil)
		}
		secretCategories[target.Category] = true
		if err := validateSafePath(target.Path); err != nil {
			return installerError("secret target", err)
		}
		if !paths[target.Path] {
			return installerError("secret target is outside planned paths", nil)
		}
	}
	if len(plan.StandardBundles) != 2 || !slices.Contains(plan.StandardBundles, "agent-compartment") || !slices.Contains(plan.StandardBundles, "durable-coding") {
		return installerError("both standard bundles must be selected explicitly", nil)
	}
	if len(plan.Capacity.SubjectQuotas) != 7 {
		return installerError("all seven subject quotas are required", nil)
	}
	if plan.Capacity.MaxSandboxes <= 0 || plan.Capacity.MaxCPUMillis <= 0 || plan.Capacity.MaxMemoryBytes <= 0 || plan.Capacity.MaxWorkspaceBytes <= 0 || plan.Capacity.ConcurrentStarts <= 0 || plan.Capacity.ConcurrentOperations <= 0 || plan.Capacity.StoragePressurePercent < 50 || plan.Capacity.StoragePressurePercent > 95 {
		return installerError("capacity plan is incomplete or unsafe", nil)
	}
	if plan.Compute.FirecrackerCPUTemplate != SingleHostFirecrackerCPUTemplate {
		return installerError("single-host compute plan requires the explicit vendor-neutral Firecracker CPU template", nil)
	}
	for name, value := range plan.Capacity.SubjectQuotas {
		if name == "" || value <= 0 {
			return installerError("subject quota is invalid", nil)
		}
	}
	if plan.Network.APIAddress == "" || plan.Network.RunnerAddress == "" || plan.Network.DataPlaneAddress == "" || plan.Network.DatabaseAddress == "" || plan.Network.GuestBridgeCIDR == "" || plan.Network.DNSUpstream == "" || len(plan.Network.Gateways) != 2 {
		return installerError("network plan is incomplete", nil)
	}
	if err := validateSafePath(plan.CLI.ConfigPath); err != nil || strings.TrimSpace(plan.CLI.TenantRef) == "" || strings.TrimSpace(plan.CLI.SubjectRef) == "" || strings.ContainsAny(plan.CLI.TenantRef+plan.CLI.SubjectRef, "\r\n\x00") {
		return installerError("CLI authority plan is incomplete or invalid", err)
	}
	cliIndex := slices.IndexFunc(plan.Paths, func(path PlannedPath) bool { return path.Name == "cli-config" })
	if cliIndex < 0 || plan.Paths[cliIndex].Path != plan.CLI.ConfigPath || plan.Paths[cliIndex].Kind != ResourceFile || plan.Paths[cliIndex].Mode != 0o600 {
		return installerError("CLI configuration does not match its planned resource", nil)
	}
	binaryRootIndex := slices.IndexFunc(plan.Paths, func(path PlannedPath) bool { return path.Name == "binary-directory-root" })
	binaryDirectoryIndex := slices.IndexFunc(plan.Paths, func(path PlannedPath) bool { return path.Name == "binary-directory" })
	if binaryRootIndex < 0 || binaryDirectoryIndex < 0 || filepath.Dir(plan.Paths[binaryDirectoryIndex].Path) != plan.Paths[binaryRootIndex].Path || plan.Paths[binaryRootIndex].Kind != ResourceDirectory || plan.Paths[binaryDirectoryIndex].Kind != ResourceDirectory {
		return installerError("binary directory hierarchy does not match its planned resources", nil)
	}
	ports := map[string]bool{}
	for _, address := range []string{plan.Network.APIAddress, plan.Network.RunnerAddress, plan.Network.DataPlaneAddress, plan.Network.DatabaseAddress} {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host != "127.0.0.1" || ports[port] {
			return installerError("network service addresses must use distinct loopback ports", nil)
		}
		ports[port] = true
	}
	// Accepted v1 plans from v0.4.6 and earlier predate the explicit Compose
	// backend field and allowed an advanced operator to state any canonical IPv4
	// guest network. Preserve that validator and canonical identity only for
	// those plans; every proposal created by this version states both RFC1918
	// networks and validates them against its observed host and Docker facts.
	if plan.Network.ComposeBackendCIDR == "" {
		ip, network, err := net.ParseCIDR(plan.Network.GuestBridgeCIDR)
		ones, bits := 0, 0
		if err == nil {
			ones, bits = network.Mask.Size()
		}
		if err != nil || ip.To4() == nil || bits != 32 || ones > 30 || !network.IP.Equal(ip) {
			return installerError("guest bridge CIDR is invalid", err)
		}
	} else {
		occupied := observedInstallIPv4Prefixes(plan.HostFacts)
		guestPrefix, err := validatedInstallCIDR(plan.Network.GuestBridgeCIDR, occupied)
		if err != nil {
			return installerError("guest bridge CIDR is invalid", err)
		}
		if _, err := validatedComposeCIDR(plan.Network.ComposeBackendCIDR, append(occupied, guestPrefix)); err != nil {
			return installerError("Compose backend CIDR is invalid or overlaps the guest bridge CIDR", err)
		}
	}
	maximumUID := int64(^uint32(0))
	uidRange := plan.Network.JailerUIDRange
	if uidRange.Start < 10000 || uidRange.Count < 1 || uidRange.Start > maximumUID || uidRange.Count > maximumUID-uidRange.Start+1 {
		return installerError("jailer UID range does not fit Linux 32-bit user IDs", nil)
	}
	dns := net.ParseIP(plan.Network.DNSUpstream)
	if dns == nil || dns.IsLoopback() || dns.IsUnspecified() {
		return installerError("DNS upstream is invalid", nil)
	}
	if len(plan.PrivilegedActions) == 0 {
		return installerError("privileged action review is absent", nil)
	}
	return nil
}

func (receipt InstallReceipt) Validate(planDigest, hostIdentity, operationID string) error {
	if receipt.SchemaVersion != ReceiptSchema {
		return installerError("receipt schema is unsupported", nil)
	}
	if !operationPattern.MatchString(receipt.OperationID) || receipt.OperationID != operationID {
		return installerError("receipt operation ID is invalid", nil)
	}
	if receipt.PlanDigest != planDigest || !digestPattern.MatchString(receipt.PlanDigest) {
		return installerError("receipt plan identity does not match", nil)
	}
	if receipt.HostIdentity != hostIdentity {
		return installerError("receipt host identity does not match", nil)
	}
	statuses := map[OperationStatus]bool{OperationPlanned: true, OperationRunning: true, OperationFailed: true, OperationSucceeded: true, OperationUninstalling: true, OperationUninstalled: true, OperationPurging: true, OperationPurged: true}
	if !statuses[receipt.Status] {
		return installerError("receipt operation status is invalid", nil)
	}
	if receipt.Status == OperationFailed {
		validFailures := map[FailureClass]bool{FailureBlocked: true, FailureNeedsAction: true, FailureRetryable: true, FailureInternal: true}
		if !validFailures[receipt.FailureClass] || slices.Index(StageSequence, receipt.FailureStage) < 0 {
			return installerError("failed receipt must identify a valid failure class and stage", nil)
		}
	} else if receipt.FailureClass != "" || receipt.FailureStage != "" {
		return installerError("non-failed receipt must not retain failure state", nil)
	}
	seenStages := map[Stage]bool{}
	last := -1
	for _, record := range receipt.CompletedStages {
		index := slices.Index(StageSequence, record.Stage)
		if index < 0 || index <= last || seenStages[record.Stage] || record.CompletedAt.IsZero() {
			return installerError("receipt stage sequence is invalid", nil)
		}
		seenStages[record.Stage] = true
		last = index
	}
	resourceIDs := map[string]bool{}
	for _, resource := range receipt.CreatedResources {
		if resource.ID == "" || resourceIDs[resource.ID] || slices.Index(StageSequence, resource.Stage) < 0 {
			return installerError("created resource identity is invalid or duplicated", nil)
		}
		resourceIDs[resource.ID] = true
		if resource.Path != "" {
			if err := validateSafePath(resource.Path); err != nil {
				return installerError("created resource path", err)
			}
		}
		if resource.Digest != "" && !digestPattern.MatchString(resource.Digest) {
			return installerError("created resource digest is invalid", nil)
		}
		if (resource.Kind == ResourceFile || resource.Kind == ResourceBinary || resource.Kind == ResourceMountUnit) && resource.Digest == "" {
			return installerError("created regular resource lacks content identity", nil)
		}
	}
	pendingIDs := map[string]bool{}
	for _, id := range receipt.PendingResourceIDs {
		if id == "" || pendingIDs[id] || resourceIDs[id] {
			return installerError("pending resource identity is invalid, duplicated, or already created", nil)
		}
		pendingIDs[id] = true
	}
	removedIDs := map[string]bool{}
	for _, id := range receipt.RemovedResourceIDs {
		if id == "" || removedIDs[id] || !resourceIDs[id] {
			return installerError("removed resource identity is invalid, duplicated, or was never created", nil)
		}
		removedIDs[id] = true
	}
	purgeSteps := map[string]bool{}
	for _, step := range receipt.CompletedPurgeSteps {
		if step != "compose-volumes" || purgeSteps[step] || (receipt.Status != OperationPurging && receipt.Status != OperationPurged) {
			return installerError("completed purge step is invalid or duplicated", nil)
		}
		purgeSteps[step] = true
	}
	if err := validateUpdateRecords(receipt.Updates); err != nil {
		return err
	}
	if _, active := receipt.ActiveUpdate(); active && receipt.Status != OperationSucceeded {
		return installerError("active update requires a successful installation receipt", nil)
	}
	return nil
}

func validateUpdateRecords(updates []UpdateRecord) error {
	ids := map[string]bool{}
	for updateIndex, update := range updates {
		if !updatePattern.MatchString(update.ID) || ids[update.ID] || update.StartedAt.IsZero() || update.UpdatedAt.Before(update.StartedAt) {
			return installerError("update record identity or time is invalid", nil)
		}
		ids[update.ID] = true
		if err := validateReleasePlan(update.SourceRelease); err != nil {
			return installerError("update source release is invalid", err)
		}
		if err := validateReleasePlan(update.TargetRelease); err != nil {
			return installerError("update target release is invalid", err)
		}
		if update.SourceRelease.ArtifactManifestDigest == update.TargetRelease.ArtifactManifestDigest {
			return installerError("update source and target releases are identical", nil)
		}
		for stageIndex, record := range update.CompletedStages {
			if stageIndex >= len(UpdateStageSequence) || record.Stage != UpdateStageSequence[stageIndex] || record.CompletedAt.IsZero() || record.CompletedAt.Before(update.StartedAt) || (stageIndex > 0 && record.CompletedAt.Before(update.CompletedStages[stageIndex-1].CompletedAt)) {
				return installerError("update stage sequence is invalid", nil)
			}
			if record.Stage == UpdateStageActivationStarted && !digestPattern.MatchString(record.Evidence["sourceComposeSubject"]) {
				return installerError("update activation lacks an authenticated source Compose identity", nil)
			}
		}
		switch update.Status {
		case UpdateRunning:
			if update.FailureClass != "" || update.FailureStage != "" || len(update.CompletedStages) == len(UpdateStageSequence) {
				return installerError("running update state is invalid", nil)
			}
		case UpdateFailed:
			validFailures := map[FailureClass]bool{FailureBlocked: true, FailureNeedsAction: true, FailureRetryable: true, FailureInternal: true}
			if !validFailures[update.FailureClass] || slices.Index(UpdateStageSequence, update.FailureStage) < 0 || len(update.CompletedStages) == len(UpdateStageSequence) {
				return installerError("failed update state is invalid", nil)
			}
		case UpdateSucceeded:
			if update.FailureClass != "" || update.FailureStage != "" || len(update.CompletedStages) != len(UpdateStageSequence) {
				return installerError("successful update state is invalid", nil)
			}
		default:
			return installerError("update status is invalid", nil)
		}
		if updateIndex < len(updates)-1 && update.Status != UpdateSucceeded {
			return installerError("only the last update may be incomplete", nil)
		}
	}
	return nil
}

func validateUpdateHistory(plan InstallPlan, receipt InstallReceipt) error {
	if len(plan.ReleaseHistory) != 1+countSucceededUpdates(receipt.Updates) {
		return installerError("release activation and update histories differ", nil)
	}
	active := plan.ReleaseHistory[0].Release
	activationIndex := 1
	for _, update := range receipt.Updates {
		if !sameReleasePlan(active, update.SourceRelease) {
			return installerError("update source does not match the preceding active release", nil)
		}
		if update.Status == UpdateSucceeded {
			activation := plan.ReleaseHistory[activationIndex]
			if activation.UpdateID != update.ID || !sameReleasePlan(activation.Release, update.TargetRelease) || !activation.ActivatedAt.Equal(update.UpdatedAt) {
				return installerError("successful update differs from release activation history", nil)
			}
			active = update.TargetRelease
			activationIndex++
		}
	}
	if !sameReleasePlan(active, plan.Release) {
		return installerError("update history active release differs from plan", nil)
	}
	return nil
}

func countSucceededUpdates(updates []UpdateRecord) int {
	count := 0
	for _, update := range updates {
		if update.Status == UpdateSucceeded {
			count++
		}
	}
	return count
}

func sameReleasePlan(left, right ReleasePlan) bool {
	leftBytes, leftErr := Canonical(left)
	rightBytes, rightErr := Canonical(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

func validateSafePath(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return errors.New("must be an absolute normalized non-root path")
	}
	if strings.ContainsAny(path, "*$?[]{}") || strings.Contains(path, "${") {
		return errors.New("must not contain variables or globs")
	}
	if strings.IndexFunc(path, func(value rune) bool { return unicode.IsControl(value) || unicode.IsSpace(value) }) >= 0 {
		return errors.New("must not contain whitespace or control characters")
	}
	return nil
}
func installerError(message string, err error) error {
	if err == nil {
		return fmt.Errorf("SecondBox installer: %s", message)
	}
	return fmt.Errorf("SecondBox installer: %s: %w", message, err)
}
