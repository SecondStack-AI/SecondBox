package contracts

import (
	"encoding/json"
	"testing"
)

func TestProfileResourceCeilingPreservesPresence(t *testing.T) {
	for _, test := range []struct {
		name, input string
		present     bool
		axes        int
	}{
		{"absent", `{}`, false, 0},
		{"empty object", `{"resourceCeiling":{}}`, true, 0},
		{"missing axes", `{"resourceCeiling":{"memoryBytes":null}}`, true, 1},
		{"explicit null axes", `{"resourceCeiling":{"vcpuCount":null,"memoryBytes":null,"workspaceBytes":null}}`, true, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			var spec ProfileRevisionSpec
			if err := json.Unmarshal([]byte(test.input), &spec); err != nil {
				t.Fatal(err)
			}
			if (spec.ResourceCeiling != nil) != test.present || len(spec.ResourceCeiling) != test.axes {
				t.Fatalf("decoded ceiling=%+v", spec.ResourceCeiling)
			}
			data, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			var encoded map[string]json.RawMessage
			if err := json.Unmarshal(data, &encoded); err != nil {
				t.Fatal(err)
			}
			if _, present := encoded["resourceCeiling"]; present != test.present {
				t.Fatalf("encoded ceiling presence changed: %s", data)
			}
			var decoded ProfileRevisionSpec
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.ResourceCeiling) != test.axes {
				t.Fatalf("lost explicit axes: %s", data)
			}
		})
	}
	for _, input := range []string{`{"resourceCeiling":null}`, `{"resourceCeiling":[]}`, `{"resourceCeiling":{"vcpuCount":1.5}}`} {
		var spec ProfileRevisionSpec
		if err := json.Unmarshal([]byte(input), &spec); err == nil {
			t.Fatalf("accepted invalid ceiling: %s", input)
		}
	}
}
