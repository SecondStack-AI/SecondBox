package executionimage

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestTenantRegistryAuthentication(t *testing.T) {
	root := t.TempDir()
	tokenFile := filepath.Join(root, "token")
	if err := os.WriteFile(tokenFile, []byte("pull-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loginFile := filepath.Join(root, "config.json")
	encoded := base64.StdEncoding.EncodeToString([]byte("_json_key:{\"private_key\":\"test\"}"))
	if err := os.WriteFile(loginFile, []byte(`{"auths":{"registry.example":{"auth":"`+encoded+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, mode, username, token, config, want string }{
		{"public", "anonymous", "", "", "", ""},
		{"token", "token", "puller", tokenFile, "", base64.StdEncoding.EncodeToString([]byte("puller:pull-token"))},
		{"docker login", "docker_config", "", "", loginFile, encoded},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := TenantRegistry{Registry: "registry.example", Repositories: []string{"team/agent"}, Authentication: RegistryAuthentication{Mode: test.mode, Username: test.username, TokenFile: test.token, DockerConfigFile: test.config}}
			document, err := registry.AuthenticationDocument("registry.example/team/agent:stable")
			if err != nil {
				t.Fatal(err)
			}
			var auth struct {
				Auths map[string]struct {
					Auth string `json:"auth"`
				} `json:"auths"`
			}
			if err := json.Unmarshal(document, &auth); err != nil {
				t.Fatal(err)
			}
			if auth.Auths["registry.example"].Auth != test.want {
				t.Fatal("unexpected registry credential")
			}
			for _, denied := range []string{"other.example/team/agent:stable", "registry.example/other/agent:stable", "registry.example/team/agent-extra:stable"} {
				if _, err := registry.AuthenticationDocument(denied); err == nil {
					t.Fatalf("accepted ungranted image %s", denied)
				}
			}
		})
	}
}
