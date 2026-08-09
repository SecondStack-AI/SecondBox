package deployconfig

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SecondStack-AI/SecondBox/internal/assetcatalog"
	"github.com/SecondStack-AI/SecondBox/internal/install"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
)

// SingleHostInstallResult identifies the create-only deployment material that
// must be used by the later Runner enrollment and Compose stages.
type SingleHostInstallResult struct {
	ManifestPath            string
	RunnerID                string
	RunnerIdentityDirectory string
	PlatformTokenPath       string
}

// InitSingleHostFromReleaseOrValidate makes the stage commit marker (the
// create-only manifest) resumable. A completed but not-yet-recorded
// materialization is adopted only when its deterministic manifest, release,
// signed-asset catalog, and resolved deployment all match the accepted plan.
func InitSingleHostFromReleaseOrValidate(plan install.InstallPlan, release releasecontract.ArtifactManifest, releaseBytes []byte, verified install.VerifiedArtifact) (SingleHostInstallResult, error) {
	manifestPath := installPath(plan, "manifest")
	if _, err := os.Lstat(manifestPath); os.IsNotExist(err) {
		if err := recoverPartialSingleHostInstall(plan); err != nil {
			return SingleHostInstallResult{}, err
		}
		return InitSingleHostFromRelease(plan, release, releaseBytes, verified)
	} else if err != nil {
		return SingleHostInstallResult{}, manifestError("inspect single-host manifest", err)
	}
	result, err := validateExistingSingleHostInstall(plan, release, releaseBytes, verified)
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	if err := removeSingleHostMaterializationMarker(plan, false); err != nil {
		return SingleHostInstallResult{}, err
	}
	return result, nil
}

// InitSingleHostFromRelease materializes one explicit loopback-only
// development deployment from an accepted installer plan and an independently
// verified public release. The operation directory must already exist, while
// every file and child directory owned by this stage must not exist.
func InitSingleHostFromRelease(plan install.InstallPlan, release releasecontract.ArtifactManifest, releaseBytes []byte, verified install.VerifiedArtifact) (SingleHostInstallResult, error) {
	if err := plan.Validate(); err != nil {
		return SingleHostInstallResult{}, err
	}
	if err := release.Validate(); err != nil {
		return SingleHostInstallResult{}, err
	}
	if releasecontract.Digest(releaseBytes) != plan.Release.ArtifactManifestDigest || release.Version != plan.Release.Version ||
		release.ControlPlane.Reference != plan.Release.Images["control-plane"] || release.Runner.Reference != plan.Release.Images["runner"] ||
		release.MicroVM.ImageReference != plan.Release.Images["microvm-artifacts"] || release.InstallerTools.Reference != plan.Release.Images["installer-tools"] ||
		release.BundledServices.Postgres != plan.Release.Images["postgres"] || release.BundledServices.ObjectStore != plan.Release.Images["object-store"] || release.BundledServices.ObjectStoreClient != plan.Release.Images["object-store-client"] ||
		release.MicroVM.SigningKeyFingerprint != plan.Release.SigningKeyFingerprint || verified.ManifestDigest != release.MicroVM.SignedManifestDigest ||
		verified.SigningKeyID != strings.ToLower(strings.TrimPrefix(release.MicroVM.SigningKeyFingerprint, "SHA256:")) {
		return SingleHostInstallResult{}, manifestError("single-host release identity differs from the accepted install plan", nil)
	}
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		binary, found := releaseBinary(release, name)
		if !found || binary.SHA256 != plan.Release.BinaryDigests[name] {
			return SingleHostInstallResult{}, manifestError("single-host binary identity differs from the accepted install plan: "+name, nil)
		}
	}

	deployment := installPath(plan, "deployment")
	manifestPath := installPath(plan, "manifest")
	secrets := installPath(plan, "secrets")
	pki := installPath(plan, "runner-pki")
	identityParent := installPath(plan, "identity-parent")
	runnerID := "runner-" + strings.TrimPrefix(plan.OperationID, "install_")
	runnerIdentity := installPath(plan, "runner-identity")
	if deployment == "" || manifestPath == "" || secrets == "" || pki == "" || identityParent == "" || runnerIdentity == "" || runnerIdentity != filepath.Join(identityParent, runnerID) {
		return SingleHostInstallResult{}, manifestError("single-host plan omits a required deployment path", nil)
	}
	info, err := os.Lstat(deployment)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return SingleHostInstallResult{}, manifestError("single-host operation directory must be an existing non-symbolic-link directory", err)
	}
	created := make([]string, 0, 16)
	cleanup := true
	defer func() {
		if cleanup {
			for index := len(created) - 1; index >= 0; index-- {
				_ = os.Remove(created[index])
			}
		}
	}()
	createDirectory := func(path string, mode os.FileMode) error {
		if err := os.Mkdir(path, mode); err != nil {
			return err
		}
		created = append(created, path)
		return nil
	}
	write := func(path string, content []byte, mode os.FileMode) error {
		if err := writeAtomic(path, content, mode, false); err != nil {
			return err
		}
		created = append(created, path)
		return nil
	}
	marker := singleHostMaterializationMarker(plan)
	digest, err := install.PlanDigest(plan)
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	if err := write(marker, []byte(digest+"\n"), 0o600); err != nil {
		return SingleHostInstallResult{}, manifestError("create single-host materialization marker", err)
	}
	for _, directory := range []string{secrets, pki, identityParent} {
		if err := createDirectory(directory, 0o700); err != nil {
			return SingleHostInstallResult{}, manifestError("create single-host protected directory", err)
		}
	}

	targets := make(map[string]string, len(plan.SecretTargets))
	for _, target := range plan.SecretTargets {
		targets[target.Category] = target.Path
	}
	randomSecret := func(category string, bytes int) (string, error) {
		path := targets[category]
		if path == "" {
			return "", manifestError("single-host secret target is absent: "+category, nil)
		}
		value := make([]byte, bytes)
		if _, err := rand.Read(value); err != nil {
			return "", err
		}
		if err := write(path, []byte(hex.EncodeToString(value)+"\n"), 0o600); err != nil {
			return "", err
		}
		return relativeTo(deployment, path), nil
	}
	postgresPassword, err := randomSecret("database-password", 32)
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	objectAccess, err := randomSecret("object-access-key", 16)
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	objectSecret, err := randomSecret("object-secret-key", 32)
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	platformToken, err := randomSecret("platform-authority", 32)
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	runnerCredential, err := randomSecret("runner-enrollment", 48)
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	applicationAuthorities := targets["application-authority"]
	if applicationAuthorities == "" {
		return SingleHostInstallResult{}, manifestError("single-host application authority target is absent", nil)
	}
	if err := write(applicationAuthorities, []byte("[]\n"), 0o600); err != nil {
		return SingleHostInstallResult{}, err
	}
	if err := generateTrackedRunnerPKI(pki, &created, generateRunnerPKI); err != nil {
		return SingleHostInstallResult{}, err
	}
	for _, name := range []string{"runner-ca.crt", "server.crt"} {
		if err := os.Chmod(filepath.Join(pki, name), 0o600); err != nil {
			return SingleHostInstallResult{}, err
		}
	}

	catalogPath := installPath(plan, "signed-asset-catalog")
	catalog := struct {
		Assets []assetcatalog.SignedAsset `json:"assets"`
	}{Assets: []assetcatalog.SignedAsset{
		componentAsset(release.MicroVM.RuntimeBundle, verified.SigningKeyID, release.GuestProtocol.Maximum),
		componentAsset(release.MicroVM.ToolchainBundle, verified.SigningKeyID, release.GuestProtocol.Maximum),
	}}
	catalogBytes, err := json.Marshal(catalog)
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	if err := write(catalogPath, append(catalogBytes, '\n'), 0o600); err != nil {
		return SingleHostInstallResult{}, err
	}
	releasePath := installPath(plan, "release-artifact-manifest")
	if err := write(releasePath, slices.Clone(releaseBytes), 0o644); err != nil {
		return SingleHostInstallResult{}, err
	}

	manifest, err := singleHostManifest(plan, release, verified.SigningKeyID, runnerID, runnerIdentity, postgresPassword, objectAccess, objectSecret, platformToken, runnerCredential, relativeTo(deployment, applicationAuthorities), relativeTo(deployment, catalogPath), relativeTo(deployment, releasePath), relativeTo(deployment, pki))
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	encoded, err := encodeManifest(manifest)
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	if err := write(manifestPath, encoded, 0o600); err != nil {
		return SingleHostInstallResult{}, err
	}
	if _, err := resolveManifestWithOptions(manifest, deployment, false); err != nil {
		return SingleHostInstallResult{}, err
	}
	if err := os.Remove(marker); err != nil {
		return SingleHostInstallResult{}, manifestError("remove single-host materialization marker", err)
	}
	cleanup = false
	return SingleHostInstallResult{ManifestPath: manifestPath, RunnerID: runnerID, RunnerIdentityDirectory: runnerIdentity, PlatformTokenPath: targets["platform-authority"]}, nil
}

func singleHostMaterializationMarker(plan install.InstallPlan) string {
	return filepath.Join(installPath(plan, "deployment"), ".secondbox-materialization")
}

// recoverPartialSingleHostInstall closes the power-loss window between the
// first create-only file and the manifest commit marker. Recovery is allowed
// only when a protected marker binds the partial tree to this exact plan.
func recoverPartialSingleHostInstall(plan install.InstallPlan) error {
	marker := singleHostMaterializationMarker(plan)
	info, err := os.Lstat(marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return manifestError("partial single-host materialization marker is unsafe", err)
	}
	digest, err := install.PlanDigest(plan)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != digest+"\n" {
		return manifestError("partial single-host materialization marker differs from the accepted plan", err)
	}
	deployment := installPath(plan, "deployment")
	for _, name := range []string{"manifest", "signed-asset-catalog", "release-artifact-manifest", "runner-identity", "identity-parent", "runner-pki", "secrets"} {
		path := installPath(plan, name)
		if path == "" {
			continue
		}
		relative, err := filepath.Rel(deployment, path)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return manifestError("partial single-host recovery path escapes the deployment: "+name, err)
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return manifestError("partial single-host recovery target is unsafe: "+name, err)
		}
		if err := os.RemoveAll(path); err != nil {
			return manifestError("remove partial single-host materialization: "+name, err)
		}
	}
	return removeSingleHostMaterializationMarker(plan, true)
}

func removeSingleHostMaterializationMarker(plan install.InstallPlan, required bool) error {
	marker := singleHostMaterializationMarker(plan)
	err := os.Remove(marker)
	if os.IsNotExist(err) && !required {
		return nil
	}
	if err != nil {
		return manifestError("remove single-host materialization marker", err)
	}
	return nil
}

func generateTrackedRunnerPKI(directory string, created *[]string, generate func(string, string, int64) error) error {
	for _, name := range []string{"runner-ca.crt", "runner-ca.key", "server.crt", "server.key"} {
		*created = append(*created, filepath.Join(directory, name))
	}
	return generate(directory, "control-plane", 825)
}

func validateExistingSingleHostInstall(plan install.InstallPlan, release releasecontract.ArtifactManifest, releaseBytes []byte, verified install.VerifiedArtifact) (SingleHostInstallResult, error) {
	if err := plan.Validate(); err != nil {
		return SingleHostInstallResult{}, err
	}
	if err := release.Validate(); err != nil {
		return SingleHostInstallResult{}, err
	}
	if releasecontract.Digest(releaseBytes) != plan.Release.ArtifactManifestDigest || release.Version != plan.Release.Version ||
		release.ControlPlane.Reference != plan.Release.Images["control-plane"] || release.Runner.Reference != plan.Release.Images["runner"] ||
		release.MicroVM.ImageReference != plan.Release.Images["microvm-artifacts"] || release.InstallerTools.Reference != plan.Release.Images["installer-tools"] ||
		release.BundledServices.Postgres != plan.Release.Images["postgres"] || release.BundledServices.ObjectStore != plan.Release.Images["object-store"] || release.BundledServices.ObjectStoreClient != plan.Release.Images["object-store-client"] ||
		release.MicroVM.SigningKeyFingerprint != plan.Release.SigningKeyFingerprint || verified.ManifestDigest != release.MicroVM.SignedManifestDigest ||
		verified.SigningKeyID != strings.ToLower(strings.TrimPrefix(release.MicroVM.SigningKeyFingerprint, "SHA256:")) {
		return SingleHostInstallResult{}, manifestError("existing single-host release identity differs from the accepted install plan", nil)
	}
	for _, name := range []string{"secondbox", "secondbox-deploy"} {
		binary, found := releaseBinary(release, name)
		if !found || binary.SHA256 != plan.Release.BinaryDigests[name] {
			return SingleHostInstallResult{}, manifestError("existing single-host binary identity differs from the accepted install plan: "+name, nil)
		}
	}
	deployment := installPath(plan, "deployment")
	manifestPath := installPath(plan, "manifest")
	runnerID := "runner-" + strings.TrimPrefix(plan.OperationID, "install_")
	runnerIdentity := installPath(plan, "runner-identity")
	targets := make(map[string]string, len(plan.SecretTargets))
	for _, target := range plan.SecretTargets {
		targets[target.Category] = target.Path
	}
	relativeTarget := func(category string) string { return relativeTo(deployment, targets[category]) }
	catalogPath := installPath(plan, "signed-asset-catalog")
	releasePath := installPath(plan, "release-artifact-manifest")
	pkiPath := installPath(plan, "runner-pki")
	expectedManifest, err := singleHostManifest(plan, release, verified.SigningKeyID, runnerID, runnerIdentity, relativeTarget("database-password"), relativeTarget("object-access-key"), relativeTarget("object-secret-key"), relativeTarget("platform-authority"), relativeTarget("runner-enrollment"), relativeTarget("application-authority"), relativeTo(deployment, catalogPath), relativeTo(deployment, releasePath), relativeTo(deployment, pkiPath))
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	expectedManifestBytes, err := encodeManifest(expectedManifest)
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	actualManifestBytes, err := readSingleHostPlannedFile(plan, "manifest")
	if err != nil || !bytes.Equal(actualManifestBytes, expectedManifestBytes) {
		return SingleHostInstallResult{}, manifestError("existing single-host manifest differs from the accepted install plan", err)
	}
	actualReleaseBytes, err := readSingleHostPlannedFile(plan, "release-artifact-manifest")
	if err != nil || !bytes.Equal(actualReleaseBytes, releaseBytes) {
		return SingleHostInstallResult{}, manifestError("existing single-host release manifest differs from the verified release", err)
	}
	catalog := struct {
		Assets []assetcatalog.SignedAsset `json:"assets"`
	}{Assets: []assetcatalog.SignedAsset{
		componentAsset(release.MicroVM.RuntimeBundle, verified.SigningKeyID, release.GuestProtocol.Maximum),
		componentAsset(release.MicroVM.ToolchainBundle, verified.SigningKeyID, release.GuestProtocol.Maximum),
	}}
	expectedCatalog, err := json.Marshal(catalog)
	if err != nil {
		return SingleHostInstallResult{}, err
	}
	actualCatalog, err := readSingleHostPlannedFile(plan, "signed-asset-catalog")
	if err != nil || !bytes.Equal(actualCatalog, append(expectedCatalog, '\n')) {
		return SingleHostInstallResult{}, manifestError("existing single-host signed-asset catalog differs from the verified release", err)
	}
	if _, err := resolveManifestWithOptions(expectedManifest, deployment, false); err != nil {
		return SingleHostInstallResult{}, manifestError("existing single-host deployment does not resolve", err)
	}
	return SingleHostInstallResult{ManifestPath: manifestPath, RunnerID: runnerID, RunnerIdentityDirectory: runnerIdentity, PlatformTokenPath: targets["platform-authority"]}, nil
}

func readSingleHostPlannedFile(plan install.InstallPlan, name string) ([]byte, error) {
	path := installPath(plan, name)
	var expected *install.PlannedPath
	for index := range plan.Paths {
		if plan.Paths[index].Name == name {
			expected = &plan.Paths[index]
			break
		}
	}
	if expected == nil || path == "" || expected.Kind != install.ResourceFile {
		return nil, manifestError("single-host planned file is absent: "+name, nil)
	}
	if err := install.ValidatePlannedPath(*expected); err != nil {
		return nil, manifestError("existing single-host planned file differs: "+name, err)
	}
	return os.ReadFile(path)
}

func singleHostManifest(plan install.InstallPlan, release releasecontract.ArtifactManifest, signingKeyID, runnerID, runnerIdentity, postgresPassword, objectAccess, objectSecret, platformToken, runnerCredential, applicationAuthorities, catalogPath, releasePath, pkiPath string) (ManifestV1, error) {
	apiHost, apiPort, err := splitPlanAddress(plan.Network.APIAddress)
	if err != nil {
		return ManifestV1{}, err
	}
	runnerHost, runnerPort, err := splitPlanAddress(plan.Network.RunnerAddress)
	if err != nil {
		return ManifestV1{}, err
	}
	_, dataPort, err := splitPlanAddress(plan.Network.DataPlaneAddress)
	if err != nil {
		return ManifestV1{}, err
	}
	databaseHost, databasePort, err := splitPlanAddress(plan.Network.DatabaseAddress)
	if err != nil {
		return ManifestV1{}, err
	}
	objectStoreHost, objectStorePort, err := splitPlanAddress(plan.Network.ObjectStoreAddress)
	if err != nil {
		return ManifestV1{}, err
	}
	objectStoreConsoleHost, objectStoreConsolePort, err := splitPlanAddress(plan.Network.ObjectStoreConsoleAddress)
	if err != nil {
		return ManifestV1{}, err
	}
	if objectStoreConsoleHost != objectStoreHost {
		return ManifestV1{}, manifestError("single-host object-store addresses must use one loopback host", nil)
	}
	prefix, err := netip.ParsePrefix(plan.Network.GuestBridgeCIDR)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() > 30 {
		return ManifestV1{}, manifestError("single-host guest bridge must be an IPv4 subnet with usable host addresses", err)
	}
	bridge := prefix.Addr().Next()
	guest := bridge.Next()
	features := []string{"compute", "evidence", "exec-streaming", "file-streaming", "local-workspace", "port-proxy", "pty"}
	pools := make([]StandardRunnerPool, 0, len(plan.StandardBundles))
	for _, bundle := range plan.StandardBundles {
		pools = append(pools, StandardRunnerPool{Bundle: bundle, Name: standardresources.PoolAMD64, Architectures: []string{"amd64"}, Capabilities: slices.Clone(features), State: "ready", MaxSandboxes: integer(plan.Capacity.MaxSandboxes), MaxCPUMillis: integer(plan.Capacity.MaxCPUMillis), MaxMemoryBytes: integer(plan.Capacity.MaxMemoryBytes)})
	}
	runnerRoot := installPath(plan, "runner-root")
	state := installPath(plan, "state")
	runnerStorage := installPath(plan, "runner-storage")
	artifacts := installPath(plan, "artifacts")
	if runnerRoot == "" || runnerStorage == "" || state == "" || artifacts == "" {
		return ManifestV1{}, manifestError("single-host plan omits Runner host paths", nil)
	}
	gatewayEntries := make([]string, 0, len(plan.Network.Gateways))
	for _, domain := range plan.Network.Gateways {
		gatewayEntries = append(gatewayEntries, domain+"="+bridge.String())
	}
	slices.Sort(gatewayEntries)
	quota := func(name string) *int64 { return integer(plan.Capacity.SubjectQuotas[name]) }
	storageDeny := plan.Capacity.StoragePressurePercent
	maxVCPUs := max(int64(1), int64(plan.HostFacts.CPUCount)/plan.Capacity.MaxSandboxes)
	maxMemoryMiB := max(int64(512), plan.Capacity.MaxMemoryBytes/plan.Capacity.MaxSandboxes/(1<<20))
	maxDiskMiB := max(int64(1024), plan.Capacity.MaxWorkspaceBytes/plan.Capacity.MaxSandboxes/(1<<20))
	manifest := ManifestV1{
		SchemaVersion:     1,
		Deployment:        Deployment{Mode: "development", ComposeProjectName: "secondbox-" + strings.ReplaceAll(strings.TrimPrefix(plan.OperationID, "install_"), "_", "-"), ComposeBackendCIDR: plan.Network.ComposeBackendCIDR, PublicBaseURL: "http://" + plan.Network.APIAddress, TLSTermination: "development-loopback", ControlPlaneImage: release.ControlPlane.Reference, RunnerImage: release.Runner.Reference, PostgresImage: release.BundledServices.Postgres, ObjectStoreImage: release.BundledServices.ObjectStore, ObjectStoreClientImage: release.BundledServices.ObjectStoreClient, APIBindIP: apiHost, APIPublishedPort: integer(apiPort), ListenAddress: "0.0.0.0:8080", RunnerBindIP: runnerHost, RunnerPublishedPort: integer(runnerPort), RunnerListenAddress: "0.0.0.0:9443", LogPath: "/var/log/secondbox/control-plane.jsonl", SignedAssetCatalog: catalogPath, SignedAssetCatalogPath: "/etc/secondbox/signed-assets.json", DevelopmentWaitSeconds: integer(300)},
		Database:          Database{Mode: "bundled", BindIP: databaseHost, PublishedPort: integer(databasePort), Name: "secondbox", User: "secondbox", PasswordFile: postgresPassword},
		ObjectStore:       ObjectStore{Mode: "bundled", Endpoint: "http://object-store:9000", Bucket: "secondbox-" + strings.TrimPrefix(plan.OperationID, "install_"), Region: "us-east-1", UsePathStyle: boolean(true), TempDirectory: "/tmp", AccessKeyFile: objectAccess, SecretKeyFile: objectSecret, BindIP: objectStoreHost, PublishedPort: integer(objectStorePort), ConsolePublishedPort: integer(objectStoreConsolePort)},
		RunnerTrust:       RunnerTrust{EnrollmentCredentialFile: runnerCredential, CACertificateFile: filepath.Join(pkiPath, "runner-ca.crt"), CAPrivateKeyFile: filepath.Join(pkiPath, "runner-ca.key"), ServerCertificateFile: filepath.Join(pkiPath, "server.crt"), ServerPrivateKeyFile: filepath.Join(pkiPath, "server.key"), ServerName: "control-plane", CertificateLifetimeDays: integer(825)},
		Applications:      Applications{PlatformTokenFile: platformToken, ApplicationAuthoritiesFile: applicationAuthorities},
		StandardResources: StandardResources{ArtifactManifest: releasePath, Bundles: slices.Clone(plan.StandardBundles), RunnerPools: pools, ApplyWaitSeconds: integer(300)},
		Policy:            Policy{DataPlaneRetentionSeconds: integer(plan.RetentionSeconds), DataPlanePollIntervalMilliseconds: integer(250), RunnerCommandPollIntervalMilliseconds: integer(250), RunnerEnabledFeatures: strings.Join(features[1:], ","), DefaultSubjectMaxSandboxes: quota("maxSandboxes"), DefaultSubjectMaxActiveInstances: quota("maxActiveInstances"), DefaultSubjectMaxCPUMillis: quota("maxCpuMillis"), DefaultSubjectMaxMemoryBytes: quota("maxMemoryBytes"), DefaultSubjectMaxArtifactBytes: quota("maxArtifactBytes"), DefaultSubjectMaxSnapshots: quota("maxSnapshots"), DefaultSubjectMaxArtifacts: quota("maxArtifacts"), DefaultSubjectMaxPortSessions: quota("maxPortSessions"), DefaultSubjectMaxConcurrentOperations: quota("maxConcurrentOperations")},
	}
	dnsUpstream := netip.MustParseAddr(plan.Network.DNSUpstream)
	manifest.Runners = []Runner{{RunnerID: runnerID, Placement: "same-host", PoolID: standardresources.PoolAMD64, SoftwareVersion: release.Version, ControlPlaneAddress: plan.Network.RunnerAddress, ControlPlaneServerName: "control-plane", IdentityDirectory: "/run/secondbox-runner-identity", IdentityHostDirectory: runnerIdentity, ArtifactHostDirectory: artifacts, StateHostDirectory: runnerStorage, WorkspaceHostDirectory: plan.Storage.WorkspacePath, LogPath: "/var/lib/secondbox-runner/state/logs/runner.jsonl", LogDirectory: "/var/lib/secondbox-runner/state/logs", FirecrackerPath: "/usr/local/bin/firecracker", FirecrackerJailerPath: "/usr/local/bin/jailer", FirecrackerJailRoot: "/var/lib/secondbox-runner/jail", FirecrackerJailerUIDStart: integer(plan.Network.JailerUIDRange.Start), FirecrackerJailerUIDCount: integer(plan.Network.JailerUIDRange.Count), FirecrackerJailerUIDAllowLow: boolean(false), FirecrackerJailerGID: integer(plan.Network.JailerUIDRange.Start), FirecrackerCgroupVersion: integer(2), FirecrackerCgroupParent: plan.Network.CgroupParent, FirecrackerKernelPath: "/opt/secondbox-artifacts/kernel", FirecrackerRootFSPath: "/opt/secondbox-artifacts/rootfs.ext4", FirecrackerSharedImagePath: "/opt/secondbox-artifacts/shared.img", FirecrackerKernelArgs: "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw quiet loglevel=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init", FirecrackerCPUTemplate: plan.Compute.FirecrackerCPUTemplate, FirecrackerRunDirectory: "/var/lib/secondbox-runner/state/run", FirecrackerLogDirectory: "/var/lib/secondbox-runner/state/firecracker-logs", FirecrackerAllowUnjailed: boolean(false), SnapshotTemplateCacheRoot: "/var/lib/secondbox-runner/state/snapshot-template-cache", ArtifactPublicKey: "/opt/secondbox-artifacts/signing.pub", ArtifactPublicKeySHA256: signingKeyID, WorkspaceRoot: "/var/lib/secondbox-runner/workspaces", StorageRecoveryPercent: integer(storageDeny - 10), StorageWarningPercent: integer(storageDeny - 5), StorageAdmissionDenyPercent: integer(storageDeny), SandboxMaxVCPUs: integer(maxVCPUs), SandboxMaxMemoryMiB: integer(maxMemoryMiB), SandboxMaxDiskMiB: integer(maxDiskMiB), SandboxMemoryBudgetMiB: integer(plan.Capacity.MaxMemoryBytes / (1 << 20)), SandboxGuestIP: guest.String(), SandboxBridgeName: "sbx0", SandboxBridgeCIDR: bridge.String() + "/" + fmt.Sprint(prefix.Bits()), SandboxGuestCIDR: prefix.String(), SandboxTapPrefix: plan.Network.TAPPrefix, SandboxNetworkStateDir: "/var/lib/secondbox-runner/state/network", SandboxDeleteBridge: boolean(true), NetworkPolicyNFTPath: "/usr/sbin/nft", NetworkPolicyMaxDNSPins: integer(256), NetworkPolicyMaxDNSTTL: "5m", NetworkPolicyRunnerAddresses: bridge.String(), NetworkPolicyManagementCIDRs: prefix.String(), NetworkPolicyRunnerGateways: strings.Join(gatewayEntries, ","), NetworkPolicyDNSUpstream: netip.AddrPortFrom(dnsUpstream, 53).String(), MaxConcurrentPerSandbox: integer(4), MaxConcurrentGlobal: integer(plan.Capacity.MaxSandboxes), MaxConcurrentStarts: integer(plan.Capacity.ConcurrentStarts), MaxConcurrentWorkspaceCreates: integer(plan.Capacity.ConcurrentStarts), MaxConcurrentOperationsGlobal: integer(plan.Capacity.ConcurrentOperations), FileTransferMaxBytes: integer(1 << 30), GuestControlVSockPort: integer(1024), GuestProtocolVSockPort: integer(1025), GuestHeartbeatInterval: "5s", DataPlaneListenAddress: "127.0.0.1:" + fmt.Sprint(dataPort), DataPlaneAdvertisedAddress: plan.Network.DataPlaneAddress}}
	return manifest, nil
}

func componentAsset(component releasecontract.SignedComponent, keyID string, protocol uint32) assetcatalog.SignedAsset {
	return assetcatalog.SignedAsset{ArtifactID: component.ArtifactID, ManifestDigest: component.ManifestDigest, SignatureKeyID: keyID, Architecture: standardresources.ArchitectureAMD64, GuestProtocolGeneration: protocol, MandatoryGuestFeatures: slices.Clone(component.MandatoryGuestFeatures)}
}

func installPath(plan install.InstallPlan, name string) string {
	for _, path := range plan.Paths {
		if path.Name == name {
			return path.Path
		}
	}
	return ""
}

func relativeTo(base, path string) string {
	relative, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return relative
}

func splitPlanAddress(address string) (string, int64, error) {
	parsed, err := netip.ParseAddrPort(address)
	if err != nil {
		return "", 0, manifestError("single-host service address", err)
	}
	return parsed.Addr().String(), int64(parsed.Port()), nil
}

func releaseBinary(release releasecontract.ArtifactManifest, name string) (releasecontract.BinaryArtifact, bool) {
	for _, binary := range release.Binaries {
		if binary.Name == name && binary.Platform == "linux/amd64" {
			return binary, true
		}
	}
	return releasecontract.BinaryArtifact{}, false
}
