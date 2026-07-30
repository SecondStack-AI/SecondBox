package service

import (
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// TestNewOpaqueIDMatchesTheDeclaredSandboxPrefix pins the coupling that lets a
// client tell an identifier from a name without asking the service.
func TestNewOpaqueIDMatchesTheDeclaredSandboxPrefix(t *testing.T) {
	identifier := NewOpaqueID("sbx")
	if !strings.HasPrefix(identifier, contracts.SandboxIDPrefix) {
		t.Fatalf("minted identifier %q does not carry %q", identifier, contracts.SandboxIDPrefix)
	}
}

func TestValidateSandboxMetadataAcceptsAnOrdinaryName(t *testing.T) {
	err := validateSandboxMetadata(map[string]string{
		contracts.SandboxNameMetadataKey: "my-box", "tier": "gold",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidateSandboxMetadataIgnoresAbsentNames(t *testing.T) {
	if err := validateSandboxMetadata(map[string]string{"tier": "gold"}); err != nil {
		t.Fatal(err)
	}
	if err := validateSandboxMetadata(map[string]string{}); err != nil {
		t.Fatal(err)
	}
}

// TestValidateSandboxMetadataRejectsUnresolvableNames covers the names that
// could never resolve: one that shadows an identifier, and blank ones.
func TestValidateSandboxMetadataRejectsUnresolvableNames(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		wantIn string
	}{
		{"identifier prefix", contracts.SandboxIDPrefix + "abcdefgh", "identifies a Sandbox"},
		{"bare prefix", contracts.SandboxIDPrefix, "identifies a Sandbox"},
		{"empty", "", "blank"},
		{"whitespace only", "   ", "blank"},
		{"leading whitespace", " my-box", "whitespace"},
		{"trailing whitespace", "my-box ", "whitespace"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSandboxMetadata(map[string]string{
				contracts.SandboxNameMetadataKey: test.value,
			})
			if err == nil || !strings.Contains(err.Error(), test.wantIn) {
				t.Fatalf("error = %v; want one containing %q", err, test.wantIn)
			}
		})
	}
}

// TestValidateSandboxMetadataStillEnforcesItsBounds proves the reserved-name
// check was added to the existing validation rather than replacing it.
func TestValidateSandboxMetadataStillEnforcesItsBounds(t *testing.T) {
	if err := validateSandboxMetadata(nil); err == nil {
		t.Error("absent metadata must still be rejected")
	}
	oversized := make(map[string]string, 33)
	for index := range 33 {
		oversized[strings.Repeat("k", index%12+1)+string(rune('a'+index))] = "v"
	}
	if err := validateSandboxMetadata(oversized); err == nil {
		t.Error("an oversized metadata object must still be rejected")
	}
	if err := validateSandboxMetadata(map[string]string{
		"tier": strings.Repeat("v", 1025),
	}); err == nil {
		t.Error("an oversized value must still be rejected")
	}
}
