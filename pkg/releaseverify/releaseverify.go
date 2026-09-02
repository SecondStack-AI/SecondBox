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
	"strings"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
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
	return artifactManifest(ctx, location, fetch, false)
}

// RecordedArtifactManifest verifies a previously published release while
// treating its immutable standard Profile lineage as recorded data. This lets
// a newer updater authenticate an older release after code-owned policy has
// appended later Profile revisions.
func RecordedArtifactManifest(ctx context.Context, location string, fetch FetchFunc) (VerifiedRelease, error) {
	return artifactManifest(ctx, location, fetch, true)
}

func artifactManifest(ctx context.Context, location string, fetch FetchFunc, recorded bool) (VerifiedRelease, error) {
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
	if err := verifyManifestObjects(ctx, manifest, fetch, recorded); err != nil {
		return VerifiedRelease{}, err
	}
	return VerifiedRelease{Manifest: manifest, ManifestBytes: data}, nil
}

func verifyManifestObjects(ctx context.Context, manifest releasecontract.ArtifactManifest, fetch FetchFunc, recorded bool) error {
	verifiedObjects := map[string][]byte{}
	references := []releasecontract.Reference{manifest.OpenAPI.Reference, manifest.GoSDK.Package, manifest.TypeScriptSDK.Package, manifest.InstallBootstrap}
	if manifest.GVisor.Materialization != (releasecontract.Reference{}) {
		references = append(references, manifest.GVisor.Materialization)
	}
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
		qualificationSubject, err := manifest.InstallerQualificationSubjectDigest()
		if err != nil {
			return err
		}
		installerEvidence, err := releasecontract.DecodeInstallerQualificationEvidence(installerEvidenceData)
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
		var documentName, profileName, signedManifestDigest, runtimeBundleDigest, toolchainBundleDigest string
		var profileNumbers []int64
		var profileDigests []string
		if recorded {
			document, decodeErr := standardresources.DecodeRecordedDocument(data)
			err = decodeErr
			if err == nil {
				documentName, profileName = document.Name, document.Profile.Name
				signedManifestDigest, runtimeBundleDigest, toolchainBundleDigest = document.SignedManifestDigest, document.RuntimeBundleDigest, document.ToolchainBundleDigest
				for _, revision := range document.Profile.Revisions {
					profileNumbers = append(profileNumbers, revision.Number)
					profileDigests = append(profileDigests, revision.SpecDigest)
				}
			}
		} else {
			document, decodeErr := standardresources.DecodeDocument(data)
			err = decodeErr
			if err == nil {
				documentName, profileName = document.Name, document.Profile.Name
				signedManifestDigest, runtimeBundleDigest, toolchainBundleDigest = document.SignedManifestDigest, document.RuntimeBundleDigest, document.ToolchainBundleDigest
				for _, revision := range document.Profile.Revisions {
					profileNumbers = append(profileNumbers, revision.Number)
					profileDigests = append(profileDigests, revision.SpecDigest)
				}
			}
		}
		if err != nil {
			return fmt.Errorf("SecondBox release verification: standard bundle %s: %w", bundle.Name, err)
		}
		if documentName != bundle.Name || profileName != bundle.Name || len(profileNumbers) != len(bundle.Profiles) ||
			signedManifestDigest != manifest.MicroVM.SignedManifestDigest ||
			runtimeBundleDigest != manifest.MicroVM.RuntimeBundle.ManifestDigest ||
			toolchainBundleDigest != manifest.MicroVM.ToolchainBundle.ManifestDigest {
			return fmt.Errorf("SecondBox release verification: standard bundle %s identity mismatch", bundle.Name)
		}
		for index, profile := range bundle.Profiles {
			if profile.Name != bundle.Name || profileNumbers[index] != profile.Revision || profileDigests[index] != profile.SpecDigest {
				return fmt.Errorf("SecondBox release verification: standard bundle %s Profile lineage mismatch", bundle.Name)
			}
		}
	}
	return nil
}
