// Package materialization validates provider-private launch materializations.
package materialization

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

const SchemaVersion = "secondbox.runner/backend-materialization/v1"

const (
	BackendFirecracker  = "firecracker"
	BackendMicrosandbox = "microsandbox"
)

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Key is the complete immutable assignment-to-materialization lookup tuple.
type Key struct {
	BackendKind             string `json:"backendKind"`
	GuestArchitecture       string `json:"guestArchitecture"`
	RuntimeManifestDigest   string `json:"runtimeManifestDigest"`
	ToolchainManifestDigest string `json:"toolchainManifestDigest"`
}

// LaunchArtifact binds one provider-private local launch input by content.
type LaunchArtifact struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

// Manifest binds one locally present backend composition without exposing paths.
type Manifest struct {
	SchemaVersion           string           `json:"schemaVersion"`
	Key                     Key              `json:"key"`
	SourceOCIManifestDigest string           `json:"sourceOciManifestDigest,omitempty"`
	FlatRootDigest          string           `json:"flatRootDigest,omitempty"`
	LaunchArtifacts         []LaunchArtifact `json:"launchArtifacts"`
	AgentProtocolGeneration uint32           `json:"agentProtocolGeneration"`
	AgentFeatures           []string         `json:"agentFeatures"`
	BackendBuildID          string           `json:"backendBuildId"`
	HelperBuildID           string           `json:"helperBuildId,omitempty"`
}

// Load reads one strict manifest and verifies its externally pinned digest.
func Load(path, expectedDigest string) (Manifest, error) {
	if strings.TrimSpace(path) == "" || !digestPattern.MatchString(expectedDigest) {
		return Manifest{}, errors.New("SecondBox backend materialization requires path and pinned digest")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("SecondBox backend materialization read: %w", err)
	}
	manifest, err := Decode(data)
	if err != nil {
		return Manifest{}, err
	}
	digest, err := manifest.Digest()
	if err != nil {
		return Manifest{}, err
	}
	if digest != expectedDigest {
		return Manifest{}, errors.New("SecondBox backend materialization digest differs from pinned identity")
	}
	return manifest, nil
}

// Decode strictly decodes and validates one manifest document exactly as
// runner startup does: unknown fields, trailing values, and invalid identity
// are rejected before any digest is computed over the typed manifest.
func Decode(data []byte) (Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("SecondBox backend materialization decode: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Manifest{}, errors.New("SecondBox backend materialization contains trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("SecondBox backend materialization trailing JSON: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Validate rejects incomplete, mutable, aliased, or backend-incoherent identity.
func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf("SecondBox backend materialization schemaVersion must be %q", SchemaVersion)
	}
	if manifest.Key.BackendKind != BackendFirecracker && manifest.Key.BackendKind != BackendMicrosandbox {
		return errors.New("SecondBox backend materialization backend kind is unsupported")
	}
	if manifest.Key.GuestArchitecture != "amd64" && manifest.Key.GuestArchitecture != "arm64" {
		return errors.New("SecondBox backend materialization guest architecture is unsupported")
	}
	if !digestPattern.MatchString(manifest.Key.RuntimeManifestDigest) ||
		!digestPattern.MatchString(manifest.Key.ToolchainManifestDigest) ||
		manifest.Key.RuntimeManifestDigest == manifest.Key.ToolchainManifestDigest {
		return errors.New("SecondBox backend materialization runtime and toolchain digests are invalid")
	}
	if manifest.Key.BackendKind == BackendMicrosandbox {
		if !digestPattern.MatchString(manifest.SourceOCIManifestDigest) || !digestPattern.MatchString(manifest.FlatRootDigest) {
			return errors.New("SecondBox Microsandbox materialization requires digest-pinned source OCI and flat root identity")
		}
		if strings.TrimSpace(manifest.HelperBuildID) == "" {
			return errors.New("SecondBox Microsandbox materialization helper build identity is required")
		}
	} else if manifest.SourceOCIManifestDigest != "" || manifest.FlatRootDigest != "" || manifest.HelperBuildID != "" {
		return errors.New("SecondBox Firecracker materialization contains Microsandbox-only identity")
	}
	if manifest.AgentProtocolGeneration == 0 || strings.TrimSpace(manifest.BackendBuildID) == "" || len(manifest.LaunchArtifacts) == 0 {
		return errors.New("SecondBox backend materialization build, agent, and launch identity is incomplete")
	}
	seenArtifacts := make(map[string]struct{}, len(manifest.LaunchArtifacts))
	for _, artifact := range manifest.LaunchArtifacts {
		if strings.TrimSpace(artifact.ID) == "" || !digestPattern.MatchString(artifact.SHA256) {
			return errors.New("SecondBox backend materialization launch artifact is incomplete")
		}
		if _, exists := seenArtifacts[artifact.ID]; exists {
			return errors.New("SecondBox backend materialization launch artifact ID is duplicated")
		}
		seenArtifacts[artifact.ID] = struct{}{}
	}
	if !slices.IsSortedFunc(manifest.LaunchArtifacts, func(left, right LaunchArtifact) int {
		return strings.Compare(left.ID, right.ID)
	}) {
		return errors.New("SecondBox backend materialization launch artifacts must be sorted")
	}
	if !slices.IsSorted(manifest.AgentFeatures) || slices.Contains(manifest.AgentFeatures, "") {
		return errors.New("SecondBox backend materialization agent features must be sorted and non-empty")
	}
	for index := 1; index < len(manifest.AgentFeatures); index++ {
		if manifest.AgentFeatures[index-1] == manifest.AgentFeatures[index] {
			return errors.New("SecondBox backend materialization agent feature is duplicated")
		}
	}
	return nil
}

// Digest returns the canonical content identity of one validated manifest.
func (manifest Manifest) Digest() (string, error) {
	if err := manifest.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("SecondBox backend materialization encode: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
