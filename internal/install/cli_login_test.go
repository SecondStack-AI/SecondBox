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
		if request.URL.Path != "/v1/tenants" || request.URL.Query().Get("limit") != "1" || request.Header.Get("Authorization") != "Bearer platform-token" {
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
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	oldConfiguration := []byte("{\n  \"url\": \"http://127.0.0.1:9000\",\n  \"token\": \"older-token\",\n  \"authorityKind\": \"platform\"\n}\n")
	if err := os.WriteFile(configPath, oldConfiguration, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := InstallPlan{
		HostFacts: HostFacts{InvokingUID: uid, InvokingGID: gid},
		Network:   NetworkPlan{APIAddress: strings.TrimPrefix(server.URL, "http://")},
		CLI:       CLIPlan{ConfigPath: configPath},
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
	if string(content) == string(oldConfiguration) || strings.Contains(string(content), "older-token") {
		t.Fatal("older SecondBox CLI configuration was not atomically upgraded")
	}
	if !strings.Contains(string(content), `"authorityKind": "platform"`) ||
		strings.Contains(string(content), "tenantRef") || strings.Contains(string(content), "subjectRef") {
		t.Fatalf("installed CLI configuration is not platform-only:\n%s", content)
	}
}

func TestValidateCLIConfigurationTargetRefusesUnrelatedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	original := []byte("unrelated configuration\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := InstallPlan{
		HostFacts: HostFacts{InvokingUID: int64(os.Getuid()), InvokingGID: int64(os.Getgid())},
		CLI:       CLIPlan{ConfigPath: path},
		Paths:     []PlannedPath{plannedPath("cli-config", path, PathUserDeployment, ResourceFile, 0o600, int64(os.Getuid()), int64(os.Getgid()), false, true)},
	}
	if err := validateCLIConfigurationTarget(plan); err == nil || !strings.Contains(err.Error(), "not a SecondBox session document") {
		t.Fatalf("unrelated CLI configuration error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(original) {
		t.Fatal("unrelated CLI configuration was modified")
	}
}

func TestValidateCLIConfigurationTargetRefusesApplicationCredentialAsPlatform(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	content := []byte("{\n  \"url\": \"https://secondbox.example\",\n  \"token\": \"application-token\",\n  \"authorityKind\": \"application\"\n}\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := InstallPlan{
		HostFacts: HostFacts{InvokingUID: int64(os.Getuid()), InvokingGID: int64(os.Getgid())},
		CLI:       CLIPlan{ConfigPath: path},
		Paths:     []PlannedPath{plannedPath("cli-config", path, PathUserDeployment, ResourceFile, 0o600, int64(os.Getuid()), int64(os.Getgid()), false, true)},
	}
	if err := validateCLIConfigurationTarget(plan); err == nil || !strings.Contains(err.Error(), "must contain a platform authority") {
		t.Fatalf("application credential replacement error = %v", err)
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(content) {
		t.Fatal("rejected application credential was modified")
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
