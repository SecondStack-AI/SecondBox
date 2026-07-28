package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var catalogDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// SignedAsset is immutable release-catalog evidence consumed by placement.
type SignedAsset struct {
	ArtifactID              string   `json:"artifactId"`
	ManifestDigest          string   `json:"manifestDigest"`
	SignatureKeyID          string   `json:"signatureKeyId"`
	Architecture            string   `json:"architecture"`
	GuestProtocolGeneration uint32   `json:"guestProtocolGeneration"`
	MandatoryGuestFeatures  []string `json:"mandatoryGuestFeatures"`
}

// SignedAssetCatalog resolves Profile digests without inventing trust metadata.
type SignedAssetCatalog interface {
	Resolve(string) (SignedAsset, error)
}

// FileAssetCatalog is an immutable startup snapshot of a release-generated catalog.
type FileAssetCatalog struct {
	byDigest map[string]SignedAsset
}

// LoadFileAssetCatalog validates one absolute read-only catalog path.
func LoadFileAssetCatalog(path string) (*FileAssetCatalog, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("SecondBox signed asset catalog path must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("SecondBox signed asset catalog open failed: %w", err)
	}
	defer file.Close()
	var document struct {
		Assets []SignedAsset `json:"assets"`
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("SecondBox signed asset catalog decoding failed: %w", err)
	}
	if len(document.Assets) == 0 {
		return nil, errors.New("SecondBox signed asset catalog contains no assets")
	}
	catalog := &FileAssetCatalog{byDigest: make(map[string]SignedAsset, len(document.Assets))}
	for _, asset := range document.Assets {
		if asset.ArtifactID == "" || !catalogDigestPattern.MatchString(asset.ManifestDigest) ||
			asset.SignatureKeyID == "" ||
			(asset.Architecture != "amd64" && asset.Architecture != "arm64") ||
			asset.GuestProtocolGeneration == 0 {
			return nil, errors.New("SecondBox signed asset catalog contains incomplete trust evidence")
		}
		if _, duplicate := catalog.byDigest[asset.ManifestDigest]; duplicate {
			return nil, errors.New("SecondBox signed asset catalog contains a duplicate manifest digest")
		}
		catalog.byDigest[asset.ManifestDigest] = asset
	}
	return catalog, nil
}

// Resolve returns an independent copy of one exact signed asset record.
func (catalog *FileAssetCatalog) Resolve(digest string) (SignedAsset, error) {
	asset, found := catalog.byDigest[digest]
	if !found {
		return SignedAsset{}, fmt.Errorf("SecondBox signed asset catalog has no record for %s", digest)
	}
	asset.MandatoryGuestFeatures = append([]string(nil), asset.MandatoryGuestFeatures...)
	return asset, nil
}
