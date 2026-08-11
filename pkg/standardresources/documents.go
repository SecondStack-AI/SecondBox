package standardresources

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
)

const BundleSchemaVersion = "secondbox.standard-bundle/v2"

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

func Documents(signedManifestDigest, runtimeBundleDigest, toolchainBundleDigest string) ([]BundleDocument, error) {
	result := make([]BundleDocument, 0, 2)
	for _, name := range []string{AgentCompartment, DurableCoding} {
		profile, err := ProfileLineage(name, runtimeBundleDigest, toolchainBundleDigest)
		if err != nil {
			return nil, err
		}
		gateway := map[string]string{AgentCompartment: AgentGateway, DurableCoding: PlatformGateway}[name]
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
func DecodeRecordedDocument(data []byte) (BundleDocument, error) {
	document, err := decodeDocument(data)
	if err != nil {
		return BundleDocument{}, err
	}
	if err := document.ValidateRecorded(); err != nil {
		return BundleDocument{}, err
	}
	return document, nil
}

func decodeDocument(data []byte) (BundleDocument, error) {
	var document BundleDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return BundleDocument{}, fmt.Errorf("SecondBox standard bundle decode failed: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return BundleDocument{}, errors.New("SecondBox standard bundle must contain one JSON value")
	}
	return document, nil
}

// ValidateRecorded proves the self-contained structure and digests of a
// previously published bundle. It deliberately does not compare that lineage
// with the current binary's append-only ProfileLineage.
func (document BundleDocument) ValidateRecorded() error {
	if document.SchemaVersion != BundleSchemaVersion || (document.Name != AgentCompartment && document.Name != DurableCoding) || document.Architecture != ArchitectureAMD64 || document.RunnerPoolSelector != PoolAMD64 || len(document.ParameterSchema) == 0 || !json.Valid(document.ParameterSchema) {
		return errors.New("SecondBox recorded standard bundle identity or parameter schema is incomplete")
	}
	wantGateway := map[string]string{AgentCompartment: AgentGateway, DurableCoding: PlatformGateway}[document.Name]
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
		digest, err := resourceapply.SpecDigest(revision.Spec)
		if err != nil || digest != revision.SpecDigest || revision.Spec.Pool != PoolAMD64 || revision.Spec.Architecture != ArchitectureAMD64 {
			return fmt.Errorf("SecondBox recorded standard bundle %q Profile revision %d is invalid", document.Name, revision.Number)
		}
	}
	latest := document.Profile.Revisions[len(document.Profile.Revisions)-1].Spec
	if latest.RuntimeBundleDigest != document.RuntimeBundleDigest || latest.ToolchainBundleDigest != document.ToolchainBundleDigest {
		return fmt.Errorf("SecondBox recorded standard bundle %q latest Profile revision differs from its execution assets", document.Name)
	}
	return nil
}

func (document BundleDocument) Validate() error {
	if document.SchemaVersion != BundleSchemaVersion || (document.Name != AgentCompartment && document.Name != DurableCoding) || document.Architecture != ArchitectureAMD64 || document.RunnerPoolSelector != PoolAMD64 || len(document.ParameterSchema) == 0 {
		return errors.New("SecondBox standard bundle identity or parameter schema is incomplete")
	}
	wantGateway := map[string]string{AgentCompartment: AgentGateway, DurableCoding: PlatformGateway}[document.Name]
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
