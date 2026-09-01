package deployconfig

import (
	"encoding/json"

	"github.com/SecondStack-AI/SecondBox/pkg/networkpolicycontract"
)

const runnerEgressContextConfigSchema = "secondbox.runner-egress-contexts/v1"

type runnerEgressContextConfigDocument struct {
	SchemaVersion string                `json:"schemaVersion"`
	GeneratedBy   string                `json:"generatedBy"`
	Contexts      []RunnerEgressContext `json:"contexts"`
}

func encodeRunnerEgressContextConfig(contexts []RunnerEgressContext) ([]byte, error) {
	if contexts == nil {
		contexts = []RunnerEgressContext{}
	}
	content, err := json.MarshalIndent(runnerEgressContextConfigDocument{
		SchemaVersion: runnerEgressContextConfigSchema,
		GeneratedBy:   networkpolicycontract.GeneratedConfigProvenance,
		Contexts:      contexts,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}
