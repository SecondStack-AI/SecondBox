package deployment_test

import (
	"bytes"
	"errors"
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
	base := readRepositoryFile(t, "deploy/compose.yml")
	development := readRepositoryFile(t, "deploy/compose.development.yml")
	runner := readRepositoryFile(t, "deploy/compose.same-host-runner.yml")
	for _, forbidden := range []string{"same-host-runner:", "postgres:", "profiles:", "SECONDBOX_RUNNER_PROTOCOL_MINIMUM", "SECONDBOX_RUNNER_PROTOCOL_MAXIMUM"} {
		if strings.Contains(base, forbidden) {
			t.Errorf("base Compose model contains inactive topology %q", forbidden)
		}
	}
	for _, required := range []string{"control-plane:", "runner-pki-init:", "SECONDBOX_HTTP_TIMEOUT_SECONDS:"} {
		if !strings.Contains(base, required) {
			t.Errorf("base Compose model missing %q", required)
		}
	}
	for _, required := range []string{"postgres:", "pg_isready -h 127.0.0.1"} {
		if !strings.Contains(development, required) {
			t.Errorf("development overlay missing %q", required)
		}
	}
	for _, required := range []string{"same-host-runner:", "privileged: true", "/dev/kvm:/dev/kvm", "/dev/net/tun:/dev/net/tun", "stop_grace_period: 45s"} {
		if !strings.Contains(runner, required) {
			t.Errorf("same-host Runner overlay missing %q", required)
		}
	}
	if count := strings.Count(runner, "create_host_path: false"); count != 3 {
		t.Errorf("same-host Runner overlay disables host path creation for %d of 3 operator-owned binds", count)
	}
	if strings.Contains(runner, "source: ${SECONDBOX_RUNNER_WORKSPACE_HOST_DIR") {
		t.Error("same-host Runner overlay must not create a nested Workspace bind inside the Runner storage bind")
	}
	controlPlane := strings.Split(strings.Split(base, "  control-plane:")[1], "\n  runner-pki-init:")[0]
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

func TestInstallerToolsImageExecutesPackagedBtrfsFormatter(t *testing.T) {
	dockerfile := readRepositoryFile(t, "deploy/installer-tools.Dockerfile")
	for _, required := range []string{
		"apt-get install -y --no-install-recommends btrfs-progs",
		`ENTRYPOINT ["/usr/sbin/mkfs.btrfs"]`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("installer-tools image must contain %q", required)
		}
	}
	if strings.Contains(dockerfile, `ENTRYPOINT ["/usr/bin/mkfs.btrfs"]`) {
		t.Fatal("installer-tools image must use the btrfs-progs executable path shipped by Debian")
	}
}

func TestReleaseStagingRequiresQualificationEvidenceAndHostedPublishRemainsPublishOnly(t *testing.T) {
	repositoryRoot := repositoryRootForDeploymentPolicy(t)
	if _, err := os.Stat(filepath.Join(repositoryRoot, ".github/workflows/scenario-qualification.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("scenario qualification must not require a configured GitHub self-hosted runner")
	}
	staging := readRepositoryFile(t, "scripts/release-stage.sh")
	for _, required := range []string{
		".tmp/scenario-qualification-evidence.json",
		".tmp/installer-qualification-evidence.json",
		"secondbox-${version}-qualification-evidence.json",
		"secondbox-${version}-installer-qualification-evidence.json",
		"validate_qualification_evidence",
		"validate_installer_qualification_evidence",
		"evidence_commit",
		"repositoryDirty",
		"run just test-scenario",
	} {
		if !strings.Contains(staging, required) {
			t.Errorf("release staging does not enforce qualification evidence through %q", required)
		}
	}
	for _, path := range []string{"scripts/release-upload.sh", "scripts/release-publish.sh", ".github/workflows/release.yml"} {
		releaseFlow := readRepositoryFile(t, path)
		if strings.Contains(releaseFlow, "test-scenario") || strings.Contains(releaseFlow, "/dev/kvm") || strings.Contains(releaseFlow, "qualification") {
			t.Errorf("%s must remain publish-only and free of scenario qualification", path)
		}
	}
	const jobMarker = "  scenario-qualification:\n"
	ciWorkflow := readRepositoryFile(t, ".github/workflows/ci.yml")
	if strings.Contains(ciWorkflow, jobMarker) {
		t.Fatal("CI workflow must not define the scenario-qualification job")
	}
	if !strings.Contains(ciWorkflow, "run: just test-non-kvm") ||
		!strings.Contains(ciWorkflow, "runs-on: ubuntu-latest") {
		t.Fatal("portable non-KVM CI gate must remain on a GitHub-hosted runner")
	}
	for _, path := range []string{"cmd/secondbox-deploy/main.go", "cmd/secondbox-release-tool/main.go"} {
		command := readRepositoryFile(t, path)
		for _, removed := range []string{"qualification-attestation", "release-index", "candidate-evidence", "publication-input", "verify-publication"} {
			if strings.Contains(command, removed) {
				t.Errorf("%s retains removed release surface %q", path, removed)
			}
		}
	}
	for _, path := range []string{"pkg/releasefinalize", "pkg/releasepublish"} {
		if _, err := os.Stat(filepath.Join(repositoryRoot, path)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("removed release package %s still exists", path)
		}
	}
}

func TestInstallerQualificationUsesRepositoryOwnedIsolatedLibvirtDriver(t *testing.T) {
	wrapper := readRepositoryFile(t, "scripts/test-installer-qualified.sh")
	for _, required := range []string{
		`driver="$repo_root/scripts/installer-qualification-driver"`,
		`--base-image "$SECONDBOX_INSTALLER_QUALIFICATION_IMAGE"`,
		`--base-image-sha256 "$SECONDBOX_INSTALLER_QUALIFICATION_IMAGE_SHA256"`,
	} {
		if !strings.Contains(wrapper, required) {
			t.Errorf("installer qualification wrapper lacks %q", required)
		}
	}
	if strings.Contains(wrapper, "SECONDBOX_INSTALLER_QUALIFICATION_DRIVER") {
		t.Fatal("installer qualification must not delegate release evidence to an external driver")
	}

	driver := readRepositoryFile(t, "scripts/installer-qualification-driver")
	for _, required := range []string{
		"qemu:///system",
		"--cpu host-passthrough",
		"run_guest btrfs_image",
		"run_guest existing_reflink_filesystem",
		"run_guest existing_reflink_update",
		`destroy "$domain"`,
		`undefine "$domain"`,
		"cleanup_success",
		`mktemp -d "$existing_workspace_root/secondbox-installer-qualification-`,
		`chmod 0700 "$guest_root"`,
		`cp --reflink=auto -- "$base_image" "$qualification_base_image"`,
		`setfacl -m "u:${account}:rw-"`,
		`[modprobe, btrfs]`,
		"repository_base_image_sha256",
		"candidate_snapshot",
		"candidateManifestDigest",
		"local mode domain guest_root",
		`mode="$1"`,
		`cloud-localds --network-config "$guest_root/network-config"`,
		`--network "user,model=virtio,mac=$guest_mac"`,
		"allocate_loopback_port",
		`hostfwd_add hostnet0 tcp:127.0.0.1:`,
		`TCP\[HOST_FORWARD\]`,
		`--serial "pty,log.file=$console_log,log.append=on"`,
		`tail -n 120 -- "$console_log"`,
		"report_qualification_failure",
	} {
		if !strings.Contains(driver, required) {
			t.Errorf("repository qualification driver lacks %q", required)
		}
	}
	for _, forbidden := range []string{"net-create", "net-start", "net-destroy", `network=$network`, "destroy --all", "undefine --all"} {
		if strings.Contains(driver, forbidden) {
			t.Errorf("repository qualification driver contains unsafe libvirt operation %q", forbidden)
		}
	}

	guest := readRepositoryFile(t, "tests/installer/qualified-guest.sh")
	for _, required := range []string{
		"--candidate-directory",
		"oci-archive:",
		"qualification-$mode",
		"docker.io/library/registry@sha256:",
		"hello after reboot",
		`--path "sandboxId=$sandbox_id"`,
		".microvm.imageReference",
		"purge accepted an unrecorded nested mount",
		"resume accepted a replacement filesystem",
		`sudo cp --reflink=always -- "$isolation/source" "$isolation/copy"`,
		`sudo cat -- "$isolation/source"`,
		`workspaceId:.workspace.id`,
		`workspace_generation_after == generation_after`,
		"report_qualified_guest_failure",
	} {
		if !strings.Contains(guest, required) {
			t.Errorf("qualified guest executor lacks %q", required)
		}
	}
	if strings.Contains(guest, ".microVM.imageReference") {
		t.Error("qualified guest reads the microVM image from a noncanonical manifest field")
	}
}

func TestDeploymentCannotReconstructAbsentHomeOnReplacementRunner(t *testing.T) {
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
		t.Fatalf("replacement Runner changed exact-home result: %v", err)
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

func TestBackupScriptContainsOnlyDatabaseAuthority(t *testing.T) {
	backup := readRepositoryFile(t, "scripts/backup.sh")

	for _, required := range []string{
		"secondbox-backup/v4",
		"secondbox-backup-database-state/v3",
		"secondbox-backup-database-fence/v1",
		"database-share-lock-held",
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
			t.Errorf("database-only backup script contains stale Workspace authority %q", forbidden)
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
  '{"contractVersion":"secondbox-backup-database-state/v3","databaseRecoveryPosition":"0/ABC","quiescence":{"activeSandboxes":1,"activeAssignments":0,"activeLifecycleEffects":0,"activeDataPlaneSessions":0},"fencing":{"activeInstances":0,"activeAssignments":0}}'`)
	command := exec.Command(filepath.Join(repositoryRoot, "scripts", "backup.sh"))
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"PG_DUMP_MARKER="+pgDumpMarker,
		"SECONDBOX_BACKUP_DATABASE_URL=postgresql://backup@example/secondbox",
		"SECONDBOX_BACKUP_DIR="+t.TempDir(),
		"SECONDBOX_BACKUP_RECOVERY_POINT_ID=test-recovery-point",
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

func TestBackupHoldsSharedDatabaseFenceThroughDump(t *testing.T) {
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
			t.Fatalf("backup fence is missing %q", required)
		}
		if index >= dumpIndex {
			t.Fatalf("backup acquires its fence after pg_dump at %q", required)
		}
	}
	if !strings.Contains(backup, "COMMIT;") {
		t.Fatal("backup fence has no successful release")
	}
	for _, forbidden := range []string{"database mutations must already be stopped", "does not expose a shared backup fence"} {
		if strings.Contains(backup, forbidden) {
			t.Fatalf("backup still delegates database fencing to the operator: %q", forbidden)
		}
	}
}

func TestBackupManifestContainsNoWorkspaceImagesOrObjectState(t *testing.T) {
	backup := readRepositoryFile(t, "scripts/backup.sh")
	for _, required := range []string{"secondbox-backup/v4", "databaseRecoveryPosition"} {
		if !strings.Contains(backup, required) {
			t.Errorf("database-only backup script must contain %q", required)
		}
	}
	for _, forbidden := range []string{"checkpoint", "workspace_checkpoints", "current_checkpoint", "freshRunner", "materialization", "object-state.tar", "artifactReachability", "SECONDBOX_BACKUP_OBJECT_EXPORT"} {
		if strings.Contains(backup, forbidden) {
			t.Errorf("database-only backup script retains %q", forbidden)
		}
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
