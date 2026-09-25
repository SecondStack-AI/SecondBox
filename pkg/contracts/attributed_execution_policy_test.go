package contracts

import (
	"encoding/json"
	"testing"
)

func TestAttributedConnectionLimitsStrictFiniteJSON(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"maximumConnections":null}`, `{"maximumConnections":0}`, `{"maximumConnections":-1}`, `{"maximumConnections":4097}`, `{"maximumConnections":1.5}`, `{"maximumConnections":128,"gateway":"forged"}`} {
		var value AttributedExecutionConnectionLimits
		if err := json.Unmarshal([]byte(raw), &value); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{"maximumConnections":1}`, `{"maximumConnections":128}`, `{"maximumConnections":4096}`} {
		var value AttributedExecutionConnectionLimits
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	var spec ProfileRevisionSpec
	if err := json.Unmarshal([]byte(`{"attributedExecutionCeiling":null}`), &spec); err == nil {
		t.Fatal("accepted null Profile ceiling")
	}
}

func TestAttributedConnectionGrantResolution(t *testing.T) {
	spec := ProfileRevisionSpec{AttributedExecution: &AttributedExecutionPolicy{Gateway: "pinned", MaximumConnections: 128}, AttributedExecutionCeiling: AttributedExecutionConnectionLimits{MaximumConnections: 4096}}
	grant, err := spec.AttributedConnectionGrant()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		selection *AttributedExecutionConnectionLimits
		want      int64
	}{
		{nil, 128}, {&AttributedExecutionConnectionLimits{1}, 1}, {&AttributedExecutionConnectionLimits{4096}, 4096},
	} {
		got, err := grant.Resolve(test.selection)
		if err != nil || got.MaximumConnections != test.want {
			t.Fatalf("resolve = %+v, %v", got, err)
		}
	}
	spec.AttributedExecutionCeiling.MaximumConnections = 256
	grant, err = spec.AttributedConnectionGrant()
	if err != nil {
		t.Fatal(err)
	}
	got, err := grant.Resolve(&AttributedExecutionConnectionLimits{512})
	if err != nil || got.MaximumConnections != 256 {
		t.Fatalf("tightened grant = %+v, %v", got, err)
	}
	spec.AttributedExecutionCeiling = AttributedExecutionConnectionLimits{}
	grant, err = spec.AttributedConnectionGrant()
	if err != nil || grant.MaximumConnectionsCeiling != 128 {
		t.Fatalf("implicit ceiling = %+v, %v", grant, err)
	}
	spec.AttributedExecutionCeiling = AttributedExecutionConnectionLimits{1}
	if _, err := spec.AttributedConnectionGrant(); err == nil {
		t.Fatal("accepted default above ceiling")
	}
	spec.AttributedExecution = nil
	if _, err := spec.AttributedConnectionGrant(); err == nil {
		t.Fatal("accepted ceiling without permission")
	}
}
