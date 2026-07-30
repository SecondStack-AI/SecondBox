package imagepipeline_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const immutableOCIReference = "registry.example/secondbox/rootfs@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSecondBoxImagePipelineRequiresExplicitImmutableInput(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "prepared-rootfs")
	baseEnvironment := []string{
		"PATH=" + os.Getenv("PATH"),
		"SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR=" + outputDir,
		"SECONDBOX_RUNNER_MICROVM_BROWSER_POLICY=forbid",
	}

	output, err := runSecondBoxInputValidation(t, baseEnvironment)
	if err == nil || !strings.Contains(output, "an immutable OCI base reference or explicit SecondBox image definition is required") {
		t.Fatalf("missing source = (%v, %q), want explicit rejection", err, output)
	}

	mutableEnvironment := append(
		slicesClone(baseEnvironment),
		"SECONDBOX_RUNNER_MICROVM_OCI_BASE_REFERENCE=registry.example/secondbox/rootfs:stable",
	)
	output, err = runSecondBoxInputValidation(t, mutableEnvironment)
	if err == nil || !strings.Contains(output, "must end with @sha256:") {
		t.Fatalf("mutable OCI source = (%v, %q), want digest rejection", err, output)
	}

	immutableEnvironment := append(
		slicesClone(baseEnvironment),
		"SECONDBOX_RUNNER_MICROVM_OCI_BASE_REFERENCE="+immutableOCIReference,
		"SECONDBOX_RUNNER_MICROVM_OCI_MODE=extend",
	)
	output, err = runSecondBoxInputValidation(t, immutableEnvironment)
	if err != nil || !strings.Contains(output, "inputs valid: oci (extend, browser=forbid)") {
		t.Fatalf("immutable OCI source = (%v, %q), want acceptance", err, output)
	}

	definitionPath := filepath.Join(
		repositoryRoot(t),
		"runner/scripts/microvm-image/rootfs/secondbox-debian-image-definition.json",
	)
	definitionEnvironment := append(
		slicesClone(baseEnvironment),
		"SECONDBOX_RUNNER_MICROVM_IMAGE_DEFINITION="+definitionPath,
	)
	output, err = runSecondBoxInputValidation(t, definitionEnvironment)
	if err != nil || !strings.Contains(output, "inputs valid: secondbox_image_definition") {
		t.Fatalf("explicit image definition = (%v, %q), want acceptance", err, output)
	}

	undatedDefinition := filepath.Join(t.TempDir(), "undated-image-definition.json")
	definitionContent, err := os.ReadFile(definitionPath)
	if err != nil {
		t.Fatal(err)
	}
	definitionContent = []byte(strings.Replace(
		string(definitionContent),
		"https://snapshot.debian.org/archive/debian/20250701T000000Z/",
		"https://deb.debian.org/debian/",
		1,
	))
	if err := os.WriteFile(undatedDefinition, definitionContent, 0o600); err != nil {
		t.Fatal(err)
	}
	undatedEnvironment := append(
		slicesClone(baseEnvironment),
		"SECONDBOX_RUNNER_MICROVM_IMAGE_DEFINITION="+undatedDefinition,
	)
	output, err = runSecondBoxInputValidation(t, undatedEnvironment)
	if err == nil || !strings.Contains(output, "Debian snapshot must be dated and immutable") {
		t.Fatalf("undated image definition = (%v, %q), want rejection", err, output)
	}

	ambiguousEnvironment := append(
		slicesClone(definitionEnvironment),
		"SECONDBOX_RUNNER_MICROVM_OCI_BASE_REFERENCE="+immutableOCIReference,
	)
	output, err = runSecondBoxInputValidation(t, ambiguousEnvironment)
	if err == nil || !strings.Contains(output, "set exactly one of") {
		t.Fatalf("ambiguous source = (%v, %q), want rejection", err, output)
	}
}

func TestSecondBoxImagePipelineRequiresExplicitOCIModeAndBrowserPolicy(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "prepared-rootfs")
	baseEnvironment := []string{
		"PATH=" + os.Getenv("PATH"),
		"SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR=" + outputDir,
		"SECONDBOX_RUNNER_MICROVM_OCI_BASE_REFERENCE=" + immutableOCIReference,
	}

	output, err := runSecondBoxInputValidation(t, baseEnvironment)
	if err == nil || !strings.Contains(output, "SECONDBOX_RUNNER_MICROVM_OCI_MODE") {
		t.Fatalf("missing OCI mode = (%v, %q), want explicit rejection", err, output)
	}

	missingBrowserPolicy := append(
		slicesClone(baseEnvironment),
		"SECONDBOX_RUNNER_MICROVM_OCI_MODE=prepared",
	)
	output, err = runSecondBoxInputValidation(t, missingBrowserPolicy)
	if err == nil || !strings.Contains(output, "SECONDBOX_RUNNER_MICROVM_BROWSER_POLICY") {
		t.Fatalf("missing browser policy = (%v, %q), want explicit rejection", err, output)
	}

	preparedEnvironment := append(
		slicesClone(missingBrowserPolicy),
		"SECONDBOX_RUNNER_MICROVM_BROWSER_POLICY=allow",
	)
	output, err = runSecondBoxInputValidation(t, preparedEnvironment)
	if err != nil || !strings.Contains(output, "inputs valid: oci (prepared, browser=allow)") {
		t.Fatalf("prepared OCI source = (%v, %q), want acceptance", err, output)
	}

	invalidMode := append(
		slicesClone(baseEnvironment),
		"SECONDBOX_RUNNER_MICROVM_OCI_MODE=copy",
		"SECONDBOX_RUNNER_MICROVM_BROWSER_POLICY=allow",
	)
	output, err = runSecondBoxInputValidation(t, invalidMode)
	if err == nil || !strings.Contains(output, "must be extend or prepared") {
		t.Fatalf("invalid OCI mode = (%v, %q), want rejection", err, output)
	}

	invalidBrowserPolicy := append(
		slicesClone(baseEnvironment),
		"SECONDBOX_RUNNER_MICROVM_OCI_MODE=prepared",
		"SECONDBOX_RUNNER_MICROVM_BROWSER_POLICY=detect",
	)
	output, err = runSecondBoxInputValidation(t, invalidBrowserPolicy)
	if err == nil || !strings.Contains(output, "must be allow or forbid") {
		t.Fatalf("invalid browser policy = (%v, %q), want rejection", err, output)
	}
}

func TestArtifactVerifierKeepsKnownSignedV1BundlesVerifiable(t *testing.T) {
	verifier := readRepositoryFile(t, "runner/scripts/microvm-image/verify.sh")
	for _, required := range []string{
		"legacy_v1_policy_sha=\"46da289e29e1b51bac73d1619a7e2830d256b75a347533cde15bc38d36270e3f\"",
		`([keys[]] | sort) ==`,
		`(.source | has("browserPolicy") | not)`,
		`(.source | has("ociMode") | not)`,
		"unsigned browser policy is permitted only for the known signed v1 OCI contract",
	} {
		if !strings.Contains(verifier, required) {
			t.Errorf("artifact verifier is missing signed v1 compatibility guard %q", required)
		}
	}
	if strings.Index(verifier, "openssl dgst -sha256 -verify") >
		strings.Index(verifier, "legacy_v1_contract=false") {
		t.Fatal("artifact signature must be verified before selecting the signed v1 contract")
	}
}

func TestSecondBoxImagePipelineHasNoInheritedProductVocabulary(t *testing.T) {
	rootfsDir := filepath.Join(repositoryRoot(t), "runner/scripts/microvm-image/rootfs")
	forbidden := []string{
		"agent",
		"secondstack",
		"profile.d",
		"releases/microvm/latest",
		"apt-std.txt",
		"requirements-std.txt",
		"build-rootfs-source.sh",
		"verify-standard-toolset.sh",
		"debian-rootfs.lock",
	}

	err := filepath.WalkDir(rootfsDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relativePath, err := filepath.Rel(rootfsDir, path)
		if err != nil {
			return err
		}
		lowerPath := strings.ToLower(relativePath)
		for _, forbiddenText := range forbidden {
			if strings.Contains(lowerPath, forbiddenText) {
				t.Errorf("forbidden inherited vocabulary %q remains in path %s", forbiddenText, relativePath)
			}
		}
		if entry.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lowerContent := strings.ToLower(string(content))
		for _, forbiddenText := range forbidden {
			if strings.Contains(lowerContent, forbiddenText) {
				t.Errorf("forbidden inherited vocabulary %q remains in %s", forbiddenText, relativePath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSecondBoxImagePipelineHasNoBaseImageDefaultOrMutableTag(t *testing.T) {
	dockerfile := readRepositoryFile(t, "runner/scripts/microvm-image/rootfs/Dockerfile")
	if !strings.Contains(dockerfile, "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}") {
		t.Fatal("Dockerfile must consume a caller-supplied BASE_IMAGE")
	}
	if strings.Contains(dockerfile, "ARG BASE_IMAGE=") {
		t.Fatal("Dockerfile must not default BASE_IMAGE")
	}

	allInputs := strings.Join([]string{
		dockerfile,
		readRepositoryFile(t, "runner/scripts/microvm-image/rootfs/build-secondbox-rootfs-source.sh"),
		readRepositoryFile(t, "runner/scripts/microvm-image/rootfs/README.md"),
	}, "\n")
	for _, forbidden := range []string{
		":latest",
		":base",
		":std",
		"MICROVM_BASE_IMAGE",
		"MICROVM_BASE_ROOTFS",
		"MICROVM_ROOTFS_SOURCE_MODE",
	} {
		if strings.Contains(allInputs, forbidden) {
			t.Errorf("mutable/default base assumption %q remains", forbidden)
		}
	}
	if !strings.Contains(allInputs, `@sha256:[0-9a-f]{64}`) {
		t.Fatal("build input validation must require an OCI sha256 digest")
	}
	sourceBuilder := readRepositoryFile(
		t,
		"runner/scripts/microvm-image/rootfs/build-secondbox-rootfs-source.sh",
	)
	if !strings.Contains(sourceBuilder, `base_image_reference="$oci_base_reference"`) ||
		!strings.Contains(sourceBuilder, `--build-arg "BASE_IMAGE=$base_image_reference"`) {
		t.Fatal("OCI builds must preserve the repository-qualified digest reference for BuildKit")
	}
	for _, required := range []string{
		`base_image_tag="secondbox-local-rootfs-base:build-$$"`,
		`docker tag "$base_image_id" "$base_image_tag"`,
		`base_image_reference="$base_image_tag"`,
		`docker image rm "$base_image_tag"`,
	} {
		if !strings.Contains(sourceBuilder, required) {
			t.Errorf("definition builds must make the imported base resolvable by BuildKit with %q", required)
		}
	}
	if !strings.Contains(sourceBuilder, `Dockerfile.prepared`) ||
		!strings.Contains(sourceBuilder, `OCI_MODE="$oci_mode"`) ||
		!strings.Contains(sourceBuilder, `BROWSER_POLICY="$browser_policy"`) {
		t.Fatal("prepared OCI mode and browser policy must remain explicit provenance inputs")
	}
	for _, excludedRuntimeTree := range []string{"dev/*", "proc/*", "sys/*"} {
		if !strings.Contains(sourceBuilder, "--exclude='"+excludedRuntimeTree+"'") {
			t.Errorf("unprivileged rootfs export must exclude %s", excludedRuntimeTree)
		}
	}
	if !strings.Contains(sourceBuilder, `install -d -o 0 -g 0 -m 1777 "$out_dir/tmp"`) {
		t.Fatal("fakeroot-backed prepared rootfs export must normalize /tmp to root-owned mode 1777")
	}
	if !strings.Contains(sourceBuilder, `fakeroot -s "$fakeroot_state"`) ||
		!strings.Contains(sourceBuilder, `fakeroot -i "$fakeroot_state" -s "$fakeroot_state"`) {
		t.Fatal("prepared rootfs export must persist OCI ownership through fakeroot metadata")
	}
	artifactBuilder := readRepositoryFile(t, "runner/scripts/microvm-image/build.sh")
	if !strings.Contains(artifactBuilder, `fakeroot -i "$rootfs_source_fakeroot_state"`) {
		t.Fatal("ext4 assembly must load the prepared rootfs fakeroot ownership metadata")
	}
}

func TestSecondBoxImageDefinitionAndPythonRequirementsArePinned(t *testing.T) {
	type imageDefinition struct {
		SchemaVersion int    `json:"schemaVersion"`
		Kind          string `json:"kind"`
		Debian        struct {
			Snapshot string `json:"snapshot"`
		} `json:"debian"`
	}
	var definition imageDefinition
	definitionText := readRepositoryFile(
		t,
		"runner/scripts/microvm-image/rootfs/secondbox-debian-image-definition.json",
	)
	if err := json.Unmarshal([]byte(definitionText), &definition); err != nil {
		t.Fatal(err)
	}
	if definition.SchemaVersion != 1 || definition.Kind != "secondbox.microvm-rootfs" {
		t.Fatalf("unexpected image definition identity: %#v", definition)
	}
	datedSnapshot := regexp.MustCompile(
		`^https://snapshot\.debian\.org/archive/debian/[0-9]{8}T[0-9]{6}Z/$`,
	)
	if !datedSnapshot.MatchString(definition.Debian.Snapshot) {
		t.Fatalf("Debian snapshot is not dated and immutable: %q", definition.Debian.Snapshot)
	}

	requirementPattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*==[A-Za-z0-9][A-Za-z0-9_.+!-]*$`)
	requirements := readRepositoryFile(
		t,
		"runner/scripts/microvm-image/rootfs/secondbox-python-requirements.txt",
	)
	pinnedCount := 0
	for _, line := range strings.Split(requirements, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pinnedCount++
		if !requirementPattern.MatchString(line) {
			t.Errorf("Python requirement is not exactly pinned: %q", line)
		}
	}
	if pinnedCount == 0 {
		t.Fatal("Python requirements must not be empty")
	}
}

func TestSecondBoxImagePipelineEmitsLicenseAndResolvedProvenance(t *testing.T) {
	requiredOutputs := []string{
		"rootfs-source-manifest.json",
		"rootfs-debian-packages.lock",
		"rootfs-python.freeze",
		"rootfs-debian-license-inventory.json",
		"rootfs-python-license-inventory.json",
	}
	pipelineFiles := strings.Join([]string{
		readRepositoryFile(t, "runner/scripts/microvm-image/rootfs/Dockerfile"),
		readRepositoryFile(t, "runner/scripts/microvm-image/rootfs/build-secondbox-rootfs-source.sh"),
		readRepositoryFile(t, "runner/scripts/microvm-image/rootfs/collect-secondbox-rootfs-provenance.py"),
		readRepositoryFile(t, "runner/scripts/microvm-image/rootfs/README.md"),
	}, "\n")
	for _, output := range requiredOutputs {
		if !strings.Contains(pipelineFiles, output) {
			t.Errorf("required provenance output %q is not wired through the pipeline", output)
		}
	}
	for _, requiredEvidence := range []string{
		"copyrightSha256",
		"licenseExpression",
		"licenseClassifiers",
		"metadataSha256",
		"pythonRequirementsSha256",
		"aptPackagesSha256",
		"debianSnapshot",
	} {
		if !strings.Contains(pipelineFiles, requiredEvidence) {
			t.Errorf("required provenance evidence %q is missing", requiredEvidence)
		}
	}
}

func TestPreparedOCIImageRemovesMachineIdentityAndScansOnlyRuntimeSecrets(t *testing.T) {
	preparedDockerfile := readRepositoryFile(
		t,
		"runner/scripts/microvm-image/rootfs/Dockerfile.prepared",
	)
	if !strings.Contains(preparedDockerfile, `rm -f /etc/ssh/ssh_host_*`) {
		t.Fatal("prepared OCI rootfs must remove image-baked SSH host identity")
	}
	scanner := readRepositoryFile(t, "runner/scripts/microvm-image/scan-no-secrets.sh")
	if !strings.Contains(scanner, `-g '!**/opt/go/src/**/testdata/**'`) ||
		!strings.Contains(scanner, `-g '!**/opt/go/src/crypto/x509/platform_root_key.pem'`) {
		t.Fatal("secret scan must distinguish pinned Go toolchain fixtures from runtime credentials")
	}
}

func TestStandardImageRemovesPackagedPrivateKeyFixtures(t *testing.T) {
	dockerfile := readRepositoryFile(
		t,
		"runner/scripts/microvm-image/rootfs/Dockerfile",
	)
	if !strings.Contains(dockerfile, `rm -rf /usr/share/doc/python3-aiohttp/examples`) {
		t.Fatal("standard rootfs must remove the packaged aiohttp example private key")
	}
	scanner := readRepositoryFile(t, "runner/scripts/microvm-image/scan-no-secrets.sh")
	if strings.Contains(scanner, "python3-aiohttp") {
		t.Fatal("packaged private-key fixtures must be removed, not exempted from the secret scan")
	}
}

func TestDebianSnapshotSourcePolicyAcceptsBothCanonicalURLForms(t *testing.T) {
	policy := readRepositoryFile(
		t,
		"runner/scripts/microvm-image/rootfs/config/verify-debian-snapshot-sources.sh",
	)
	pattern := regexp.MustCompile(
		`https://snapshot\\\.debian\\\.org/archive/debian/\[0-9\]\{8\}T\[0-9\]\{6\}Z/\?\(\[\[:space:\]\]\|\$\)`,
	)
	if !pattern.MatchString(policy) {
		t.Fatal("snapshot source policy must accept dated Debian URLs with or without a trailing slash")
	}
}

func TestMicroVMArtifactBuilderResolvesRunnerModuleGuestAgent(t *testing.T) {
	builder := readRepositoryFile(t, "runner/scripts/microvm-image/build.sh")
	if !strings.Contains(builder, `runner_root="$(cd "$script_dir/../.." && pwd)"`) ||
		!strings.Contains(builder, `repo_root="$(cd "$runner_root/.." && pwd)"`) {
		t.Fatal("artifact builder must distinguish the runner module from the repository root")
	}
	if !strings.Contains(
		builder,
		`go -C "$runner_root" build`,
	) || !strings.Contains(
		builder,
		`./cmd/secondbox-guest-agent`,
	) {
		t.Fatal("artifact builder must compile the guest agent from the runner module")
	}
	if !strings.Contains(builder, `mkdir -p "$runner_root/releases/microvm"`) {
		t.Fatal("artifact builder must create the release-link parent directory")
	}
}

func TestPinnedKernelBuilderWritesOnlyTheKernelPathToStandardOutput(t *testing.T) {
	builder := readRepositoryFile(t, "runner/scripts/microvm-image/build-kernel.sh")
	if !strings.Contains(builder, `sha256sum -c - >/dev/null`) {
		t.Fatal("kernel source checksum diagnostics must not contaminate the returned kernel path")
	}
	if !strings.Contains(builder, `echo "$out_dir/vmlinux"`) {
		t.Fatal("kernel builder must return the built vmlinux path")
	}
}

func runSecondBoxInputValidation(t *testing.T, environment []string) (string, error) {
	t.Helper()
	script := filepath.Join(
		repositoryRoot(t),
		"runner/scripts/microvm-image/rootfs/build-secondbox-rootfs-source.sh",
	)
	command := exec.Command("bash", script, "--validate-inputs-only")
	command.Env = environment
	output, err := command.CombinedOutput()
	return string(output), err
}

func readRepositoryFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(repositoryRoot(t), path))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func slicesClone(values []string) []string {
	return append([]string(nil), values...)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve image-pipeline test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}
