package standardresources

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const BundleSchemaVersion = "secondbox.standard-bundle/v3"

var recordedBundleDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type BundleDocument struct {
	SchemaVersion         string                `json:"schemaVersion"`
	Name                  string                `json:"name"`
	Architecture          string                `json:"architecture"`
	RunnerPoolSelector    string                `json:"runnerPoolSelector"`
	LogicalGateway        string                `json:"logicalGateway"`
	SignedManifestDigest  string                `json:"signedManifestDigest"`
	RuntimeBundleDigest   string                `json:"runtimeBundleDigest"`
	ToolchainBundleDigest string                `json:"toolchainBundleDigest"`
	Profile               resourceapply.Profile `json:"profile"`
	ParameterSchema       json.RawMessage       `json:"parameterSchema"`
}

// RecordedBundleDocument preserves the raw immutable Profile specs from a
// published release. They are authenticated against their recorded digests
// without decoding removed fields into the current operator-facing schema.
type RecordedBundleDocument struct {
	SchemaVersion         string          `json:"schemaVersion"`
	Name                  string          `json:"name"`
	Architecture          string          `json:"architecture"`
	RunnerPoolSelector    string          `json:"runnerPoolSelector"`
	LogicalGateway        string          `json:"logicalGateway"`
	SignedManifestDigest  string          `json:"signedManifestDigest"`
	RuntimeBundleDigest   string          `json:"runtimeBundleDigest"`
	ToolchainBundleDigest string          `json:"toolchainBundleDigest"`
	Profile               RecordedProfile `json:"profile"`
	ParameterSchema       json.RawMessage `json:"parameterSchema"`
}

type RecordedProfile struct {
	Name      string                    `json:"name"`
	Revisions []RecordedProfileRevision `json:"revisions"`
}

type RecordedProfileRevision struct {
	Number     int64           `json:"number"`
	SpecDigest string          `json:"specDigest"`
	Spec       json.RawMessage `json:"spec"`
}

func Documents(signedManifestDigest, runtimeBundleDigest, toolchainBundleDigest string) ([]BundleDocument, error) {
	result := make([]BundleDocument, 0, len(BundleNames()))
	for _, name := range BundleNames() {
		profile, err := ProfileLineage(name, runtimeBundleDigest, toolchainBundleDigest)
		if err != nil {
			return nil, err
		}
		gateway := logicalGateway(name)
		document := BundleDocument{SchemaVersion: BundleSchemaVersion, Name: name, Architecture: ArchitectureAMD64, RunnerPoolSelector: PoolAMD64, LogicalGateway: gateway, SignedManifestDigest: signedManifestDigest, RuntimeBundleDigest: runtimeBundleDigest, ToolchainBundleDigest: toolchainBundleDigest, Profile: profile, ParameterSchema: poolParameterSchema()}
		if err := document.Validate(); err != nil {
			return nil, err
		}
		result = append(result, document)
	}
	return result, nil
}

func DecodeDocument(data []byte) (BundleDocument, error) {
	document, err := decodeDocument(data)
	if err != nil {
		return BundleDocument{}, err
	}
	if err := document.Validate(); err != nil {
		return BundleDocument{}, err
	}
	return document, nil
}

// DecodeRecordedDocument validates an immutable published bundle without
// regenerating its Profile lineage from newer code-owned policy. The release
// manifest remains responsible for binding every recorded revision identity.
func DecodeRecordedDocument(data []byte) (RecordedBundleDocument, error) {
	var document RecordedBundleDocument
	if err := decodeStrictDocument(data, &document); err != nil {
		return RecordedBundleDocument{}, err
	}
	if err := document.ValidateRecorded(); err != nil {
		return RecordedBundleDocument{}, err
	}
	return document, nil
}

func decodeDocument(data []byte) (BundleDocument, error) {
	var document BundleDocument
	if err := decodeStrictDocument(data, &document); err != nil {
		return BundleDocument{}, err
	}
	return document, nil
}

func decodeStrictDocument(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("SecondBox standard bundle decode failed: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("SecondBox standard bundle must contain one JSON value")
	}
	return nil
}

// ValidateRecorded proves the self-contained structure and digests of a
// previously published bundle. It deliberately does not compare that lineage
// with the current binary's append-only ProfileLineage.
func (document RecordedBundleDocument) ValidateRecorded() error {
	if document.SchemaVersion != BundleSchemaVersion || !slices.Contains(BundleNames(), document.Name) || document.Architecture != ArchitectureAMD64 || document.RunnerPoolSelector != PoolAMD64 || len(document.ParameterSchema) == 0 || !json.Valid(document.ParameterSchema) {
		return errors.New("SecondBox recorded standard bundle identity or parameter schema is incomplete")
	}
	wantGateway := logicalGateway(document.Name)
	if document.LogicalGateway != wantGateway {
		return fmt.Errorf("SecondBox recorded standard bundle %q logical gateway differs from release policy", document.Name)
	}
	for _, digest := range []string{document.SignedManifestDigest, document.RuntimeBundleDigest, document.ToolchainBundleDigest} {
		if !recordedBundleDigestPattern.MatchString(digest) {
			return fmt.Errorf("SecondBox recorded standard bundle %q contains an invalid asset digest", document.Name)
		}
	}
	if document.SignedManifestDigest == document.RuntimeBundleDigest || document.SignedManifestDigest == document.ToolchainBundleDigest || document.RuntimeBundleDigest == document.ToolchainBundleDigest {
		return fmt.Errorf("SecondBox recorded standard bundle %q signed manifest and component digests must be distinct", document.Name)
	}
	if document.Profile.Name != document.Name || len(document.Profile.Revisions) == 0 {
		return fmt.Errorf("SecondBox recorded standard bundle %q Profile identity is incomplete", document.Name)
	}
	for index, revision := range document.Profile.Revisions {
		if revision.Number != int64(index+1) || !recordedBundleDigestPattern.MatchString(revision.SpecDigest) {
			return fmt.Errorf("SecondBox recorded standard bundle %q Profile lineage is invalid", document.Name)
		}
		identity, digest, err := recordedProfileSpecIdentity(revision.Spec)
		if err != nil || digest != revision.SpecDigest || identity.Pool != PoolAMD64 || identity.Architecture != ArchitectureAMD64 {
			return fmt.Errorf("SecondBox recorded standard bundle %q Profile revision %d is invalid", document.Name, revision.Number)
		}
		if index == len(document.Profile.Revisions)-1 && (identity.RuntimeBundleDigest != document.RuntimeBundleDigest || identity.ToolchainBundleDigest != document.ToolchainBundleDigest) {
			return fmt.Errorf("SecondBox recorded standard bundle %q latest Profile revision differs from its execution assets", document.Name)
		}
	}
	return nil
}

type recordedSpecIdentity struct {
	Pool                  string
	Architecture          string
	RuntimeBundleDigest   string
	ToolchainBundleDigest string
}

func recordedProfileSpecIdentity(raw json.RawMessage) (recordedSpecIdentity, string, error) {
	var current secondboxclient.ProfileRevisionSpec
	if err := decodeStrictDocument(raw, &current); err != nil {
		return recordedSpecIdentity{}, "", err
	}
	digest, err := resourceapply.SpecDigest(current)
	return recordedSpecIdentity{Pool: current.Pool, Architecture: current.Architecture, RuntimeBundleDigest: current.RuntimeBundleDigest, ToolchainBundleDigest: current.ToolchainBundleDigest}, digest, err
}

func (document BundleDocument) Validate() error {
	if document.SchemaVersion != BundleSchemaVersion || !slices.Contains(BundleNames(), document.Name) || document.Architecture != ArchitectureAMD64 || document.RunnerPoolSelector != PoolAMD64 || len(document.ParameterSchema) == 0 {
		return errors.New("SecondBox standard bundle identity or parameter schema is incomplete")
	}
	wantGateway := logicalGateway(document.Name)
	if document.LogicalGateway != wantGateway {
		return fmt.Errorf("SecondBox standard bundle %q logical gateway differs from release policy", document.Name)
	}
	if document.SignedManifestDigest == document.RuntimeBundleDigest || document.SignedManifestDigest == document.ToolchainBundleDigest || document.RuntimeBundleDigest == document.ToolchainBundleDigest {
		return fmt.Errorf("SecondBox standard bundle %q signed manifest and component digests must be distinct", document.Name)
	}
	want, err := ProfileLineage(document.Name, document.RuntimeBundleDigest, document.ToolchainBundleDigest)
	if err != nil {
		return err
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		return err
	}
	actualJSON, err := json.Marshal(document.Profile)
	if err != nil {
		return err
	}
	if !bytes.Equal(actualJSON, wantJSON) {
		return fmt.Errorf("SecondBox standard bundle %q Profile lineage differs from release policy", document.Name)
	}
	return nil
}

func poolParameterSchema() json.RawMessage {
	return json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["architectures","capabilities","capacityPolicy","state"],"properties":{"architectures":{"type":"array","contains":{"const":"amd64"}},"capabilities":{"type":"array","minItems":1,"items":{"type":"string"}},"capacityPolicy":{"type":"object","additionalProperties":{"type":"integer","minimum":0}},"state":{"const":"ready"}}}`)
}

func logicalGateway(name string) string {
	return map[string]string{AgentCompartment: AgentGateway, DurableCoding: PlatformGateway}[name]
}
