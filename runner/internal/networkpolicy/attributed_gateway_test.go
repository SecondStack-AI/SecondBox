package networkpolicy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestAttributedGatewayUsesOnlyPinnedContext(t *testing.T) {
	content := `{"schemaVersion":"secondbox.runner-egress-contexts/v1","contexts":[
		{"name":"tenant-a","gateways":[{"logicalName":"tools.internal","attributedSocket":"/run/tenant-a.sock"}]},
		{"name":"tenant-b","gateways":[{"logicalName":"tools.internal","address":"8.8.8.8","attributedSocket":"/run/tenant-b.sock"}]},
		{"name":"ordinary","gateways":[{"logicalName":"tools.internal","address":"9.9.9.9"}]}
	]}`
	config, err := LoadEgressContextConfig(writeEgressContextConfig(t, content, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	for _, contextName := range []string{"tenant-a", "tenant-b"} {
		socket, err := config.AttributedGatewaySocket(contextName, "tools.internal")
		if err != nil || socket != "/run/"+contextName+".sock" {
			t.Fatalf("context %s resolved %q: %v", contextName, socket, err)
		}
	}
	for _, test := range [][2]string{{"ordinary", "tools.internal"}, {"missing", "tools.internal"}, {"tenant-a", "other.internal"}} {
		if _, err := config.AttributedGatewaySocket(test[0], test[1]); err == nil {
			t.Fatalf("unconfigured attributed route accepted: %v", test)
		}
	}
	options, err := config.CompileOptionsForContext("tenant-a", CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(options.RunnerGateways) != 0 || len(options.ProtectedAddresses) != 2 {
		t.Fatalf("socket route changed ordinary IP authority: %#v", options)
	}
}

func TestAttributedGatewayRejectsInvalidSocketPaths(t *testing.T) {
	for _, socket := range []string{"relative.sock", "/", "/run/../socket", "/run/socket/", "/run/" + strings.Repeat("x", 108), "/run/socket\n", "/run/socket\x00"} {
		encoded, err := json.Marshal(socket)
		if err != nil {
			t.Fatal(err)
		}
		content := fmt.Sprintf(`{"schemaVersion":"secondbox.runner-egress-contexts/v1","contexts":[{"name":"tenant","gateways":[{"logicalName":"tools.internal","attributedSocket":%s}]}]}`, encoded)
		if _, err := LoadEgressContextConfig(writeEgressContextConfig(t, content, 0o600)); err == nil {
			t.Fatalf("accepted socket %q", socket)
		}
	}
}
