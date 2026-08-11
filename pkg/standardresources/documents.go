package standardresources

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
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

// RecordedBundleDocument is the immutable identity surface needed to verify a
// previously published bundle. Its Profile specs retain only the fields needed
// to bind execution assets after their complete historical wire shape and
// canonical digest have been validated.
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
	Number     int64                       `json:"number"`
	SpecDigest string                      `json:"specDigest"`
	Spec       RecordedProfileRevisionSpec `json:"spec"`
}

type RecordedProfileRevisionSpec struct {
	Pool                  string `json:"pool"`
	Architecture          string `json:"architecture"`
	RuntimeBundleDigest   string `json:"runtimeBundleDigest"`
	ToolchainBundleDigest string `json:"toolchainBundleDigest"`
}

type recordedBundleWire struct {
	SchemaVersion         string              `json:"schemaVersion"`
	Name                  string              `json:"name"`
	Architecture          string              `json:"architecture"`
	RunnerPoolSelector    string              `json:"runnerPoolSelector"`
	LogicalGateway        string              `json:"logicalGateway"`
	SignedManifestDigest  string              `json:"signedManifestDigest"`
	RuntimeBundleDigest   string              `json:"runtimeBundleDigest"`
	ToolchainBundleDigest string              `json:"toolchainBundleDigest"`
	Profile               recordedProfileWire `json:"profile"`
	ParameterSchema       json.RawMessage     `json:"parameterSchema"`
}

type recordedProfileWire struct {
	Name      string                        `json:"name"`
	Revisions []recordedProfileRevisionWire `json:"revisions"`
}

type recordedProfileRevisionWire struct {
	Number     int64           `json:"number"`
	SpecDigest string          `json:"specDigest"`
	Spec       json.RawMessage `json:"spec"`
}

// historicalProfileRevisionSpec is the exact Profile wire shape published by
// v0.4.x before application Artifact retention was removed in v0.5.0.
type historicalProfileRevisionSpec struct {
	Pool                  string                          `json:"pool"`
	Architecture          string                          `json:"architecture"`
	RuntimeBundleDigest   string                          `json:"runtimeBundleDigest"`
	ToolchainBundleDigest string                          `json:"toolchainBundleDigest"`
	Resources             secondboxclient.ResourcePolicy  `json:"resources"`
	Startup               secondboxclient.StartupPolicy   `json:"startup"`
	Lifecycle             secondboxclient.LifecyclePolicy `json:"lifecycle"`
	Retention             historicalRetentionPolicy       `json:"retention"`
	Execution             secondboxclient.ExecutionPolicy `json:"execution"`
	Network               secondboxclient.NetworkPolicy   `json:"network"`
	Ports                 []secondboxclient.PortPolicy    `json:"ports"`
}

type historicalRetentionPolicy struct {
	SnapshotLimit            int64 `json:"snapshotLimit"`
	SnapshotRetentionSeconds int64 `json:"snapshotRetentionSeconds"`
	ArtifactRetentionSeconds int64 `json:"artifactRetentionSeconds"`
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
func DecodeRecordedDocument(data []byte) (RecordedBundleDocument, error) {
	var wire recordedBundleWire
	if err := decodeStrictJSON(data, &wire); err != nil {
		return RecordedBundleDocument{}, fmt.Errorf("SecondBox standard bundle decode failed: %w", err)
	}
	document := RecordedBundleDocument{
		SchemaVersion: wire.SchemaVersion, Name: wire.Name, Architecture: wire.Architecture,
		RunnerPoolSelector: wire.RunnerPoolSelector, LogicalGateway: wire.LogicalGateway,
		SignedManifestDigest: wire.SignedManifestDigest, RuntimeBundleDigest: wire.RuntimeBundleDigest,
		ToolchainBundleDigest: wire.ToolchainBundleDigest, ParameterSchema: wire.ParameterSchema,
		Profile: RecordedProfile{Name: wire.Profile.Name, Revisions: make([]RecordedProfileRevision, 0, len(wire.Profile.Revisions))},
	}
	if err := document.validateRecordedEnvelope(); err != nil {
		return RecordedBundleDocument{}, err
	}
	if len(wire.Profile.Revisions) == 0 {
		return RecordedBundleDocument{}, fmt.Errorf("SecondBox recorded standard bundle %q Profile identity is incomplete", document.Name)
	}
	for index, revision := range wire.Profile.Revisions {
		if revision.Number != int64(index+1) || !recordedBundleDigestPattern.MatchString(revision.SpecDigest) {
			return RecordedBundleDocument{}, fmt.Errorf("SecondBox recorded standard bundle %q Profile lineage is invalid", document.Name)
		}
		spec, digest, err := decodeRecordedProfileSpec(revision.Spec)
		if err != nil || digest != revision.SpecDigest || spec.Pool != PoolAMD64 || spec.Architecture != ArchitectureAMD64 {
			return RecordedBundleDocument{}, fmt.Errorf("SecondBox recorded standard bundle %q Profile revision %d is invalid", document.Name, revision.Number)
		}
		document.Profile.Revisions = append(document.Profile.Revisions, RecordedProfileRevision{Number: revision.Number, SpecDigest: revision.SpecDigest, Spec: spec})
	}
	latest := document.Profile.Revisions[len(document.Profile.Revisions)-1].Spec
	if latest.RuntimeBundleDigest != document.RuntimeBundleDigest || latest.ToolchainBundleDigest != document.ToolchainBundleDigest {
		return RecordedBundleDocument{}, fmt.Errorf("SecondBox recorded standard bundle %q latest Profile revision differs from its execution assets", document.Name)
	}
	return document, nil
}

func decodeDocument(data []byte) (BundleDocument, error) {
	var document BundleDocument
	if err := decodeStrictJSON(data, &document); err != nil {
		return BundleDocument{}, fmt.Errorf("SecondBox standard bundle decode failed: %w", err)
	}
	return document, nil
}

func decodeStrictJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("SecondBox standard bundle must contain one JSON value")
	}
	return nil
}

func decodeRecordedProfileSpec(data []byte) (RecordedProfileRevisionSpec, string, error) {
	var current secondboxclient.ProfileRevisionSpec
	if err := decodeStrictJSON(data, &current); err == nil {
		digest, digestErr := resourceapply.SpecDigest(current)
		return recordedProfileSpecIdentity(current.Pool, current.Architecture, current.RuntimeBundleDigest, current.ToolchainBundleDigest), digest, digestErr
	}
	var historical historicalProfileRevisionSpec
	if err := decodeStrictJSON(data, &historical); err != nil {
		return RecordedProfileRevisionSpec{}, "", err
	}
	if historical.Retention.ArtifactRetentionSeconds < 1 {
		return RecordedProfileRevisionSpec{}, "", errors.New("SecondBox historical Profile Artifact retention is invalid")
	}
	canonical, err := json.Marshal(historical)
	if err != nil {
		return RecordedProfileRevisionSpec{}, "", err
	}
	digest := sha256.Sum256(canonical)
	return recordedProfileSpecIdentity(historical.Pool, historical.Architecture, historical.RuntimeBundleDigest, historical.ToolchainBundleDigest), "sha256:" + hex.EncodeToString(digest[:]), nil
}

func recordedProfileSpecIdentity(pool, architecture, runtimeBundleDigest, toolchainBundleDigest string) RecordedProfileRevisionSpec {
	return RecordedProfileRevisionSpec{Pool: pool, Architecture: architecture, RuntimeBundleDigest: runtimeBundleDigest, ToolchainBundleDigest: toolchainBundleDigest}
}

func (document RecordedBundleDocument) validateRecordedEnvelope() error {
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
	if document.Profile.Name != document.Name {
		return fmt.Errorf("SecondBox recorded standard bundle %q Profile identity is incomplete", document.Name)
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
