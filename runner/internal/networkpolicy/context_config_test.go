package networkpolicy

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validEgressContextConfig = `{
  "schemaVersion": "secondbox.runner-egress-contexts/v1",
  "contexts": [
    {
      "name": "installation-a",
      "gateways": [
        {"logicalName": "agent-gateway.secondbox.internal", "address": "8.8.8.8"},
        {"logicalName": "platform-gateway.secondbox.internal", "address": "8.8.8.8"}
      ]
    },
    {
      "name": "installation-b",
      "gateways": [
        {"logicalName": "agent-gateway.secondbox.internal", "address": "9.9.9.9"},
        {"logicalName": "shared-gateway.secondbox.internal", "address": "8.8.8.8"}
      ]
    }
  ]
}`

func TestLoadEgressContextConfigAcceptsExplicitRepeatedAddresses(t *testing.T) {
	path := writeEgressContextConfig(t, validEgressContextConfig, 0o600)
	config, err := LoadEgressContextConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := config.ContextNames(); strings.Join(got, ",") != "installation-a,installation-b" {
		t.Fatalf("context names = %#v", got)
	}
	a, err := config.CompileOptionsForContext("installation-a", CompileOptions{
		MaximumPins: 1,
		MaximumTTL:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.RunnerGateways["agent-gateway.secondbox.internal"]; got != netip.MustParseAddr("8.8.8.8") {
		t.Fatalf("installation-a gateway = %s", got)
	}
	if len(a.ProtectedAddresses) != 2 {
		t.Fatalf("global protected addresses = %#v", a.ProtectedAddresses)
	}
	if _, err := config.CompileOptionsForContext("removed-context", CompileOptions{}); err == nil ||
		!strings.Contains(err.Error(), "removed-context") ||
		!strings.Contains(err.Error(), "cannot start") {
		t.Fatalf("missing stopped-Sandbox diagnostic = %v", err)
	}
}

func TestLoadEgressContextConfigRejectsMalformedDocuments(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "unknown top-level field", content: strings.Replace(validEgressContextConfig, `"contexts":`, `"extra": true, "contexts":`, 1), want: "unknown field"},
		{name: "unknown context field", content: strings.Replace(validEgressContextConfig, `"name": "installation-a",`, `"name": "installation-a", "extra": true,`, 1), want: "unknown field"},
		{name: "unknown gateway field", content: strings.Replace(validEgressContextConfig, `"address": "8.8.8.8"`, `"address": "8.8.8.8", "extra": true`, 1), want: "unknown field"},
		{name: "unsupported schema", content: strings.Replace(validEgressContextConfig, "secondbox.runner-egress-contexts/v1", "secondbox.runner-egress-contexts/v2", 1), want: "schemaVersion"},
		{name: "duplicate context", content: strings.Replace(validEgressContextConfig, `"name": "installation-b"`, `"name": "installation-a"`, 1), want: "repeats context"},
		{name: "invalid context", content: strings.Replace(validEgressContextConfig, `"name": "installation-a"`, `"name": "Installation A"`, 1), want: "egress context name"},
		{name: "empty mapping", content: strings.Replace(validEgressContextConfig, `"gateways": [`, `"gateways": [], "discarded": [`, 1), want: "unknown field"},
		{name: "empty mapping exact", content: `{"schemaVersion":"secondbox.runner-egress-contexts/v1","contexts":[{"name":"installation-a","gateways":[]}]}`, want: "at least one gateway"},
		{name: "duplicate logical name", content: strings.Replace(validEgressContextConfig, `"platform-gateway.secondbox.internal"`, `"agent-gateway.secondbox.internal"`, 1), want: "repeats logical name"},
		{name: "invalid logical name", content: strings.Replace(validEgressContextConfig, `"agent-gateway.secondbox.internal"`, `"*.secondbox.internal"`, 1), want: "logical name"},
		{name: "padded logical name", content: strings.Replace(validEgressContextConfig, `"agent-gateway.secondbox.internal"`, `" agent-gateway.secondbox.internal"`, 1), want: "canonical"},
		{name: "invalid IP", content: strings.Replace(validEgressContextConfig, `"8.8.8.8"`, `"gateway.internal"`, 1), want: "invalid IP"},
		{name: "trailing JSON", content: validEgressContextConfig + `{}`, want: "one JSON document"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeEgressContextConfig(t, test.content, 0o600)
			_, err := LoadEgressContextConfig(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadEgressContextConfigRejectsUnsafePathsAndMetadata(t *testing.T) {
	if _, err := LoadEgressContextConfig("relative.json"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative path error = %v", err)
	}

	t.Run("leaf symlink", func(t *testing.T) {
		target := writeEgressContextConfig(t, validEgressContextConfig, 0o600)
		link := filepath.Join(t.TempDir(), "contexts.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadEgressContextConfig(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("symlink error = %v", err)
		}
	})

	t.Run("ancestor symlink", func(t *testing.T) {
		realDirectory := t.TempDir()
		target := filepath.Join(realDirectory, "contexts.json")
		if err := os.WriteFile(target, []byte(validEgressContextConfig), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "linked")
		if err := os.Symlink(realDirectory, link); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadEgressContextConfig(filepath.Join(link, "contexts.json")); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("ancestor symlink error = %v", err)
		}
	})

	t.Run("group writable", func(t *testing.T) {
		path := writeEgressContextConfig(t, validEgressContextConfig, 0o620)
		if _, err := LoadEgressContextConfig(path); err == nil || !strings.Contains(err.Error(), "mode") {
			t.Fatalf("unsafe mode error = %v", err)
		}
	})

	for _, test := range []struct {
		name         string
		ownerUID     uint32
		effectiveUID uint32
		mode         os.FileMode
		wantErr      bool
	}{
		{name: "effective owner private", ownerUID: 1000, effectiveUID: 1000, mode: 0o600},
		{name: "root owner read only", ownerUID: 0, effectiveUID: 1000, mode: 0o444},
		{name: "foreign owner", ownerUID: 1001, effectiveUID: 1000, mode: 0o600, wantErr: true},
		{name: "group writable", ownerUID: 1000, effectiveUID: 1000, mode: 0o620, wantErr: true},
		{name: "world writable", ownerUID: 1000, effectiveUID: 1000, mode: 0o602, wantErr: true},
		{name: "executable", ownerUID: 1000, effectiveUID: 1000, mode: 0o700, wantErr: true},
	} {
		t.Run("metadata "+test.name, func(t *testing.T) {
			err := validateEgressContextConfigMetadata(test.ownerUID, test.effectiveUID, test.mode)
			if (err != nil) != test.wantErr {
				t.Fatalf("metadata error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func writeEgressContextConfig(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "contexts.json")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}
