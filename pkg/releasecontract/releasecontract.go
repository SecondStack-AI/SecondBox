// Package releasecontract defines the public, provider-neutral identity of a
// SecondBox release.
package releasecontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	ArtifactManifestSchema = "secondbox.release/artifact-manifest/v6"
	// LegacyArtifactManifestSchema is accepted for recorded releases that
	// predate the gVisor artifact section; a newer updater still
	// authenticates them.
	LegacyArtifactManifestSchema               = "secondbox.release/artifact-manifest/v5"
	QualificationEvidenceSchema                = "secondbox.release/qualification-evidence/v2"
	LegacyQualificationEvidenceSchema          = "secondbox.release/qualification-evidence/v1"
	InstallerQualificationEvidenceSchema       = "secondbox.release/installer-qualification-evidence/v2"
	LegacyInstallerQualificationEvidenceSchema = "secondbox.release/installer-qualification-evidence/v1"

	TypeScriptPackage   = "@secondstack-ai/secondbox"
	GoModule            = "github.com/SecondStack-AI/SecondBox"
	ControlPlaneImage   = "ghcr.io/secondstack-ai/secondbox/control-plane"
	RunnerImage         = "ghcr.io/secondstack-ai/secondbox/runner"
	MicroVMImage        = "ghcr.io/secondstack-ai/secondbox/microvm-artifacts"
	InstallerToolsImage = "ghcr.io/secondstack-ai/secondbox/installer-tools"
	GVisorRunnerImage   = "ghcr.io/secondstack-ai/secondbox/runner-gvisor"
	GVisorImage         = "ghcr.io/secondstack-ai/secondbox/gvisor-artifacts"
)

var (
	versionPattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)
	commitPattern   = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	checksumPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	platformPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9][a-z0-9._-]*$`)
	keyPattern      = regexp.MustCompile(`^SHA256:[0-9A-F]{64}$`)
	numericPattern  = regexp.MustCompile(`^[0-9]+$`)
)

// Identity is repeated on every independently inspectable release object.
type Identity struct {
	Version      string `json:"version"`
	Tag          string `json:"tag"`
	SourceCommit string `json:"sourceCommit"`
}

type ProtocolWindow struct {
	Minimum uint32 `json:"minimum"`
	Maximum uint32 `json:"maximum"`
}

type PlatformMatrix struct {
	HostBinaries         []string `json:"hostBinaries"`
	ControlPlane         []string `json:"controlPlane"`
	Runner               []string `json:"runner"`
	InstallerTools       []string `json:"installerTools"`
	Guest                []string `json:"guest"`
	QualifiedRunnerGuest []string `json:"qualifiedRunnerGuest"`
}

// Reference identifies immutable bytes at a public HTTPS location.
type Reference struct {
	Location string `json:"location"`
	Digest   string `json:"digest"`
}

type QualificationDeviceEvidence struct {
	Path     string `json:"path"`
	Present  bool   `json:"present"`
	Readable bool   `json:"readable"`
	Writable bool   `json:"writable"`
}

type QualificationFilesystemEvidence struct {
	Mount string `json:"mount"`
	Type  string `json:"type"`
}

type QualificationHostEvidence struct {
	Platform            string                          `json:"platform"`
	KVM                 QualificationDeviceEvidence     `json:"kvm"`
	TUN                 QualificationDeviceEvidence     `json:"tun"`
	WorkspaceFilesystem QualificationFilesystemEvidence `json:"workspaceFilesystem"`
}

type QualificationEvidence struct {
	SchemaVersion    string                    `json:"schemaVersion"`
	SourceCommit     string                    `json:"sourceCommit"`
	RepositoryDirty  bool                      `json:"repositoryDirty"`
	Suite            string                    `json:"suite"`
	PassCount        int64                     `json:"passCount"`
	WallClockSeconds int64                     `json:"wallClockSeconds"`
	Host             QualificationHostEvidence `json:"host"`
	QualifiedAt      string                    `json:"qualifiedAt"`
}

type InstallerQualificationEvidence struct {
	SchemaVersion         string                    `json:"schemaVersion"`
	SourceCommit          string                    `json:"sourceCommit"`
	RepositoryDirty       bool                      `json:"repositoryDirty"`
	Suite                 string                    `json:"suite"`
	PassCount             int64                     `json:"passCount"`
	WallClockSeconds      int64                     `json:"wallClockSeconds"`
	Host                  QualificationHostEvidence `json:"host"`
	ReleaseManifestDigest string                    `json:"releaseManifestDigest"`
	FilesystemIdentity    string                    `json:"filesystemIdentity"`
	RebootPassed          bool                      `json:"rebootPassed"`
	QualifiedAt           string                    `json:"qualifiedAt"`
}

type OpenAPIArtifact struct {
	Identity Identity `json:"identity"`
	Reference
}

type SDKArtifact struct {
	Identity   Identity  `json:"identity"`
	Coordinate string    `json:"coordinate"`
	Package    Reference `json:"package"`
}

type OCIArtifact struct {
	Identity  Identity `json:"identity"`
	Reference string   `json:"reference"`
}

type BinaryArtifact struct {
	Identity Identity `json:"identity"`
	Name     string   `json:"name"`
	Platform string   `json:"platform"`
	Location string   `json:"location"`
	SHA256   string   `json:"sha256"`
}

type StandardProfileIdentity struct {
	Name       string `json:"name"`
	Revision   int64  `json:"revision"`
	SpecDigest string `json:"specDigest"`
}

type StandardBundleArtifact struct {
	Identity Identity                  `json:"identity"`
	Name     string                    `json:"name"`
	Document Reference                 `json:"document"`
	Profiles []StandardProfileIdentity `json:"profiles"`
}

// SignedComponent is one independently selected component bound by the signed
// top-level microVM manifest.
type SignedComponent struct {
	ArtifactID             string   `json:"artifactId"`
	ManifestDigest         string   `json:"manifestDigest"`
	MandatoryGuestFeatures []string `json:"mandatoryGuestFeatures"`
}

type MicroVMArtifact struct {
	Identity              Identity        `json:"identity"`
	ImageReference        string          `json:"imageReference"`
	SignedManifestDigest  string          `json:"signedManifestDigest"`
	SigningKeyFingerprint string          `json:"signingKeyFingerprint"`
	RuntimeBundle         SignedComponent `json:"runtimeBundle"`
	ToolchainBundle       SignedComponent `json:"toolchainBundle"`
}

// GVisorArtifact names the gVisor backend distribution: the runner image and
// the artifact transport carrying the prepared flat root, the launch
// artifacts, and the backend materialization whose canonical digest runners
// and platform operators pin. The materialization is also a release file.
type GVisorArtifact struct {
	Identity                 Identity  `json:"identity"`
	RunnerReference          string    `json:"runnerReference"`
	ImageReference           string    `json:"imageReference"`
	Materialization          Reference `json:"materialization"`
	MaterializationDigest    string    `json:"materializationDigest"`
	FlatRootDigest           string    `json:"flatRootDigest"`
	RunscRelease             string    `json:"runscRelease"`
	QualificationEvidence    Reference `json:"qualificationEvidence"`
	PodQualificationEvidence Reference `json:"podQualificationEvidence"`
}

// GVisorMaterialization mirrors the runner's backend materialization document
// field for field, so its canonical digest here equals the runner's.
type GVisorMaterialization struct {
	SchemaVersion           string                   `json:"schemaVersion"`
	Key                     GVisorMaterializationKey `json:"key"`
	SourceOCIManifestDigest string                   `json:"sourceOciManifestDigest,omitempty"`
	FlatRootDigest          string                   `json:"flatRootDigest,omitempty"`
	LaunchArtifacts         []GVisorLaunchArtifact   `json:"launchArtifacts"`
	AgentProtocolGeneration uint32                   `json:"agentProtocolGeneration"`
	AgentFeatures           []string                 `json:"agentFeatures"`
	BackendBuildID          string                   `json:"backendBuildId"`
	HelperBuildID           string                   `json:"helperBuildId,omitempty"`
}

type GVisorMaterializationKey struct {
	BackendKind             string `json:"backendKind"`
	GuestArchitecture       string `json:"guestArchitecture"`
	RuntimeManifestDigest   string `json:"runtimeManifestDigest"`
	ToolchainManifestDigest string `json:"toolchainManifestDigest"`
}

type GVisorLaunchArtifact struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

// GVisorMaterializationSchema is the runner's backend materialization schema.
const GVisorMaterializationSchema = "secondbox.runner/backend-materialization/v1"

// DecodeGVisorMaterialization strictly decodes a published materialization.
func DecodeGVisorMaterialization(data []byte) (GVisorMaterialization, error) {
	var materialization GVisorMaterialization
	if err := decodeStrict(data, &materialization); err != nil {
		return GVisorMaterialization{}, contractError("decode gVisor materialization: %v", err)
	}
	if err := materialization.Validate(); err != nil {
		return GVisorMaterialization{}, err
	}
	return materialization, nil
}

// Validate applies the runner's materialization invariants for the gVisor
// backend, so a release cannot publish a document a runner would refuse.
func (materialization GVisorMaterialization) Validate() error {
	if materialization.SchemaVersion != GVisorMaterializationSchema || materialization.Key.BackendKind != "gvisor" {
		return contractError("gVisor materialization must be a %q gvisor document", GVisorMaterializationSchema)
	}
	// The published gVisor runner is linux/amd64 only, and its startup
	// requires the runsc and guest-agent launch artifacts.
	if materialization.Key.GuestArchitecture != "amd64" {
		return contractError("gVisor materialization guest architecture must be amd64")
	}
	if !digestPattern.MatchString(materialization.Key.RuntimeManifestDigest) || !digestPattern.MatchString(materialization.Key.ToolchainManifestDigest) ||
		materialization.Key.RuntimeManifestDigest == materialization.Key.ToolchainManifestDigest {
		return contractError("gVisor materialization runtime and toolchain digests are invalid")
	}
	if !digestPattern.MatchString(materialization.SourceOCIManifestDigest) || !digestPattern.MatchString(materialization.FlatRootDigest) {
		return contractError("gVisor materialization requires digest-pinned source OCI and flat root identity")
	}
	if strings.TrimSpace(materialization.HelperBuildID) == "" || strings.TrimSpace(materialization.BackendBuildID) == "" ||
		materialization.AgentProtocolGeneration == 0 || len(materialization.LaunchArtifacts) == 0 {
		return contractError("gVisor materialization build, agent, and launch identity is incomplete")
	}
	seen := map[string]bool{}
	for index, artifact := range materialization.LaunchArtifacts {
		if strings.TrimSpace(artifact.ID) == "" || !digestPattern.MatchString(artifact.SHA256) || seen[artifact.ID] {
			return contractError("gVisor materialization launch artifact %d is incomplete or duplicated", index)
		}
		if index > 0 && materialization.LaunchArtifacts[index-1].ID >= artifact.ID {
			return contractError("gVisor materialization launch artifacts must be sorted")
		}
		seen[artifact.ID] = true
	}
	for index, feature := range materialization.AgentFeatures {
		if feature == "" || (index > 0 && materialization.AgentFeatures[index-1] >= feature) {
			return contractError("gVisor materialization agent features must be sorted, unique, and non-empty")
		}
	}
	if !seen["runsc"] || !seen["guest-agent"] {
		return contractError("gVisor materialization must pin the runsc and guest-agent launch artifacts")
	}
	return nil
}

// Digest is the canonical digest runners pin: sha256 over the compact JSON
// encoding of the validated typed document.
func (materialization GVisorMaterialization) Digest() (string, error) {
	if err := materialization.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(materialization)
	if err != nil {
		return "", contractError("encode gVisor materialization: %v", err)
	}
	return Digest(encoded), nil
}

// VerifyGVisorMaterialization checks that a published materialization agrees
// with the identities the artifact manifest records for it.
func (artifact GVisorArtifact) VerifyGVisorMaterialization(data []byte) error {
	materialization, err := DecodeGVisorMaterialization(data)
	if err != nil {
		return err
	}
	digest, err := materialization.Digest()
	if err != nil {
		return err
	}
	if digest != artifact.MaterializationDigest {
		return contractError("gVisor materialization canonical digest differs from the artifact manifest")
	}
	if materialization.FlatRootDigest != artifact.FlatRootDigest {
		return contractError("gVisor materialization flat-root digest differs from the artifact manifest")
	}
	if materialization.HelperBuildID != "runsc-release-"+artifact.RunscRelease {
		return contractError("gVisor materialization helper build differs from the artifact manifest runsc release")
	}
	return nil
}

// GVisorQualificationEvidence is the scenario evidence of a gVisor host or
// pod run: the same schema as the Firecracker evidence, on a host without
// KVM and naming its backend.
type GVisorQualificationEvidence struct {
	SchemaVersion    string                          `json:"schemaVersion"`
	SourceCommit     string                          `json:"sourceCommit"`
	RepositoryDirty  bool                            `json:"repositoryDirty"`
	Suite            string                          `json:"suite"`
	Backend          string                          `json:"backend"`
	PassCount        int64                           `json:"passCount"`
	WallClockSeconds int64                           `json:"wallClockSeconds"`
	Host             GVisorQualificationHostEvidence `json:"host"`
	QualifiedAt      string                          `json:"qualifiedAt"`
}

type GVisorQualificationHostEvidence struct {
	Platform            string                            `json:"platform"`
	KVM                 GVisorQualificationDeviceEvidence `json:"kvm"`
	TUN                 GVisorQualificationDeviceEvidence `json:"tun"`
	WorkspaceFilesystem QualificationFilesystemEvidence   `json:"workspaceFilesystem"`
}

type GVisorQualificationDeviceEvidence struct {
	Required bool `json:"required"`
	Present  bool `json:"present"`
}

// GVisorQualificationSuite names the evidence suite of a gVisor run.
func GVisorQualificationSuite(pod bool) string {
	if pod {
		return "test-scenario-gvisor-pod"
	}
	return "test-scenario-gvisor"
}

func DecodeGVisorQualificationEvidence(data []byte, pod bool) (GVisorQualificationEvidence, error) {
	var evidence GVisorQualificationEvidence
	if err := decodeStrict(data, &evidence); err != nil {
		return GVisorQualificationEvidence{}, contractError("decode gVisor qualification evidence: %v", err)
	}
	if err := evidence.Validate(pod); err != nil {
		return GVisorQualificationEvidence{}, err
	}
	return evidence, nil
}

func (evidence GVisorQualificationEvidence) Validate(pod bool) error {
	if evidence.SchemaVersion != QualificationEvidenceSchema {
		return contractError("gVisor qualification evidence schemaVersion must be %q", QualificationEvidenceSchema)
	}
	if !commitPattern.MatchString(evidence.SourceCommit) {
		return contractError("gVisor qualification evidence source commit must be a full lowercase Git object ID")
	}
	if evidence.Suite != GVisorQualificationSuite(pod) || evidence.Backend != "gvisor" || evidence.PassCount <= 0 || evidence.WallClockSeconds < 0 {
		return contractError("gVisor qualification evidence must describe a complete %s run", GVisorQualificationSuite(pod))
	}
	if evidence.Host.Platform != "linux-amd64" || evidence.Host.KVM.Present || evidence.Host.KVM.Required {
		return contractError("gVisor qualification evidence must come from a linux-amd64 host without KVM")
	}
	if strings.TrimSpace(evidence.Host.WorkspaceFilesystem.Mount) == "" ||
		(evidence.Host.WorkspaceFilesystem.Type != "xfs" && evidence.Host.WorkspaceFilesystem.Type != "btrfs") {
		return contractError("gVisor qualification evidence workspace filesystem facts are incomplete")
	}
	qualifiedAt, err := time.Parse(time.RFC3339, evidence.QualifiedAt)
	if err != nil || evidence.QualifiedAt != qualifiedAt.UTC().Format("2006-01-02T15:04:05Z") {
		return contractError("gVisor qualification evidence qualifiedAt must be a canonical UTC timestamp")
	}
	return nil
}

func (evidence GVisorQualificationEvidence) ValidateForRelease(sourceCommit string, pod bool) error {
	if err := evidence.Validate(pod); err != nil {
		return err
	}
	if evidence.SourceCommit != sourceCommit {
		return contractError("gVisor qualification evidence source commit does not match release")
	}
	if evidence.RepositoryDirty {
		return contractError("gVisor qualification evidence was produced from a dirty repository")
	}
	return nil
}

type BundledServiceImages struct {
	Postgres string `json:"postgres"`
}

// ArtifactManifest contains immutable release artifact identity.
type ArtifactManifest struct {
	SchemaVersion string `json:"schemaVersion"`
	Candidate     bool   `json:"candidate,omitempty"`
	Identity
	OpenAPI                        OpenAPIArtifact          `json:"openapi"`
	RunnerProtocol                 ProtocolWindow           `json:"runnerProtocol"`
	GuestProtocol                  ProtocolWindow           `json:"guestProtocol"`
	Platforms                      PlatformMatrix           `json:"platforms"`
	GoSDK                          SDKArtifact              `json:"goSdk"`
	TypeScriptSDK                  SDKArtifact              `json:"typeScriptSdk"`
	ControlPlane                   OCIArtifact              `json:"controlPlane"`
	Runner                         OCIArtifact              `json:"runner"`
	InstallerTools                 OCIArtifact              `json:"installerTools"`
	BundledServices                BundledServiceImages     `json:"bundledServices"`
	InstallBootstrap               Reference                `json:"installBootstrap"`
	MicroVM                        MicroVMArtifact          `json:"microvm"`
	GVisor                         *GVisorArtifact          `json:"gvisor,omitempty"`
	Binaries                       []BinaryArtifact         `json:"binaries"`
	SBOMs                          []Reference              `json:"sboms"`
	ArtifactAttestations           []Reference              `json:"artifactAttestations,omitempty"`
	SourceFreeSuite                Reference                `json:"sourceFreeSuite,omitempty"`
	QualificationEvidence          Reference                `json:"qualificationEvidence"`
	InstallerQualificationEvidence Reference                `json:"installerQualificationEvidence"`
	StandardBundles                []StandardBundleArtifact `json:"standardBundles"`
}

func ParseTag(tag string) (string, error) {
	if !strings.HasPrefix(tag, "v") || !versionPattern.MatchString(strings.TrimPrefix(tag, "v")) {
		return "", contractError("tag %q must be vMAJOR.MINOR.PATCH with optional SemVer prerelease identifiers and no build metadata", tag)
	}
	return strings.TrimPrefix(tag, "v"), nil
}

// CompareVersions orders two canonical release versions according to SemVer
// precedence. It rejects build metadata because release tags do not admit it.
func CompareVersions(left, right string) (int, error) {
	if !versionPattern.MatchString(left) || !versionPattern.MatchString(right) {
		return 0, contractError("versions %q and %q must be canonical SemVer", left, right)
	}
	leftCore, leftPre, _ := strings.Cut(left, "-")
	rightCore, rightPre, _ := strings.Cut(right, "-")
	leftParts, rightParts := strings.Split(leftCore, "."), strings.Split(rightCore, ".")
	for index := 0; index < 3; index++ {
		leftNumber, rightNumber := new(big.Int), new(big.Int)
		leftNumber.SetString(leftParts[index], 10)
		rightNumber.SetString(rightParts[index], 10)
		if comparison := leftNumber.Cmp(rightNumber); comparison != 0 {
			return comparison, nil
		}
	}
	if leftPre == "" && rightPre == "" {
		return 0, nil
	}
	if leftPre == "" {
		return 1, nil
	}
	if rightPre == "" {
		return -1, nil
	}
	leftIdentifiers, rightIdentifiers := strings.Split(leftPre, "."), strings.Split(rightPre, ".")
	for index := 0; index < min(len(leftIdentifiers), len(rightIdentifiers)); index++ {
		leftID, rightID := leftIdentifiers[index], rightIdentifiers[index]
		leftNumber, leftNumeric := new(big.Int), numericPattern.MatchString(leftID)
		rightNumber, rightNumeric := new(big.Int), numericPattern.MatchString(rightID)
		if leftNumeric && rightNumeric {
			leftNumber.SetString(leftID, 10)
			rightNumber.SetString(rightID, 10)
			if comparison := leftNumber.Cmp(rightNumber); comparison != 0 {
				return comparison, nil
			}
			continue
		}
		if leftNumeric != rightNumeric {
			if leftNumeric {
				return -1, nil
			}
			return 1, nil
		}
		if comparison := strings.Compare(leftID, rightID); comparison != 0 {
			return comparison, nil
		}
	}
	return len(leftIdentifiers) - len(rightIdentifiers), nil
}

func ArtifactManifestLocation(version string) string {
	return fmt.Sprintf("https://github.com/SecondStack-AI/SecondBox/releases/download/v%s/secondbox-%s-artifact-manifest.json", version, version)
}

func SourceFreeSuiteLocation(version string) string {
	return fmt.Sprintf("https://github.com/SecondStack-AI/SecondBox/releases/download/v%s/secondbox-%s-source-free-qualify", version, version)
}

func QualificationEvidenceLocation(version string) string {
	return fmt.Sprintf("https://github.com/SecondStack-AI/SecondBox/releases/download/v%s/secondbox-%s-qualification-evidence.json", version, version)
}

func InstallerQualificationEvidenceLocation(version string) string {
	return fmt.Sprintf("https://github.com/SecondStack-AI/SecondBox/releases/download/v%s/secondbox-%s-installer-qualification-evidence.json", version, version)
}

func InstallBootstrapLocation(version string) string {
	return fmt.Sprintf("https://github.com/SecondStack-AI/SecondBox/releases/download/v%s/install.sh", version)
}

// GVisorMaterializationLocation is the canonical release file carrying the
// gVisor backend materialization.
func GVisorMaterializationLocation(version string) string {
	return fmt.Sprintf("https://github.com/SecondStack-AI/SecondBox/releases/download/v%s/secondbox-%s-gvisor-materialization.json", version, version)
}

// GVisorQualificationEvidenceLocation is the canonical release file carrying
// the gVisor host (or pod) scenario evidence.
func GVisorQualificationEvidenceLocation(version string, pod bool) string {
	name := "gvisor"
	if pod {
		name = "gvisor-pod"
	}
	return fmt.Sprintf("https://github.com/SecondStack-AI/SecondBox/releases/download/v%s/secondbox-%s-%s-qualification-evidence.json", version, version, name)
}

func BinaryLocation(version, name, platform string) string {
	return fmt.Sprintf("https://github.com/SecondStack-AI/SecondBox/releases/download/v%s/%s_%s_%s", version, name, version, strings.ReplaceAll(platform, "/", "_"))
}

func DecodeArtifactManifest(data []byte) (ArtifactManifest, error) {
	var manifest ArtifactManifest
	if err := decodeStrict(data, &manifest); err != nil {
		return ArtifactManifest{}, contractError("decode artifact manifest: %v", err)
	}
	if err := manifest.Validate(); err != nil {
		return ArtifactManifest{}, err
	}
	return manifest, nil
}

func DecodeQualificationEvidence(data []byte) (QualificationEvidence, error) {
	var evidence QualificationEvidence
	if err := decodeStrict(data, &evidence); err != nil {
		return QualificationEvidence{}, contractError("decode qualification evidence: %v", err)
	}
	if err := evidence.Validate(); err != nil {
		return QualificationEvidence{}, err
	}
	return evidence, nil
}

func DecodeInstallerQualificationEvidence(data []byte) (InstallerQualificationEvidence, error) {
	var evidence InstallerQualificationEvidence
	if err := decodeStrict(data, &evidence); err != nil {
		return InstallerQualificationEvidence{}, contractError("decode installer qualification evidence: %v", err)
	}
	if err := evidence.Validate(); err != nil {
		return InstallerQualificationEvidence{}, err
	}
	return evidence, nil
}

func (evidence QualificationEvidence) Validate() error {
	if evidence.SchemaVersion != QualificationEvidenceSchema && evidence.SchemaVersion != LegacyQualificationEvidenceSchema {
		return contractError("qualification evidence schemaVersion must be %q or legacy %q", QualificationEvidenceSchema, LegacyQualificationEvidenceSchema)
	}
	if !commitPattern.MatchString(evidence.SourceCommit) {
		return contractError("qualification evidence source commit must be a full lowercase Git object ID")
	}
	if evidence.Suite != "test-scenario" || evidence.PassCount <= 0 || evidence.WallClockSeconds < 0 {
		return contractError("qualification evidence must describe a complete test-scenario run")
	}
	if err := validateQualificationHost("qualification", evidence.Host, evidence.SchemaVersion == QualificationEvidenceSchema); err != nil {
		return err
	}
	qualifiedAt, err := time.Parse(time.RFC3339, evidence.QualifiedAt)
	if err != nil || evidence.QualifiedAt != qualifiedAt.UTC().Format("2006-01-02T15:04:05Z") {
		return contractError("qualification evidence qualifiedAt must be a canonical UTC timestamp")
	}
	return nil
}

func validateQualificationHost(label string, host QualificationHostEvidence, requirePlatform bool) error {
	if (requirePlatform && host.Platform != "linux-amd64") || (!requirePlatform && host.Platform != "" && host.Platform != "linux-amd64") {
		return contractError("%s evidence host platform must be linux-amd64", label)
	}
	for name, device := range map[string]QualificationDeviceEvidence{"KVM": host.KVM, "TUN": host.TUN} {
		wantPath := "/dev/" + strings.ToLower(name)
		if name == "TUN" {
			wantPath = "/dev/net/tun"
		}
		if device.Path != wantPath || !device.Present || !device.Readable || !device.Writable {
			return contractError("%s evidence %s device facts are incomplete", label, name)
		}
	}
	if strings.TrimSpace(host.WorkspaceFilesystem.Mount) == "" ||
		(host.WorkspaceFilesystem.Type != "xfs" && host.WorkspaceFilesystem.Type != "btrfs") {
		return contractError("%s evidence workspace filesystem facts are incomplete", label)
	}
	return nil
}

func (evidence QualificationEvidence) ValidateForRelease(sourceCommit string) error {
	if err := evidence.Validate(); err != nil {
		return err
	}
	if evidence.SourceCommit != sourceCommit {
		return contractError("qualification evidence source commit does not match release")
	}
	if evidence.RepositoryDirty {
		return contractError("qualification evidence was produced from a dirty repository")
	}
	return nil
}

func (evidence InstallerQualificationEvidence) Validate() error {
	if evidence.SchemaVersion != InstallerQualificationEvidenceSchema && evidence.SchemaVersion != LegacyInstallerQualificationEvidenceSchema {
		return contractError("installer qualification evidence schemaVersion must be %q or legacy %q", InstallerQualificationEvidenceSchema, LegacyInstallerQualificationEvidenceSchema)
	}
	if !commitPattern.MatchString(evidence.SourceCommit) || evidence.Suite != "test-installer-qualified" || evidence.PassCount <= 0 || evidence.WallClockSeconds < 0 || !digestPattern.MatchString(evidence.ReleaseManifestDigest) || strings.TrimSpace(evidence.FilesystemIdentity) == "" || !evidence.RebootPassed {
		return contractError("installer qualification evidence must describe a complete qualified installer run")
	}
	if err := validateQualificationHost("installer qualification", evidence.Host, evidence.SchemaVersion == InstallerQualificationEvidenceSchema); err != nil {
		return err
	}
	qualifiedAt, err := time.Parse(time.RFC3339, evidence.QualifiedAt)
	if err != nil || evidence.QualifiedAt != qualifiedAt.UTC().Format("2006-01-02T15:04:05Z") {
		return contractError("installer qualification evidence qualifiedAt must be a canonical UTC timestamp")
	}
	return nil
}

func (evidence InstallerQualificationEvidence) ValidateForRelease(sourceCommit, qualificationSubjectDigest string) error {
	if err := evidence.Validate(); err != nil {
		return err
	}
	if evidence.SourceCommit != sourceCommit {
		return contractError("installer qualification evidence source commit does not match release")
	}
	if evidence.RepositoryDirty {
		return contractError("installer qualification evidence was produced from a dirty repository")
	}
	if !digestPattern.MatchString(qualificationSubjectDigest) || evidence.ReleaseManifestDigest != qualificationSubjectDigest {
		return contractError("installer qualification evidence release identity does not match release")
	}
	return nil
}

// InstallerQualificationSubjectDigest identifies all public release contract
// fields except the installer-evidence reference itself. Omitting that one
// reference avoids a digest cycle while still binding qualification to the
// exact binaries, images, protocols, bundles, and other immutable release
// objects that the final manifest publishes.
func (manifest ArtifactManifest) InstallerQualificationSubjectDigest() (string, error) {
	subject := manifest
	subject.Candidate = false
	subject.InstallerQualificationEvidence = Reference{}
	encoded, err := json.Marshal(subject)
	if err != nil {
		return "", contractError("encode installer qualification subject: %v", err)
	}
	return Digest(encoded), nil
}

func (manifest ArtifactManifest) Validate() error {
	if manifest.SchemaVersion != ArtifactManifestSchema && manifest.SchemaVersion != LegacyArtifactManifestSchema {
		return contractError("artifact manifest schemaVersion must be %q or legacy %q", ArtifactManifestSchema, LegacyArtifactManifestSchema)
	}
	if manifest.SchemaVersion == LegacyArtifactManifestSchema {
		// The legacy schema is only what pre-gVisor releases recorded; a
		// release that must distribute gVisor cannot opt out through it.
		if manifest.GVisor != nil {
			return contractError("legacy artifact manifest must not carry a gVisor artifact")
		}
		if order, err := CompareVersions(manifest.Version, "0.9.0"); err == nil && order >= 0 {
			return contractError("artifact manifest %s must use schemaVersion %q", manifest.Tag, ArtifactManifestSchema)
		}
	} else if manifest.GVisor == nil {
		return contractError("artifact manifest requires the gVisor artifact")
	}
	if err := validateIdentity(manifest.Identity); err != nil {
		return err
	}
	for name, identity := range map[string]Identity{
		"OpenAPI": manifest.OpenAPI.Identity, "Go SDK": manifest.GoSDK.Identity,
		"TypeScript SDK": manifest.TypeScriptSDK.Identity, "control plane": manifest.ControlPlane.Identity,
		"Runner": manifest.Runner.Identity, "installer tools": manifest.InstallerTools.Identity, "microVM": manifest.MicroVM.Identity,
	} {
		if identity != manifest.Identity {
			return contractError("%s identity does not match artifact manifest", name)
		}
	}
	for _, binary := range manifest.Binaries {
		if binary.Identity != manifest.Identity {
			return contractError("binary %q identity does not match artifact manifest", binary.Name)
		}
	}
	for _, bundle := range manifest.StandardBundles {
		if bundle.Identity != manifest.Identity {
			return contractError("standard bundle %q identity does not match artifact manifest", bundle.Name)
		}
	}
	if err := validateWindow("Runner", manifest.RunnerProtocol); err != nil {
		return err
	}
	if err := validateWindow("guest", manifest.GuestProtocol); err != nil {
		return err
	}
	if err := validatePlatforms(manifest.Platforms); err != nil {
		return err
	}
	if err := validateReference("OpenAPI", manifest.OpenAPI.Reference); err != nil {
		return err
	}
	if manifest.GoSDK.Coordinate != GoModule+"@"+manifest.Tag {
		return contractError("Go SDK coordinate must be %s@%s", GoModule, manifest.Tag)
	}
	if manifest.TypeScriptSDK.Coordinate != TypeScriptPackage+"@"+manifest.Version {
		return contractError("TypeScript SDK coordinate must be %s@%s", TypeScriptPackage, manifest.Version)
	}
	if err := validateReference("Go SDK package", manifest.GoSDK.Package); err != nil {
		return err
	}
	if err := validateReference("TypeScript SDK package", manifest.TypeScriptSDK.Package); err != nil {
		return err
	}
	for name, artifact := range map[string]OCIArtifact{
		ControlPlaneImage: manifest.ControlPlane, RunnerImage: manifest.Runner, InstallerToolsImage: manifest.InstallerTools,
	} {
		if err := validateOCIReference(name, artifact.Reference); err != nil {
			return err
		}
	}
	for name, reference := range map[string]string{"bundled Postgres": manifest.BundledServices.Postgres} {
		repository, _, found := strings.Cut(reference, "@")
		if !found || repository == "" {
			return contractError("%s image must be digest-pinned", name)
		}
		if err := validateOCIReference(repository, reference); err != nil {
			return err
		}
	}
	if err := validateOCIReference(MicroVMImage, manifest.MicroVM.ImageReference); err != nil {
		return err
	}
	if !digestPattern.MatchString(manifest.MicroVM.SignedManifestDigest) {
		return contractError("microVM signed manifest digest must be canonical sha256")
	}
	if !keyPattern.MatchString(manifest.MicroVM.SigningKeyFingerprint) {
		return contractError("microVM signing key fingerprint must be SHA256 followed by 64 uppercase hexadecimal characters")
	}
	if err := validateSignedComponent("microVM runtime bundle", manifest.MicroVM.RuntimeBundle); err != nil {
		return err
	}
	if err := validateSignedComponent("microVM toolchain bundle", manifest.MicroVM.ToolchainBundle); err != nil {
		return err
	}
	if manifest.MicroVM.RuntimeBundle.ArtifactID == manifest.MicroVM.ToolchainBundle.ArtifactID ||
		manifest.MicroVM.RuntimeBundle.ManifestDigest == manifest.MicroVM.ToolchainBundle.ManifestDigest {
		return contractError("microVM runtime and toolchain components must have distinct identities")
	}
	if manifest.MicroVM.SignedManifestDigest == manifest.MicroVM.RuntimeBundle.ManifestDigest ||
		manifest.MicroVM.SignedManifestDigest == manifest.MicroVM.ToolchainBundle.ManifestDigest {
		return contractError("microVM signed manifest and component digests must be distinct")
	}
	if manifest.GVisor != nil {
		if manifest.GVisor.Identity != manifest.Identity {
			return contractError("gVisor identity does not match artifact manifest")
		}
		if err := validateOCIReference(GVisorRunnerImage, manifest.GVisor.RunnerReference); err != nil {
			return err
		}
		if err := validateOCIReference(GVisorImage, manifest.GVisor.ImageReference); err != nil {
			return err
		}
		if err := validateReference("gVisor materialization", manifest.GVisor.Materialization); err != nil {
			return err
		}
		if manifest.GVisor.Materialization.Location != GVisorMaterializationLocation(manifest.Version) {
			return contractError("gVisor materialization location is not canonical for %s", manifest.Tag)
		}
		if !digestPattern.MatchString(manifest.GVisor.MaterializationDigest) || !digestPattern.MatchString(manifest.GVisor.FlatRootDigest) {
			return contractError("gVisor materialization and flat root digests must be canonical sha256")
		}
		if manifest.GVisor.RunscRelease == "" {
			return contractError("gVisor artifact requires the runsc release")
		}
		for name, evidence := range map[string]struct {
			reference Reference
			location  string
		}{
			"gVisor qualification evidence":     {manifest.GVisor.QualificationEvidence, GVisorQualificationEvidenceLocation(manifest.Version, false)},
			"gVisor pod qualification evidence": {manifest.GVisor.PodQualificationEvidence, GVisorQualificationEvidenceLocation(manifest.Version, true)},
		} {
			if err := validateReference(name, evidence.reference); err != nil {
				return err
			}
			if evidence.reference.Location != evidence.location {
				return contractError("%s location is not canonical for %s", name, manifest.Tag)
			}
		}
	}
	if err := validateBinaries(manifest); err != nil {
		return err
	}
	if len(manifest.SBOMs) == 0 {
		return contractError("artifact manifest requires an SBOM reference")
	}
	for index, ref := range append(slices.Clone(manifest.SBOMs), manifest.ArtifactAttestations...) {
		if err := validateReference(fmt.Sprintf("evidence reference %d", index), ref); err != nil {
			return err
		}
	}
	if manifest.SourceFreeSuite != (Reference{}) {
		if err := validateReference("source-free qualification suite", manifest.SourceFreeSuite); err != nil {
			return err
		}
		if manifest.SourceFreeSuite.Location != SourceFreeSuiteLocation(manifest.Version) {
			return contractError("source-free qualification suite location is not canonical for %s", manifest.Tag)
		}
	}
	if err := validateReference("qualification evidence", manifest.QualificationEvidence); err != nil {
		return err
	}
	if manifest.QualificationEvidence.Location != QualificationEvidenceLocation(manifest.Version) {
		return contractError("qualification evidence location is not canonical for %s", manifest.Tag)
	}
	if manifest.Candidate {
		if manifest.InstallerQualificationEvidence != (Reference{}) {
			return contractError("release candidate must not claim installer qualification evidence")
		}
	} else {
		if err := validateReference("installer qualification evidence", manifest.InstallerQualificationEvidence); err != nil {
			return err
		}
		if manifest.InstallerQualificationEvidence.Location != InstallerQualificationEvidenceLocation(manifest.Version) {
			return contractError("installer qualification evidence location is not canonical for %s", manifest.Tag)
		}
	}
	if err := validateReference("install bootstrap", manifest.InstallBootstrap); err != nil {
		return err
	}
	if manifest.InstallBootstrap.Location != InstallBootstrapLocation(manifest.Version) {
		return contractError("install bootstrap location is not canonical for %s", manifest.Tag)
	}
	if err := validateBundles(manifest.StandardBundles); err != nil {
		return err
	}
	return nil
}

func validateSignedComponent(name string, component SignedComponent) error {
	if strings.TrimSpace(component.ArtifactID) == "" {
		return contractError("%s artifact ID is required", name)
	}
	if !digestPattern.MatchString(component.ManifestDigest) {
		return contractError("%s manifest digest must be canonical sha256", name)
	}
	seen := make(map[string]bool, len(component.MandatoryGuestFeatures))
	for _, feature := range component.MandatoryGuestFeatures {
		if strings.TrimSpace(feature) == "" || seen[feature] {
			return contractError("%s mandatory guest features must be unique non-empty values", name)
		}
		seen[feature] = true
	}
	return nil
}

func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return fmt.Errorf("trailing content: %w", err)
	}
	return nil
}

func validateIdentity(identity Identity) error {
	version, err := ParseTag(identity.Tag)
	if err != nil {
		return err
	}
	if identity.Version != version {
		return contractError("version %q does not match tag %q", identity.Version, identity.Tag)
	}
	if !commitPattern.MatchString(identity.SourceCommit) {
		return contractError("source commit must be a full lowercase Git object ID")
	}
	return nil
}

func validateWindow(name string, window ProtocolWindow) error {
	if window.Minimum == 0 || window.Maximum < window.Minimum {
		return contractError("%s protocol window is invalid", name)
	}
	return nil
}

func validatePlatforms(platforms PlatformMatrix) error {
	sets := map[string][]string{
		"host binary": platforms.HostBinaries, "control-plane": platforms.ControlPlane,
		"Runner": platforms.Runner, "installer tools": platforms.InstallerTools, "guest": platforms.Guest,
		"qualified Runner/guest": platforms.QualifiedRunnerGuest,
	}
	for name, values := range sets {
		if len(values) == 0 {
			return contractError("%s platform list is empty", name)
		}
		seen := map[string]bool{}
		for _, platform := range values {
			if !platformPattern.MatchString(platform) || seen[platform] {
				return contractError("%s platform %q is malformed or duplicated", name, platform)
			}
			seen[platform] = true
		}
	}
	for _, platform := range platforms.Runner {
		if !slices.Contains(platforms.QualifiedRunnerGuest, platform) {
			return contractError("Runner platform %q lacks required qualification", platform)
		}
	}
	for _, platform := range platforms.Guest {
		if !slices.Contains(platforms.QualifiedRunnerGuest, platform) {
			return contractError("guest platform %q lacks required qualification", platform)
		}
	}
	return nil
}

func validateReference(name string, ref Reference) error {
	parsed, err := url.Parse(ref.Location)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return contractError("%s location must be a public HTTPS URL", name)
	}
	if !digestPattern.MatchString(ref.Digest) {
		return contractError("%s digest must be canonical sha256", name)
	}
	return nil
}

func validateOCIReference(repository, reference string) error {
	prefix := repository + "@"
	if !strings.HasPrefix(reference, prefix) || !digestPattern.MatchString(strings.TrimPrefix(reference, prefix)) {
		return contractError("OCI reference %q must use immutable canonical coordinate %s@sha256:<digest>", reference, repository)
	}
	return nil
}

func validateBinaries(manifest ArtifactManifest) error {
	if len(manifest.Binaries) == 0 {
		return contractError("artifact manifest requires binaries")
	}
	want := map[string]bool{}
	for _, platform := range manifest.Platforms.HostBinaries {
		want["secondbox\x00"+platform] = false
		want["secondbox-deploy\x00"+platform] = false
	}
	for _, binary := range manifest.Binaries {
		key := binary.Name + "\x00" + binary.Platform
		if _, exists := want[key]; !exists || want[key] {
			return contractError("binary %q for %q is unexpected or duplicated", binary.Name, binary.Platform)
		}
		if binary.Location != BinaryLocation(manifest.Version, binary.Name, binary.Platform) || !checksumPattern.MatchString(binary.SHA256) {
			return contractError("binary %q for %q has noncanonical location or checksum", binary.Name, binary.Platform)
		}
		want[key] = true
	}
	for key, present := range want {
		if !present {
			parts := strings.Split(key, "\x00")
			return contractError("binary %q for %q is missing", parts[0], parts[1])
		}
	}
	return nil
}

func validateBundles(bundles []StandardBundleArtifact) error {
	if len(bundles) != 3 {
		return contractError("artifact manifest must contain exactly the agent-compartment, durable-coding, and agent-compartment-isolated standard bundles")
	}
	want := map[string]bool{"agent-compartment": false, "durable-coding": false, "agent-compartment-isolated": false}
	for _, bundle := range bundles {
		if _, ok := want[bundle.Name]; !ok || want[bundle.Name] {
			return contractError("standard bundle %q is unexpected or duplicated", bundle.Name)
		}
		if err := validateReference("standard bundle "+bundle.Name, bundle.Document); err != nil {
			return err
		}
		if len(bundle.Profiles) == 0 {
			return contractError("standard bundle %q has no Profile lineage", bundle.Name)
		}
		for index, profile := range bundle.Profiles {
			if profile.Name != bundle.Name || profile.Revision != int64(index+1) || !digestPattern.MatchString(profile.SpecDigest) {
				return contractError("standard bundle %q Profile lineage is not sequential and canonical", bundle.Name)
			}
		}
		want[bundle.Name] = true
	}
	return nil
}

func contractError(format string, arguments ...any) error {
	return fmt.Errorf("SecondBox release contract: "+format, arguments...)
}
