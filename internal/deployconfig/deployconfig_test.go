package deployconfig

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type recordingComposeExecutor struct{ calls [][]string }

func (executor *recordingComposeExecutor) Run(_ context.Context, arguments []string) error {
	executor.calls = append(executor.calls, slices.Clone(arguments))
	return nil
}

func initializedDevelopment(t *testing.T) string {
	t.Helper()
	path, err := InitDevelopment(filepath.Join(t.TempDir(), "deployment"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDevelopmentInitializationAndRenderAreCompleteAndReproducible(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifestInfo.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %o", manifestInfo.Mode().Perm())
	}
	secretInfo, err := os.Stat(filepath.Join(filepath.Dir(manifestPath), "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if secretInfo.Mode().Perm() != 0o700 {
		t.Fatalf("secret directory mode = %o", secretInfo.Mode().Perm())
	}
	resolved, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Manifest.Runners) != 0 {
		t.Fatalf("development init started with runners: %#v", resolved.Manifest.Runners)
	}
	for _, definition := range OverrideRegistry() {
		if _, exists := resolved.Environment[definition.Environment]; exists {
			t.Errorf("unset override %s was rendered", definition.Environment)
		}
	}
	for _, name := range []string{"SECONDBOX_RUNNER_PROTOCOL_MINIMUM", "SECONDBOX_RUNNER_PROTOCOL_MAXIMUM"} {
		if _, exists := resolved.Environment[name]; exists {
			t.Errorf("removed protocol declaration %s was rendered", name)
		}
	}
	first := filepath.Join(filepath.Dir(manifestPath), "generated.env")
	if _, err := Render(manifestPath, first); err != nil {
		t.Fatal(err)
	}
	firstBytes, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("SECONDBOX_PLATFORM_TOKEN=poison\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(manifestPath, first); err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("deterministic rerender did not overwrite the altered artifact")
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("environment mode = %o", info.Mode().Perm())
	}
	resolved, err = Render(manifestPath, first)
	if err != nil {
		t.Fatal(err)
	}
	for _, composePath := range resolved.ComposeFiles {
		if !filepath.IsAbs(composePath) {
			t.Errorf("rendered Compose path is relative: %s", composePath)
		}
		if info, err := os.Stat(composePath); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Errorf("rendered Compose asset %s = %v, %v", composePath, info, err)
		}
	}
}

func TestExampleManifestIsGeneratedFromTheRegistry(t *testing.T) {
	manifest := developmentManifest("secrets/postgres-password", "secrets/platform-token", "secrets/runner-enrollment-credential")
	for _, pool := range manifest.StandardResources.RunnerPools {
		if !slices.Contains(pool.Capabilities, "compute") {
			t.Fatalf("development RunnerPool %q cannot admit compute", pool.Name)
		}
	}
	want, err := encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("..", "..", "deploy", "secondbox.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("deploy/secondbox.example.toml drifted from the typed schema or override registry")
	}
	if len(OverrideRegistry()) != 15 {
		t.Fatalf("override count = %d", len(OverrideRegistry()))
	}
}

func TestComposeProjectIsolatesDeploymentsAndDefaultsForOlderManifests(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(original, []byte("compose_project_name = 'secondbox'")) {
		t.Fatal("development initialization did not state its Compose project explicitly")
	}
	resolved, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.ComposeProject(); got != DefaultComposeProjectName {
		t.Fatalf("initialized Compose project = %q", got)
	}

	// A manifest written before the field existed keeps deploying under the
	// project it always used.
	absent := bytes.Replace(original, []byte("compose_project_name = 'secondbox'\n"), nil, 1)
	if bytes.Equal(absent, original) {
		t.Fatal("Compose project line was not removed")
	}
	absentPath := filepath.Join(filepath.Dir(manifestPath), "absent.toml")
	if err := os.WriteFile(absentPath, absent, 0o600); err != nil {
		t.Fatal(err)
	}
	resolvedAbsent, err := Resolve(absentPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolvedAbsent.ComposeProject(); got != DefaultComposeProjectName {
		t.Fatalf("absent Compose project = %q", got)
	}

	// A second deployment on the same host owns a distinct project, which is
	// what keeps Compose from binding this deployment's volumes.
	distinct := bytes.Replace(original, []byte("compose_project_name = 'secondbox'"), []byte("compose_project_name = 'secondbox-v030-test'"), 1)
	distinctPath := filepath.Join(filepath.Dir(manifestPath), "distinct.toml")
	if err := os.WriteFile(distinctPath, distinct, 0o600); err != nil {
		t.Fatal(err)
	}
	resolvedDistinct, err := Resolve(distinctPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolvedDistinct.ComposeProject(); got != "secondbox-v030-test" {
		t.Fatalf("distinct Compose project = %q", got)
	}
}

func TestExplicitComposeBackendCIDRSelectsIPAMOverlay(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Deployment.ComposeBackendCIDR = "10.42.0.0/24"
	resolved, err := resolveManifest(manifest, filepath.Dir(manifestPath))
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Environment["SECONDBOX_COMPOSE_BACKEND_CIDR"] != "10.42.0.0/24" || !slices.Contains(resolved.ComposeFiles, "deploy/compose.explicit-network.yml") {
		t.Fatalf("explicit Compose network resolution = env %q, files %#v", resolved.Environment["SECONDBOX_COMPOSE_BACKEND_CIDR"], resolved.ComposeFiles)
	}
}

func TestPermanentComposePurgeRemovesExactProjectVolumes(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	executor := &recordingComposeExecutor{}
	if err := PurgeComposeVolumes(context.Background(), manifestPath, executor); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("Compose purge calls = %#v", executor.calls)
	}
	arguments := executor.calls[0]
	if !slices.Contains(arguments, "--project-name") || !slices.Contains(arguments, DefaultComposeProjectName) || !slices.Equal(arguments[len(arguments)-3:], []string{"down", "--remove-orphans", "--volumes"}) {
		t.Fatalf("Compose purge arguments = %#v", arguments)
	}
}

func TestComposeCanFenceControlPlaneWithoutStoppingDatabase(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	executor := &recordingComposeExecutor{}
	if err := RunCompose(context.Background(), manifestPath, "stop-control-plane", executor, nil); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("Compose control-plane fence calls = %#v", executor.calls)
	}
	arguments := executor.calls[0]
	if !slices.Contains(arguments, "--project-name") || !slices.Equal(arguments[len(arguments)-2:], []string{"stop", "control-plane"}) {
		t.Fatalf("Compose control-plane fence arguments = %#v", arguments)
	}
}

func TestComposeDiagnosticsUseInstalledMaterializedAssets(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	environmentPath := filepath.Join(filepath.Dir(manifestPath), ".secondbox.generated.env")
	resolved, err := Render(manifestPath, environmentPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	arguments, err := ComposeDiagnosticArguments(manifestPath, "ps")
	if err != nil {
		t.Fatal(err)
	}
	for _, materialized := range resolved.ComposeFiles {
		if !filepath.IsAbs(materialized) || !slices.Contains(arguments, materialized) {
			t.Fatalf("diagnostic arguments do not use materialized Compose asset %q: %#v", materialized, arguments)
		}
	}
}

func TestStrictDecodeRejectsUnknownDuplicateAndUnsupportedSchema(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{"unknown": append(append([]byte{}, original...), []byte("\nunknown = true\n")...), "duplicate": append(append([]byte{}, original...), []byte("\nschema_version = 1\n")...), "schema": bytes.Replace(original, []byte("schema_version = 1"), []byte("schema_version = 2"), 1)}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secondbox.toml")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve(path); err == nil || !strings.Contains(err.Error(), "SecondBox deployment manifest") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestManifestValidationRejectsUnsafeDeploymentInputs(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*ManifestV1)
	}{
		{name: "missing development wait timeout", want: "development_prepare_wait_timeout_seconds is required", mutate: func(manifest *ManifestV1) { manifest.Deployment.DevelopmentWaitSeconds = nil }},
		{name: "control plane published beyond loopback", want: "bind every published port to 127.0.0.1", mutate: func(manifest *ManifestV1) { manifest.Deployment.APIBindIP = "0.0.0.0" }},
		{name: "Runner endpoint published beyond loopback", want: "bind every published port to 127.0.0.1", mutate: func(manifest *ManifestV1) { manifest.Deployment.RunnerBindIP = "0.0.0.0" }},
		{name: "database published beyond loopback", want: "bind every published port to 127.0.0.1", mutate: func(manifest *ManifestV1) { manifest.Database.BindIP = "0.0.0.0" }},
		{name: "control plane listener mismatches container", want: "listen_address must be 0.0.0.0:8080", mutate: func(manifest *ManifestV1) { manifest.Deployment.ListenAddress = "0.0.0.0:9999" }},
		{name: "Runner listener mismatches container", want: "runner_listen_address must be 0.0.0.0:9443", mutate: func(manifest *ManifestV1) { manifest.Deployment.RunnerListenAddress = "0.0.0.0:9999" }},
		{name: "asset catalog path mismatches container", want: "signed_asset_catalog_path must be /etc/secondbox/signed-assets.json", mutate: func(manifest *ManifestV1) {
			manifest.Deployment.SignedAssetCatalogPath = "/different/signed-assets.json"
		}},
		{name: "Compose project name carries uppercase", want: "deployment.compose_project_name", mutate: func(manifest *ManifestV1) { manifest.Deployment.ComposeProjectName = "SecondBox" }},
		{name: "Compose project name starts with a hyphen", want: "deployment.compose_project_name", mutate: func(manifest *ManifestV1) { manifest.Deployment.ComposeProjectName = "-secondbox" }},
		{name: "Compose project name carries a forbidden byte", want: "deployment.compose_project_name", mutate: func(manifest *ManifestV1) { manifest.Deployment.ComposeProjectName = "secondbox/test" }},
		{name: "Compose project name exceeds the bound", want: "deployment.compose_project_name", mutate: func(manifest *ManifestV1) {
			manifest.Deployment.ComposeProjectName = strings.Repeat("a", 64)
		}},
		{name: "Compose backend CIDR is not a network", want: "deployment.compose_backend_cidr", mutate: func(manifest *ManifestV1) { manifest.Deployment.ComposeBackendCIDR = "10.42.0.1/24" }},
		{name: "Compose backend CIDR is public", want: "deployment.compose_backend_cidr", mutate: func(manifest *ManifestV1) { manifest.Deployment.ComposeBackendCIDR = "198.51.100.0/24" }},
		{name: "Compose backend CIDR is undersized", want: "deployment.compose_backend_cidr", mutate: func(manifest *ManifestV1) { manifest.Deployment.ComposeBackendCIDR = "10.42.0.0/30" }},
		{name: "Compose backend CIDR overlaps same-host guests", want: "deployment.compose_backend_cidr must not overlap runners[0].sandbox_guest_cidr", mutate: func(manifest *ManifestV1) {
			runner := validSameHostTestRunner("runner-local")
			manifest.Runners = []Runner{runner}
			manifest.Deployment.ComposeBackendCIDR = runner.SandboxGuestCIDR
		}},
		{name: "Compose backend CIDR overlaps broader same-host bridge", want: "deployment.compose_backend_cidr must not overlap runners[0].sandbox_bridge_cidr", mutate: func(manifest *ManifestV1) {
			runner := validSameHostTestRunner("runner-local")
			runner.SandboxBridgeCIDR = "10.42.0.1/16"
			runner.SandboxGuestCIDR = "10.42.0.0/24"
			manifest.Runners = []Runner{runner}
			manifest.Deployment.ComposeBackendCIDR = "10.42.1.0/24"
		}},
		{name: "control plane port exceeds TCP range", want: "deployment.api_published_port", mutate: func(manifest *ManifestV1) { manifest.Deployment.APIPublishedPort = integer(70000) }},
		{name: "Runner port exceeds TCP range", want: "deployment.runner_published_port", mutate: func(manifest *ManifestV1) { manifest.Deployment.RunnerPublishedPort = integer(70000) }},
		{name: "database port exceeds TCP range", want: "database.published_port", mutate: func(manifest *ManifestV1) { manifest.Database.PublishedPort = integer(70000) }},
		{name: "unsupported Runner feature", want: "runner feature \"unsupported-feature\" is unsupported", mutate: func(manifest *ManifestV1) {
			manifest.Policy.RunnerEnabledFeatures = "local-workspace,unsupported-feature"
		}},
		{name: "Runner features omit local workspace", want: "runner features require local-workspace", mutate: func(manifest *ManifestV1) { manifest.Policy.RunnerEnabledFeatures = "evidence" }},
		{name: "standard resources absent", want: "standard_resources.artifact_manifest is required", mutate: func(manifest *ManifestV1) { manifest.StandardResources = StandardResources{} }},
		{name: "standard bundle duplicate", want: "unique agent-compartment or durable-coding", mutate: func(manifest *ManifestV1) {
			manifest.StandardResources.Bundles = []string{"agent-compartment", "agent-compartment"}
		}},
		{name: "standard bundle has no pool", want: "must bind selected bundle durable-coding", mutate: func(manifest *ManifestV1) {
			manifest.StandardResources.RunnerPools = manifest.StandardResources.RunnerPools[:1]
		}},
		{name: "standard pool capacity absent", want: "max_sandboxes must be positive", mutate: func(manifest *ManifestV1) { manifest.StandardResources.RunnerPools[0].MaxSandboxes = nil }},
		{name: "standard gateway unresolved", want: "must resolve agent-gateway.secondbox.internal", mutate: func(manifest *ManifestV1) {
			runner := validTestRunner("runner-a", "remote")
			runner.PoolID = "standard-amd64"
			runner.NetworkPolicyRunnerGateways = "platform-gateway.secondbox.internal=172.30.0.1"
			manifest.Runners = []Runner{runner}
		}},
		{name: "Compose environment contains newline", want: "forbidden byte", mutate: func(manifest *ManifestV1) { manifest.Deployment.ControlPlaneImage = "image\npoison" }},
		{name: "remote Runner environment contains newline", want: "invalid generated systemd environment", mutate: func(manifest *ManifestV1) {
			runner := validTestRunner("runner-a", "remote")
			runner.LogPath = "/var/log/runner\npoison"
			manifest.Runners = []Runner{runner}
		}},
		{name: "Runner ID escapes artifact directory", want: "valid opaque Runner ID", mutate: func(manifest *ManifestV1) { manifest.Runners = []Runner{validTestRunner("../escaped", "remote")} }},
		{name: "remote Runner path is relative", want: "identity_directory must be an absolute Runner-host path", mutate: func(manifest *ManifestV1) {
			runner := validTestRunner("runner-a", "remote")
			runner.IdentityDirectory = "relative/identity"
			manifest.Runners = []Runner{runner}
		}},
		{name: "same-host identity path misses fixed mount", want: "identity_directory must be /run/secondbox-runner-identity", mutate: func(manifest *ManifestV1) {
			runner := validSameHostTestRunner("runner-local")
			runner.IdentityDirectory = "/different/identity"
			manifest.Runners = []Runner{runner}
		}},
		{name: "same-host artifact path misses fixed mount", want: "firecracker_kernel_path must be within /opt/secondbox-artifacts", mutate: func(manifest *ManifestV1) {
			runner := validSameHostTestRunner("runner-local")
			runner.FirecrackerKernelPath = "/different/kernel"
			manifest.Runners = []Runner{runner}
		}},
		{name: "same-host state path misses fixed mount", want: "firecracker_run_directory must be within /var/lib/secondbox-runner", mutate: func(manifest *ManifestV1) {
			runner := validSameHostTestRunner("runner-local")
			runner.FirecrackerRunDirectory = "/different/run"
			manifest.Runners = []Runner{runner}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifestPath := initializedDevelopment(t)
			manifest, err := ReadManifest(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&manifest)
			encoded, err := encodeManifest(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestManifestValidationUsesTheRuntimeAssetCatalogSchema(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(filepath.Dir(manifestPath), manifest.Deployment.SignedAssetCatalog)
	incomplete := `{"assets":[{"manifestDigest":"` + developmentRuntimeDigest + `","signatureKeyId":"review-key"}]}`
	if err := os.WriteFile(catalogPath, []byte(incomplete), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "incomplete trust evidence") {
		t.Fatalf("runtime catalog schema error = %v", err)
	}
}

func TestBundledDatabaseURLPreservesUserInfoBytes(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	password := "a long password+with/slash?and#fragment"
	passwordPath := filepath.Join(filepath.Dir(manifestPath), manifest.Database.PasswordFile)
	if err := os.WriteFile(passwordPath, []byte(password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.Database.User = "second box"
	encoded, err := encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(resolved.Environment["SECONDBOX_DATABASE_URL"])
	if err != nil {
		t.Fatal(err)
	}
	gotPassword, ok := parsed.User.Password()
	if parsed.User.Username() != manifest.Database.User || !ok || gotPassword != password {
		t.Fatalf("database userinfo = %q/%q/%t", parsed.User.Username(), gotPassword, ok)
	}
}

func TestBundledDatabasePasswordRequiresLegacyMinimumStrength(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	passwordPath := filepath.Join(filepath.Dir(manifestPath), manifest.Database.PasswordFile)
	if err := os.WriteFile(passwordPath, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "database.password_file must contain at least 24 bytes") {
		t.Fatalf("short bundled database password error = %v", err)
	}
}

func TestProductionExternalDatabaseURLRequiresEffectiveVerifiedTLS(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "verified TLS", value: "postgresql://secondbox:secret@database.example/secondbox?sslmode=verify-full", valid: true},
		{name: "userinfo decoy", value: "postgresql://secondbox:sslmode=verify-full@database.example/secondbox?sslmode=disable"},
		{name: "path decoy", value: "postgresql://database.example/sslmode=verify-full?sslmode=disable"},
		{name: "duplicate override", value: "postgresql://database.example/secondbox?sslmode=verify-full&sslmode=disable"},
		{name: "absent mode", value: "postgresql://database.example/secondbox"},
		{name: "not a PostgreSQL URL", value: "sslmode=verify-full"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateExternalDatabaseURL(test.value, true)
			if (err == nil) != test.valid {
				t.Fatalf("error = %v, valid = %t", err, test.valid)
			}
		})
	}
}

func TestCredentialsRemainSeparateAcrossTrustBoundaries(t *testing.T) {
	for name, writeCollision := range map[string]func(string, ManifestV1, []byte) error{
		"platform token": func(base string, manifest ManifestV1, runnerCredential []byte) error {
			return os.WriteFile(filepath.Join(base, manifest.Applications.PlatformTokenFile), runnerCredential, 0o600)
		},
		"application authority token": func(base string, manifest ManifestV1, runnerCredential []byte) error {
			authorities := []map[string]any{{"id": "review", "token": strings.TrimSpace(string(runnerCredential)), "tenantRef": "tenant", "subjectRef": "subject", "scopes": []string{"sandbox:read"}, "profileGrants": []string{"agent-compartment"}}}
			content, err := json.Marshal(authorities)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(base, manifest.Applications.ApplicationAuthoritiesFile), append(content, '\n'), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifestPath := initializedDevelopment(t)
			manifest, err := ReadManifest(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			base := filepath.Dir(manifestPath)
			runnerCredential, err := os.ReadFile(filepath.Join(base, manifest.RunnerTrust.EnrollmentCredentialFile))
			if err != nil {
				t.Fatal(err)
			}
			if err := writeCollision(base, manifest, runnerCredential); err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "distinct credentials for separate trust boundaries") {
				t.Fatalf("credential separation error = %v", err)
			}
		})
	}
}

func TestOverridesPreserveAbsenceAndExactValues(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Overrides.HTTPTimeoutSeconds = integer(41)
	manifest.Overrides.AssignmentRetryLimit = integer(0)
	encoded, err := encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Environment["SECONDBOX_HTTP_TIMEOUT_SECONDS"] != "41" || resolved.Environment["SECONDBOX_ASSIGNMENT_RETRY_LIMIT"] != "0" {
		t.Fatalf("overrides = %#v", resolved.Environment)
	}
}

func TestResolutionIgnoresAmbientSecondBoxAndComposeVariables(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	t.Setenv("SECONDBOX_PLATFORM_TOKEN", "poison")
	t.Setenv("SECONDBOX_HTTP_TIMEOUT_SECONDS", "999")
	t.Setenv("COMPOSE_FILE", "poison.yml")
	first, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SECONDBOX_PLATFORM_TOKEN", "different")
	t.Setenv("COMPOSE_PROFILES", "same-host-runner")
	second, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Environment, second.Environment) || len(second.ComposeFiles) != 2 {
		t.Fatal("ambient environment changed the resolved deployment")
	}
}

func TestSecretAndArtifactEncodersPreserveLiteralBytes(t *testing.T) {
	values := map[string]string{"DOLLAR": "$VALUE ${OTHER}", "QUOTES": "single' double\" backslash\\ space"}
	compose, err := EncodeComposeEnvironment(values)
	if err != nil {
		t.Fatal(err)
	}
	systemd, err := EncodeSystemdEnvironment(values)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"$VALUE ${OTHER}", "single\\' double\" backslash\\ space"} {
		if !bytes.Contains(compose, []byte(needle)) {
			t.Errorf("Compose encoding missing %q: %s", needle, compose)
		}
	}
	for _, needle := range []string{"$VALUE ${OTHER}", `double\" backslash\\ space`} {
		if !bytes.Contains(systemd, []byte(needle)) {
			t.Errorf("systemd encoding missing %q: %s", needle, systemd)
		}
	}
	for _, bad := range []string{"line\nbreak", "carriage\rreturn", "nul\x00byte"} {
		if _, err := EncodeComposeEnvironment(map[string]string{"BAD": bad}); err == nil {
			t.Errorf("Compose accepted %q", bad)
		}
		if _, err := EncodeSystemdEnvironment(map[string]string{"BAD": bad}); err == nil {
			t.Errorf("systemd accepted %q", bad)
		}
	}
}

func TestSecretResolutionRejectsSymlinksAdditionalLinesAndCarriageReturns(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(filepath.Dir(manifestPath), "secrets", "platform-token")
	tests := map[string][]byte{"lines": []byte("one\ntwo\n"), "carriage": []byte("one\r\n")}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(secretPath, value, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve(manifestPath); err == nil {
				t.Fatal("invalid secret resolved")
			}
		})
	}
	target := filepath.Join(filepath.Dir(manifestPath), "secrets", "real-token")
	if err := os.WriteFile(target, []byte(strings.Repeat("x", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(secretPath)
	if err := os.Symlink(target, secretPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(manifestPath); err == nil {
		t.Fatal("symbolic-link secret resolved")
	}
	_ = manifest
}

func TestSecretAndPrivateKeyReferencesRejectPermissiveModes(t *testing.T) {
	for _, reference := range []string{"secrets/platform-token", "secrets/runner-pki/runner-ca.key", "secrets/runner-pki/server.key"} {
		t.Run(filepath.Base(reference), func(t *testing.T) {
			manifestPath := initializedDevelopment(t)
			path := filepath.Join(filepath.Dir(manifestPath), reference)
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "private file permissions") {
				t.Fatalf("permissive private file error = %v", err)
			}
		})
	}
}

func TestRunnerInitAcceptsTheValidatedPKCS8CAKeyFormat(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	caKeyPath := filepath.Join(filepath.Dir(manifestPath), manifest.RunnerTrust.CAPrivateKeyFile)
	content, err := os.ReadFile(caKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	block, remainder := pem.Decode(content)
	if block == nil || len(remainder) != 0 {
		t.Fatal("generated CA key is not one PEM block")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.Runners = []Runner{validTestRunner("runner-pkcs8", "remote")}
	encoded, err := encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := RunnerInit(manifestPath, "runner-pkcs8", filepath.Join(t.TempDir(), "runner-pkcs8")); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerInitValidatesTheManifestBeforeIssuingIdentity(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Runners = []Runner{validTestRunner("runner-invalid-manifest", "remote")}
	manifest.RunnerTrust.CertificateLifetimeDays = nil
	encoded, err := encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	err = RunnerInit(manifestPath, "runner-invalid-manifest", filepath.Join(t.TempDir(), "identity"))
	if err == nil || !strings.Contains(err.Error(), "runner_trust.certificate_lifetime_days must be positive") {
		t.Fatalf("runner-init invalid manifest error = %v", err)
	}
}

func TestInspectRedactsSecretValuesAndPathsAndShowsAllDefaults(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	provisionSameHostTestRunner(t, manifestPath, "runner-local")
	resolved, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	output, err := Inspect(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, secret := range []string{resolved.Environment["SECONDBOX_PLATFORM_TOKEN"], resolved.Environment["SECONDBOX_RUNNER_CREDENTIAL"], resolved.SecretPaths["applications.platform_token_file"], resolved.Environment["SECONDBOX_RUNNER_PKI_HOST_DIR"], resolved.Environment["SECONDBOX_RUNNER_IDENTITY_HOST_DIR"]} {
		if strings.Contains(text, secret) {
			t.Errorf("inspect exposed secret material %q", secret)
		}
	}
	if strings.Count(text, "codeDefault") != 15 {
		t.Fatalf("inspect defaults = %d, want 15", strings.Count(text, "codeDefault"))
	}
	if !strings.Contains(text, `"name": "data_plane_retention_seconds"`) || !strings.Contains(text, dataPlaneRetentionHelp) {
		t.Fatalf("inspect omitted retention policy help: %s", text)
	}
}

func TestRenderRefusesSymlinkedRunnerArtifactDirectory(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	environmentPath := filepath.Join(t.TempDir(), "generated.env")
	victimDirectory := t.TempDir()
	victimPath := filepath.Join(victimDirectory, "keep.env")
	if err := os.WriteFile(victimPath, []byte(generatedEnvironmentHeader+"KEEP='true'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimDirectory, environmentPath+".runners"); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(manifestPath, environmentPath); err == nil || !strings.Contains(err.Error(), "non-symbolic-link directory") {
		t.Fatalf("render symlinked runner directory error = %v", err)
	}
	if _, err := os.Stat(victimPath); err != nil {
		t.Fatalf("render removed file through symlink: %v", err)
	}
}

func TestProductionInitializationReportsEveryUnresolvedDecisionGroup(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "production")
	path, err := InitProduction(directory)
	if path == "" || err == nil {
		t.Fatalf("path, error = %q, %v", path, err)
	}
	for _, group := range []string{"deployment", "database", "Runner topology", "execution-asset trust", "application authorities", "tenancy policy", "lifecycle policy"} {
		if !strings.Contains(err.Error(), group) {
			t.Errorf("production error omitted %q: %v", group, err)
		}
	}
	if info, statErr := os.Stat(path); statErr != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("production skeleton = %v, %v", info, statErr)
	}
}

func TestProductionInitializationCleansUpFailedSkeletonWrite(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "production")
	wantErr := errors.New("injected skeleton write failure")
	if _, err := initProduction(directory, func(string, []byte) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("production write error = %v", err)
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("failed production target was retained: %v", err)
	}
	if path, err := InitProduction(directory); path == "" || err == nil {
		t.Fatalf("production retry path, error = %q, %v", path, err)
	}
}

func TestProductionInitializationRefusesSymlinkedParent(t *testing.T) {
	realParent := t.TempDir()
	symlinkParent := filepath.Join(t.TempDir(), "linked-parent")
	if err := os.Symlink(realParent, symlinkParent); err != nil {
		t.Fatal(err)
	}
	if _, err := InitProduction(filepath.Join(symlinkParent, "production")); err == nil || !strings.Contains(err.Error(), "non-symbolic-link directory") {
		t.Fatalf("symlinked production parent error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(realParent, "production")); !os.IsNotExist(err) {
		t.Fatalf("production skeleton was written through symlink: %v", err)
	}
}

func TestProductionQualifiesBundledAndExternalDatabase(t *testing.T) {
	for _, databaseMode := range []string{"bundled", "external"} {
		t.Run(databaseMode+"_database", func(t *testing.T) {
			manifestPath := initializedDevelopment(t)
			manifest, err := ReadManifest(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			digestRef := "registry.example/secondbox@sha256:" + strings.Repeat("a", 64)
			manifest.Deployment.Mode = "production"
			manifest.Deployment.PublicBaseURL = "https://secondbox.example.com"
			manifest.Deployment.TLSTermination = "external"
			manifest.Deployment.ControlPlaneImage = digestRef
			manifest.Deployment.RunnerImage = digestRef
			manifest.Deployment.PostgresImage = digestRef
			if databaseMode == "external" {
				urlPath := filepath.Join(filepath.Dir(manifestPath), "secrets", "database-url-production")
				if err := os.WriteFile(urlPath, []byte("postgres://secondbox:secret@database.example/secondbox?sslmode=verify-full\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				manifest.Database = Database{Mode: "external", URLFile: "secrets/database-url-production"}
			}
			if databaseMode == "bundled" {
				if _, err := resolveManifest(manifest, filepath.Dir(manifestPath)); err == nil || !strings.Contains(err.Error(), "operator-supplied signed asset catalog") {
					t.Fatalf("development catalog in production error = %v", err)
				}
			}
			productionCatalog := `{"assets":[{"artifactId":"secondbox-development-runtime","manifestDigest":"` + developmentRuntimeDigest + `","signatureKeyId":"` + strings.Repeat("d", 64) + `","architecture":"amd64","guestProtocolGeneration":1,"mandatoryGuestFeatures":[]},{"artifactId":"secondbox-development-toolchain","manifestDigest":"` + developmentToolchainDigest + `","signatureKeyId":"` + strings.Repeat("d", 64) + `","architecture":"amd64","guestProtocolGeneration":1,"mandatoryGuestFeatures":[]}]}` + "\n"
			if err := os.WriteFile(filepath.Join(filepath.Dir(manifestPath), manifest.Deployment.SignedAssetCatalog), []byte(productionCatalog), 0o600); err != nil {
				t.Fatal(err)
			}
			encoded, err := encodeManifest(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
				t.Fatal(err)
			}
			resolved, err := Resolve(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			_, hasPostgres := resolved.Environment["SECONDBOX_POSTGRES_IMAGE"]
			if hasPostgres != (databaseMode == "bundled") {
				t.Fatalf("inactive environment leaked: postgres=%t", hasPostgres)
			}
			files := strings.Join(resolved.ComposeFiles, ",")
			if strings.Contains(files, "compose.development.yml") || strings.Contains(files, "bundled-database") != (databaseMode == "bundled") {
				t.Fatalf("production overlay selection = %#v", resolved.ComposeFiles)
			}
			automated, err := InitProductionFromManifest(manifestPath, filepath.Join(t.TempDir(), "production"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve(automated); err != nil {
				t.Fatal(err)
			}
			release, err := developmentReleaseManifest()
			if err != nil {
				t.Fatal(err)
			}
			releaseBytes, err := json.Marshal(release)
			if err != nil {
				t.Fatal(err)
			}
			fromRelease, err := InitProductionFromRelease(manifestPath, filepath.Join(t.TempDir(), "release-production"), release, releaseBytes)
			if err != nil {
				t.Fatal(err)
			}
			materialized, err := ReadManifest(fromRelease)
			if err != nil {
				t.Fatal(err)
			}
			if materialized.Deployment.ControlPlaneImage != release.ControlPlane.Reference || materialized.Deployment.RunnerImage != release.Runner.Reference || materialized.StandardResources.ArtifactManifest != "release-artifact-manifest.json" {
				t.Fatalf("release software facts were not materialized: %#v", materialized.Deployment)
			}
		})
	}
}

func TestMultipleRemoteRunnerArtifactsAreIsolatedAndHostPathsStayOpaque(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Runners = []Runner{validTestRunner("runner-a", "remote"), validTestRunner("runner-b", "remote")}
	manifest.Runners[0].IdentityDirectory = "/remote-a/identity"
	manifest.Runners[1].IdentityDirectory = "/remote-b/identity"
	manifest.Runners[0].WorkspaceRoot = "/remote-a/workspaces"
	manifest.Runners[1].WorkspaceRoot = "/remote-b/workspaces"
	encoded, err := encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.RemoteRunnerEnvironment) != 2 || len(resolved.ComposeFiles) != 2 {
		t.Fatalf("remote runners/overlays = %d/%#v", len(resolved.RemoteRunnerEnvironment), resolved.ComposeFiles)
	}
	if resolved.RemoteRunnerEnvironment["runner-a"]["SECONDBOX_RUNNER_WORKSPACE_ROOT"] == resolved.RemoteRunnerEnvironment["runner-b"]["SECONDBOX_RUNNER_WORKSPACE_ROOT"] {
		t.Fatal("runner artifacts leaked across immutable runner IDs")
	}
	if got := resolved.RemoteRunnerEnvironment["runner-a"]["SECONDBOX_RUNNER_CLIENT_CERTIFICATE"]; got != "/remote-a/identity/runner.crt" {
		t.Fatalf("remote Runner identity path was not preserved: %q", got)
	}
	renderedPath := filepath.Join(filepath.Dir(manifestPath), "generated.env")
	if _, err := Render(manifestPath, renderedPath); err != nil {
		t.Fatal(err)
	}
	manifest.Runners = manifest.Runners[:1]
	encoded, err = encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(manifestPath, renderedPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(renderedPath+".runners", "runner-b.env")); !os.IsNotExist(err) {
		t.Fatalf("removed Runner retained a stale handoff: %v", err)
	}
	handoff := filepath.Join(t.TempDir(), "runner-a")
	if err := RunnerInit(manifestPath, "runner-a", handoff); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runner.crt", "runner.key", "runner-ca.crt", "runner.env"} {
		if info, err := os.Stat(filepath.Join(handoff, name)); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("runner handoff %s = %v, %v", name, info, err)
		}
	}
	if err := RunnerInit(manifestPath, "runner-a", handoff); err == nil {
		t.Fatal("runner-init replaced an existing identity")
	}
	if err := RunnerInitOrValidate(manifestPath, "runner-a", handoff); err != nil {
		t.Fatalf("installer retry did not adopt the exact existing identity: %v", err)
	}
	if err := os.WriteFile(filepath.Join(handoff, "runner.env"), []byte("SECONDBOX_RUNNER_ID=runner-other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RunnerInitOrValidate(manifestPath, "runner-a", handoff); err == nil {
		t.Fatal("installer retry adopted a changed existing identity")
	}
	if err := RunnerInit(manifestPath, "unknown", filepath.Join(t.TempDir(), "unknown")); err == nil {
		t.Fatal("runner-init accepted an undeclared identity")
	}
}

func TestSameHostRunnerSelectsOnlyItsOverlayAndRejectsAmbiguousIdentity(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	provisionSameHostTestRunner(t, manifestPath, "runner-local")
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.Environment["SECONDBOX_RUNNER_ID"]; got != "runner-local" {
		t.Fatalf("same-host runner ID = %q", got)
	}
	if len(resolved.ComposeFiles) != 3 || resolved.ComposeFiles[2] != "deploy/compose.same-host-runner.yml" {
		t.Fatalf("Compose files = %#v", resolved.ComposeFiles)
	}
	manifest.Runners = append(manifest.Runners, validSameHostTestRunner("runner-local-2"))
	encoded, _ := encodeManifest(manifest)
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("second same-host error = %v", err)
	}
	manifest.Runners = []Runner{validTestRunner("duplicate", "remote"), validTestRunner("duplicate", "remote")}
	encoded, _ = encodeManifest(manifest)
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "duplicate runner_id") {
		t.Fatalf("duplicate ID error = %v", err)
	}
}

func TestSameHostRunnerPreflightRejectsUnsafeHostState(t *testing.T) {
	t.Run("accepted installer retains root-only host paths", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root can traverse a mode-000 fixture")
		}
		manifestPath := initializedDevelopment(t)
		runner := provisionSameHostTestRunner(t, manifestPath, "runner-local")
		hostRoot := runner.StateHostDirectory
		if err := os.Chmod(hostRoot, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(hostRoot, 0o700) })
		if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("ordinary same-host resolution did not retain host preflight: %v", err)
		}
		resolved, err := ResolveForAcceptedInstaller(manifestPath)
		if err != nil {
			t.Fatalf("accepted installer repeated unprivileged host traversal: %v", err)
		}
		if resolved.ComposeProject() == "" || resolved.Environment["SECONDBOX_RUNNER_STATE_HOST_DIR"] != runner.StateHostDirectory {
			t.Fatalf("accepted installer resolution lost exact host identity: %#v", resolved.Environment)
		}
	})

	t.Run("jailer UID is assigned to a host account", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root UID is rejected by manifest range validation before host-account preflight")
		}
		manifestPath := initializedDevelopment(t)
		provisionSameHostTestRunner(t, manifestPath, "runner-local")
		manifest, err := ReadManifest(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Runners[0].FirecrackerJailerUIDStart = integer(int64(os.Getuid()))
		manifest.Runners[0].FirecrackerJailerUIDAllowLow = boolean(os.Getuid() < 1000)
		encoded, err := encodeManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
			t.Fatal(err)
		}
		if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "already assigned to host account") {
			t.Fatalf("assigned jailer UID error = %v", err)
		}
	})

	t.Run("missing bind source", func(t *testing.T) {
		manifestPath := initializedDevelopment(t)
		runner := provisionSameHostTestRunner(t, manifestPath, "runner-local")
		if err := os.Remove(runner.ArtifactHostDirectory); err != nil {
			t.Fatal(err)
		}
		if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "artifact_host_directory must be an existing") {
			t.Fatalf("missing same-host bind source error = %v", err)
		}
	})

	t.Run("workspace uses host root filesystem", func(t *testing.T) {
		manifestPath := initializedDevelopment(t)
		provisionSameHostTestRunner(t, manifestPath, "runner-local")
		manifest, err := ReadManifest(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		rootBackedStorage, err := os.MkdirTemp("/var/tmp", "secondbox-root-storage-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(rootBackedStorage) })
		manifest.Runners[0].StateHostDirectory = rootBackedStorage
		manifest.Runners[0].WorkspaceHostDirectory = filepath.Join(rootBackedStorage, "workspaces")
		manifest.Runners[0].ArtifactHostDirectory = filepath.Join(rootBackedStorage, "release", "artifacts")
		for _, directory := range []string{manifest.Runners[0].WorkspaceHostDirectory, manifest.Runners[0].ArtifactHostDirectory} {
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		encoded, err := encodeManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
			t.Fatal(err)
		}
		if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "dedicated non-root filesystem") {
			t.Fatalf("root-backed workspace error = %v", err)
		}
	})

	t.Run("identity trusts a different CA", func(t *testing.T) {
		manifestPath := initializedDevelopment(t)
		runner := provisionSameHostTestRunner(t, manifestPath, "runner-local")
		manifest, err := ReadManifest(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		serverCertificate, err := os.ReadFile(filepath.Join(filepath.Dir(manifestPath), manifest.RunnerTrust.ServerCertificateFile))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runner.IdentityHostDirectory, "runner-ca.crt"), serverCertificate, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "must trust the configured") {
			t.Fatalf("mismatched Runner CA error = %v", err)
		}
	})
}

func validTestRunner(id, placement string) Runner {
	return Runner{RunnerID: id, Placement: placement, PoolID: "secondbox-local", SoftwareVersion: "development", ControlPlaneAddress: "control-plane.example:9443", ControlPlaneServerName: "control-plane", IdentityDirectory: "/etc/secondbox/identity", IdentityHostDirectory: "/var/lib/secondbox/identity", ArtifactHostDirectory: "/var/lib/secondbox/artifacts", StateHostDirectory: "/var/lib/secondbox/state", WorkspaceHostDirectory: "/var/lib/secondbox/workspace", LogPath: "/var/log/secondbox-runner.jsonl", LogDirectory: "/var/lib/secondbox/log", FirecrackerPath: "/usr/local/bin/firecracker", FirecrackerJailerPath: "/usr/local/bin/jailer", FirecrackerJailRoot: "/var/lib/secondbox/jailer", FirecrackerJailerUIDStart: integer(10001), FirecrackerJailerUIDCount: integer(16), FirecrackerJailerUIDAllowLow: boolean(false), FirecrackerJailerGID: integer(10001), FirecrackerCgroupVersion: integer(2), FirecrackerCgroupParent: "secondbox-runner", FirecrackerKernelPath: "/opt/secondbox/kernel", FirecrackerRootFSPath: "/opt/secondbox/rootfs.ext4", FirecrackerSharedImagePath: "/opt/secondbox/shared.img", FirecrackerKernelArgs: "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw quiet loglevel=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init", FirecrackerCPUTemplate: "T2", FirecrackerRunDirectory: "/var/lib/secondbox/run", FirecrackerLogDirectory: "/var/lib/secondbox/firecracker-log", FirecrackerAllowUnjailed: boolean(false), SnapshotTemplateCacheRoot: "/var/lib/secondbox/snapshot-templates", ArtifactPublicKey: "/opt/secondbox/manifest-public.pem", ArtifactPublicKeySHA256: strings.Repeat("a", 64), WorkspaceRoot: "/var/lib/secondbox/workspaces", StorageRecoveryPercent: integer(70), StorageWarningPercent: integer(80), StorageAdmissionDenyPercent: integer(90), SandboxMaxVCPUs: integer(2), SandboxMaxMemoryMiB: integer(2048), SandboxMaxDiskMiB: integer(10240), SandboxMemoryBudgetMiB: integer(8192), SandboxGuestIP: "172.30.0.2", SandboxBridgeName: "sbx0", SandboxBridgeCIDR: "172.30.0.1/24", SandboxGuestCIDR: "172.30.0.0/24", SandboxTapPrefix: "sbx", SandboxNetworkStateDir: "/var/lib/secondbox/network", SandboxDeleteBridge: boolean(true), NetworkPolicyNFTPath: "/usr/sbin/nft", NetworkPolicyMaxDNSPins: integer(256), NetworkPolicyMaxDNSTTL: "5m", NetworkPolicyRunnerAddresses: "172.30.0.1", NetworkPolicyManagementCIDRs: "172.30.0.0/24", NetworkPolicyRunnerGateways: "agent-gateway.secondbox.internal=172.30.0.1,platform-gateway.secondbox.internal=172.30.0.1", NetworkPolicyDNSUpstream: "1.1.1.1:53", MaxConcurrentPerSandbox: integer(4), MaxConcurrentGlobal: integer(16), MaxConcurrentStarts: integer(8), MaxConcurrentWorkspaceCreates: integer(8), MaxConcurrentOperationsGlobal: integer(64), FileTransferMaxBytes: integer(1073741824), GuestControlVSockPort: integer(1024), GuestProtocolVSockPort: integer(1025), GuestHeartbeatInterval: "5s", DataPlaneListenAddress: "127.0.0.1:7443", DataPlaneAdvertisedAddress: "127.0.0.1:7443"}
}

func validSameHostTestRunner(id string) Runner {
	runner := validTestRunner(id, "same-host")
	runner.IdentityDirectory = "/run/secondbox-runner-identity"
	runner.StateHostDirectory = "/var/lib/secondbox-runner-storage"
	runner.WorkspaceHostDirectory = "/var/lib/secondbox-runner-storage/workspaces"
	runner.LogPath = "/var/lib/secondbox-runner/state/logs/runner.jsonl"
	runner.LogDirectory = "/var/lib/secondbox-runner/state/logs"
	runner.FirecrackerJailRoot = "/var/lib/secondbox-runner/jail"
	runner.FirecrackerKernelPath = "/opt/secondbox-artifacts/kernel"
	runner.FirecrackerRootFSPath = "/opt/secondbox-artifacts/rootfs.ext4"
	runner.FirecrackerSharedImagePath = "/opt/secondbox-artifacts/shared.img"
	runner.FirecrackerRunDirectory = "/var/lib/secondbox-runner/state/run"
	runner.FirecrackerLogDirectory = "/var/lib/secondbox-runner/state/firecracker-log"
	runner.SnapshotTemplateCacheRoot = "/var/lib/secondbox-runner/state/snapshot-templates"
	runner.ArtifactPublicKey = "/opt/secondbox-artifacts/manifest-public.pem"
	runner.WorkspaceRoot = "/var/lib/secondbox-runner/workspaces"
	runner.SandboxNetworkStateDir = "/var/lib/secondbox-runner/state/network"
	return runner
}

func provisionSameHostTestRunner(t *testing.T, manifestPath, id string) Runner {
	t.Helper()
	hostRoot := t.TempDir()
	storageDirectory, err := os.MkdirTemp("/dev/shm", "secondbox-runner-storage-")
	if err != nil {
		t.Skipf("dedicated tmpfs unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(storageDirectory) })
	runner := validSameHostTestRunner(id)
	runner.IdentityHostDirectory = filepath.Join(hostRoot, "identity")
	runner.ArtifactHostDirectory = filepath.Join(storageDirectory, "release", "artifacts")
	runner.StateHostDirectory = storageDirectory
	runner.WorkspaceHostDirectory = filepath.Join(storageDirectory, "workspaces")
	for _, directory := range []string{runner.ArtifactHostDirectory, runner.WorkspaceHostDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Runners = []Runner{runner}
	encoded, err := encodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	if err := RunnerInit(manifestPath, runner.RunnerID, runner.IdentityHostDirectory); err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestRunnerValidationMatchesRuntimeInvariants(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*Runner)
	}{
		{name: "storage threshold at 100", want: "admission deny < 100", mutate: func(r *Runner) {
			r.StorageRecoveryPercent, r.StorageWarningPercent, r.StorageAdmissionDenyPercent = integer(80), integer(90), integer(100)
		}},
		{name: "starts exceed global", want: "max_concurrent_starts", mutate: func(r *Runner) { r.MaxConcurrentStarts, r.MaxConcurrentGlobal = integer(17), integer(16) }},
		{name: "UID range smaller than capacity", want: "uid_count must be at least max_concurrent_global", mutate: func(r *Runner) { r.FirecrackerJailerUIDCount = integer(15) }},
		{name: "low UID range unacknowledged", want: "below 1000 requires", mutate: func(r *Runner) { r.FirecrackerJailerUIDStart = integer(999) }},
		{name: "UID range includes zero", want: "uid_start must be positive", mutate: func(r *Runner) {
			r.FirecrackerJailerUIDStart = integer(0)
			r.FirecrackerJailerUIDAllowLow = boolean(true)
		}},
		{name: "UID range overflows uid_t", want: "unsigned 32-bit", mutate: func(r *Runner) {
			r.FirecrackerJailerUIDStart = integer(4294967290)
			r.FirecrackerJailerUIDCount = integer(16)
		}},
		{name: "duplicate vsock port", want: "vsock ports", mutate: func(r *Runner) { r.GuestProtocolVSockPort = r.GuestControlVSockPort }},
		{name: "oversized vsock port", want: "vsock ports", mutate: func(r *Runner) { r.GuestProtocolVSockPort = integer(65536) }},
		{name: "unjailed", want: "must be false", mutate: func(r *Runner) { r.FirecrackerAllowUnjailed = boolean(true) }},
		{name: "Firecracker jail socket path too long", want: "maximum Firecracker API socket path below 108 bytes", mutate: func(r *Runner) {
			r.FirecrackerJailRoot = "/" + strings.Repeat("j", 80)
		}},
		{name: "placeholder artifact fingerprint", want: "provisioned signed artifact key", mutate: func(r *Runner) { r.ArtifactPublicKeySHA256 = strings.Repeat("0", 64) }},
		{name: "missing kernel argument", want: "i8042.noaux", mutate: func(r *Runner) {
			r.FirecrackerKernelArgs = strings.ReplaceAll(r.FirecrackerKernelArgs, " i8042.noaux", "")
		}},
		{name: "invalid heartbeat", want: "supported duration range", mutate: func(r *Runner) { r.GuestHeartbeatInterval = "0s" }},
		{name: "invalid DNS TTL", want: "supported duration range", mutate: func(r *Runner) { r.NetworkPolicyMaxDNSTTL = "invalid" }},
		{name: "invalid Runner address", want: "invalid network value", mutate: func(r *Runner) { r.NetworkPolicyRunnerAddresses = "not-an-ip" }},
		{name: "invalid management CIDR", want: "invalid network value", mutate: func(r *Runner) { r.NetworkPolicyManagementCIDRs = "127.0.0.1" }},
		{name: "invalid gateway", want: "domain=IP", mutate: func(r *Runner) { r.NetworkPolicyRunnerGateways = "invalid" }},
		{name: "invalid DNS upstream", want: "must be an IP:port", mutate: func(r *Runner) { r.NetworkPolicyDNSUpstream = "localhost:53" }},
		{name: "invalid advertised address", want: "explicit reachable host", mutate: func(r *Runner) { r.DataPlaneAdvertisedAddress = "0.0.0.0:7443" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := validTestRunner("runner-a", "remote")
			test.mutate(&runner)
			if err := validateRunner("runners[0]", runner); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestCanonicalRunnerEnvironmentFixtureMatchesResolvedModel(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Runners = []Runner{validTestRunner("runner-conformance", "remote")}
	encoded, _ := encodeManifest(manifest)
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	environment := resolved.RemoteRunnerEnvironment["runner-conformance"]
	environment["SECONDBOX_RUNNER_CREDENTIAL"] = "<runner-credential>"
	fixture, err := os.ReadFile(filepath.Join(
		"..", "..", "runner", "internal", "runtimeconfig", "testdata", "runner-environment.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(environment, want) {
		t.Fatalf("Runner conformance fixture drifted\nresolved: %#v\nfixture: %#v", environment, want)
	}
}
