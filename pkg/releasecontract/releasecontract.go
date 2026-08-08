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
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	ArtifactManifestSchema               = "secondbox.release/artifact-manifest/v5"
	QualificationEvidenceSchema          = "secondbox.release/qualification-evidence/v1"
	InstallerQualificationEvidenceSchema = "secondbox.release/installer-qualification-evidence/v1"

	TypeScriptPackage   = "@secondstack-ai/secondbox"
	GoModule            = "github.com/SecondStack-AI/SecondBox"
	ControlPlaneImage   = "ghcr.io/secondstack-ai/secondbox/control-plane"
	RunnerImage         = "ghcr.io/secondstack-ai/secondbox/runner"
	MicroVMImage        = "ghcr.io/secondstack-ai/secondbox/microvm-artifacts"
	InstallerToolsImage = "ghcr.io/secondstack-ai/secondbox/installer-tools"
)

var (
	versionPattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)
	commitPattern   = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	checksumPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	platformPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9][a-z0-9._-]*$`)
	keyPattern      = regexp.MustCompile(`^SHA256:[0-9A-F]{64}$`)
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

type BundledServiceImages struct {
	Postgres          string `json:"postgres"`
	ObjectStore       string `json:"objectStore"`
	ObjectStoreClient string `json:"objectStoreClient"`
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
	if evidence.SchemaVersion != QualificationEvidenceSchema {
		return contractError("qualification evidence schemaVersion must be %q", QualificationEvidenceSchema)
	}
	if !commitPattern.MatchString(evidence.SourceCommit) {
		return contractError("qualification evidence source commit must be a full lowercase Git object ID")
	}
	if evidence.Suite != "test-scenario" || evidence.PassCount <= 0 || evidence.WallClockSeconds < 0 {
		return contractError("qualification evidence must describe a complete test-scenario run")
	}
	if err := validateQualificationHost("qualification", evidence.Host); err != nil {
		return err
	}
	qualifiedAt, err := time.Parse(time.RFC3339, evidence.QualifiedAt)
	if err != nil || evidence.QualifiedAt != qualifiedAt.UTC().Format("2006-01-02T15:04:05Z") {
		return contractError("qualification evidence qualifiedAt must be a canonical UTC timestamp")
	}
	return nil
}

func validateQualificationHost(label string, host QualificationHostEvidence) error {
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
	if evidence.SchemaVersion != InstallerQualificationEvidenceSchema {
		return contractError("installer qualification evidence schemaVersion must be %q", InstallerQualificationEvidenceSchema)
	}
	if !commitPattern.MatchString(evidence.SourceCommit) || evidence.Suite != "test-installer-qualified" || evidence.PassCount <= 0 || evidence.WallClockSeconds < 0 || !digestPattern.MatchString(evidence.ReleaseManifestDigest) || strings.TrimSpace(evidence.FilesystemIdentity) == "" || !evidence.RebootPassed {
		return contractError("installer qualification evidence must describe a complete qualified installer run")
	}
	if err := validateQualificationHost("installer qualification", evidence.Host); err != nil {
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
	if manifest.SchemaVersion != ArtifactManifestSchema {
		return contractError("artifact manifest schemaVersion must be %q", ArtifactManifestSchema)
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
	for name, reference := range map[string]string{"bundled Postgres": manifest.BundledServices.Postgres, "bundled object store": manifest.BundledServices.ObjectStore, "bundled object-store client": manifest.BundledServices.ObjectStoreClient} {
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
	if len(bundles) != 2 {
		return contractError("artifact manifest must contain exactly the agent-compartment and durable-coding standard bundles")
	}
	want := map[string]bool{"agent-compartment": false, "durable-coding": false}
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
