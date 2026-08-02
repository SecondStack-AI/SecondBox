package deployconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
}

func TestExampleManifestIsGeneratedFromTheRegistry(t *testing.T) {
	manifest := developmentManifest("secrets/postgres-password", "secrets/object-store-access-key", "secrets/object-store-secret-key", "secrets/platform-token", "secrets/runner-enrollment-credential")
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
	if len(OverrideRegistry()) != 19 {
		t.Fatalf("override count = %d", len(OverrideRegistry()))
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

func TestOverridesPreserveAbsenceExactValuesAndCrossFieldValidation(t *testing.T) {
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
	manifest.Overrides.DataPlaneMaximumFrameBytes = integer(100)
	manifest.Overrides.DataPlaneMaximumSessionBytes = integer(99)
	encoded, _ = encodeManifest(manifest)
	if err := writeAtomic(manifestPath, encoded, 0o600, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(manifestPath); err == nil || !strings.Contains(err.Error(), "session byte bound") {
		t.Fatalf("cross-field error = %v", err)
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

func TestInspectRedactsSecretValuesAndPathsAndShowsAllDefaults(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	resolved, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	output, err := Inspect(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, secret := range []string{resolved.Environment["SECONDBOX_PLATFORM_TOKEN"], resolved.Environment["SECONDBOX_RUNNER_CREDENTIAL"], resolved.SecretPaths["applications.platform_token_file"]} {
		if strings.Contains(text, secret) {
			t.Errorf("inspect exposed secret material %q", secret)
		}
	}
	if strings.Count(text, "codeDefault") != 19 {
		t.Fatalf("inspect defaults = %d, want 19", strings.Count(text, "codeDefault"))
	}
}

func TestProductionInitializationReportsEveryUnresolvedDecisionGroup(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "production")
	path, err := InitProduction(directory)
	if path == "" || err == nil {
		t.Fatalf("path, error = %q, %v", path, err)
	}
	for _, group := range []string{"deployment", "database", "object store", "Runner topology", "execution-asset trust", "application authorities", "tenancy policy", "lifecycle policy"} {
		if !strings.Contains(err.Error(), group) {
			t.Errorf("production error omitted %q: %v", group, err)
		}
	}
	if info, statErr := os.Stat(path); statErr != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("production skeleton = %v, %v", info, statErr)
	}
}

func TestProductionQualifiesBundledAndExternalDatabaseAndObjectStoreCombinations(t *testing.T) {
	for _, databaseMode := range []string{"bundled", "external"} {
		for _, objectMode := range []string{"bundled", "external"} {
			t.Run(databaseMode+"_database_"+objectMode+"_object_store", func(t *testing.T) {
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
				manifest.Deployment.ObjectStoreImage = digestRef
				manifest.Deployment.ObjectStoreClientImage = digestRef
				manifest.ObjectStore.Endpoint = "http://object-store:9000"
				assetDigest := "sha256:" + strings.Repeat("a", 64)
				manifest.Policy.AgentCompartmentRuntimeBundleDigest = assetDigest
				manifest.Policy.AgentCompartmentToolchainBundleDigest = assetDigest
				manifest.Policy.CodingEnvironmentRuntimeBundleDigest = assetDigest
				manifest.Policy.CodingEnvironmentToolchainBundleDigest = assetDigest
				if databaseMode == "external" {
					urlPath := filepath.Join(filepath.Dir(manifestPath), "secrets", "database-url-production")
					if err := os.WriteFile(urlPath, []byte("postgres://secondbox:secret@database.example/secondbox?sslmode=verify-full\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					manifest.Database = Database{Mode: "external", URLFile: "secrets/database-url-production"}
				}
				if objectMode == "external" {
					manifest.ObjectStore.Mode = "external"
					manifest.ObjectStore.Endpoint = "https://object-store.example.com"
					manifest.ObjectStore.BindIP = ""
					manifest.ObjectStore.PublishedPort = nil
					manifest.ObjectStore.ConsolePublishedPort = nil
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
				_, hasObject := resolved.Environment["SECONDBOX_OBJECT_STORE_IMAGE"]
				if hasPostgres != (databaseMode == "bundled") || hasObject != (objectMode == "bundled") {
					t.Fatalf("inactive environment leaked: postgres=%t object=%t", hasPostgres, hasObject)
				}
				files := strings.Join(resolved.ComposeFiles, ",")
				if strings.Contains(files, "compose.development.yml") || strings.Contains(files, "bundled-database") != (databaseMode == "bundled") || strings.Contains(files, "bundled-object-store") != (objectMode == "bundled") {
					t.Fatalf("production overlay selection = %#v", resolved.ComposeFiles)
				}
				automated, err := InitProductionFromManifest(manifestPath, filepath.Join(t.TempDir(), "production"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := Resolve(automated); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestMultipleRemoteRunnerArtifactsAreIsolatedAndHostPathsStayOpaque(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Runners = []Runner{validTestRunner("runner-a", "remote"), validTestRunner("runner-b", "remote")}
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
	if err := RunnerInit(manifestPath, "unknown", filepath.Join(t.TempDir(), "unknown")); err == nil {
		t.Fatal("runner-init accepted an undeclared identity")
	}
}

func TestSameHostRunnerSelectsOnlyItsOverlayAndRejectsAmbiguousIdentity(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Runners = []Runner{validTestRunner("runner-local", "same-host")}
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
	if got := resolved.Environment["SECONDBOX_RUNNER_ID"]; got != "runner-local" {
		t.Fatalf("same-host runner ID = %q", got)
	}
	if len(resolved.ComposeFiles) != 3 || resolved.ComposeFiles[2] != "deploy/compose.same-host-runner.yml" {
		t.Fatalf("Compose files = %#v", resolved.ComposeFiles)
	}
	manifest.Runners = append(manifest.Runners, validTestRunner("runner-local-2", "same-host"))
	encoded, _ = encodeManifest(manifest)
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

func validTestRunner(id, placement string) Runner {
	return Runner{RunnerID: id, Placement: placement, PoolID: "secondbox-local", SoftwareVersion: "development", ControlPlaneAddress: "control-plane.example:9443", ControlPlaneServerName: "control-plane", IdentityDirectory: "/etc/secondbox/identity", IdentityHostDirectory: "/var/lib/secondbox/identity", ArtifactHostDirectory: "/var/lib/secondbox/artifacts", StateHostDirectory: "/var/lib/secondbox/state", WorkspaceHostDirectory: "/var/lib/secondbox/workspace", LogPath: "/var/log/secondbox-runner.jsonl", LogDirectory: "/var/lib/secondbox/log", FirecrackerPath: "/usr/local/bin/firecracker", FirecrackerJailerPath: "/usr/local/bin/jailer", FirecrackerJailRoot: "/var/lib/secondbox/jailer", FirecrackerJailerUID: integer(10001), FirecrackerJailerGID: integer(10001), FirecrackerCgroupVersion: integer(2), FirecrackerCgroupParent: "secondbox-runner", FirecrackerKernelPath: "/opt/secondbox/kernel", FirecrackerRootFSPath: "/opt/secondbox/rootfs.ext4", FirecrackerSharedImagePath: "/opt/secondbox/shared.img", FirecrackerKernelArgs: "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw quiet loglevel=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init", FirecrackerCPUTemplate: "T2", FirecrackerRunDirectory: "/var/lib/secondbox/run", FirecrackerLogDirectory: "/var/lib/secondbox/firecracker-log", FirecrackerAllowUnjailed: boolean(false), ArtifactPublicKey: "/opt/secondbox/manifest-public.pem", ArtifactPublicKeySHA256: strings.Repeat("a", 64), WorkspaceRoot: "/var/lib/secondbox/workspaces", StorageRecoveryPercent: integer(70), StorageWarningPercent: integer(80), StorageAdmissionDenyPercent: integer(90), SandboxMaxVCPUs: integer(2), SandboxMaxMemoryMiB: integer(2048), SandboxMaxDiskMiB: integer(10240), SandboxMemoryBudgetMiB: integer(8192), SandboxGuestIP: "172.30.0.2", SandboxBridgeName: "sbx0", SandboxBridgeCIDR: "172.30.0.1/24", SandboxGuestCIDR: "172.30.0.0/24", SandboxTapPrefix: "sbx", SandboxNetworkStateDir: "/var/lib/secondbox/network", SandboxDeleteBridge: boolean(true), NetworkPolicyNFTPath: "/usr/sbin/nft", NetworkPolicyMaxDNSPins: integer(256), NetworkPolicyMaxDNSTTL: "5m", NetworkPolicyRunnerAddresses: "172.30.0.1", NetworkPolicyManagementCIDRs: "172.30.0.0/24", NetworkPolicyRunnerGateways: "none", NetworkPolicyDNSUpstream: "1.1.1.1:53", MaxConcurrentPerSandbox: integer(4), MaxConcurrentGlobal: integer(16), MaxConcurrentStarts: integer(8), MaxConcurrentWorkspaceCreates: integer(8), MaxConcurrentOperationsGlobal: integer(64), FileTransferMaxBytes: integer(1073741824), GuestControlVSockPort: integer(1024), GuestProtocolVSockPort: integer(1025), GuestHeartbeatInterval: "5s", DataPlaneListenAddress: "127.0.0.1:7443", DataPlaneAdvertisedAddress: "127.0.0.1:7443"}
}

func TestLegacyMigrationIsOneShotStrictAndPreservesTheSource(t *testing.T) {
	manifestPath := initializedDevelopment(t)
	resolved, err := Resolve(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for name, value := range resolved.Environment {
		values[name] = value
	}
	for _, definition := range OverrideRegistry() {
		values[definition.Environment] = definition.Default
	}
	for name := range legacyEnvironmentNames() {
		if values[name] == "" {
			values[name] = "1"
		}
	}
	values["SECONDBOX_SAME_HOST_RUNNER_ENABLED"] = "false"
	values["SECONDBOX_RUNNER_PROTOCOL_MINIMUM"] = "1"
	values["SECONDBOX_RUNNER_PROTOCOL_MAXIMUM"] = "1"
	legacyPath := filepath.Join(t.TempDir(), "legacy.env")
	keys := sortedKeys(values)
	var source strings.Builder
	for _, name := range keys {
		source.WriteString(name + "=" + values[name] + "\n")
	}
	if err := os.WriteFile(legacyPath, []byte(source.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "migrated")
	migrated, err := MigrateLegacyEnvironment(legacyPath, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(migrated); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("migration modified its source")
	}
	if _, err := MigrateLegacyEnvironment(legacyPath, target); err == nil {
		t.Fatal("migration replaced an existing target")
	}
	if len(legacyEnvironmentNames()) != 146 {
		t.Fatalf("legacy mapping count = %d, want 146", len(legacyEnvironmentNames()))
	}
	for name, extra := range map[string]string{"unknown": "SECONDBOX_UNKNOWN=value\n", "duplicate": "SECONDBOX_DEPLOYMENT_MODE=development\n", "placeholder": ""} {
		t.Run(name, func(t *testing.T) {
			copyPath := filepath.Join(t.TempDir(), "legacy.env")
			content := append([]byte{}, before...)
			if name == "placeholder" {
				content = []byte(strings.Replace(string(content), "SECONDBOX_PLATFORM_TOKEN="+values["SECONDBOX_PLATFORM_TOKEN"], "SECONDBOX_PLATFORM_TOKEN=REPLACE_WITH_SECRET", 1))
			} else {
				content = append(content, []byte(extra)...)
			}
			if err := os.WriteFile(copyPath, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := MigrateLegacyEnvironment(copyPath, filepath.Join(t.TempDir(), "target")); err == nil {
				t.Fatal("invalid legacy environment migrated")
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
