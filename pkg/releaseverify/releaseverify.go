// Package releaseverify retrieves and independently verifies a public release
// artifact manifest without a repository checkout.
package releaseverify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
)

const maximumReleaseObjectBytes = 512 << 20

type FetchFunc func(context.Context, string) ([]byte, error)

type VerifiedRelease struct {
	Manifest      releasecontract.ArtifactManifest
	ManifestBytes []byte
}

// DirectoryFetcher resolves canonical release URLs to exact regular files in
// one explicitly supplied staging directory. It exists only for qualification
// of pre-publication candidates; ordinary installers use HTTPFetcher.
func DirectoryFetcher(directory string) FetchFunc {
	return func(_ context.Context, location string) ([]byte, error) {
		absolute, err := filepath.Abs(directory)
		if err != nil {
			return nil, fmt.Errorf("SecondBox release verification: candidate directory: %w", err)
		}
		info, err := os.Lstat(absolute)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("SecondBox release verification: candidate directory must be a non-symbolic-link directory: %w", err)
		}
		parsed, err := url.Parse(location)
		if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("SecondBox release verification: %q is not a canonical GitHub release location", location)
		}
		const prefix = "/SecondStack-AI/SecondBox/releases/download/"
		if !strings.HasPrefix(parsed.Path, prefix) {
			return nil, fmt.Errorf("SecondBox release verification: %q is outside the SecondBox release namespace", location)
		}
		name := filepath.Base(parsed.Path)
		if name == "." || name == string(filepath.Separator) || name == "" {
			return nil, fmt.Errorf("SecondBox release verification: %q has no release filename", location)
		}
		path := filepath.Join(absolute, name)
		fileInfo, err := os.Lstat(path)
		if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("SecondBox release verification: candidate object %q must be a non-symbolic-link regular file: %w", name, err)
		}
		if fileInfo.Size() > maximumReleaseObjectBytes {
			return nil, fmt.Errorf("SecondBox release verification: candidate object %q exceeds size limit", name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("SecondBox release verification: read candidate object %q: %w", name, err)
		}
		return data, nil
	}
}

// CandidateDirectory verifies the one candidate artifact manifest and every
// referenced release object from an explicit staging directory.
func CandidateDirectory(ctx context.Context, directory string) (VerifiedRelease, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return VerifiedRelease{}, fmt.Errorf("SecondBox release verification: read candidate directory: %w", err)
	}
	manifestNames := []string{}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "secondbox-") && strings.HasSuffix(entry.Name(), "-artifact-manifest.json") {
			manifestNames = append(manifestNames, entry.Name())
		}
	}
	if len(manifestNames) != 1 {
		return VerifiedRelease{}, fmt.Errorf("SecondBox release verification: candidate directory must contain exactly one artifact manifest, found %d", len(manifestNames))
	}
	manifestBytes, err := os.ReadFile(filepath.Join(directory, manifestNames[0]))
	if err != nil {
		return VerifiedRelease{}, err
	}
	manifest, err := releasecontract.DecodeArtifactManifest(manifestBytes)
	if err != nil {
		return VerifiedRelease{}, err
	}
	if !manifest.Candidate {
		return VerifiedRelease{}, errors.New("SecondBox release verification: candidate directory manifest is not marked as a candidate")
	}
	if manifestNames[0] != fmt.Sprintf("secondbox-%s-artifact-manifest.json", manifest.Version) {
		return VerifiedRelease{}, errors.New("SecondBox release verification: candidate artifact manifest filename differs from its version")
	}
	return ArtifactManifest(ctx, releasecontract.ArtifactManifestLocation(manifest.Version), DirectoryFetcher(directory))
}

func HTTPFetcher(client *http.Client) FetchFunc {
	return func(ctx context.Context, location string) ([]byte, error) {
		parsed, err := url.Parse(location)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
			return nil, fmt.Errorf("SecondBox release verification: %q is not a public HTTPS location", location)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
		if err != nil {
			return nil, err
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("SecondBox release verification: fetch %s: %w", location, err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("SecondBox release verification: fetch %s returned %s", location, response.Status)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, maximumReleaseObjectBytes+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maximumReleaseObjectBytes {
			return nil, fmt.Errorf("SecondBox release verification: object at %s exceeds size limit", location)
		}
		return data, nil
	}
}

func ArtifactManifest(ctx context.Context, location string, fetch FetchFunc) (VerifiedRelease, error) {
	data, err := fetch(ctx, location)
	if err != nil {
		return VerifiedRelease{}, err
	}
	manifest, err := releasecontract.DecodeArtifactManifest(data)
	if err != nil {
		return VerifiedRelease{}, err
	}
	if location != releasecontract.ArtifactManifestLocation(manifest.Version) {
		return VerifiedRelease{}, fmt.Errorf("SecondBox release verification: artifact manifest location is not canonical for %s", manifest.Tag)
	}
	if err := verifyManifestObjects(ctx, manifest, fetch); err != nil {
		return VerifiedRelease{}, err
	}
	return VerifiedRelease{Manifest: manifest, ManifestBytes: data}, nil
}

func verifyManifestObjects(ctx context.Context, manifest releasecontract.ArtifactManifest, fetch FetchFunc) error {
	verifiedObjects := map[string][]byte{}
	references := []releasecontract.Reference{manifest.OpenAPI.Reference, manifest.GoSDK.Package, manifest.TypeScriptSDK.Package, manifest.InstallBootstrap}
	if manifest.SourceFreeSuite != (releasecontract.Reference{}) {
		references = append(references, manifest.SourceFreeSuite)
	}
	references = append(references, manifest.SBOMs...)
	references = append(references, manifest.ArtifactAttestations...)
	references = append(references, manifest.QualificationEvidence)
	if !manifest.Candidate {
		references = append(references, manifest.InstallerQualificationEvidence)
	}
	for _, bundle := range manifest.StandardBundles {
		references = append(references, bundle.Document)
	}
	for _, reference := range references {
		data, err := fetch(ctx, reference.Location)
		if err != nil {
			return err
		}
		if releasecontract.Digest(data) != reference.Digest {
			return fmt.Errorf("SecondBox release verification: digest mismatch at %s", reference.Location)
		}
		verifiedObjects[reference.Location] = data
	}
	evidenceData := verifiedObjects[manifest.QualificationEvidence.Location]
	evidence, err := releasecontract.DecodeQualificationEvidence(evidenceData)
	if err != nil {
		return err
	}
	if err := evidence.ValidateForRelease(manifest.SourceCommit); err != nil {
		return err
	}
	if !manifest.Candidate {
		installerEvidenceData := verifiedObjects[manifest.InstallerQualificationEvidence.Location]
		installerEvidence, err := releasecontract.DecodeInstallerQualificationEvidence(installerEvidenceData)
		if err != nil {
			return err
		}
		qualificationSubject, err := manifest.InstallerQualificationSubjectDigest()
		if err != nil {
			return err
		}
		if err := installerEvidence.ValidateForRelease(manifest.SourceCommit, qualificationSubject); err != nil {
			return err
		}
	}
	for _, binary := range manifest.Binaries {
		data, err := fetch(ctx, binary.Location)
		if err != nil {
			return err
		}
		if releasecontract.Digest(data) != "sha256:"+binary.SHA256 {
			return fmt.Errorf("SecondBox release verification: binary digest mismatch at %s", binary.Location)
		}
	}
	for _, bundle := range manifest.StandardBundles {
		data, err := fetch(ctx, bundle.Document.Location)
		if err != nil {
			return err
		}
		document, err := standardresources.DecodeDocument(data)
		if err != nil {
			return fmt.Errorf("SecondBox release verification: standard bundle %s: %w", bundle.Name, err)
		}
		if document.Name != bundle.Name || document.Profile.Name != bundle.Name || len(document.Profile.Revisions) != len(bundle.Profiles) ||
			document.SignedManifestDigest != manifest.MicroVM.SignedManifestDigest ||
			document.RuntimeBundleDigest != manifest.MicroVM.RuntimeBundle.ManifestDigest ||
			document.ToolchainBundleDigest != manifest.MicroVM.ToolchainBundle.ManifestDigest {
			return fmt.Errorf("SecondBox release verification: standard bundle %s identity mismatch", bundle.Name)
		}
		for _, profile := range bundle.Profiles {
			matched := slices.ContainsFunc(document.Profile.Revisions, func(revision resourceapply.ProfileRevision) bool {
				return revision.Number == profile.Revision && revision.SpecDigest == profile.SpecDigest
			})
			if !matched {
				return fmt.Errorf("SecondBox release verification: standard bundle %s Profile lineage mismatch", bundle.Name)
			}
		}
	}
	return nil
}
