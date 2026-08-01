package deployment_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
)

func TestComposeSeparatesOptionalPrivilegedRunnerFromControlPlane(t *testing.T) {
	compose := readRepositoryFile(t, "deploy/compose.yml")

	for _, required := range []string{
		"control-plane:",
		"same-host-runner:",
		"postgres:",
		"object-store:",
		"profiles: [\"development\"]",
		"profiles: [\"same-host-runner\"]",
		"stop_grace_period: 45s",
		"pg_isready -h 127.0.0.1",
		"image: ${SECONDBOX_RUNNER_IMAGE:?",
		"org.secondbox.runner.qualification: \"not-established-by-compose\"",
		"${SECONDBOX_RUNNER_IDENTITY_HOST_DIR:?",
		"${SECONDBOX_RUNNER_ARTIFACT_HOST_DIR:?",
		"${SECONDBOX_RUNNER_STATE_HOST_DIR:?",
		"${SECONDBOX_RUNNER_WORKSPACE_HOST_DIR:?",
		"${SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS:?",
		"${SECONDBOX_RUNNER_MAX_CONCURRENT_WORKSPACE_CREATES:?",
		"${SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS:?",
		"${SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_GATEWAYS:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM:?",
		"${SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT:?",
		"${SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT:?",
		"${SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT:?",
		"${SECONDBOX_RUNNER_WORKSPACE_ROOT:?",
		"/dev/kvm:/dev/kvm",
		"/dev/net/tun:/dev/net/tun",
		"secondbox-runner\", \"-healthcheck\"",
		"cap_drop:",
		"- ALL",
		"read_only: true",
		"no-new-privileges:true",
		"${SECONDBOX_CONTROL_PLANE_IMAGE:?",
		"${SECONDBOX_DATABASE_URL:?",
		"${SECONDBOX_RUNNER_PUBLISHED_PORT:?",
		"${SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_SIZE:?",
		"${SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_WAIT_MILLISECONDS:?",
		"${SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS:?",
		"${SECONDBOX_DATA_PLANE_CLAIM_DURATION_MILLISECONDS:?",
		"${SECONDBOX_DATA_PLANE_RETENTION_SECONDS:?",
		"${SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES:?",
		"${SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES:?",
		"${SECONDBOX_LIFECYCLE_RECONCILE_POLL_INTERVAL_MILLISECONDS:?",
		"${SECONDBOX_LIFECYCLE_RECONCILE_CLAIM_DURATION_MILLISECONDS:?",
		"${SECONDBOX_OBJECT_STORE_IMAGE:?",
		"${SECONDBOX_POSTGRES_IMAGE:?",
		"${SECONDBOX_RUNNER_PKI_HOST_DIR:?",
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("deploy/compose.yml must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"docker.sock",
		":latest",
		":-",
	} {
		if strings.Contains(compose, forbidden) {
			t.Errorf("deploy/compose.yml must not contain %q", forbidden)
		}
	}
	controlPlane := strings.Split(strings.Split(compose, "  control-plane:")[1], "\n  runner-pki-init:")[0]
	for _, forbidden := range []string{"privileged: true", "/dev/kvm", "/dev/net/tun", "/sys/fs/cgroup"} {
		if strings.Contains(controlPlane, forbidden) {
			t.Errorf("control-plane service must not contain %q", forbidden)
		}
	}
}

// A privileged runner container receives the host's /dev. It must not boot an
// init system that can start console, seat, or login services against host
// devices.
func TestRunnerImageExecutesRunnerAsPID1WithoutSystemd(t *testing.T) {
	dockerfile := readRepositoryFile(t, "runner/Dockerfile")
	entrypoint := readRepositoryFile(t, "runner/scripts/container/secondbox-runner-entrypoint.sh")

	for _, forbidden := range []string{
		" dbus ",
		" systemd ",
		"/lib/systemd/systemd",
		"/etc/systemd/system",
		"runner.env",
		"compgen -e",
	} {
		if strings.Contains(" "+dockerfile+"\n"+entrypoint+" ", forbidden) {
			t.Errorf("runner image startup must not contain %q", forbidden)
		}
	}
	for _, required := range []string{
		`STOPSIGNAL SIGTERM`,
		`ENTRYPOINT ["/usr/local/bin/secondbox-runner-entrypoint"]`,
		`CMD ["/usr/local/bin/secondbox-runner"]`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("runner/Dockerfile must contain %q", required)
		}
	}
	for _, required := range []string{
		`"$1" != "/usr/local/bin/secondbox-runner"`,
		`exec "$@"`,
	} {
		if !strings.Contains(entrypoint, required) {
			t.Errorf("runner entrypoint must contain %q", required)
		}
	}
}

func TestDockerBuildContextExcludesLocalSecondBoxState(t *testing.T) {
	dockerignore := readRepositoryFile(t, ".dockerignore")
	if !strings.Contains(dockerignore, "\n.secondbox\n") {
		t.Fatal(".dockerignore must exclude local .secondbox operator and qualification state")
	}
}

func TestScenarioQualificationCIRequiresSelfHostedKVM(t *testing.T) {
	workflow := readRepositoryFile(t, ".github/workflows/ci.yml")
	const jobMarker = "  scenario-qualification:\n"
	jobStart := strings.Index(workflow, jobMarker)
	if jobStart == -1 {
		t.Fatal("CI workflow must define the scenario-qualification job")
	}
	scenarioJob := workflow[jobStart:]
	for _, required := range []string{
		"runs-on: [self-hosted, linux, x64, secondbox-kvm]",
		"timeout-minutes: 45",
		`SECONDBOX_REQUIRE_QUALIFIED_SCENARIO: "1"`,
		"SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR:",
		"SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY:",
		"SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256:",
		"SECONDBOX_RUNNER_WORKSPACE_ROOT:",
		"run: just test-scenario",
	} {
		if !strings.Contains(scenarioJob, required) {
			t.Errorf("scenario qualification CI job must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"runs-on: ubuntu-latest",
		"runs-on: ubuntu-",
	} {
		if strings.Contains(scenarioJob, forbidden) {
			t.Errorf("scenario qualification CI job must not contain %q", forbidden)
		}
	}
	if !strings.Contains(workflow, "run: just test-non-kvm") ||
		!strings.Contains(workflow, "runs-on: ubuntu-latest") {
		t.Fatal("portable non-KVM CI gate must remain on a GitHub-hosted runner")
	}
}

func TestDeploymentCannotReconstructAbsentHomeFromAvailableObjectStore(t *testing.T) {
	objectStore := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer objectStore.Close()
	response, err := http.Get(objectStore.URL)
	if err != nil {
		t.Fatalf("prove object store availability: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("object store status = %d", response.StatusCode)
	}

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	requirements := scheduler.Requirements{
		PoolName: "deployment", BackendKind: "firecracker", Architecture: "amd64",
		RequiredCapabilities:    []string{"local-workspace"},
		GuestProtocolGeneration: 1,
		Capacity: scheduler.Capacity{
			CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 10 << 30,
			Instances: 1, Operations: 1,
		},
	}
	replacement := scheduler.RunnerSnapshot{
		ID: "runner-replacement", PoolName: "deployment", Architecture: "amd64",
		Capabilities: map[string]bool{
			"compute": true, "network-policy": true, "storage": true,
			"cleanup": true, "local-workspace": true,
		},
		Allocatable: scheduler.Capacity{
			CPUMillis: 8000, MemoryBytes: 32 << 30, DiskBytes: 200 << 30,
			Instances: 8, Operations: 32,
		},
		DrainPhase: scheduler.DrainPhaseActive, LastHeartbeatAt: now,
		GuestProtocolMinimum: 1, GuestProtocolMaximum: 1,
	}
	if _, err := scheduler.SelectHomeRunner(
		"runner-home", requirements, []scheduler.RunnerSnapshot{replacement},
		now, 30*time.Second,
	); !errors.Is(err, scheduler.ErrHomeRunnerUnavailable) {
		t.Fatalf("available S3 and replacement Runner changed exact-home result: %v", err)
	}
}

func TestDeploymentEnvironmentHasNoBlankOrSharedCredentials(t *testing.T) {
	example := readRepositoryFile(t, "deploy/environment.example")

	for _, runnerSetting := range []string{
		"SECONDBOX_RUNNER_IMAGE=secondbox-runner:development",
		"SECONDBOX_SAME_HOST_RUNNER_ENABLED=false",
		"SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT=1024",
		"SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT=1025",
		"SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL=5s",
		"SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS=console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw quiet loglevel=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init",
		"SECONDBOX_RUNNER_ENABLED_FEATURES=exec-streaming,file-streaming,pty,evidence,local-workspace,port-proxy",
		"SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH=/usr/sbin/nft",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS=256",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL=5m",
		"SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES=172.30.0.1",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS=172.30.0.0/24",
		"SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_GATEWAYS=none",
		"SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM=1.1.1.1:53",
		"SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT=70",
		"SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT=80",
		"SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT=90",
		"SECONDBOX_RUNNER_WORKSPACE_ROOT=/var/lib/secondbox-runner/workspaces",
		"SECONDBOX_RUNNER_WORKSPACE_HOST_DIR=/var/lib/secondbox/runner-workspaces",
		"SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS=8",
		"SECONDBOX_RUNNER_MAX_CONCURRENT_WORKSPACE_CREATES=8",
		"SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS=127.0.0.1:7443",
		"SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS=127.0.0.1:7443",
		"SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_SIZE=16",
		"SECONDBOX_RUNNER_EVENT_PERSISTENCE_BATCH_WAIT_MILLISECONDS=2",
		"SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS=250",
		"SECONDBOX_DATA_PLANE_CLAIM_DURATION_MILLISECONDS=30000",
		"SECONDBOX_DATA_PLANE_RETENTION_SECONDS=86400",
		"SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES=1048576",
		"SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES=67108864",
		"SECONDBOX_LIFECYCLE_RECONCILE_POLL_INTERVAL_MILLISECONDS=250",
		"SECONDBOX_LIFECYCLE_RECONCILE_CLAIM_DURATION_MILLISECONDS=30000",
	} {
		if !strings.Contains(example, runnerSetting) {
			t.Errorf("deploy/environment.example must contain %q", runnerSetting)
		}
	}
	for _, secret := range []string{
		"SECONDBOX_POSTGRES_PASSWORD=GENERATE_WITH_DEPLOY_BOOTSTRAP",
		"SECONDBOX_PLATFORM_TOKEN=GENERATE_WITH_DEPLOY_BOOTSTRAP",
		"SECONDBOX_RUNNER_CREDENTIAL=GENERATE_WITH_DEPLOY_BOOTSTRAP",
		"SECONDBOX_RUNNER_PKI_HOST_DIR=GENERATE_RUNNER_PKI",
		"SECONDBOX_RUNNER_CA_PRIVATE_KEY=GENERATE_RUNNER_CA_PRIVATE_KEY",
		"SECONDBOX_OBJECT_STORE_ROOT_USER=GENERATE_WITH_DEPLOY_BOOTSTRAP",
		"SECONDBOX_OBJECT_STORE_ROOT_PASSWORD=GENERATE_WITH_DEPLOY_BOOTSTRAP",
	} {
		if !strings.Contains(example, secret) {
			t.Errorf("deploy/environment.example must contain %q", secret)
		}
	}
	for lineNumber, line := range strings.Split(example, "\n") {
		if strings.HasSuffix(line, "=") {
			t.Errorf("deploy/environment.example line %d has a blank setting", lineNumber+1)
		}
	}
}

func TestRunnerTrustBootstrapIsCreateOnlyAndIdempotent(t *testing.T) {
	script := readRepositoryFile(t, "deploy/bin/bootstrap-runner-trust.sh")

	for _, required := range []string{
		"SECONDBOX_RUNNER_CA_PRIVATE_KEY",
		"SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS",
		"openssl genpkey",
		"openssl req",
		"subjectAltName=URI:spiffe://secondbox/runner/",
		"-CAkey",
		"Runner trust already bootstrapped",
		"openssl verify",
		"chmod 600",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("runner trust bootstrap must contain %q", required)
		}
	}
	for _, forbidden := range []string{"set -x", "REPLACE_WITH_", "mv -f"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("runner trust bootstrap must not contain %q", forbidden)
		}
	}
}

func TestRunnerTrustBootstrapRerunPreservesIssuedAuthority(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	authorityDirectory := t.TempDir()
	caKey := filepath.Join(authorityDirectory, "runner-ca.key")
	caCertificate := filepath.Join(authorityDirectory, "runner-ca.crt")
	for _, arguments := range [][]string{
		{"genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048", "-out", caKey},
		{"req", "-new", "-x509", "-key", caKey, "-subj", "/CN=Test Runner CA", "-out", caCertificate},
	} {
		if output, err := exec.Command("openssl", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("create test runner CA: %v\n%s", err, output)
		}
	}
	identityDirectory := filepath.Join(t.TempDir(), "identity")
	script := filepath.Join(repositoryRoot, "deploy", "bin", "bootstrap-runner-trust.sh")
	first := exec.Command(script, identityDirectory)
	first.Env = append(os.Environ(),
		"SECONDBOX_RUNNER_ID=runner-test-1",
		"SECONDBOX_RUNNER_CA_CERTIFICATE="+caCertificate,
		"SECONDBOX_RUNNER_CA_PRIVATE_KEY="+caKey,
		"SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS=30",
	)
	if output, err := first.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap runner trust: %v\n%s", err, output)
	}
	firstHash := hashDirectoryFiles(t, identityDirectory, "runner-ca.crt", "runner.crt", "runner.key")

	second := exec.Command(script, identityDirectory)
	second.Env = append(os.Environ(),
		"SECONDBOX_RUNNER_ID=runner-test-1",
		"SECONDBOX_RUNNER_CA_CERTIFICATE="+caCertificate,
	)
	if output, err := second.CombinedOutput(); err != nil {
		t.Fatalf("repeat runner trust bootstrap without enrollment authority: %v\n%s", err, output)
	}
	secondHash := hashDirectoryFiles(t, identityDirectory, "runner-ca.crt", "runner.crt", "runner.key")
	if firstHash != secondHash {
		t.Fatal("runner trust bootstrap rerun changed the issued certificate authority")
	}
}

func TestDeploymentValidatorRejectsInvalidRunnerRuntimeSettings(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	template := readRepositoryFile(t, "deploy/environment.example")
	validEnvironmentPath := filepath.Join(t.TempDir(), "environment")
	if err := os.WriteFile(validEnvironmentPath, []byte(template), 0o600); err != nil {
		t.Fatalf("write bootstrap environment: %v", err)
	}
	bootstrap := filepath.Join(repositoryRoot, "deploy", "bin", "bootstrap-environment.sh")
	if output, err := exec.Command(bootstrap, validEnvironmentPath).CombinedOutput(); err != nil {
		t.Fatalf("bootstrap environment: %v\n%s", err, output)
	}
	validEnvironment, err := os.ReadFile(validEnvironmentPath)
	if err != nil {
		t.Fatalf("read bootstrapped environment: %v", err)
	}
	validator := filepath.Join(repositoryRoot, "deploy", "bin", "validate-environment.sh")

	testCases := []struct {
		name        string
		oldSetting  string
		newSetting  string
		errorMarker string
	}{
		{
			name:        "zero control port",
			oldSetting:  "SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT=1024",
			newSetting:  "SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT=0",
			errorMarker: "SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT",
		},
		{
			name:        "same listener port",
			oldSetting:  "SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT=1025",
			newSetting:  "SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT=1024",
			errorMarker: "distinct",
		},
		{
			name:        "heartbeat exceeds maximum",
			oldSetting:  "SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL=5s",
			newSetting:  "SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL=61s",
			errorMarker: "SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL",
		},
		{
			name:        "latency kernel argument omitted",
			oldSetting:  "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS=console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw quiet loglevel=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init",
			newSetting:  "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS=console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw quiet loglevel=1 i8042.noaux i8042.nomux i8042.nopnp init=/init",
			errorMarker: "i8042.dumbkbd",
		},
		{
			name:        "zero DNS pin capacity",
			oldSetting:  "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS=256",
			newSetting:  "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS=0",
			errorMarker: "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS",
		},
		{
			name:        "zero DNS TTL",
			oldSetting:  "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL=5m",
			newSetting:  "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL=0s",
			errorMarker: "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL",
		},
		{
			name:        "data-plane session bound below frame bound",
			oldSetting:  "SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES=67108864",
			newSetting:  "SECONDBOX_DATA_PLANE_MAXIMUM_SESSION_BYTES=1048575",
			errorMarker: "must be at least SECONDBOX_DATA_PLANE_MAXIMUM_FRAME_BYTES",
		},
		{
			name:        "storage recovery reaches warning",
			oldSetting:  "SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT=70",
			newSetting:  "SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT=80",
			errorMarker: "storage pressure thresholds",
		},
		{
			name:        "storage deny reaches one hundred",
			oldSetting:  "SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT=90",
			newSetting:  "SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT=100",
			errorMarker: "storage pressure thresholds",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			invalidEnvironment := strings.Replace(
				string(validEnvironment),
				testCase.oldSetting,
				testCase.newSetting,
				1,
			)
			if invalidEnvironment == string(validEnvironment) {
				t.Fatalf("valid environment lacks %q", testCase.oldSetting)
			}
			invalidEnvironmentPath := filepath.Join(t.TempDir(), "environment")
			if err := os.WriteFile(invalidEnvironmentPath, []byte(invalidEnvironment), 0o600); err != nil {
				t.Fatalf("write invalid environment: %v", err)
			}
			output, err := exec.Command(validator, invalidEnvironmentPath).CombinedOutput()
			if err == nil {
				t.Fatalf("validator accepted invalid setting %q", testCase.newSetting)
			}
			if !bytes.Contains(output, []byte(testCase.errorMarker)) {
				t.Fatalf("validator error does not identify %q:\n%s", testCase.errorMarker, output)
			}
		})
	}
}

func TestDeploymentValidatorRequiresExplicitProductionDataEndpoints(t *testing.T) {
	validator := readRepositoryFile(t, "deploy/bin/validate-environment.sh")
	for _, required := range []string{
		"Production SECONDBOX_DATABASE_URL must use sslmode=verify-full",
		"Production SECONDBOX_OBJECT_STORE_ENDPOINT must use HTTPS",
		"SECONDBOX_POSTGRES_IMAGE",
		"SECONDBOX_OBJECT_STORE_IMAGE",
		"SECONDBOX_RUNNER_IMAGE",
		"must be pinned by digest",
	} {
		if !strings.Contains(validator, required) {
			t.Errorf("production deployment validator must contain %q", required)
		}
	}
}

func TestBootstrapIsPrivateIdempotentAndDoesNotPrintSecrets(t *testing.T) {
	script := readRepositoryFile(t, "deploy/bin/bootstrap-environment.sh")

	for _, required := range []string{
		"umask 077",
		"GENERATE_WITH_DEPLOY_BOOTSTRAP",
		"openssl rand -hex",
		"openssl genpkey",
		"runner-ca.key",
		"server.key",
		"chmod 600",
		"Refusing symbolic-link environment target",
		"Environment already bootstrapped",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("bootstrap script must contain %q", required)
		}
	}
	for _, forbidden := range []string{"set -x", "cat \"$environment_path\""} {
		if strings.Contains(script, forbidden) {
			t.Errorf("bootstrap script must not contain %q", forbidden)
		}
	}
}

func TestBootstrapCreatesPrivateIdempotentDevelopmentAssetCatalog(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	environmentPath := filepath.Join(t.TempDir(), "environment")
	bootstrap := filepath.Join(repositoryRoot, "deploy", "bin", "bootstrap-environment.sh")

	if output, err := exec.Command(bootstrap, environmentPath).CombinedOutput(); err != nil {
		t.Fatalf("bootstrap development environment: %v\n%s", err, output)
	}
	environment := readEnvironmentSettings(t, environmentPath)
	catalogPath := environment["SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH"]
	wantCatalogPath := filepath.Join(environmentPath+".secrets", "development-signed-assets.json")
	if catalogPath != wantCatalogPath {
		t.Fatalf("development asset catalog path = %q, want %q", catalogPath, wantCatalogPath)
	}
	fileInfo, err := os.Lstat(catalogPath)
	if err != nil {
		t.Fatalf("inspect development asset catalog: %v", err)
	}
	if !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("development asset catalog must be a regular non-symbolic-link file: %s", catalogPath)
	}
	if fileInfo.Mode().Perm() != 0o644 {
		t.Fatalf("development asset catalog mode = %o, want 644", fileInfo.Mode().Perm())
	}

	var catalog struct {
		Assets []struct {
			ArtifactID              string   `json:"artifactId"`
			ManifestDigest          string   `json:"manifestDigest"`
			SignatureKeyID          string   `json:"signatureKeyId"`
			Architecture            string   `json:"architecture"`
			GuestProtocolGeneration int      `json:"guestProtocolGeneration"`
			MandatoryGuestFeatures  []string `json:"mandatoryGuestFeatures"`
		} `json:"assets"`
	}
	catalogBytes, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("read development asset catalog: %v", err)
	}
	if err := json.Unmarshal(catalogBytes, &catalog); err != nil {
		t.Fatalf("decode development asset catalog: %v", err)
	}
	if len(catalog.Assets) != 1 {
		t.Fatalf("development asset catalog has %d assets, want 1", len(catalog.Assets))
	}
	asset := catalog.Assets[0]
	if asset.ArtifactID == "" ||
		!strings.HasPrefix(asset.ManifestDigest, "sha256:") ||
		len(asset.ManifestDigest) != len("sha256:")+64 ||
		asset.SignatureKeyID == "" ||
		asset.Architecture != "amd64" ||
		asset.GuestProtocolGeneration != 1 ||
		asset.MandatoryGuestFeatures == nil {
		t.Fatalf("development asset catalog entry is incomplete: %#v", asset)
	}
	firstHash := sha256.Sum256(catalogBytes)

	if output, err := exec.Command(bootstrap, environmentPath).CombinedOutput(); err != nil {
		t.Fatalf("repeat development bootstrap: %v\n%s", err, output)
	}
	repeatedBytes, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("read repeated development asset catalog: %v", err)
	}
	if sha256.Sum256(repeatedBytes) != firstHash {
		t.Fatal("idempotent development bootstrap changed the asset catalog")
	}
}

func TestDevelopmentInventoryPreparationCreatesConfiguredBucketBeforeControlPlane(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	testDirectory := t.TempDir()
	environmentPath := filepath.Join(testDirectory, "environment")
	bootstrap := filepath.Join(repositoryRoot, "deploy", "bin", "bootstrap-environment.sh")
	if output, err := exec.Command(bootstrap, environmentPath).CombinedOutput(); err != nil {
		t.Fatalf("bootstrap development environment: %v\n%s", err, output)
	}

	fakeBin := filepath.Join(testDirectory, "bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatalf("create fake command directory: %v", err)
	}
	dockerLog := filepath.Join(testDirectory, "docker.log")
	fakeDocker := `#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$FAKE_DOCKER_LOG"
`
	if err := os.WriteFile(filepath.Join(fakeBin, "docker"), []byte(fakeDocker), 0o700); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}

	prepare := filepath.Join(repositoryRoot, "deploy", "bin", "prepare-development-inventory.sh")
	for attempt := 0; attempt < 2; attempt++ {
		command := exec.Command(prepare, environmentPath)
		command.Env = append(
			os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"FAKE_DOCKER_LOG="+dockerLog,
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("prepare development inventory attempt %d: %v\n%s", attempt+1, err, output)
		}
	}

	dockerCalls, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatalf("read fake Docker calls: %v", err)
	}
	callLog := string(dockerCalls)
	for _, required := range []string{
		"compose --env-file " + environmentPath + " --file " + filepath.Join(repositoryRoot, "deploy", "compose.yml") + " --profile development up --detach --wait",
		"postgres object-store",
		"compose --env-file " + environmentPath + " --file " + filepath.Join(repositoryRoot, "deploy", "compose.yml") + " --profile development run --rm --no-deps object-store-init",
	} {
		if !strings.Contains(callLog, required) {
			t.Fatalf("development inventory preparation did not run %q:\n%s", required, callLog)
		}
	}
	if strings.Contains(callLog, "control-plane") {
		t.Fatalf("development inventory preparation started the control plane:\n%s", callLog)
	}

	compose := readRepositoryFile(t, "deploy/compose.yml")
	for _, required := range []string{
		"object-store-init:",
		"image: ${SECONDBOX_OBJECT_STORE_CLIENT_IMAGE:?",
		"MC_HOST_secondbox:",
		"- mb\n      - --ignore-existing",
		"secondbox/${SECONDBOX_OBJECT_STORE_BUCKET:?",
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("development object-store initializer must contain %q", required)
		}
	}
}

func TestProductionBootstrapRequiresOperatorSuppliedAssetCatalog(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	template := readRepositoryFile(t, "deploy/environment.example")
	template = strings.Replace(
		template,
		"SECONDBOX_DEPLOYMENT_MODE=development",
		"SECONDBOX_DEPLOYMENT_MODE=production",
		1,
	)
	environmentPath := filepath.Join(t.TempDir(), "environment")
	if err := os.WriteFile(environmentPath, []byte(template), 0o600); err != nil {
		t.Fatalf("write production bootstrap environment: %v", err)
	}

	bootstrap := filepath.Join(repositoryRoot, "deploy", "bin", "bootstrap-environment.sh")
	output, err := exec.Command(bootstrap, environmentPath).CombinedOutput()
	if err == nil {
		t.Fatal("production bootstrap generated development asset inventory")
	}
	if !bytes.Contains(output, []byte("production requires an operator-supplied SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH")) {
		t.Fatalf("production bootstrap did not identify the required asset catalog:\n%s", output)
	}
}

func TestProductionValidatorRejectsDevelopmentAssetCatalog(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	environmentPath := filepath.Join(t.TempDir(), "environment")
	bootstrap := filepath.Join(repositoryRoot, "deploy", "bin", "bootstrap-environment.sh")
	if output, err := exec.Command(bootstrap, environmentPath).CombinedOutput(); err != nil {
		t.Fatalf("bootstrap development environment: %v\n%s", err, output)
	}
	environment, err := os.ReadFile(environmentPath)
	if err != nil {
		t.Fatalf("read bootstrapped environment: %v", err)
	}
	productionEnvironment := strings.Replace(
		string(environment),
		"SECONDBOX_DEPLOYMENT_MODE=development",
		"SECONDBOX_DEPLOYMENT_MODE=production",
		1,
	)
	if err := os.WriteFile(environmentPath, []byte(productionEnvironment), 0o600); err != nil {
		t.Fatalf("write production-mode environment: %v", err)
	}

	validator := filepath.Join(repositoryRoot, "deploy", "bin", "validate-environment.sh")
	output, err := exec.Command(validator, environmentPath).CombinedOutput()
	if err == nil {
		t.Fatal("production validation accepted development asset trust inventory")
	}
	if !bytes.Contains(output, []byte("Production requires an operator-supplied signed asset catalog")) {
		t.Fatalf("production validator did not reject the development catalog:\n%s", output)
	}
}

func TestSupportBundleCollectionIsBoundedAndSecretAvoiding(t *testing.T) {
	script := readRepositoryFile(t, "deploy/bin/collect-support-bundle.sh")

	for _, required := range []string{
		"SECONDBOX_SUPPORT_MAX_LOG_BYTES",
		"SECONDBOX_SUPPORT_MAX_PROBE_BYTES",
		"SECONDBOX_SUPPORT_TIMING_WINDOW_SECONDS",
		"tail -c",
		"sha256sum",
		"healthz",
		"readyz",
		"metrics",
		"timing-summary.json",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("support-bundle script must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"BOOTSTRAP_ADMIN_TOKEN",
		"API_KEY_HASH_SECRET",
		"OBJECT_STORE_ROOT_PASSWORD",
		"env >",
		"printenv",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("support-bundle script must not contain %q", forbidden)
		}
	}
}

func TestSupportBundleExecutesOnlyImplementedProbesAndBoundsLogs(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	fakeBin := t.TempDir()
	curlLog := filepath.Join(t.TempDir(), "curl.log")
	fakeCurl := `#!/bin/sh
set -eu
output=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output="$2"; shift 2 ;;
    --write-out) shift 2 ;;
    --max-time) shift 2 ;;
    --max-filesize) shift 2 ;;
    --header) shift 2 ;;
    --silent|--show-error) shift ;;
    *) url="$1"; shift ;;
  esac
done
printf '%s\n' "$url" >>"$FAKE_CURL_LOG"
printf 'probe body\n' >"$output"
printf '200'
`
	if err := os.WriteFile(filepath.Join(fakeBin, "curl"), []byte(fakeCurl), 0o700); err != nil {
		t.Fatalf("write fake curl: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "control-plane.jsonl")
	if err := os.WriteFile(logPath, bytes.Repeat([]byte("x"), 128), 0o600); err != nil {
		t.Fatalf("write control-plane log: %v", err)
	}
	outputPath := filepath.Join(t.TempDir(), "support.tar.gz")
	command := exec.Command(
		filepath.Join(repositoryRoot, "deploy", "bin", "collect-support-bundle.sh"),
		outputPath,
	)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"FAKE_CURL_LOG="+curlLog,
		"SECONDBOX_SUPPORT_BASE_URL=http://127.0.0.1:8080",
		"SECONDBOX_SUPPORT_CONTROL_PLANE_LOG="+logPath,
		"SECONDBOX_SUPPORT_MAX_LOG_BYTES=17",
		"SECONDBOX_SUPPORT_MAX_PROBE_BYTES=1048576",
		"SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS=2",
		"SECONDBOX_SUPPORT_TIMING_WINDOW_SECONDS=300",
		"SECONDBOX_SUPPORT_PLATFORM_TOKEN=test-support-platform-token-at-least-24-bytes",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("collect support bundle: %v\n%s", err, output)
	}
	probes, err := os.ReadFile(curlLog)
	if err != nil {
		t.Fatalf("read probe log: %v", err)
	}
	if got, want := string(probes),
		"http://127.0.0.1:8080/healthz\nhttp://127.0.0.1:8080/readyz\nhttp://127.0.0.1:8080/metrics\nhttp://127.0.0.1:8080/v1/timings?windowSeconds=300\n"; got != want {
		t.Fatalf("support collector probes = %q, want %q", got, want)
	}
	extractDirectory := t.TempDir()
	if output, err := exec.Command("tar", "-xzf", outputPath, "-C", extractDirectory).CombinedOutput(); err != nil {
		t.Fatalf("extract support bundle: %v\n%s", err, output)
	}
	boundedLog, err := os.ReadFile(filepath.Join(extractDirectory, "control-plane.log.tail"))
	if err != nil {
		t.Fatalf("read bounded log: %v", err)
	}
	if len(boundedLog) != 17 {
		t.Fatalf("bounded log bytes = %d, want 17", len(boundedLog))
	}
	if _, err := os.Stat(filepath.Join(extractDirectory, "timing-summary.json")); err != nil {
		t.Fatalf("timing summary missing from support bundle: %v", err)
	}
	checksums := exec.Command("sha256sum", "--check", "SHA256SUMS")
	checksums.Dir = extractDirectory
	if output, err := checksums.CombinedOutput(); err != nil {
		t.Fatalf("support checksums: %v\n%s", err, output)
	}
}

func TestAuditExportUsesImplementedDatabaseStateAndBoundedQuery(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	fakeBin := t.TempDir()
	queryLog := filepath.Join(t.TempDir(), "query.log")
	fakePSQL := `#!/bin/sh
set -eu
printf '%s\n' "$*" >"$FAKE_PSQL_QUERY_LOG"
printf '%s\n' '{"id":"audit-1","action":"project.created"}'
`
	if err := os.WriteFile(filepath.Join(fakeBin, "psql"), []byte(fakePSQL), 0o700); err != nil {
		t.Fatalf("write fake psql: %v", err)
	}
	outputPath := filepath.Join(t.TempDir(), "audit.jsonl")
	command := exec.Command(
		filepath.Join(repositoryRoot, "deploy", "bin", "export-audit.sh"),
		outputPath,
	)
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"FAKE_PSQL_QUERY_LOG="+queryLog,
		"SECONDBOX_AUDIT_DATABASE_URL=postgresql://audit@example/secondbox",
		"SECONDBOX_AUDIT_LIMIT=37",
		"SECONDBOX_AUDIT_CONNECT_TIMEOUT_SECONDS=2",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("export audit state: %v\n%s", err, output)
	}
	exported, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read audit export: %v", err)
	}
	if string(exported) != "{\"id\":\"audit-1\",\"action\":\"project.created\"}\n" {
		t.Fatalf("audit export = %q", exported)
	}
	fileInfo, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("stat audit export: %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("audit export mode = %o, want 600", fileInfo.Mode().Perm())
	}
	query, err := os.ReadFile(queryLog)
	if err != nil {
		t.Fatalf("read audit query: %v", err)
	}
	for _, required := range []string{
		"FROM secondbox.audit_events",
		"ORDER BY created_at DESC, id DESC",
		"LIMIT 37",
	} {
		if !bytes.Contains(query, []byte(required)) {
			t.Fatalf("audit query must contain %q:\n%s", required, query)
		}
	}
}

func TestBootstrapGeneratesCertificateForConfiguredRunnerServerName(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	template := readRepositoryFile(t, "deploy/environment.example")
	template = strings.Replace(
		template,
		"SECONDBOX_RUNNER_SERVER_NAME=control-plane",
		"SECONDBOX_RUNNER_SERVER_NAME=runner.ops.example",
		1,
	)
	testDirectory := t.TempDir()
	catalogPath := filepath.Join(testDirectory, "signed-assets.json")
	if err := os.WriteFile(
		catalogPath,
		[]byte(`{"assets":[{"artifactId":"test","manifestDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","signatureKeyId":"test-key","architecture":"amd64","guestProtocolGeneration":1,"mandatoryGuestFeatures":[]}]}`),
		0o600,
	); err != nil {
		t.Fatalf("write signed asset catalog fixture: %v", err)
	}
	template = strings.Replace(
		template,
		"SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH=GENERATE_DEVELOPMENT_ASSET_CATALOG",
		"SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH="+catalogPath,
		1,
	)
	// Bootstrap fills the built-in Profile digests only when it also generates
	// the development catalog, so a deployment supplying its own catalog names
	// the bundle that catalog carries.
	template = strings.ReplaceAll(
		template,
		"GENERATE_DEVELOPMENT_BUNDLE_DIGEST",
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	)
	environmentPath := filepath.Join(testDirectory, "environment")
	if err := os.WriteFile(environmentPath, []byte(template), 0o600); err != nil {
		t.Fatalf("write custom bootstrap environment: %v", err)
	}

	bootstrap := filepath.Join(repositoryRoot, "deploy", "bin", "bootstrap-environment.sh")
	command := exec.Command(bootstrap, environmentPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap custom runner server name: %v\n%s", err, output)
	}
	validator := filepath.Join(repositoryRoot, "deploy", "bin", "validate-environment.sh")
	if output, err := exec.Command(validator, environmentPath).CombinedOutput(); err != nil {
		t.Fatalf("validate custom runner server environment: %v\n%s", err, output)
	}
	certificatePath := filepath.Join(environmentPath+".secrets", "runner-pki", "server.crt")
	checkCertificate := exec.Command(
		"openssl", "x509", "-in", certificatePath, "-noout", "-checkhost", "runner.ops.example",
	)
	if output, err := checkCertificate.CombinedOutput(); err != nil {
		t.Fatalf("runner server certificate does not cover configured name: %v\n%s", err, output)
	}

	firstAuthority := hashDeploymentAuthority(t, environmentPath)
	if output, err := exec.Command(bootstrap, environmentPath).CombinedOutput(); err != nil {
		t.Fatalf("repeat runner PKI bootstrap: %v\n%s", err, output)
	}
	secondAuthority := hashDeploymentAuthority(t, environmentPath)
	if firstAuthority != secondAuthority {
		t.Fatal("idempotent bootstrap changed deployment credentials or runner PKI authority")
	}
	for _, privatePath := range []string{
		filepath.Join(environmentPath+".secrets", "runner-pki", "runner-ca.key"),
		filepath.Join(environmentPath+".secrets", "runner-pki", "server.key"),
	} {
		fileInfo, err := os.Stat(privatePath)
		if err != nil {
			t.Fatalf("inspect generated private key: %v", err)
		}
		if fileInfo.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", privatePath, fileInfo.Mode().Perm())
		}
	}
}

func TestBackupScriptContainsOnlyDatabaseAndArtifactAuthority(t *testing.T) {
	backup := readRepositoryFile(t, "scripts/backup.sh")

	for _, required := range []string{
		"secondbox-backup/v3",
		"secondbox-backup-database-state/v2",
		"secondbox-backup-publication-fence/v1",
		"database-share-lock-held",
		"SECONDBOX_BACKUP_OBJECT_EXPORT",
		"secondbox-artifact-reachability/v1",
		"databaseRecoveryPosition",
		"--schema=secondbox",
		"psql",
		"sha256sum",
	} {
		if !strings.Contains(backup, required) {
			t.Errorf("backup script must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"SECONDBOX_RUNNER_STATE_DIR",
		"SECONDBOX_BACKUP_QUIESCENCE_EVIDENCE",
		"SECONDBOX_BACKUP_FENCING_EVIDENCE",
		"SECONDBOX_BACKUP_CHECKPOINT_REACHABILITY_EVIDENCE",
		"--schema=sandbox",
		"host-state.tar",
		"workspace_checkpoints",
		"current_checkpoint",
		"checkpoint",
		"freshRunner",
		"materialization",
	} {
		if strings.Contains(backup, forbidden) {
			t.Errorf("artifact-only backup script contains stale Workspace authority %q", forbidden)
		}
	}
	restorePath := filepath.Join(repositoryRootForDeploymentPolicy(t), "scripts", "restore-drill.sh")
	if _, err := os.Stat(restorePath); !os.IsNotExist(err) {
		t.Fatalf("portable restore drill still exists: %v", err)
	}
}

func TestBackupFailsBeforeDatabaseDumpWhenDatabaseIsNotQuiescent(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	fakeBin := t.TempDir()
	pgDumpMarker := filepath.Join(t.TempDir(), "pg-dump-called")
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	writeExecutable("pg_dump", "touch \"$PG_DUMP_MARKER\"")
	writeExecutable("psql", `
case "$*" in
  *--no-psqlrc*)
    while IFS= read -r line; do
      case "$line" in
        *SECONDBOX_BACKUP_FENCE_READY*) printf '%s\n' 'SECONDBOX_BACKUP_FENCE_READY' ;;
        COMMIT\;) exit 0 ;;
      esac
    done
    exit 1
    ;;
esac
printf '%s\n' \
  '{"contractVersion":"secondbox-backup-database-state/v2","databaseRecoveryPosition":"0/ABC","quiescence":{"activeSandboxes":1,"activeAssignments":0,"activeLifecycleEffects":0,"activeObjectPublications":0,"activeDataPlaneSessions":0},"fencing":{"activeInstances":0,"activeAssignments":0},"objects":[]}'`)
	command := exec.Command(filepath.Join(repositoryRoot, "scripts", "backup.sh"))
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"PG_DUMP_MARKER="+pgDumpMarker,
		"SECONDBOX_BACKUP_DATABASE_URL=postgresql://backup@example/secondbox",
		"SECONDBOX_BACKUP_DIR="+t.TempDir(),
		"SECONDBOX_BACKUP_RECOVERY_POINT_ID=test-recovery-point",
		"SECONDBOX_BACKUP_OBJECT_EXPORT="+t.TempDir(),
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("backup succeeded while a Sandbox mutation was active")
	}
	if !bytes.Contains(output, []byte("not quiescent")) {
		t.Fatalf("backup failure did not identify database quiescence:\n%s", output)
	}
	if _, statErr := os.Stat(pgDumpMarker); !os.IsNotExist(statErr) {
		t.Fatal("backup invoked pg_dump before proving database quiescence")
	}
}

func TestBackupHoldsSharedDatabasePublicationFenceThroughDump(t *testing.T) {
	backup := readRepositoryFile(t, "scripts/backup.sh")
	dumpIndex := strings.LastIndex(backup, "\npg_dump ")
	if dumpIndex < 0 {
		t.Fatal("backup does not invoke pg_dump")
	}
	for _, required := range []string{
		"BEGIN;",
		"LOCK TABLE %I.%I IN SHARE MODE",
		"SECONDBOX_BACKUP_FENCE_READY",
	} {
		index := strings.Index(backup, required)
		if index < 0 {
			t.Fatalf("backup publication fence is missing %q", required)
		}
		if index >= dumpIndex {
			t.Fatalf("backup acquires publication fence after pg_dump at %q", required)
		}
	}
	if !strings.Contains(backup, "COMMIT;") {
		t.Fatal("backup publication fence has no successful release")
	}
	for _, forbidden := range []string{
		"publication must already be stopped",
		"does not expose a shared backup publication fence",
	} {
		if strings.Contains(backup, forbidden) {
			t.Fatalf("backup still delegates publication fencing to the operator: %q", forbidden)
		}
	}
}

func TestBackupArtifactManifestContainsNoWorkspaceImages(t *testing.T) {
	backup := readRepositoryFile(t, "scripts/backup.sh")
	for _, required := range []string{"secondbox-backup/v3", "secondbox-artifact-reachability/v1", "artifactReachability"} {
		if !strings.Contains(backup, required) {
			t.Errorf("artifact-only backup script must contain %q", required)
		}
	}
	for _, forbidden := range []string{"checkpoint", "workspace_checkpoints", "current_checkpoint", "freshRunner", "materialization"} {
		if strings.Contains(backup, forbidden) {
			t.Errorf("artifact-only backup script retains %q", forbidden)
		}
	}
}

func TestBootstrapRejectsUnsafeRunnerServerName(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	template := readRepositoryFile(t, "deploy/environment.example")
	template = strings.Replace(
		template,
		"SECONDBOX_RUNNER_SERVER_NAME=control-plane",
		"SECONDBOX_RUNNER_SERVER_NAME=runner.example,IP:192.0.2.1",
		1,
	)
	environmentPath := filepath.Join(t.TempDir(), "environment")
	if err := os.WriteFile(environmentPath, []byte(template), 0o600); err != nil {
		t.Fatalf("write unsafe bootstrap environment: %v", err)
	}

	bootstrap := filepath.Join(repositoryRoot, "deploy", "bin", "bootstrap-environment.sh")
	output, err := exec.Command(bootstrap, environmentPath).CombinedOutput()
	if err == nil {
		t.Fatal("bootstrap accepted an injection-shaped runner server name")
	}
	if !bytes.Contains(output, []byte("SECONDBOX_RUNNER_SERVER_NAME")) {
		t.Fatalf("bootstrap error does not identify the unsafe setting:\n%s", output)
	}
}

func TestBootstrapGeneratesCertificateForConfiguredRunnerServerIPv4(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	template := readRepositoryFile(t, "deploy/environment.example")
	template = strings.Replace(
		template,
		"SECONDBOX_RUNNER_SERVER_NAME=control-plane",
		"SECONDBOX_RUNNER_SERVER_NAME=192.0.2.20",
		1,
	)
	environmentPath := filepath.Join(t.TempDir(), "environment")
	if err := os.WriteFile(environmentPath, []byte(template), 0o600); err != nil {
		t.Fatalf("write IPv4 bootstrap environment: %v", err)
	}

	bootstrap := filepath.Join(repositoryRoot, "deploy", "bin", "bootstrap-environment.sh")
	if output, err := exec.Command(bootstrap, environmentPath).CombinedOutput(); err != nil {
		t.Fatalf("bootstrap IPv4 runner server name: %v\n%s", err, output)
	}
	certificatePath := filepath.Join(environmentPath+".secrets", "runner-pki", "server.crt")
	checkCertificate := exec.Command(
		"openssl", "x509", "-in", certificatePath, "-noout", "-checkip", "192.0.2.20",
	)
	if output, err := checkCertificate.CombinedOutput(); err != nil {
		t.Fatalf("runner server certificate does not cover configured IPv4 address: %v\n%s", err, output)
	}
}

func readRepositoryFile(t *testing.T, relativePath string) string {
	t.Helper()
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	data, err := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(relativePath)))
	if err != nil {
		t.Fatalf("read %s: %v", relativePath, err)
	}
	return string(data)
}

func repositoryRootForDeploymentPolicy(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve deployment policy test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
}

func readEnvironmentSettings(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read environment settings: %v", err)
	}
	settings := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			t.Fatalf("environment line is not KEY=VALUE: %q", line)
		}
		settings[key] = value
	}
	return settings
}

func hashDeploymentAuthority(t *testing.T, environmentPath string) [32]byte {
	t.Helper()
	var authority bytes.Buffer
	environment, err := os.ReadFile(environmentPath)
	if err != nil {
		t.Fatalf("read deployment environment: %v", err)
	}
	authority.Write(environment)
	for _, name := range []string{"runner-ca.crt", "runner-ca.key", "server.crt", "server.key"} {
		data, readErr := os.ReadFile(filepath.Join(environmentPath+".secrets", "runner-pki", name))
		if readErr != nil {
			t.Fatalf("read deployment authority %s: %v", name, readErr)
		}
		authority.Write(data)
	}
	return sha256.Sum256(authority.Bytes())
}

func hashDirectoryFiles(t *testing.T, directory string, names ...string) [32]byte {
	t.Helper()
	var contents bytes.Buffer
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		contents.Write(data)
	}
	return sha256.Sum256(contents.Bytes())
}
