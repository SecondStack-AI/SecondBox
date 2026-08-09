package install

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginCLIRecordsDigestForPurgeFence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/sandboxes" || request.Header.Get("Authorization") != "Bearer platform-token" {
			t.Errorf("CLI authority verification request = %s, %q", request.URL.Path, request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()

	root := t.TempDir()
	uid, gid := int64(os.Getuid()), int64(os.Getgid())
	tokenPath := filepath.Join(root, "platform-token")
	if err := os.WriteFile(tokenPath, []byte("platform-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configRoot := filepath.Join(root, "config")
	configDirectory := filepath.Join(configRoot, "secondbox")
	configPath := filepath.Join(configDirectory, "config.json")
	plan := InstallPlan{
		HostFacts: HostFacts{InvokingUID: uid, InvokingGID: gid},
		Network:   NetworkPlan{APIAddress: strings.TrimPrefix(server.URL, "http://")},
		CLI:       CLIPlan{ConfigPath: configPath, TenantRef: "tenant", SubjectRef: "subject"},
		Paths: []PlannedPath{
			plannedPath("cli-config-root", configRoot, PathUserDeployment, ResourceDirectory, 0o700, uid, gid, false, true),
			plannedPath("cli-config-directory", configDirectory, PathUserDeployment, ResourceDirectory, 0o700, uid, gid, false, true),
			plannedPath("cli-config", configPath, PathUserDeployment, ResourceFile, 0o600, uid, gid, false, true),
		},
		SecretTargets: []SecretTarget{{Category: "platform-authority", Path: tokenPath}},
	}
	resources, err := LoginCLI(context.Background(), plan, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	resource := resources[len(resources)-1]
	if resource.ID != "cli-config" || resource.Digest != Digest(content) {
		t.Fatalf("CLI configuration deletion fence = %#v", resource)
	}
}

func TestEnsureOwnedDirectoryRejectsWritableExistingBoundary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "existing")
	if err := os.Mkdir(path, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o775); err != nil {
		t.Fatal(err)
	}
	planned := plannedPath("existing", path, PathUserDeployment, ResourceDirectory, 0o755, int64(os.Getuid()), int64(os.Getgid()), false, true)
	if _, err := ensureOwnedDirectory(planned); err == nil || !strings.Contains(err.Error(), "safe invoking-user directory boundary") {
		t.Fatalf("unsafe existing directory error = %v", err)
	}
}
