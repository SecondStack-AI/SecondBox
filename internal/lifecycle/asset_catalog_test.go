package lifecycle

import (
	"errors"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestProfileAssetsResolveTwoExactDigestsWithoutSubstitution(t *testing.T) {
	runtimeDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	toolchainDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	catalog := fakeSignedAssetCatalog{assets: map[string]SignedAsset{
		runtimeDigest: {
			ArtifactID: "runtime-component", ManifestDigest: runtimeDigest,
			SignatureKeyID: "release-key", Architecture: "amd64",
			GuestProtocolGeneration: 3, MandatoryGuestFeatures: []string{"runtime"},
		},
		toolchainDigest: {
			ArtifactID: "toolchain-component", ManifestDigest: toolchainDigest,
			SignatureKeyID: "release-key", Architecture: "amd64",
			GuestProtocolGeneration: 3, MandatoryGuestFeatures: []string{"toolchain"},
		},
	}}
	assets, generation, err := resolveProfileAssets(catalog, contracts.ProfileRevisionSpec{
		RuntimeBundleDigest: runtimeDigest, ToolchainBundleDigest: toolchainDigest,
		Architecture: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if generation != 3 || len(assets) != 2 ||
		assets[0].ArtifactId != "runtime-component" ||
		assets[0].ManifestDigest != runtimeDigest ||
		assets[1].ArtifactId != "toolchain-component" ||
		assets[1].ManifestDigest != toolchainDigest {
		t.Fatalf("resolved signed assets = %#v, generation %d", assets, generation)
	}
	catalog.assets[runtimeDigest] = catalog.assets[toolchainDigest]
	if _, _, err := resolveProfileAssets(catalog, contracts.ProfileRevisionSpec{
		RuntimeBundleDigest: runtimeDigest, ToolchainBundleDigest: toolchainDigest,
		Architecture: "amd64",
	}); err == nil {
		t.Fatal("catalog substitution was accepted")
	}
}

type fakeSignedAssetCatalog struct {
	assets map[string]SignedAsset
}

func (catalog fakeSignedAssetCatalog) Resolve(digest string) (SignedAsset, error) {
	asset, found := catalog.assets[digest]
	if !found {
		return SignedAsset{}, errors.New("missing asset")
	}
	return asset, nil
}
