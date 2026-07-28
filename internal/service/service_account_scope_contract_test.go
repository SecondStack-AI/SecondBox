package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestServiceAccountScopeAuthorizationMatchesCanonicalOpenAPI(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate ServiceAccountScope contract test source")
	}
	contractPath := filepath.Join(
		filepath.Dir(sourceFile), "..", "..", "contracts", "openapi", "v1", "secondbox.openapi.json",
	)
	contents, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Enum []string `json:"enum"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}
	canonical := document.Components.Schemas["ServiceAccountScope"].Enum
	internal := []string{
		contracts.ScopeSandboxRead,
		contracts.ScopeSandboxLifecycle,
		contracts.ScopeSandboxExec,
		contracts.ScopeSandboxFiles,
		contracts.ScopeSandboxArtifacts,
		contracts.ScopeSandboxPorts,
	}
	sort.Strings(canonical)
	sort.Strings(internal)
	if len(canonical) != len(internal) {
		t.Fatalf("ServiceAccountScope count drift: OpenAPI=%v internal=%v", canonical, internal)
	}
	for index := range canonical {
		if canonical[index] != internal[index] {
			t.Fatalf("ServiceAccountScope drift: OpenAPI=%v internal=%v", canonical, internal)
		}
		if err := validateApplicationScopes([]string{canonical[index]}); err != nil {
			t.Errorf("canonical ServiceAccountScope %q was rejected: %v", canonical[index], err)
		}
	}
	for _, removedScope := range []string{"sandbox:create", "sandbox:manage", "exec", "files", "artifacts", "ports"} {
		if err := validateApplicationScopes([]string{removedScope}); err == nil {
			t.Errorf("removed non-canonical ServiceAccountScope %q was accepted", removedScope)
		}
	}
}
