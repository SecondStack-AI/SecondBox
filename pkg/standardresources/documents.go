package standardresources

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
)

const BundleSchemaVersion = "secondbox.standard-bundle/v4"

type BundleDocument struct {
	SchemaVersion      string                `json:"schemaVersion"`
	Name               string                `json:"name"`
	Architecture       string                `json:"architecture"`
	RunnerPoolSelector string                `json:"runnerPoolSelector"`
	LogicalGateway     string                `json:"logicalGateway"`
	Profile            resourceapply.Profile `json:"profile"`
	ParameterSchema    json.RawMessage       `json:"parameterSchema"`
}

// Documents returns the release-owned standard bundles of one guest architecture.
func Documents(architecture string) ([]BundleDocument, error) {
	pool, err := StandardPool(architecture)
	if err != nil {
		return nil, err
	}
	result := make([]BundleDocument, 0, len(BundleNames()))
	for _, name := range BundleNames() {
		profile, err := ProfileLineage(name, architecture)
		if err != nil {
			return nil, err
		}
		gateway := logicalGateway(name)
		document := BundleDocument{SchemaVersion: BundleSchemaVersion, Name: name, Architecture: architecture, RunnerPoolSelector: pool, LogicalGateway: gateway, Profile: profile, ParameterSchema: poolParameterSchema(architecture)}
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

func (document BundleDocument) Validate() error {
	pool, err := StandardPool(document.Architecture)
	if err != nil {
		return err
	}
	if document.SchemaVersion != BundleSchemaVersion || !slices.Contains(BundleNames(), document.Name) || document.RunnerPoolSelector != pool || len(document.ParameterSchema) == 0 {
		return errors.New("SecondBox standard bundle identity or parameter schema is incomplete")
	}
	wantGateway := logicalGateway(document.Name)
	if document.LogicalGateway != wantGateway {
		return fmt.Errorf("SecondBox standard bundle %q logical gateway differs from release policy", document.Name)
	}
	want, err := ProfileLineage(document.Name, document.Architecture)
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

func poolParameterSchema(architecture string) json.RawMessage {
	return json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["architectures","capabilities","state"],"properties":{"architectures":{"type":"array","contains":{"const":"` + architecture + `"}},"capabilities":{"type":"array","minItems":1,"items":{"type":"string"}},"state":{"const":"ready"}}}`)
}

func logicalGateway(name string) string {
	return map[string]string{AgentCompartment: AgentGateway, DurableCoding: PlatformGateway}[name]
}
