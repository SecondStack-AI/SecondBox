package standardresources

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
)

const BundleSchemaVersion = "secondbox.standard-bundle/v1"

type BundleDocument struct {
	SchemaVersion        string                `json:"schemaVersion"`
	Name                 string                `json:"name"`
	Architecture         string                `json:"architecture"`
	RunnerPoolSelector   string                `json:"runnerPoolSelector"`
	LogicalGateway       string                `json:"logicalGateway"`
	SignedManifestDigest string                `json:"signedManifestDigest"`
	Profile              resourceapply.Profile `json:"profile"`
	ParameterSchema      json.RawMessage       `json:"parameterSchema"`
}

func Documents(assetDigest string) ([]BundleDocument, error) {
	result := make([]BundleDocument, 0, 2)
	for _, name := range []string{AgentCompartment, DurableCoding} {
		profile, err := ProfileLineage(name, assetDigest)
		if err != nil {
			return nil, err
		}
		gateway := map[string]string{AgentCompartment: AgentGateway, DurableCoding: PlatformGateway}[name]
		document := BundleDocument{SchemaVersion: BundleSchemaVersion, Name: name, Architecture: ArchitectureAMD64, RunnerPoolSelector: PoolAMD64, LogicalGateway: gateway, SignedManifestDigest: assetDigest, Profile: profile, ParameterSchema: poolParameterSchema()}
		if err := document.Validate(); err != nil {
			return nil, err
		}
		result = append(result, document)
	}
	return result, nil
}

func DecodeDocument(data []byte) (BundleDocument, error) {
	var document BundleDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return BundleDocument{}, fmt.Errorf("SecondBox standard bundle decode failed: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return BundleDocument{}, errors.New("SecondBox standard bundle must contain one JSON value")
	}
	if err := document.Validate(); err != nil {
		return BundleDocument{}, err
	}
	return document, nil
}

func (document BundleDocument) Validate() error {
	if document.SchemaVersion != BundleSchemaVersion || (document.Name != AgentCompartment && document.Name != DurableCoding) || document.Architecture != ArchitectureAMD64 || document.RunnerPoolSelector != PoolAMD64 || len(document.ParameterSchema) == 0 {
		return errors.New("SecondBox standard bundle identity or parameter schema is incomplete")
	}
	wantGateway := map[string]string{AgentCompartment: AgentGateway, DurableCoding: PlatformGateway}[document.Name]
	if document.LogicalGateway != wantGateway {
		return fmt.Errorf("SecondBox standard bundle %q logical gateway differs from release policy", document.Name)
	}
	want, err := ProfileLineage(document.Name, document.SignedManifestDigest)
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
