// Package assetcatalog imports the components of client-selected execution images.
package assetcatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

var catalogDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// imageComponent is one runtime or toolchain component an execution image
// manifest binds.
type imageComponent struct {
	ArtifactID             string   `json:"artifactId"`
	ManifestDigest         string   `json:"manifestDigest"`
	MandatoryGuestFeatures []string `json:"mandatoryGuestFeatures"`
}

// ImportExecutionImageManifest reads component identities from the manifest of
// a digest-resolved execution image bundle the Runner verified.
func ImportExecutionImageManifest(manifest []byte) ([]*runnerv1.AssetReference, error) {
	if len(manifest) == 0 || len(manifest) > 1<<20 {
		return nil, errors.New("SecondBox execution image import requires bounded manifest metadata")
	}
	var document struct {
		Architecture    string                            `json:"architecture"`
		GuestProtocol   struct{ Minimum, Maximum uint32 } `json:"guestProtocol"`
		RuntimeBundle   imageComponent                    `json:"runtimeBundle"`
		ToolchainBundle imageComponent                    `json:"toolchainBundle"`
	}
	if err := json.Unmarshal(manifest, &document); err != nil {
		return nil, fmt.Errorf("SecondBox execution image manifest decode: %w", err)
	}
	if (document.Architecture != "amd64" && document.Architecture != "arm64") || document.GuestProtocol.Minimum == 0 || document.GuestProtocol.Maximum < document.GuestProtocol.Minimum {
		return nil, errors.New("SecondBox execution image manifest architecture or guest protocol is invalid")
	}
	assets := make([]*runnerv1.AssetReference, 0, 2)
	for _, component := range []imageComponent{document.RuntimeBundle, document.ToolchainBundle} {
		if component.ArtifactID == "" || !catalogDigestPattern.MatchString(component.ManifestDigest) {
			return nil, errors.New("SecondBox execution image manifest component identity is invalid")
		}
		assets = append(assets, &runnerv1.AssetReference{ArtifactId: component.ArtifactID, ManifestDigest: component.ManifestDigest, Architecture: document.Architecture, GuestProtocolGeneration: document.GuestProtocol.Maximum, MandatoryGuestFeatures: component.MandatoryGuestFeatures})
	}
	return assets, nil
}
