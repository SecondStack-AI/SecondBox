package deployment_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/deployconfig"
)

func TestDevelopmentManifestBindsStandardResourceAssetsToGeneratedCatalog(t *testing.T) {
	manifestPath, err := deployconfig.InitDevelopment(filepath.Join(t.TempDir(), "deployment"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := deployconfig.Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := os.ReadFile(resolved.Environment["SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH"])
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range resolved.ResourceDocument.Profiles {
		for _, revision := range profile.Revisions {
			for _, digest := range []string{revision.Spec.RuntimeBundleDigest, revision.Spec.ToolchainBundleDigest} {
				if !strings.Contains(string(catalog), digest) {
					t.Errorf("catalog does not bind standard Profile %s digest %s", profile.Name, digest)
				}
			}
		}
	}
}

func TestDeploymentCompilerReplacesLegacyOperatorSurface(t *testing.T) {
	root := repositoryRootForDeploymentPolicy(t)
	for _, removed := range []string{"deploy/environment.example", "deploy/bin/bootstrap-environment.sh", "deploy/bin/validate-environment.sh"} {
		if _, err := os.Stat(filepath.Join(root, removed)); !os.IsNotExist(err) {
			t.Errorf("legacy operator input still exists: %s", removed)
		}
	}
	for _, present := range []string{"deploy/secondbox.example.toml", "deploy/compose.yml", "deploy/compose.development.yml", "deploy/compose.explicit-network.yml", "deploy/compose.bundled-database.yml", "deploy/compose.same-host-runner.yml"} {
		if _, err := os.Stat(filepath.Join(root, present)); err != nil {
			t.Errorf("new deployment artifact missing: %s: %v", present, err)
		}
	}
}

func TestStaticApplicationAuthorityConfigurationIsAbsentFromShippedSurfaces(t *testing.T) {
	root := repositoryRootForDeploymentPolicy(t)
	retired := [][]byte{
		[]byte("SECONDBOX_APPLICATION_" + "AUTHORITIES_JSON"),
		[]byte("application_" + "authorities_file"),
		[]byte("application-" + "authorities.json"),
	}
	for _, directory := range []string{"cmd", "deploy", "internal", "scripts"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, value := range retired {
				if bytes.Contains(content, value) {
					t.Errorf("shipped surface %s retains %q", path, value)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestDevelopmentTenancyBootstrapIsExplicitAndPostStart(t *testing.T) {
	root := repositoryRootForDeploymentPolicy(t)
	path := filepath.Join(root, "scripts", "bootstrap-development-tenancy.sh")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("development tenancy bootstrap is not executable")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	ordered := []string{"platform login", "tenant create", "controller-authority create", "controller login", "subject create", "application-authority create", "application login", "sandboxes list"}
	position := 0
	for _, command := range ordered {
		next := strings.Index(text[position:], command)
		if next < 0 {
			t.Fatalf("development bootstrap lacks ordered command %q", command)
		}
		position += next + len(command)
	}
	justfile, err := os.ReadFile(filepath.Join(root, "Justfile"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(justfile), "deploy-development-up directory:")
	if start < 0 {
		t.Fatal("development startup recipe is absent")
	}
	startup := string(justfile)[start:]
	if next := strings.Index(startup[1:], "\n\n"); next >= 0 {
		startup = startup[:next+1]
	}
	if strings.Contains(startup, "bootstrap-development-tenancy") || strings.Contains(startup, "/v1/tenants") {
		t.Fatal("development startup hides tenancy bootstrap work")
	}
}

func TestDocumentedRunnerTemplateMatchesCommandOutput(t *testing.T) {
	root := repositoryRootForDeploymentPolicy(t)
	documentation, err := os.ReadFile(filepath.Join(root, "docs", "operations", "deployment.md"))
	if err != nil {
		t.Fatal(err)
	}
	prefix := "<!-- runner-template-output:start -->\n```toml\n"
	suffix := "```\n<!-- runner-template-output:end -->"
	start := strings.Index(string(documentation), prefix)
	if start < 0 {
		t.Fatal("deployment documentation has no Runner template output block")
	}
	start += len(prefix)
	end := strings.Index(string(documentation[start:]), suffix)
	if end < 0 {
		t.Fatal("deployment documentation Runner template output block is not closed")
	}
	documented := documentation[start : start+end]
	if string(documented) != string(deployconfig.RunnerTemplate()) {
		t.Fatal("documented Runner template differs from secondbox-deploy runner-template output")
	}
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "secondbox-deploy runner-template") {
		t.Fatal("README same-host path does not point to runner-template")
	}
}

func TestExplicitComposeNetworkOverlayRendersReviewedSubnet(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker Compose is unavailable")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose v2 is unavailable")
	}
	manifestPath, err := deployconfig.InitDevelopment(filepath.Join(t.TempDir(), "deployment"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = []byte(strings.Replace(string(manifest), "compose_project_name = 'secondbox'\n", "compose_project_name = 'secondbox'\ncompose_backend_cidr = '10.254.253.0/24'\n", 1))
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	environmentPath := filepath.Join(filepath.Dir(manifestPath), "generated.env")
	resolved, err := deployconfig.Render(manifestPath, environmentPath)
	if err != nil {
		t.Fatal(err)
	}
	arguments := []string{"compose", "--project-name", "secondbox-network-config-test", "--env-file", environmentPath}
	for _, composeFile := range resolved.ComposeFiles {
		arguments = append(arguments, "--file", composeFile)
	}
	arguments = append(arguments, "config", "--format", "json")
	output, err := exec.Command("docker", arguments...).Output()
	if err != nil {
		t.Fatal(err)
	}
	var model struct {
		Networks map[string]struct {
			IPAM struct {
				Config []struct {
					Subnet string `json:"subnet"`
				} `json:"config"`
			} `json:"ipam"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(output, &model); err != nil {
		t.Fatal(err)
	}
	config := model.Networks["secondbox-backend"].IPAM.Config
	if len(config) != 1 || config[0].Subnet != "10.254.253.0/24" {
		t.Fatalf("rendered Compose backend IPAM = %#v", config)
	}
}

func TestComposeArtifactPreservesAbsentAndSelectedOverrides(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker Compose is unavailable")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose v2 is unavailable")
	}
	manifestPath, err := deployconfig.InitDevelopment(filepath.Join(t.TempDir(), "deployment"))
	if err != nil {
		t.Fatal(err)
	}
	environmentPath := filepath.Join(filepath.Dir(manifestPath), "generated.env")
	if _, err := deployconfig.Render(manifestPath, environmentPath); err != nil {
		t.Fatal(err)
	}
	readEnvironment := func() map[string]any {
		command := exec.Command("docker", "compose", "--project-name", "secondbox-config-test", "--env-file", environmentPath, "--file", filepath.Join(repositoryRootForDeploymentPolicy(t), "deploy", "compose.yml"), "--file", filepath.Join(repositoryRootForDeploymentPolicy(t), "deploy", "compose.development.yml"), "config", "--format", "json")
		output, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		var model struct {
			Services map[string]struct {
				Environment map[string]any `json:"environment"`
			} `json:"services"`
		}
		if err := json.Unmarshal(output, &model); err != nil {
			t.Fatal(err)
		}
		return model.Services["control-plane"].Environment
	}
	environment := readEnvironment()
	for _, name := range []string{"SECONDBOX_HTTP_TIMEOUT_SECONDS", "SECONDBOX_ASSIGNMENT_RETRY_LIMIT", "SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES"} {
		value, exists := environment[name]
		if !exists || value != nil {
			t.Errorf("absent override %s = %#v, exists=%t", name, value, exists)
		}
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = []byte(strings.Replace(string(manifest), "[overrides]\n", "[overrides]\nhttp_timeout_seconds = 41\nassignment_retry_limit = 0\n", 1))
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := deployconfig.Render(manifestPath, environmentPath); err != nil {
		t.Fatal(err)
	}
	environment = readEnvironment()
	if environment["SECONDBOX_HTTP_TIMEOUT_SECONDS"] != "41" || environment["SECONDBOX_ASSIGNMENT_RETRY_LIMIT"] != "0" {
		t.Fatalf("selected overrides = %#v", environment)
	}
}

func TestComposeEnvFileGrammarPreservesMetacharacters(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker Compose is unavailable")
	}
	value := `$dollar ${nested} 'single' "double" \ slash {"json":"literal"}`
	encoded, err := deployconfig.EncodeComposeEnvironment(map[string]string{"PROBE_VALUE": value})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	envPath := filepath.Join(directory, "probe.env")
	composePath := filepath.Join(directory, "compose.yml")
	if err := os.WriteFile(envPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	compose := []byte("services:\n  probe:\n    image: busybox:1.36\n    environment:\n      PROBE_VALUE: ${PROBE_VALUE}\n    command: [\"/bin/sh\", \"-c\", \"printf %s \\\"$$PROBE_VALUE\\\"\"]\n")
	if err := os.WriteFile(composePath, compose, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("docker", "compose", "--project-name", "secondbox-encoder-test", "--env-file", envPath, "--file", composePath, "config", "--format", "json")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("Compose parser: %v", err)
	}
	var model struct {
		Services map[string]struct {
			Environment map[string]any `json:"environment"`
		} `json:"services"`
	}
	if err := json.Unmarshal(output, &model); err != nil {
		t.Fatal(err)
	}
	if got := model.Services["probe"].Environment["PROBE_VALUE"]; got != strings.ReplaceAll(value, "$", "$$") {
		t.Fatalf("Compose canonical environment = %#v, want dollar-escaped %q", got, value)
	}
	if exec.Command("docker", "image", "inspect", "busybox:1.36").Run() == nil {
		probe := exec.Command("docker", "compose", "--project-name", "secondbox-encoder-probe", "--env-file", envPath, "--file", composePath, "run", "--rm", "--no-deps", "probe")
		actual, err := probe.Output()
		if err != nil {
			t.Fatalf("real Compose probe: %v", err)
		}
		if string(actual) != value {
			t.Fatalf("real process value = %q, want %q", actual, value)
		}
	}
}

func TestSystemdEnvironmentFileConsumerPreservesMetacharacters(t *testing.T) {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run is unavailable")
	}
	value := `$dollar ${nested} 'single' "double" \ slash {"json":"literal"}`
	encoded, err := deployconfig.EncodeSystemdEnvironment(map[string]string{"SECONDBOX_SYSTEMD_PROBE": value})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runner.env")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("systemd-run", "--user", "--wait", "--pipe", "--quiet", "--property=EnvironmentFile="+path, "/usr/bin/env")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Skipf("qualified systemd user manager unavailable: %v: %s", err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if line == "SECONDBOX_SYSTEMD_PROBE="+value {
			return
		}
	}
	t.Fatalf("systemd EnvironmentFile changed the probe value: %s", output)
}
