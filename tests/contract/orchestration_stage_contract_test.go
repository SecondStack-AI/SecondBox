package contract_test

import (
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/store"
)

// TestCanonicalOrchestrationStagesMatchThePersistedVocabulary keeps the
// published stage enum equal to the milestones the control plane actually
// persists. The two drifted apart once already: the contract listed five
// stages while the placement transaction wrote ten more, so a timing response
// for a real Operation could not validate against its own schema.
func TestCanonicalOrchestrationStagesMatchThePersistedVocabulary(t *testing.T) {
	document := loadOpenAPIContract(t)
	schemas := object(t, object(t, document["components"], "components")["schemas"], "schemas")
	stageSchema := object(
		t,
		object(
			t,
			object(t, schemas["OperationStageTiming"], "OperationStageTiming")["properties"],
			"OperationStageTiming.properties",
		)["stage"],
		"OperationStageTiming.properties.stage",
	)
	rawEnum, ok := stageSchema["enum"].([]any)
	if !ok {
		t.Fatal("OperationStageTiming.stage must declare an enum")
	}
	published := make([]string, len(rawEnum))
	for index, value := range rawEnum {
		name, ok := value.(string)
		if !ok {
			t.Fatalf("OperationStageTiming.stage enum entry %d is %T, want string", index, value)
		}
		published[index] = name
	}
	if len(published) != len(store.OrchestrationStages) {
		t.Fatalf(
			"published orchestration stages = %v, persisted vocabulary = %v",
			published, store.OrchestrationStages,
		)
	}
	for index := range published {
		if published[index] != store.OrchestrationStages[index] {
			t.Fatalf(
				"published orchestration stages = %v, persisted vocabulary = %v",
				published, store.OrchestrationStages,
			)
		}
	}

	orchestration := object(
		t,
		object(
			t,
			object(t, schemas["OperationTiming"], "OperationTiming")["properties"],
			"OperationTiming.properties",
		)["orchestration"],
		"OperationTiming.properties.orchestration",
	)
	maximumItems, ok := orchestration["maxItems"].(float64)
	if !ok {
		t.Fatal("OperationTiming.orchestration must bound its item count")
	}
	// One row per (Operation, stage) exists at most, so the bound is exactly
	// the vocabulary size. A smaller bound rejects a truthful response.
	if int(maximumItems) != len(store.OrchestrationStages) {
		t.Fatalf(
			"OperationTiming.orchestration maxItems = %d, want %d",
			int(maximumItems), len(store.OrchestrationStages),
		)
	}
}
