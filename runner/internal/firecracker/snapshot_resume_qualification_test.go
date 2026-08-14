package firecracker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

const snapshotTemplateQualificationSchemaVersion = 1

type snapshotTemplateQualificationReport struct {
	SchemaVersion         int    `json:"schemaVersion"`
	SourceCommit          string `json:"sourceCommit"`
	SourceTreeDirty       bool   `json:"sourceTreeDirty"`
	GoVersion             string `json:"goVersion"`
	ComputeBackendVersion string `json:"firecrackerVersion"`
	HostKernel            string `json:"hostKernel"`
	HostCPU               string `json:"hostCpu"`
	WorkspaceFilesystem   string `json:"workspaceFilesystem"`
	CompletedAt           string `json:"completedAt"`
	IdentityNeutral       bool   `json:"identityNeutralTemplate"`
	TemplateID            string `json:"templateId"`
	MemoryFileBytes       int64  `json:"memoryFileBytes"`
	RootfsFileBytes       int64  `json:"rootfsFileBytes"`
	VMStateFileBytes      int64  `json:"vmStateFileBytes"`
	TemplateBuildMillis   int64  `json:"templateBuildMilliseconds"`
	AdmissionMillis       int64  `json:"cacheAdmissionMilliseconds"`
	StableIdentityNanos   int64  `json:"perStartStableIdentityNanoseconds"`
	UnjailedResumeError   string `json:"unjailedResumeRefusal"`
}

// TestSmokeSnapshotResumeTemplateLifecycle qualifies the runner-managed
// template artifact: build it from a real signed boot, seal VM state, memory,
// and the post-boot rootfs at one paused point, publish it atomically, admit it
// through the runner-local cache with full digest verification, and prove the
// per-start stable-identity check.
//
// It deliberately does not resume an Instance. Firecracker opens every block
// device at the path the VM state recorded, during the load itself, so a
// restored Instance can only receive per-Instance disks when those paths are
// chroot-relative — that is, under the jailer, which this suite cannot run
// unprivileged. The test proves instead that an unjailed resume is refused
// before any file is staged. The low-level resume floor is measured separately
// by TestSmokeSnapshotResumeLoadMeasurement.
//
// The template this test builds is identity-neutral: the guest boots with
// secondbox.template_mode=1, carries no Sandbox ID, generation, secrets, or
// mounted Workspace, and refuses every protocol connection until a resumed
// Instance is bound through the control endpoint.
func TestSmokeSnapshotResumeTemplateLifecycle(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_SNAPSHOT_RESUME") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_SNAPSHOT_RESUME=1 to qualify the snapshot resume template")
	}
	memoryMiB := requiredPositiveEnvInt(t, "SECONDBOX_SNAPSHOT_RESUME_MEMORY_MIB")
	workspaceMiB := requiredPositiveEnvInt(t, "SECONDBOX_SNAPSHOT_RESUME_WORKSPACE_MIB")
	outputPath := requiredEnv(t, "SECONDBOX_SNAPSHOT_RESUME_OUTPUT")
	if !filepath.IsAbs(outputPath) || filepath.Clean(outputPath) != outputPath {
		t.Fatalf("SECONDBOX_SNAPSHOT_RESUME_OUTPUT must be a clean absolute path")
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SECONDBOX_SNAPSHOT_RESUME_OUTPUT must be absent: %v", err)
	}

	report := snapshotTemplateQualificationReport{
		SchemaVersion:         snapshotTemplateQualificationSchemaVersion,
		SourceCommit:          requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_SOURCE_COMMIT"),
		SourceTreeDirty:       requiredEnvBool(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_SOURCE_TREE_DIRTY"),
		GoVersion:             runtime.Version(),
		ComputeBackendVersion: firecrackerQualificationVersion(t),
		HostKernel:            requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_HOST_KERNEL"),
		HostCPU:               requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_HOST_CPU"),
		WorkspaceFilesystem:   requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_FILESYSTEM"),
		IdentityNeutral:       true,
	}

	workDir := shortSmokeDir(t)
	cfg := snapshotLoadQualificationConfig(t, workDir, memoryMiB, workspaceMiB)
	cache, err := NewSnapshotTemplateCache(filepath.Join(cfg.MicroVMRunDir, "templates"))
	if err != nil {
		t.Fatalf("new template cache: %v", err)
	}

	buildStartedAt := time.Now()
	key, manifest := buildSnapshotResumeTemplate(t, cfg, cache, memoryMiB, workspaceMiB, nil)
	report.TemplateBuildMillis = time.Since(buildStartedAt).Milliseconds()
	report.TemplateID = manifest.TemplateID
	report.MemoryFileBytes = manifest.Memory.Bytes
	report.RootfsFileBytes = manifest.Rootfs.Bytes
	report.VMStateFileBytes = manifest.VMState.Bytes

	admissionStartedAt := time.Now()
	template, err := cache.Resolve(key)
	if err != nil {
		t.Fatalf("admit template: %v", err)
	}
	report.AdmissionMillis = time.Since(admissionStartedAt).Milliseconds()

	identityStartedAt := time.Now()
	if err := template.VerifyStableIdentity(); err != nil {
		t.Fatalf("verify admitted template identity: %v", err)
	}
	report.StableIdentityNanos = time.Since(identityStartedAt).Nanoseconds()

	// Every start after admission must cost a stat, not a rehash. Ten thousand
	// checks of a 10.7 GiB template would be impossible otherwise.
	repeatStartedAt := time.Now()
	for range 100 {
		if err := template.VerifyStableIdentity(); err != nil {
			t.Fatalf("repeat stable identity check: %v", err)
		}
	}
	if elapsed := time.Since(repeatStartedAt); elapsed > time.Second {
		t.Fatalf("100 per-start identity checks took %s, which is not a stat-only path", elapsed)
	}

	manager := &Manager{cfg: cfg}
	instanceDir := filepath.Join(cfg.MicroVMRunDir, "refused-resume")
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatalf("create refusal instance dir: %v", err)
	}
	_, resumeErr := manager.prepareSnapshotResumeLaunch(
		"refused-resume",
		instanceDir,
		template,
		managerTestAttachment(t, filepath.Join(workDir, "template-workspace.ext4")),
		"",
		12000,
		&runtimemanager.SandboxRuntimePolicy{VCPUs: 1, MemoryMiB: memoryMiB},
	)
	if resumeErr == nil {
		t.Fatal("an unjailed resume was accepted; a restored Instance would open the template source's disks")
	}
	if !strings.Contains(resumeErr.Error(), "snapshot resume requires the jailer") {
		t.Fatalf("unjailed resume refusal does not name the jailer requirement: %v", resumeErr)
	}
	report.UnjailedResumeError = resumeErr.Error()

	report.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("encode template qualification report: %v", err)
	}
	if err := os.WriteFile(outputPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write template qualification report: %v", err)
	}
	if err := syncDirectory(filepath.Dir(outputPath)); err != nil {
		t.Fatalf("sync template qualification report directory: %v", err)
	}
	t.Logf(
		"template %s built in %d ms, admitted in %d ms, per-start identity check %d ns; memory %d B, rootfs %d B",
		report.TemplateID[:16],
		report.TemplateBuildMillis,
		report.AdmissionMillis,
		report.StableIdentityNanos,
		report.MemoryFileBytes,
		report.RootfsFileBytes,
	)
}

// buildSnapshotResumeTemplate boots the signed bundle, pauses it at a coherent
// point, and seals VM state, memory, and the post-boot rootfs into the cache.
// Capturing the rootfs alongside memory is required: boot mutates the disk, so
// memory alone is not a coherent template.
//
// prepareOpts states the Profile shape and signed identity the template is
// built for. A gate that only resumes its own template passes nil and takes the
// smoke shape; the interim operator publish flow passes the exact shape the
// Runner's assignments will carry, because the compatibility key records it.
func buildSnapshotResumeTemplate(
	t *testing.T,
	cfg *config.Config,
	cache *SnapshotTemplateCache,
	memoryMiB int,
	workspaceMiB int,
	prepareOpts func(runtimemanager.StartOpts) runtimemanager.StartOpts,
) (SnapshotTemplateKey, SnapshotTemplateManifest) {
	t.Helper()
	workDir := filepath.Dir(cfg.MicroVMLogDir)
	manager, err := New(cfg)
	if err != nil {
		t.Fatalf("new template source manager: %v", err)
	}
	// The template build owns the runner DNS proxy for as long as its Manager
	// exists. Every later Manager binds the same bridge address, so this one has
	// to let go once the capture is sealed.
	defer releaseManagerNetworkPolicy(t, manager)
	workspaceStore, err := workspacestore.New(t.Context(), workspacestore.Config{
		Root:                  cfg.RunnerWorkspaceRoot,
		TemplateCapacityBytes: int64(workspaceMiB) << 20,
		FormatterKind:         workspacestore.FormatterMke2fs,
	})
	if err != nil {
		t.Fatalf("new template WorkspaceStore: %v", err)
	}
	if err := manager.SetWorkspaceStore(workspaceStore); err != nil {
		t.Fatalf("bind template WorkspaceStore: %v", err)
	}
	const workspaceID = "snapshot-resume-template"
	if _, err := workspaceStore.Create(t.Context(), workspacestore.CreateWorkspaceRequest{
		Mutation: workspacestore.Mutation{
			OperationID:  "create-" + workspaceID,
			WorkspaceID:  workspaceID,
			FencingToken: []byte("01234567890123456789012345678901"),
		},
		CapacityBytes: int64(workspaceMiB) << 20,
	}); err != nil {
		t.Fatalf("create template Workspace: %v", err)
	}
	attachment, err := workspaceStore.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatalf("open template Workspace: %v", err)
	}
	opts := smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone:            "UTC",
		CompartmentID:       "cmp_snapshot_resume_template",
		WorkspaceAttachment: attachment,
	})
	// The template boots identity-neutral: no Sandbox ID, no generation, no
	// secrets, no mounted Workspace, and a protocol listener that refuses every
	// connection until a resumed Instance is bound.
	opts.TemplateMode = true
	// A template is keyed by the runtime class it will serve, exactly as a start
	// is, so the harness states the one class the runner implements.
	opts.RuntimeClass = runtimemanager.RuntimeClassToolExecutor
	if prepareOpts != nil {
		opts = prepareOpts(opts)
	}
	instanceID, err := manager.createAndStart(t.Context(), "resumetmpl", opts)
	if err != nil {
		_ = attachment.Close()
		t.Fatalf("boot template source: %v\n%s", err, latestSmokeLog(t, workDir))
	}
	inst := manager.lookup(instanceID)
	if inst == nil || inst.cmd == nil || inst.cmd.Process == nil {
		t.Fatalf("template source %q has no Firecracker process", instanceID)
	}
	stageDir, err := cache.StageDirectory()
	if err != nil {
		t.Fatalf("stage template: %v", err)
	}
	client := inst.apiClient(30 * time.Second)
	if err := client.Pause(t.Context()); err != nil {
		t.Fatalf("pause template source: %v", err)
	}
	vmStatePath := filepath.Join(stageDir, snapshotTemplateVMStateName)
	memoryPath := filepath.Join(stageDir, snapshotTemplateMemoryName)
	// A jailed Firecracker resolves every API path inside its own chroot, so the
	// capture is requested at the jail-relative names the resume path expects and
	// moved into the cache staging directory afterwards. That is not incidental:
	// the same chroot resolution is why a template built under the jailer records
	// chroot-relative drive paths, which is the only way a restored Instance can
	// open its own disks.
	requestedVMState, requestedMemory := vmStatePath, memoryPath
	if inst.jailRoot != "" {
		requestedVMState, requestedMemory = snapshotTemplateVMStateName, snapshotTemplateMemoryName
	}
	if err := client.CreateFullSnapshot(t.Context(), requestedVMState, requestedMemory); err != nil {
		t.Fatalf("capture template snapshot: %v", err)
	}
	if inst.jailRoot != "" {
		for captured, destination := range map[string]string{
			filepath.Join(inst.jailRoot, snapshotTemplateVMStateName): vmStatePath,
			filepath.Join(inst.jailRoot, snapshotTemplateMemoryName):  memoryPath,
		} {
			if err := os.Rename(captured, destination); err != nil {
				t.Fatalf("move captured template file out of the jail: %v", err)
			}
			// Template files belong to the runner and are read through a hard
			// link by every Instance's jailer UID. Ownership never transfers to
			// an Instance, so no Instance can modify or unlink an artifact every
			// other Instance depends on.
			if err := os.Chown(destination, os.Getuid(), os.Getgid()); err != nil {
				t.Fatalf("take runner ownership of captured template file: %v", err)
			}
			// Read-only to everyone, stated rather than inherited from whatever
			// umask the build ran under. A jailed Firecracker opens the golden
			// memory file as its own per-Instance UID, and the capture was
			// written by a different one, so a mode that depended on the umask
			// would make a resume fail on a file nothing was wrong with.
			if err := os.Chmod(destination, 0o444); err != nil {
				t.Fatalf("make the captured template file readable by every jailer UID: %v", err)
			}
		}
	}
	// The VM stays paused through the rootfs seal so disk and memory are
	// captured at one point in time.
	rootfsPath := filepath.Join(stageDir, snapshotTemplateRootfsName)
	if err := reflinkOnlyFile(rootfsPath, inst.rootfsPath, 0o600); err != nil {
		t.Fatalf("seal post-boot rootfs: %v", err)
	}
	if err := reflinkOnlyFile(filepath.Join(workDir, "template-workspace.ext4"), inst.workspacePath, 0o600); err != nil {
		t.Fatalf("seal template Workspace shape: %v", err)
	}
	if err := syscall.Kill(-inst.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("terminate template source: %v", err)
	}
	select {
	case <-inst.done:
	case <-time.After(30 * time.Second):
		t.Fatal("template source did not terminate")
	}

	key := snapshotResumeQualificationKey(t, manager, opts, inst.sharedImagePath != "")
	templateID, err := key.TemplateID()
	if err != nil {
		t.Fatalf("template identity: %v", err)
	}
	manifest := SnapshotTemplateManifest{
		SchemaVersion: snapshotTemplateManifestSchemaVersion,
		TemplateID:    templateID,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Key:           key,
		VMState:       describeSnapshotTemplateFile(t, vmStatePath),
		Memory:        describeSnapshotTemplateFile(t, memoryPath),
		Rootfs:        describeSnapshotTemplateFile(t, rootfsPath),
	}
	if _, err := cache.Publish(stageDir, manifest); err != nil {
		t.Fatalf("publish template: %v", err)
	}
	return key, manifest
}

func describeSnapshotTemplateFile(t *testing.T, path string) SnapshotTemplateFile {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat template file %q: %v", path, err)
	}
	digest, err := fileSHA256(path)
	if err != nil {
		t.Fatalf("digest template file %q: %v", path, err)
	}
	return SnapshotTemplateFile{Name: filepath.Base(path), SHA256: digest, Bytes: info.Size()}
}

// snapshotResumeQualificationKey derives the template's identity through the
// exact production function the resume start path uses. That is the point: a
// template a qualification harness publishes into a Runner's cache is resolvable
// by that Runner only if both sides agree on every field of the compatibility
// key, so there is one derivation and no second implementation to drift.
func snapshotResumeQualificationKey(
	t *testing.T,
	manager *Manager,
	opts runtimemanager.StartOpts,
	hasSharedImage bool,
) SnapshotTemplateKey {
	t.Helper()
	// The template's recorded network shape follows the runner's deployment
	// configuration, exactly as a cold boot's does. It belongs in the key because
	// a resumed guest can never acquire an interface the capture did not record.
	key, err := manager.snapshotResumeTemplateKey(opts, microVMNetworkRequired(manager.cfg), hasSharedImage)
	if err != nil {
		t.Fatalf("derive snapshot template key: %v", err)
	}
	return key
}
