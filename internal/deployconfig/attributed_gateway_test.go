package deployconfig

import (
	"encoding/json"
	"testing"
)

func TestAttributedGatewayDeploymentConfig(t *testing.T) {
	contexts := []RunnerEgressContext{{Name: "tenant", Gateways: []RunnerLogicalGateway{{LogicalName: "tools.internal", AttributedSocket: "/run/tenant.sock"}}}}
	if err := validateRunnerEgressContexts("runner.egress_contexts", contexts); err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeRunnerEgressContextConfig(contexts)
	if err != nil {
		t.Fatal(err)
	}
	var document runnerEgressContextConfigDocument
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if gateway := document.Contexts[0].Gateways[0]; gateway.AttributedSocket != "/run/tenant.sock" || gateway.Address != "" {
		t.Fatalf("rendered gateway: %#v", gateway)
	}
	if len(runnerGatewayNames(contexts)) != 0 {
		t.Fatal("socket-only route enabled an ordinary gateway")
	}
	contexts[0].Gateways[0].AttributedSocket = "relative.sock"
	if err := validateRunnerEgressContexts("runner.egress_contexts", contexts); err == nil {
		t.Fatal("invalid socket passed deployment validation")
	}
	contexts[0].Gateways[0].AttributedSocket = ""
	if err := validateRunnerEgressContexts("runner.egress_contexts", contexts); err == nil {
		t.Fatal("gateway without any route passed deployment validation")
	}
}
