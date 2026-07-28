package operations_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
		"pg_isready -h 127.0.0.1",
		"image: ${SECONDBOX_RUNNER_IMAGE:?",
		"org.secondbox.runner.qualification: \"not-established-by-compose\"",
		"${SECONDBOX_RUNNER_IDENTITY_HOST_DIR:?",
		"${SECONDBOX_RUNNER_ARTIFACT_HOST_DIR:?",
		"${SECONDBOX_RUNNER_STATE_HOST_DIR:?",
		"${SECONDBOX_RUNNER_WORKSPACE_HOST_DIR:?",
		"${SECONDBOX_RUNNER_CHECKPOINT_RESTORE_SPOOL_HOST_DIR:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS:?",
		"${SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM:?",
		"${SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT:?",
		"${SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT:?",
		"${SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT:?",
		"${SECONDBOX_RUNNER_CHECKPOINT_RESTORE_SPOOL_DIR:?",
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

func TestDeploymentEnvironmentHasNoBlankOrSharedCredentials(t *testing.T) {
	example := readRepositoryFile(t, "deploy/environment.example")

	for _, runnerSetting := range []string{
		"SECONDBOX_RUNNER_IMAGE=secondbox-runner:development",
		"SECONDBOX_SAME_HOST_RUNNER_ENABLED=false",
		"SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT=1024",
		"SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT=1025",
		"SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL=5s",
		"SECONDBOX_RUNNER_ENABLED_FEATURES=exec-streaming,file-streaming,pty,evidence,checkpoint,port-proxy",
		"SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH=/usr/sbin/nft",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS=256",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL=5m",
		"SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES=172.30.0.1",
		"SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS=172.30.0.0/24",
		"SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM=1.1.1.1:53",
		"SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT=70",
		"SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT=80",
		"SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT=90",
		"SECONDBOX_RUNNER_CHECKPOINT_RESTORE_SPOOL_DIR=/var/lib/secondbox-runner/checkpoint-restore-spool",
		"SECONDBOX_RUNNER_WORKSPACE_HOST_DIR=/var/lib/secondbox/runner-workspaces",
		"SECONDBOX_RUNNER_CHECKPOINT_RESTORE_SPOOL_HOST_DIR=/var/lib/secondbox/runner-restore-spool",
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
		"SECONDBOX_BOOTSTRAP_ADMIN_TOKEN=GENERATE_WITH_DEPLOY_BOOTSTRAP",
		"SECONDBOX_API_KEY_HASH_SECRET=GENERATE_WITH_DEPLOY_BOOTSTRAP",
		"SECONDBOX_RUNNER_ENROLLMENT_HASH_SECRET=GENERATE_WITH_DEPLOY_BOOTSTRAP",
		"SECONDBOX_RUNNER_PKI_HOST_DIR=GENERATE_RUNNER_PKI",
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
		"SECONDBOX_RUNNER_IDENTITY_BINARY",
		"SECONDBOX_RUNNER_ENROLLMENT_TOKEN",
		"openssl genpkey",
		"openssl req",
		"redeem",
		"--certificate-output",
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
	fakeIdentity := filepath.Join(t.TempDir(), "secondbox-runner-identity")
	fakeIdentityScript := `#!/bin/sh
set -eu
[ "$1" = redeem ]
shift
while [ "$#" -gt 0 ]; do
  case "$1" in
    --csr) csr="$2"; shift 2 ;;
    --certificate-output) output="$2"; shift 2 ;;
    *) exit 2 ;;
  esac
done
openssl x509 -req -in "$csr" -CA "$TEST_CA_CERTIFICATE" -CAkey "$TEST_CA_KEY" \
  -CAcreateserial -out "$output"
`
	if err := os.WriteFile(fakeIdentity, []byte(fakeIdentityScript), 0o700); err != nil {
		t.Fatalf("write fake runner identity command: %v", err)
	}
	identityDirectory := filepath.Join(t.TempDir(), "identity")
	script := filepath.Join(repositoryRoot, "deploy", "bin", "bootstrap-runner-trust.sh")
	first := exec.Command(script, identityDirectory)
	first.Env = append(os.Environ(),
		"SECONDBOX_RUNNER_ID=runner-test-1",
		"SECONDBOX_RUNNER_CA_CERTIFICATE="+caCertificate,
		"SECONDBOX_RUNNER_IDENTITY_BINARY="+fakeIdentity,
		"SECONDBOX_RUNNER_ENROLLMENT_TOKEN=test-single-use-token",
		"TEST_CA_CERTIFICATE="+caCertificate,
		"TEST_CA_KEY="+caKey,
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
		"tail -c",
		"sha256sum",
		"healthz",
		"readyz",
		"metrics",
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
		"SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS=2",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("collect support bundle: %v\n%s", err, output)
	}
	probes, err := os.ReadFile(curlLog)
	if err != nil {
		t.Fatalf("read probe log: %v", err)
	}
	if got, want := string(probes),
		"http://127.0.0.1:8080/healthz\nhttp://127.0.0.1:8080/readyz\nhttp://127.0.0.1:8080/metrics\n"; got != want {
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

func TestRecoveryScriptsRejectRunnerLocalStateAndRequireQualifiedEvidence(t *testing.T) {
	backup := readRepositoryFile(t, "scripts/backup.sh")
	restore := readRepositoryFile(t, "scripts/restore-drill.sh")

	for _, required := range []string{
		"secondbox-backup/v2",
		"secondbox-backup-database-state/v1",
		"secondbox-backup-publication-fence/v1",
		"database-share-lock-held",
		"SECONDBOX_BACKUP_OBJECT_EXPORT",
		"secondbox-checkpoint-reachability/v1",
		"databaseRecoveryPosition",
		"--schema=secondbox",
		"psql",
		"sha256sum",
	} {
		if !strings.Contains(backup, required) {
			t.Errorf("backup script must contain %q", required)
		}
	}
	for _, required := range []string{
		"secondbox-backup/v2",
		"secondbox-backup-publication-fence/v1",
		"database-share-lock-held",
		"SECONDBOX_RESTORE_FRESH_RUNNER_RESULT",
		"SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_COMMAND",
		"SECONDBOX_RESTORE_CONTROL_PLANE_URL",
		"SECONDBOX_RESTORE_CONTROL_PLANE_TOKEN",
		"SECONDBOX_RESTORE_OBJECT_TARGET",
		"checkpointReachability",
		"freshRunnerVerification",
		"to_regnamespace('secondbox')",
		"secondbox-restore-database-roots/v1",
		"secondbox-fresh-runner-database-verification/v1",
		"secondbox-fresh-runner-identity/v1",
		"generation_fenced",
		"runnerCredentialSerials",
		"readyMaterialization",
	} {
		if !strings.Contains(restore, required) {
			t.Errorf("restore drill must contain %q", required)
		}
	}
	for _, script := range []string{backup, restore} {
		for _, forbidden := range []string{
			"SECONDBOX_RUNNER_STATE_DIR",
			"SECONDBOX_BACKUP_QUIESCENCE_EVIDENCE",
			"SECONDBOX_BACKUP_FENCING_EVIDENCE",
			"SECONDBOX_BACKUP_CHECKPOINT_REACHABILITY_EVIDENCE",
			"SECONDBOX_RESTORE_FRESH_RUNNER_EVIDENCE",
			"--schema=sandbox",
			"host-state.tar",
			"to_regnamespace('sandbox')",
		} {
			if strings.Contains(script, forbidden) {
				t.Errorf("recovery script must not contain stale authority %q", forbidden)
			}
		}
	}
	for _, forbidden := range []string{
		".newCredential == true",
		".checkpointRestored == true",
		".staleGenerationRejected == true",
	} {
		if strings.Contains(restore, forbidden) {
			t.Errorf("restore drill must not accept boolean-only evidence %q", forbidden)
		}
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
  '{"contractVersion":"secondbox-backup-database-state/v1","databaseRecoveryPosition":"0/ABC","quiescence":{"activeSandboxes":1,"activeAssignments":0,"activeLifecycleEffects":0,"activeObjectPublications":0,"activeDataPlaneSessions":0},"fencing":{"activeInstances":0,"activeAssignments":0},"danglingCheckpointReferences":0,"runnerCredentialSerials":[],"objects":[]}'`)
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

func TestRestoreDrillSupervisesFreshRunnerVerifierThroughLiveChecks(t *testing.T) {
	restore := readRepositoryFile(t, "scripts/restore-drill.sh")
	for _, required := range []string{
		"SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS",
		"fresh_runner_verifier_pid",
		"kill -TERM",
		"wait \"$fresh_runner_verifier_pid\"",
	} {
		if !strings.Contains(restore, required) {
			t.Fatalf("restore drill does not supervise the fresh-Runner verifier: missing %q", required)
		}
	}
}

func TestBackupAndRestoreScriptsPreserveCoordinatedEvidenceContract(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	fakeBin := t.TempDir()
	backupFenceMarker := filepath.Join(t.TempDir(), "backup-fence-active")
	writeFakeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	writeFakeExecutable("pg_dump", `
if [ ! -f "$FAKE_BACKUP_FENCE_ACTIVE" ]; then
  echo "pg_dump ran without the shared backup publication fence" >&2
  exit 1
fi
for argument in "$@"; do
  case "$argument" in --file=*) output="${argument#--file=}";; esac
done
printf 'test database dump\n' >"$output"`)
	writeFakeExecutable("createdb", "exit 0")
	writeFakeExecutable("dropdb", "exit 0")
	writeFakeExecutable("pg_restore", "printf 'SELECT 1;\\n'")
	writeFakeExecutable("psql", `
case "$*" in
  *--no-psqlrc*)
    while IFS= read -r line; do
      case "$line" in
        *SECONDBOX_BACKUP_FENCE_READY*)
          : >"$FAKE_BACKUP_FENCE_ACTIVE"
          printf '%s\n' 'SECONDBOX_BACKUP_FENCE_READY'
          ;;
        COMMIT\;)
          rm -f "$FAKE_BACKUP_FENCE_ACTIVE"
          exit 0
          ;;
      esac
    done
    exit 1
    ;;
esac
input="$(cat)"
case "$* $input" in
  *secondbox-backup-database-state/v1*)
    printf '{"contractVersion":"secondbox-backup-database-state/v1","databaseRecoveryPosition":"0/ABC","quiescence":{"activeSandboxes":0,"activeAssignments":0,"activeLifecycleEffects":0,"activeObjectPublications":0,"activeDataPlaneSessions":0},"fencing":{"activeInstances":0,"activeAssignments":0},"danglingCheckpointReferences":0,"runnerCredentialSerials":["credential-before-backup"],"objects":[{"kind":"checkpoint","id":"chk-operations","storageObjectId":"checkpoint.bin","sha256":"%s","sizeBytes":19}]}\n' "$FAKE_OBJECT_SHA256"
    ;;
  *secondbox-restore-database-roots/v1*)
    printf '{"contractVersion":"secondbox-restore-database-roots/v1","objects":[{"kind":"checkpoint","id":"chk-operations","storageObjectId":"checkpoint.bin","sha256":"%s","sizeBytes":19}]}\n' "$FAKE_RESTORED_OBJECT_SHA256"
    ;;
  *secondbox-fresh-runner-database-verification/v1*)
    printf '%s\n' '{"contractVersion":"secondbox-fresh-runner-database-verification/v1","freshCredential":true,"readyAssignment":true,"readyMaterialization":true,"authoritativeCheckpoint":true}'
    ;;
  *to_regnamespace*) printf 't\n' ;;
  *) : ;;
esac`)

	recoveryPointID := "operations-contract-1"
	evidenceDirectory := t.TempDir()
	objectExport := t.TempDir()
	checkpointBytes := []byte("portable checkpoint")
	if err := os.WriteFile(filepath.Join(objectExport, "checkpoint.bin"), checkpointBytes, 0o600); err != nil {
		t.Fatalf("write object export: %v", err)
	}
	checkpointSHA := sha256.Sum256(checkpointBytes)
	if err := os.WriteFile(filepath.Join(objectExport, "unreachable.bin"), []byte("not rooted"), 0o600); err != nil {
		t.Fatalf("write unreachable object: %v", err)
	}
	rejectedBackup := exec.Command(filepath.Join(repositoryRoot, "scripts", "backup.sh"))
	rejectedBackup.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"FAKE_BACKUP_FENCE_ACTIVE="+backupFenceMarker,
		fmt.Sprintf("FAKE_OBJECT_SHA256=%x", checkpointSHA),
		fmt.Sprintf("FAKE_RESTORED_OBJECT_SHA256=%x", checkpointSHA),
		"SECONDBOX_BACKUP_DATABASE_URL=postgresql://backup@example/secondbox",
		"SECONDBOX_BACKUP_DIR="+t.TempDir(),
		"SECONDBOX_BACKUP_RECOVERY_POINT_ID="+recoveryPointID,
		"SECONDBOX_BACKUP_OBJECT_EXPORT="+objectExport,
	)
	if output, err := rejectedBackup.CombinedOutput(); err == nil {
		t.Fatal("backup accepted an object export containing a non-database-rooted object")
	} else if !bytes.Contains(output, []byte("differs from database roots")) {
		t.Fatalf("backup object-root mismatch was not diagnosed:\n%s", output)
	}
	if _, err := os.Stat(backupFenceMarker); !os.IsNotExist(err) {
		t.Fatal("failed backup retained the shared database publication fence")
	}
	if err := os.Remove(filepath.Join(objectExport, "unreachable.bin")); err != nil {
		t.Fatalf("remove unreachable object fixture: %v", err)
	}
	backupDirectory := t.TempDir()
	backup := exec.Command(filepath.Join(repositoryRoot, "scripts", "backup.sh"))
	backup.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"FAKE_BACKUP_FENCE_ACTIVE="+backupFenceMarker,
		fmt.Sprintf("FAKE_OBJECT_SHA256=%x", checkpointSHA),
		fmt.Sprintf("FAKE_RESTORED_OBJECT_SHA256=%x", checkpointSHA),
		"SECONDBOX_BACKUP_DATABASE_URL=postgresql://backup@example/secondbox",
		"SECONDBOX_BACKUP_DIR="+backupDirectory,
		"SECONDBOX_BACKUP_RECOVERY_POINT_ID="+recoveryPointID,
		"SECONDBOX_BACKUP_OBJECT_EXPORT="+objectExport,
	)
	if output, err := backup.CombinedOutput(); err != nil {
		t.Fatalf("create coordinated backup contract: %v\n%s", err, output)
	}
	if _, err := os.Stat(backupFenceMarker); !os.IsNotExist(err) {
		t.Fatal("successful backup retained the shared database publication fence")
	}
	bundles, err := filepath.Glob(filepath.Join(backupDirectory, "secondbox-backup-*.tar"))
	if err != nil || len(bundles) != 1 {
		t.Fatalf("backup bundles = %v, error = %v", bundles, err)
	}
	portableDirectory := t.TempDir()
	portableBundle := filepath.Join(portableDirectory, filepath.Base(bundles[0]))
	if err := os.Rename(bundles[0], portableBundle); err != nil {
		t.Fatalf("move recovery bundle: %v", err)
	}
	if err := os.Rename(bundles[0]+".sha256", portableBundle+".sha256"); err != nil {
		t.Fatalf("move recovery bundle checksum: %v", err)
	}

	freshRunnerEvidence := filepath.Join(evidenceDirectory, "fresh-runner.json")
	freshRunnerVerifier := filepath.Join(fakeBin, "verify-fresh-runner")
	writeFakeExecutable("verify-fresh-runner", `
output="$1"
printf '{"contractVersion":"secondbox-fresh-runner-identity/v1","recoveryPointId":"%s","runner":{"id":"runner-after-restore","credentialSerial":"credential-after-restore"},"restoration":{"sandboxId":"sandbox-restored","workspaceId":"workspace-restored","assignmentId":"assignment-restored","materializationId":"materialization-restored","checkpointId":"chk-operations","checkpointSHA256":"%s","generation":8}}\n' \
  "$SECONDBOX_RESTORE_VERIFICATION_RECOVERY_POINT_ID" "$FAKE_OBJECT_SHA256" >"$output.tmp"
chmod 600 "$output.tmp"
mv "$output.tmp" "$output"
trap 'exit 0' TERM
while :; do sleep 1; done`)
	controlPlaneToken := "restore-drill-token"
	controlPlane := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost ||
			request.URL.Path != "/v1/sandboxes/sandbox-restored:ping" ||
			request.Header.Get("Authorization") != "Bearer "+controlPlaneToken {
			http.Error(writer, "unexpected restore-drill request", http.StatusBadRequest)
			return
		}
		switch request.Header.Get("SecondBox-Generation") {
		case "8":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(
				`{"sandboxId":"sandbox-restored","generation":8,"healthy":true,"observedAt":"2026-07-28T19:00:00Z"}`,
			))
		case "7":
			writer.Header().Set("Content-Type", "application/problem+json")
			writer.WriteHeader(http.StatusConflict)
			_, _ = writer.Write([]byte(
				`{"type":"https://secondbox.dev/problems/generation_fenced","title":"Sandbox generation is fenced","status":409,"code":"generation_fenced","requestId":"restore-drill-stale-generation","retryable":false}`,
			))
		default:
			http.Error(writer, "unexpected generation", http.StatusBadRequest)
		}
	}))
	defer controlPlane.Close()
	restoreParent := t.TempDir()
	objectTarget := filepath.Join(restoreParent, "restored-objects")
	restore := exec.Command(filepath.Join(repositoryRoot, "scripts", "restore-drill.sh"))
	restore.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		fmt.Sprintf("FAKE_OBJECT_SHA256=%x", checkpointSHA),
		fmt.Sprintf("FAKE_RESTORED_OBJECT_SHA256=%x", checkpointSHA),
		"SECONDBOX_RESTORE_DATABASE_URL=postgresql://restore@example/postgres",
		"SECONDBOX_RESTORE_CONTROL_PLANE_URL="+controlPlane.URL,
		"SECONDBOX_RESTORE_CONTROL_PLANE_TOKEN="+controlPlaneToken,
		"SECONDBOX_RESTORE_BUNDLE="+portableBundle,
		"SECONDBOX_RESTORE_STAGE_DIR="+t.TempDir(),
		"SECONDBOX_RESTORE_OBJECT_TARGET="+objectTarget,
		"SECONDBOX_RESTORE_FRESH_RUNNER_RESULT="+freshRunnerEvidence,
		"SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_COMMAND="+freshRunnerVerifier,
		"SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS=10",
	)
	if output, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("run isolated restore contract: %v\n%s", err, output)
	}
	restoredObject, err := os.ReadFile(filepath.Join(objectTarget, "checkpoint.bin"))
	if err != nil {
		t.Fatalf("read restored object: %v", err)
	}
	if string(restoredObject) != "portable checkpoint" {
		t.Fatalf("restored object = %q", restoredObject)
	}

	mismatchedEvidence := filepath.Join(evidenceDirectory, "fresh-runner-mismatched-roots.json")
	mismatchedRestoreParent := t.TempDir()
	mismatchedRestore := exec.Command(filepath.Join(repositoryRoot, "scripts", "restore-drill.sh"))
	mismatchedRestore.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		fmt.Sprintf("FAKE_OBJECT_SHA256=%x", checkpointSHA),
		"FAKE_RESTORED_OBJECT_SHA256="+strings.Repeat("0", 64),
		"SECONDBOX_RESTORE_DATABASE_URL=postgresql://restore@example/postgres",
		"SECONDBOX_RESTORE_CONTROL_PLANE_URL="+controlPlane.URL,
		"SECONDBOX_RESTORE_CONTROL_PLANE_TOKEN="+controlPlaneToken,
		"SECONDBOX_RESTORE_BUNDLE="+portableBundle,
		"SECONDBOX_RESTORE_STAGE_DIR="+t.TempDir(),
		"SECONDBOX_RESTORE_OBJECT_TARGET="+filepath.Join(mismatchedRestoreParent, "restored-objects"),
		"SECONDBOX_RESTORE_FRESH_RUNNER_RESULT="+mismatchedEvidence,
		"SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_COMMAND="+freshRunnerVerifier,
		"SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS=10",
	)
	if output, err := mismatchedRestore.CombinedOutput(); err == nil {
		t.Fatal("restore drill accepted database roots that differed from the recovery manifest")
	} else if !bytes.Contains(output, []byte("Restored database roots differ")) {
		t.Fatalf("restore database-root mismatch was not diagnosed:\n%s", output)
	}
	if _, err := os.Stat(mismatchedEvidence); !os.IsNotExist(err) {
		t.Fatal("restore invoked the fresh-Runner verifier before database-root comparison passed")
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
