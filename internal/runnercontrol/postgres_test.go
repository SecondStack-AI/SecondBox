package runnercontrol

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestPublicProtocolVersionsJSONMatchesRunnerContract(t *testing.T) {
	encoded, err := publicProtocolVersionsJSON(1)
	if err != nil {
		t.Fatalf("SecondBox public protocol versions encoding failed: %v", err)
	}
	var versions []string
	if err := json.Unmarshal(encoded, &versions); err != nil {
		t.Fatalf("SecondBox public protocol versions decoding failed: %v", err)
	}
	if !slices.Equal(versions, []string{"1"}) {
		t.Fatalf("SecondBox public protocol versions = %v, want [1]", versions)
	}
}
