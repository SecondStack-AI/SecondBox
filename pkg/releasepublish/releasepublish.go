package releasepublish

import (
	"errors"
	"fmt"
	"sort"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

type CandidateEvidence struct {
	SchemaVersion int `json:"schemaVersion"`
	releasecontract.Identity
	ArtifactManifestDigest    string `json:"artifactManifestDigest"`
	SignedGuestManifestDigest string `json:"signedGuestManifestDigest"`
	Architecture              string `json:"architecture"`
	RunnerEnvironment         string `json:"runnerEnvironment"`
	Result                    string `json:"result"`
}

type Object struct {
	Coordinate string
	Digest     string
}

func ValidateEvidence(evidence CandidateEvidence, manifest releasecontract.ArtifactManifest, manifestDigest string) error {
	if evidence.SchemaVersion != 1 {
		return errors.New("release candidate evidence schema version is unsupported")
	}
	if evidence.Identity != manifest.Identity {
		return errors.New("release candidate evidence identity does not match artifact manifest")
	}
	if evidence.ArtifactManifestDigest != manifestDigest {
		return errors.New("release candidate evidence identifies a different artifact manifest")
	}
	if evidence.SignedGuestManifestDigest != manifest.MicroVM.SignedManifestDigest {
		return errors.New("release candidate evidence identifies a different signed guest")
	}
	if evidence.Architecture != "linux/amd64" || evidence.RunnerEnvironment == "" || evidence.Result != "passed" {
		return errors.New("release candidate evidence is not a passing qualified KVM result")
	}
	return nil
}

// Plan applies immutable retry semantics. Existing coordinates are accepted
// only when they already contain the exact desired digest.
func Plan(existing map[string]string, desired []Object) ([]Object, error) {
	seen := make(map[string]bool, len(desired))
	missing := make([]Object, 0, len(desired))
	for _, object := range desired {
		if object.Coordinate == "" || object.Digest == "" || seen[object.Coordinate] {
			return nil, errors.New("release publication contains an invalid or duplicated coordinate")
		}
		seen[object.Coordinate] = true
		current, ok := existing[object.Coordinate]
		if !ok {
			missing = append(missing, object)
			continue
		}
		if current != object.Digest {
			return nil, fmt.Errorf("release coordinate %s already contains different content", object.Coordinate)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Coordinate < missing[j].Coordinate })
	return missing, nil
}
