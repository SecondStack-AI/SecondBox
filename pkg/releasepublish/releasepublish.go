package releasepublish

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

const (
	PublicationInputSchema = 1
	MaximumTransportBytes  = int64(2_000_000_000)
	RoleCandidate          = "candidate"
	RoleEvidence           = "candidate-evidence"
)

var sha256DigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var sourceCommitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

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

type PublicationFile struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Role   string `json:"role"`
}

// PublicationInput is the private draft-Release transport contract. It binds
// every locally built candidate byte and the pre-publication KVM evidence.
type PublicationInput struct {
	SchemaVersion int `json:"schemaVersion"`
	releasecontract.Identity
	ArtifactManifestDigest string            `json:"artifactManifestDigest"`
	Files                  []PublicationFile `json:"files"`
}

func CandidateEvidenceFor(manifest releasecontract.ArtifactManifest, manifestBytes []byte, runnerEnvironment string) (CandidateEvidence, error) {
	evidence := CandidateEvidence{
		SchemaVersion:             1,
		Identity:                  manifest.Identity,
		ArtifactManifestDigest:    releasecontract.Digest(manifestBytes),
		SignedGuestManifestDigest: manifest.MicroVM.SignedManifestDigest,
		Architecture:              "linux/amd64",
		RunnerEnvironment:         runnerEnvironment,
		Result:                    "passed",
	}
	if err := ValidateEvidence(evidence, manifest, releasecontract.Digest(manifestBytes)); err != nil {
		return CandidateEvidence{}, err
	}
	return evidence, nil
}

func BuildPublicationInput(candidateDirectory, evidencePath string) (PublicationInput, error) {
	entries, err := os.ReadDir(candidateDirectory)
	if err != nil {
		return PublicationInput{}, fmt.Errorf("release publication input: read candidate: %w", err)
	}
	manifestPath := ""
	files := make([]PublicationFile, 0, len(entries)+1)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Base(entry.Name()) != entry.Name() {
			return PublicationInput{}, fmt.Errorf("release publication input: invalid candidate object %q", entry.Name())
		}
		if strings.HasSuffix(entry.Name(), "-artifact-manifest.json") {
			if manifestPath != "" {
				return PublicationInput{}, errors.New("release publication input: multiple artifact manifests")
			}
			manifestPath = filepath.Join(candidateDirectory, entry.Name())
		}
		file, err := publicationFile(filepath.Join(candidateDirectory, entry.Name()), RoleCandidate)
		if err != nil {
			return PublicationInput{}, err
		}
		files = append(files, file)
	}
	if manifestPath == "" {
		return PublicationInput{}, errors.New("release publication input: artifact manifest is absent")
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return PublicationInput{}, err
	}
	manifest, err := releasecontract.DecodeArtifactManifest(manifestBytes)
	if err != nil {
		return PublicationInput{}, err
	}
	evidenceFile, err := publicationFile(evidencePath, RoleEvidence)
	if err != nil {
		return PublicationInput{}, err
	}
	wantEvidenceName := fmt.Sprintf("secondbox-%s-candidate-kvm-evidence.json", manifest.Version)
	if evidenceFile.Name != wantEvidenceName {
		return PublicationInput{}, fmt.Errorf("release publication input: evidence name must be %s", wantEvidenceName)
	}
	for _, file := range files {
		if file.Name == evidenceFile.Name {
			return PublicationInput{}, errors.New("release publication input: evidence collides with a candidate object")
		}
	}
	evidenceBytes, err := os.ReadFile(evidencePath)
	if err != nil {
		return PublicationInput{}, err
	}
	evidence, err := DecodeCandidateEvidence(evidenceBytes)
	if err != nil {
		return PublicationInput{}, err
	}
	if err := ValidateEvidence(evidence, manifest, releasecontract.Digest(manifestBytes)); err != nil {
		return PublicationInput{}, err
	}
	files = append(files, evidenceFile)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	input := PublicationInput{SchemaVersion: PublicationInputSchema, Identity: manifest.Identity, ArtifactManifestDigest: releasecontract.Digest(manifestBytes), Files: files}
	if err := input.Validate(); err != nil {
		return PublicationInput{}, err
	}
	return input, nil
}

func DecodePublicationInput(data []byte) (PublicationInput, error) {
	var input PublicationInput
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return PublicationInput{}, fmt.Errorf("release publication input: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return PublicationInput{}, errors.New("release publication input: trailing JSON value")
	}
	if err := input.Validate(); err != nil {
		return PublicationInput{}, err
	}
	return input, nil
}

func (input PublicationInput) Validate() error {
	parsedVersion, tagError := releasecontract.ParseTag(input.Tag)
	if input.SchemaVersion != PublicationInputSchema || tagError != nil || parsedVersion != input.Version || !sourceCommitPattern.MatchString(input.SourceCommit) || !sha256DigestPattern.MatchString(input.ArtifactManifestDigest) || len(input.Files) == 0 {
		return errors.New("release publication input: identity is invalid")
	}
	wantManifestName := fmt.Sprintf("secondbox-%s-artifact-manifest.json", input.Version)
	wantEvidenceName := fmt.Sprintf("secondbox-%s-candidate-kvm-evidence.json", input.Version)
	seen := make(map[string]bool, len(input.Files))
	manifestCount := 0
	evidenceCount := 0
	previous := ""
	for _, file := range input.Files {
		if file.Name == "" || filepath.Base(file.Name) != file.Name || file.Name <= previous || seen[file.Name] || !sha256DigestPattern.MatchString(file.Digest) || file.Size < 1 || file.Size > MaximumTransportBytes {
			return fmt.Errorf("release publication input: invalid file %q", file.Name)
		}
		if file.Role != RoleCandidate && file.Role != RoleEvidence {
			return fmt.Errorf("release publication input: invalid role for %s", file.Name)
		}
		if file.Name == wantManifestName && file.Role == RoleCandidate {
			manifestCount++
			if file.Digest != input.ArtifactManifestDigest {
				return errors.New("release publication input: artifact manifest digest mismatch")
			}
		}
		if file.Role == RoleEvidence {
			evidenceCount++
			if file.Name != wantEvidenceName {
				return errors.New("release publication input: candidate evidence name is invalid")
			}
		}
		seen[file.Name] = true
		previous = file.Name
	}
	if manifestCount != 1 || evidenceCount != 1 {
		return errors.New("release publication input: manifest or evidence cardinality is invalid")
	}
	return nil
}

func VerifyPublicationDirectory(directory string, input PublicationInput, inputFilename string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("release publication input: read directory: %w", err)
	}
	want := make([]string, 0, len(input.Files)+1)
	for _, file := range input.Files {
		want = append(want, file.Name)
		size, digest, err := fileIdentity(filepath.Join(directory, file.Name))
		if err != nil {
			return fmt.Errorf("release publication input: read %s: %w", file.Name, err)
		}
		if size != file.Size || digest != file.Digest {
			return fmt.Errorf("release publication input: %s does not match its transport identity", file.Name)
		}
	}
	want = append(want, inputFilename)
	sort.Strings(want)
	actual := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("release publication input: unexpected directory %s", entry.Name())
		}
		actual = append(actual, entry.Name())
	}
	if strings.Join(actual, "\x00") != strings.Join(want, "\x00") {
		return errors.New("release publication input: directory contains missing or unknown files")
	}
	return nil
}

func VerifyPublicationSources(candidateDirectory, evidencePath string, input PublicationInput) error {
	if err := input.Validate(); err != nil {
		return err
	}
	entries, err := os.ReadDir(candidateDirectory)
	if err != nil {
		return fmt.Errorf("release publication input: read candidate directory: %w", err)
	}
	wantCandidates := make([]string, 0, len(input.Files)-1)
	evidenceName := filepath.Base(evidencePath)
	for _, file := range input.Files {
		path := filepath.Join(candidateDirectory, file.Name)
		if file.Role == RoleEvidence {
			if file.Name != evidenceName {
				return errors.New("release publication input: evidence path does not match the transport manifest")
			}
			path = evidencePath
		} else {
			wantCandidates = append(wantCandidates, file.Name)
		}
		size, digest, err := fileIdentity(path)
		if err != nil {
			return fmt.Errorf("release publication input: read %s: %w", file.Name, err)
		}
		if size != file.Size || digest != file.Digest {
			return fmt.Errorf("release publication input: %s does not match its transport identity", file.Name)
		}
	}
	actualCandidates := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("release publication input: unexpected candidate directory %s", entry.Name())
		}
		actualCandidates = append(actualCandidates, entry.Name())
	}
	sort.Strings(wantCandidates)
	if strings.Join(actualCandidates, "\x00") != strings.Join(wantCandidates, "\x00") {
		return errors.New("release publication input: candidate contains missing or unknown files")
	}
	return nil
}

func DecodeCandidateEvidence(data []byte) (CandidateEvidence, error) {
	var evidence CandidateEvidence
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return CandidateEvidence{}, fmt.Errorf("release candidate evidence: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CandidateEvidence{}, errors.New("release candidate evidence: trailing JSON value")
	}
	return evidence, nil
}

func publicationFile(path, role string) (PublicationFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return PublicationFile{}, fmt.Errorf("release publication input: stat %s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > MaximumTransportBytes {
		return PublicationFile{}, fmt.Errorf("release publication input: %s is not a bounded regular file", filepath.Base(path))
	}
	_, digest, err := fileIdentity(path)
	if err != nil {
		return PublicationFile{}, err
	}
	return PublicationFile{Name: filepath.Base(path), Digest: digest, Size: info.Size(), Role: role}, nil
}

func fileIdentity(path string) (int64, string, error) {
	entry, err := os.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	if !entry.Mode().IsRegular() {
		return 0, "", errors.New("not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, "", err
	}
	if !info.Mode().IsRegular() || !os.SameFile(entry, info) {
		return 0, "", errors.New("not a regular file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return 0, "", err
	}
	return info.Size(), fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
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
	if evidence.Architecture != "linux/amd64" || !sha256DigestPattern.MatchString(evidence.RunnerEnvironment) || evidence.Result != "passed" {
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
