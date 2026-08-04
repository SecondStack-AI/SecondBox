package releasefinalize

import (
	"errors"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

type QualificationInput struct {
	SuiteDigest        string `json:"suiteDigest"`
	OperatingSystem    string `json:"operatingSystem"`
	Kernel             string `json:"kernel"`
	FirecrackerVersion string `json:"firecrackerVersion"`
	CPUModel           string `json:"cpuModel"`
	CompletedAt        string `json:"completedAt"`
}

func Qualification(manifest releasecontract.ArtifactManifest, manifestBytes []byte, input QualificationInput) (releasecontract.QualificationAttestation, error) {
	if input.SuiteDigest != manifest.SourceFreeSuite.Digest {
		return releasecontract.QualificationAttestation{}, errors.New("SecondBox release finalization: source-free suite digest does not match the artifact manifest")
	}
	result := releasecontract.QualificationAttestation{
		SchemaVersion:           releasecontract.QualificationAttestationSchema,
		Identity:                manifest.Identity,
		ArtifactManifest:        releasecontract.Reference{Location: releasecontract.ArtifactManifestLocation(manifest.Version), Digest: releasecontract.Digest(manifestBytes)},
		Suite:                   "secondbox-source-free-v1",
		SuiteDigest:             input.SuiteDigest,
		Architecture:            "linux/amd64",
		RunnerProtocolVersion:   manifest.RunnerProtocol.Maximum,
		GuestProtocolGeneration: manifest.GuestProtocol.Maximum,
		Guest:                   releasecontract.QualifiedGuest{ManifestDigest: manifest.MicroVM.SignedManifestDigest, SigningKeyFingerprint: manifest.MicroVM.SigningKeyFingerprint},
		RunnerEnvironment:       releasecontract.RunnerEnvironment{RunnerImageReference: manifest.Runner.Reference, OperatingSystem: input.OperatingSystem, Kernel: input.Kernel, FirecrackerVersion: input.FirecrackerVersion, CPUModel: input.CPUModel},
		Result:                  "passed", CompletedAt: input.CompletedAt,
	}
	if err := result.Validate(); err != nil {
		return releasecontract.QualificationAttestation{}, err
	}
	return result, nil
}

func Index(manifest releasecontract.ArtifactManifest, manifestBytes, qualificationBytes []byte) (releasecontract.ReleaseIndex, error) {
	qualification, err := releasecontract.DecodeQualificationAttestation(qualificationBytes)
	if err != nil {
		return releasecontract.ReleaseIndex{}, err
	}
	manifestRef := releasecontract.Reference{Location: releasecontract.ArtifactManifestLocation(manifest.Version), Digest: releasecontract.Digest(manifestBytes)}
	if qualification.Identity != manifest.Identity || qualification.ArtifactManifest != manifestRef {
		return releasecontract.ReleaseIndex{}, errors.New("SecondBox release finalization: qualification is not bound to this artifact manifest")
	}
	index := releasecontract.ReleaseIndex{SchemaVersion: releasecontract.ReleaseIndexSchema, Identity: manifest.Identity, ArtifactManifest: manifestRef, Qualification: releasecontract.Reference{Location: releasecontract.QualificationAttestationLocation(manifest.Version), Digest: releasecontract.Digest(qualificationBytes)}}
	if err := index.Validate(); err != nil {
		return releasecontract.ReleaseIndex{}, err
	}
	return index, nil
}
