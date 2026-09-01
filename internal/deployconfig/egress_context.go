package deployconfig

import "encoding/json"

const runnerEgressContextConfigSchema = "secondbox.runner-egress-contexts/v1"

type runnerEgressContextConfigDocument struct {
	SchemaVersion string                `json:"schemaVersion"`
	Contexts      []RunnerEgressContext `json:"contexts"`
}

func encodeRunnerEgressContextConfig(contexts []RunnerEgressContext) ([]byte, error) {
	if contexts == nil {
		contexts = []RunnerEgressContext{}
	}
	content, err := json.MarshalIndent(runnerEgressContextConfigDocument{
		SchemaVersion: runnerEgressContextConfigSchema,
		Contexts:      contexts,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}
