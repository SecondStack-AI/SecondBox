// Package releasecontract defines the public, provider-neutral identity chain
// for a coordinated SecondBox release.
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
)

const (
	ArtifactManifestSchema         = "secondbox.release/artifact-manifest/v1"
	QualificationAttestationSchema = "secondbox.release/qualification-attestation/v1"
	ReleaseIndexSchema             = "secondbox.release/release-index/v1"

	TypeScriptPackage = "@secondstack-ai/secondbox"
	GoModule          = "github.com/SecondStack-AI/SecondBox"
	ControlPlaneImage = "ghcr.io/secondstack-ai/secondbox/control-plane"
	RunnerImage       = "ghcr.io/secondstack-ai/secondbox/runner"
	MicroVMImage      = "ghcr.io/secondstack-ai/secondbox/microvm-artifacts"
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
	Guest                []string `json:"guest"`
	QualifiedRunnerGuest []string `json:"qualifiedRunnerGuest"`
}

// Reference identifies immutable bytes at a public HTTPS location.
type Reference struct {
	Location string `json:"location"`
	Digest   string `json:"digest"`
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

type MicroVMArtifact struct {
	Identity              Identity `json:"identity"`
	ImageReference        string   `json:"imageReference"`
	SignedManifestDigest  string   `json:"signedManifestDigest"`
	SigningKeyFingerprint string   `json:"signingKeyFingerprint"`
}

// ArtifactManifest contains immutable artifact identity only. Qualification is
// deliberately separate so the final index has no digest cycle.
type ArtifactManifest struct {
	SchemaVersion string `json:"schemaVersion"`
	Identity
	OpenAPI              OpenAPIArtifact          `json:"openapi"`
	RunnerProtocol       ProtocolWindow           `json:"runnerProtocol"`
	GuestProtocol        ProtocolWindow           `json:"guestProtocol"`
	Platforms            PlatformMatrix           `json:"platforms"`
	GoSDK                SDKArtifact              `json:"goSdk"`
	TypeScriptSDK        SDKArtifact              `json:"typeScriptSdk"`
	ControlPlane         OCIArtifact              `json:"controlPlane"`
	Runner               OCIArtifact              `json:"runner"`
	MicroVM              MicroVMArtifact          `json:"microvm"`
	Binaries             []BinaryArtifact         `json:"binaries"`
	SBOMs                []Reference              `json:"sboms"`
	ArtifactAttestations []Reference              `json:"artifactAttestations"`
	SourceFreeSuite      Reference                `json:"sourceFreeSuite"`
	StandardBundles      []StandardBundleArtifact `json:"standardBundles"`
}

type QualifiedGuest struct {
	ManifestDigest        string `json:"manifestDigest"`
	SigningKeyFingerprint string `json:"signingKeyFingerprint"`
}

type RunnerEnvironment struct {
	RunnerImageReference string `json:"runnerImageReference"`
	OperatingSystem      string `json:"operatingSystem"`
	Kernel               string `json:"kernel"`
	FirecrackerVersion   string `json:"firecrackerVersion"`
	CPUModel             string `json:"cpuModel"`
}

// QualificationAttestation proves source-free use of one exact manifest.
type QualificationAttestation struct {
	SchemaVersion string `json:"schemaVersion"`
	Identity
	ArtifactManifest        Reference         `json:"artifactManifest"`
	Suite                   string            `json:"suite"`
	SuiteDigest             string            `json:"suiteDigest"`
	Architecture            string            `json:"architecture"`
	RunnerProtocolVersion   uint32            `json:"runnerProtocolVersion"`
	GuestProtocolGeneration uint32            `json:"guestProtocolGeneration"`
	Guest                   QualifiedGuest    `json:"guest"`
	RunnerEnvironment       RunnerEnvironment `json:"runnerEnvironment"`
	Result                  string            `json:"result"`
	CompletedAt             string            `json:"completedAt"`
}

// ReleaseIndex is the last artifact published. Its presence is the release
// completeness signal consumed by normal deployments.
type ReleaseIndex struct {
	SchemaVersion string `json:"schemaVersion"`
	Identity
	ArtifactManifest Reference `json:"artifactManifest"`
	Qualification    Reference `json:"qualificationAttestation"`
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

func QualificationAttestationLocation(version string) string {
	return fmt.Sprintf("https://github.com/SecondStack-AI/SecondBox/releases/download/v%s/secondbox-%s-qualification-attestation.json", version, version)
}

func ReleaseIndexLocation(version string) string {
	return fmt.Sprintf("https://github.com/SecondStack-AI/SecondBox/releases/download/v%s/secondbox-%s-release-index.json", version, version)
}

func SourceFreeSuiteLocation(version string) string {
	return fmt.Sprintf("https://github.com/SecondStack-AI/SecondBox/releases/download/v%s/secondbox-%s-source-free-qualify", version, version)
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

func DecodeQualificationAttestation(data []byte) (QualificationAttestation, error) {
	var attestation QualificationAttestation
	if err := decodeStrict(data, &attestation); err != nil {
		return QualificationAttestation{}, contractError("decode qualification attestation: %v", err)
	}
	if err := attestation.Validate(); err != nil {
		return QualificationAttestation{}, err
	}
	return attestation, nil
}

func DecodeReleaseIndex(data []byte) (ReleaseIndex, error) {
	var index ReleaseIndex
	if err := decodeStrict(data, &index); err != nil {
		return ReleaseIndex{}, contractError("decode release index: %v", err)
	}
	if err := index.Validate(); err != nil {
		return ReleaseIndex{}, err
	}
	return index, nil
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
		"Runner": manifest.Runner.Identity, "microVM": manifest.MicroVM.Identity,
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
		ControlPlaneImage: manifest.ControlPlane, RunnerImage: manifest.Runner,
	} {
		if err := validateOCIReference(name, artifact.Reference); err != nil {
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
	if err := validateBinaries(manifest); err != nil {
		return err
	}
	if len(manifest.SBOMs) == 0 || len(manifest.ArtifactAttestations) == 0 {
		return contractError("artifact manifest requires SBOM and artifact-attestation references")
	}
	for index, ref := range append(slices.Clone(manifest.SBOMs), manifest.ArtifactAttestations...) {
		if err := validateReference(fmt.Sprintf("evidence reference %d", index), ref); err != nil {
			return err
		}
	}
	if err := validateReference("source-free qualification suite", manifest.SourceFreeSuite); err != nil {
		return err
	}
	if manifest.SourceFreeSuite.Location != SourceFreeSuiteLocation(manifest.Version) {
		return contractError("source-free qualification suite location is not canonical for %s", manifest.Tag)
	}
	if err := validateBundles(manifest.StandardBundles); err != nil {
		return err
	}
	return nil
}

func (attestation QualificationAttestation) Validate() error {
	if attestation.SchemaVersion != QualificationAttestationSchema {
		return contractError("qualification attestation schemaVersion must be %q", QualificationAttestationSchema)
	}
	if err := validateIdentity(attestation.Identity); err != nil {
		return err
	}
	if err := validateReference("qualification artifact manifest", attestation.ArtifactManifest); err != nil {
		return err
	}
	if attestation.ArtifactManifest.Location != ArtifactManifestLocation(attestation.Version) {
		return contractError("qualification artifact manifest location is not canonical for %s", attestation.Tag)
	}
	if attestation.Suite == "" || !digestPattern.MatchString(attestation.SuiteDigest) {
		return contractError("qualification suite and canonical suite digest are required")
	}
	if !platformPattern.MatchString(attestation.Architecture) {
		return contractError("qualification architecture must be os/architecture")
	}
	if attestation.RunnerProtocolVersion == 0 || attestation.GuestProtocolGeneration == 0 {
		return contractError("qualification protocol selections must be positive")
	}
	if !digestPattern.MatchString(attestation.Guest.ManifestDigest) || !keyPattern.MatchString(attestation.Guest.SigningKeyFingerprint) {
		return contractError("qualification guest signing identity is malformed")
	}
	if err := validateOCIReference(RunnerImage, attestation.RunnerEnvironment.RunnerImageReference); err != nil {
		return err
	}
	if attestation.RunnerEnvironment.OperatingSystem == "" || attestation.RunnerEnvironment.Kernel == "" || attestation.RunnerEnvironment.FirecrackerVersion == "" || attestation.RunnerEnvironment.CPUModel == "" {
		return contractError("qualification Runner environment is incomplete")
	}
	if attestation.Result != "passed" {
		return contractError("qualification result must be passed")
	}
	if attestation.CompletedAt == "" {
		return contractError("qualification completion time is required")
	}
	return nil
}

func (index ReleaseIndex) Validate() error {
	if index.SchemaVersion != ReleaseIndexSchema {
		return contractError("release index schemaVersion must be %q", ReleaseIndexSchema)
	}
	if err := validateIdentity(index.Identity); err != nil {
		return err
	}
	if err := validateReference("release-index artifact manifest", index.ArtifactManifest); err != nil {
		return err
	}
	if err := validateReference("release-index qualification attestation", index.Qualification); err != nil {
		return err
	}
	if index.ArtifactManifest.Location != ArtifactManifestLocation(index.Version) || index.Qualification.Location != QualificationAttestationLocation(index.Version) {
		return contractError("release index references must use canonical public locations for %s", index.Tag)
	}
	return nil
}

// VerifyFinalRelease binds the exact public index bytes to the exact manifest
// and qualification bytes fetched independently by the caller.
func VerifyFinalRelease(indexBytes, manifestBytes, qualificationBytes []byte) (ReleaseIndex, ArtifactManifest, QualificationAttestation, error) {
	index, err := DecodeReleaseIndex(indexBytes)
	if err != nil {
		return ReleaseIndex{}, ArtifactManifest{}, QualificationAttestation{}, err
	}
	manifest, err := DecodeArtifactManifest(manifestBytes)
	if err != nil {
		return ReleaseIndex{}, ArtifactManifest{}, QualificationAttestation{}, err
	}
	qualification, err := DecodeQualificationAttestation(qualificationBytes)
	if err != nil {
		return ReleaseIndex{}, ArtifactManifest{}, QualificationAttestation{}, err
	}
	if err := verifyDigest("artifact manifest", index.ArtifactManifest.Digest, manifestBytes); err != nil {
		return ReleaseIndex{}, ArtifactManifest{}, QualificationAttestation{}, err
	}
	if err := verifyDigest("qualification attestation", index.Qualification.Digest, qualificationBytes); err != nil {
		return ReleaseIndex{}, ArtifactManifest{}, QualificationAttestation{}, err
	}
	if index.Identity != manifest.Identity || index.Identity != qualification.Identity {
		return ReleaseIndex{}, ArtifactManifest{}, QualificationAttestation{}, contractError("release index, artifact manifest, and qualification identities differ")
	}
	if qualification.ArtifactManifest != index.ArtifactManifest {
		return ReleaseIndex{}, ArtifactManifest{}, QualificationAttestation{}, contractError("qualification is bound to a different artifact manifest")
	}
	if qualification.Architecture != "linux/amd64" || !slices.Contains(manifest.Platforms.QualifiedRunnerGuest, qualification.Architecture) {
		return ReleaseIndex{}, ArtifactManifest{}, QualificationAttestation{}, contractError("qualification architecture %q is not a declared qualified Runner/guest platform", qualification.Architecture)
	}
	if qualification.RunnerProtocolVersion < manifest.RunnerProtocol.Minimum || qualification.RunnerProtocolVersion > manifest.RunnerProtocol.Maximum || qualification.GuestProtocolGeneration < manifest.GuestProtocol.Minimum || qualification.GuestProtocolGeneration > manifest.GuestProtocol.Maximum {
		return ReleaseIndex{}, ArtifactManifest{}, QualificationAttestation{}, contractError("qualification protocol selection is outside the artifact manifest compatibility windows")
	}
	if qualification.Guest.ManifestDigest != manifest.MicroVM.SignedManifestDigest || qualification.Guest.SigningKeyFingerprint != manifest.MicroVM.SigningKeyFingerprint {
		return ReleaseIndex{}, ArtifactManifest{}, QualificationAttestation{}, contractError("qualification guest signing identity does not match the artifact manifest")
	}
	if qualification.RunnerEnvironment.RunnerImageReference != manifest.Runner.Reference {
		return ReleaseIndex{}, ArtifactManifest{}, QualificationAttestation{}, contractError("qualification Runner image does not match the artifact manifest")
	}
	if qualification.Suite != "secondbox-source-free-v1" || qualification.SuiteDigest != manifest.SourceFreeSuite.Digest {
		return ReleaseIndex{}, ArtifactManifest{}, QualificationAttestation{}, contractError("qualification suite identity does not match the artifact manifest")
	}
	return index, manifest, qualification, nil
}

func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// AcceptImmutableRetry permits a publication retry only when the bytes already
// present at an immutable coordinate are identical to the staged candidate.
func AcceptImmutableRetry(existing, staged []byte) error {
	if !bytes.Equal(existing, staged) {
		return contractError("immutable publication coordinate already contains different bytes")
	}
	return nil
}

// ValidateRuntimeCombination applies the v1 mixed-version policy. Until a
// future schema records explicit cross-release evidence, all runtime members
// must carry the artifact manifest's exact coordinated identity.
func ValidateRuntimeCombination(manifest ArtifactManifest, controlPlane, runner, guest Identity) error {
	if controlPlane != manifest.Identity || runner != manifest.Identity || guest != manifest.Identity {
		return contractError("mixed control-plane, Runner, and guest release identities have no recorded compatibility evidence")
	}
	return nil
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
		"Runner": platforms.Runner, "guest": platforms.Guest,
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

func verifyDigest(name, expected string, data []byte) error {
	if actual := Digest(data); actual != expected {
		return contractError("%s digest mismatch: expected %s, got %s", name, expected, actual)
	}
	return nil
}

func contractError(format string, arguments ...any) error {
	return fmt.Errorf("SecondBox release contract: "+format, arguments...)
}
