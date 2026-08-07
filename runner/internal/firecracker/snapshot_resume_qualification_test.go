package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

const snapshotResumeQualificationSchemaVersion = 1

type snapshotResumeQualificationReport struct {
	SchemaVersion       int                               `json:"schemaVersion"`
	SourceCommit        string                            `json:"sourceCommit"`
	SourceTreeDirty     bool                              `json:"sourceTreeDirty"`
	GoVersion           string                            `json:"goVersion"`
	FirecrackerVersion  string                            `json:"firecrackerVersion"`
	HostKernel          string                            `json:"hostKernel"`
	HostCPU             string                            `json:"hostCpu"`
	WorkspaceFilesystem string                            `json:"workspaceFilesystem"`
	CompletedAt         string                            `json:"completedAt"`
	IdentityNeutral     bool                              `json:"identityNeutralTemplate"`
	TemplateID          string                            `json:"templateId"`
	MemoryFileBytes     int64                             `json:"memoryFileBytes"`
	RootfsFileBytes     int64                             `json:"rootfsFileBytes"`
	TemplateBuildMillis int64                             `json:"templateBuildMilliseconds"`
	AdmissionMillis     int64                             `json:"cacheAdmissionMilliseconds"`
	Concurrency         []snapshotResumeQualificationRung `json:"concurrency"`
	GatePassed          bool                              `json:"gatePassed"`
	GateFailures        []string                          `json:"gateFailures,omitempty"`
}

type snapshotResumeQualificationRung struct {
	Concurrency          int                                 `json:"concurrency"`
	Samples              []snapshotResumeQualificationSample `json:"samples"`
	ResumeP50Millis      int64                               `json:"resumeP50Milliseconds"`
	ResumeP95Millis      int64                               `json:"resumeP95Milliseconds"`
	TotalP50Millis       int64                               `json:"totalP50Milliseconds"`
	TotalP95Millis       int64                               `json:"totalP95Milliseconds"`
	SharedMemoryInode    bool                                `json:"sharedMemoryInode"`
	AggregateReadBytes   uint64                              `json:"aggregateReadBytes"`
	MemoryImageMultiples float64                             `json:"aggregateReadBytesAsMemoryImageMultiples"`
}

type snapshotResumeQualificationSample struct {
	TemplateResolveMillis     int64  `json:"templateResolveMilliseconds"`
	StageMillis               int64  `json:"stageMilliseconds"`
	ProcessAPIReadyMillis     int64  `json:"processApiReadyMilliseconds"`
	SnapshotLoadMillis        int64  `json:"snapshotLoadMilliseconds"`
	FirstControlMillis        int64  `json:"firstControlMilliseconds"`
	PostResumeHardeningMillis int64  `json:"postResumeHardeningMilliseconds"`
	ResumeMillis              int64  `json:"resumeMilliseconds"`
	TotalMillis               int64  `json:"totalMilliseconds"`
	ReadCharacters            uint64 `json:"readCharacters"`
	ReadBytes                 uint64 `json:"readBytes"`
	MajorFaults               uint64 `json:"majorFaults"`
	SharedMemoryInode         bool   `json:"sharedMemoryInode"`
}

// TestSmokeSnapshotResumeTemplateLifecycle qualifies the composed resume path:
// build a template from a real signed boot, seal and publish it, admit it
// through the runner-local cache, then resume Instances from it concurrently
// and prove the first control response and post-resume hardening.
//
// The template this test builds is identity-bearing, because the shipped guest
// takes its Sandbox identity from kernel arguments and cannot yet boot without
// one. It therefore measures the resume mechanism — staging, shared memory
// backing, snapshot load with vsock and network overrides, hardening — and not
// the identity-neutral template the plan's Task 3 and Task 4 will produce. The
// report records that distinction rather than implying it away.
func TestSmokeSnapshotResumeTemplateLifecycle(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_SNAPSHOT_RESUME") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_SNAPSHOT_RESUME=1 to qualify snapshot resume")
	}
	memoryMiB := requiredPositiveEnvInt(t, "SECONDBOX_SNAPSHOT_RESUME_MEMORY_MIB")
	workspaceMiB := requiredPositiveEnvInt(t, "SECONDBOX_SNAPSHOT_RESUME_WORKSPACE_MIB")
	concurrencies := requiredSnapshotConcurrencyRungs(t)
	outputPath := requiredEnv(t, "SECONDBOX_SNAPSHOT_RESUME_OUTPUT")
	if !filepath.IsAbs(outputPath) || filepath.Clean(outputPath) != outputPath {
		t.Fatalf("SECONDBOX_SNAPSHOT_RESUME_OUTPUT must be a clean absolute path")
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SECONDBOX_SNAPSHOT_RESUME_OUTPUT must be absent: %v", err)
	}

	report := snapshotResumeQualificationReport{
		SchemaVersion:       snapshotResumeQualificationSchemaVersion,
		SourceCommit:        requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_SOURCE_COMMIT"),
		SourceTreeDirty:     requiredEnvBool(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_SOURCE_TREE_DIRTY"),
		GoVersion:           runtime.Version(),
		FirecrackerVersion:  firecrackerQualificationVersion(t),
		HostKernel:          requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_HOST_KERNEL"),
		HostCPU:             requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_HOST_CPU"),
		WorkspaceFilesystem: requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_FILESYSTEM"),
		IdentityNeutral:     false,
		GatePassed:          true,
	}

	workDir := shortSmokeDir(t)
	cfg := snapshotLoadQualificationConfig(t, workDir, memoryMiB, workspaceMiB)
	cache, err := NewSnapshotTemplateCache(filepath.Join(cfg.MicroVMRunDir, "templates"))
	if err != nil {
		t.Fatalf("new template cache: %v", err)
	}

	buildStartedAt := time.Now()
	key, manifest := buildSnapshotResumeTemplate(t, cfg, cache, memoryMiB, workspaceMiB, workDir)
	report.TemplateBuildMillis = time.Since(buildStartedAt).Milliseconds()
	report.TemplateID = manifest.TemplateID
	report.MemoryFileBytes = manifest.Memory.Bytes
	report.RootfsFileBytes = manifest.Rootfs.Bytes

	admissionStartedAt := time.Now()
	template, err := cache.Resolve(key)
	if err != nil {
		t.Fatalf("admit template: %v", err)
	}
	report.AdmissionMillis = time.Since(admissionStartedAt).Milliseconds()

	sourceWorkspace := filepath.Join(workDir, "resume-workspace-source.ext4")
	if err := reflinkOnlyFile(sourceWorkspace, filepath.Join(workDir, "template-workspace.ext4"), 0o600); err != nil {
		t.Fatalf("clone resume Workspace source: %v", err)
	}

	for _, concurrency := range concurrencies {
		rung := measureSnapshotResumeRung(t, cfg, template, sourceWorkspace, concurrency)
		report.Concurrency = append(report.Concurrency, rung)
		if !rung.SharedMemoryInode {
			report.GatePassed = false
			report.GateFailures = append(report.GateFailures, fmt.Sprintf(
				"concurrency=%d resumed Instances did not share one golden memory inode",
				concurrency,
			))
		}
		if rung.MemoryImageMultiples >= float64(concurrency)*0.8 {
			report.GatePassed = false
			report.GateFailures = append(report.GateFailures, fmt.Sprintf(
				"concurrency=%d read %.2f memory images, so resumes did not share the page cache",
				concurrency,
				rung.MemoryImageMultiples,
			))
		}
	}
	report.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("encode resume qualification report: %v", err)
	}
	if err := os.WriteFile(outputPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write resume qualification report: %v", err)
	}
	if err := syncDirectory(filepath.Dir(outputPath)); err != nil {
		t.Fatalf("sync resume qualification report directory: %v", err)
	}
	if !report.GatePassed {
		t.Fatalf("snapshot resume qualification failed: %s", strings.Join(report.GateFailures, "; "))
	}
	for _, rung := range report.Concurrency {
		t.Logf(
			"concurrency=%d resume p50/p95 %d/%d ms, total p50/p95 %d/%d ms, aggregate reads %.2f memory images",
			rung.Concurrency,
			rung.ResumeP50Millis,
			rung.ResumeP95Millis,
			rung.TotalP50Millis,
			rung.TotalP95Millis,
			rung.MemoryImageMultiples,
		)
	}
}

// buildSnapshotResumeTemplate boots the signed bundle, pauses it at a coherent
// point, and seals VM state, memory, and the post-boot rootfs into the cache.
// Capturing the rootfs alongside memory is required: boot mutates the disk, so
// memory alone is not a coherent template.
func buildSnapshotResumeTemplate(
	t *testing.T,
	cfg *config.Config,
	cache *SnapshotTemplateCache,
	memoryMiB int,
	workspaceMiB int,
	workDir string,
) (SnapshotTemplateKey, SnapshotTemplateManifest) {
	t.Helper()
	manager, err := New(cfg)
	if err != nil {
		t.Fatalf("new template source manager: %v", err)
	}
	workspaceStore, err := workspacestore.New(t.Context(), workspacestore.Config{
		Root:                  cfg.RunnerWorkspaceRoot,
		TemplateCapacityBytes: int64(workspaceMiB) << 20,
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
	if err := client.CreateFullSnapshot(t.Context(), vmStatePath, memoryPath); err != nil {
		t.Fatalf("capture template snapshot: %v", err)
	}
	// The VM stays paused through the rootfs seal so disk and memory are
	// captured at one point in time.
	rootfsPath := filepath.Join(stageDir, snapshotTemplateRootfsName)
	if err := reflinkOnlyFile(rootfsPath, inst.rootfsPath, 0o600); err != nil {
		t.Fatalf("seal post-boot rootfs: %v", err)
	}
	templateWorkspace := filepath.Join(workDir, "template-workspace.ext4")
	if err := reflinkOnlyFile(templateWorkspace, inst.workspacePath, 0o600); err != nil {
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
	// The VM state records the source's own drive paths. Remove them so a
	// resumed Instance that silently opened the template source's disks instead
	// of its own staged files fails loudly here rather than sharing one rootfs
	// and one Workspace across every Instance.
	if err := os.RemoveAll(inst.dir); err != nil {
		t.Fatalf("remove template source run directory: %v", err)
	}

	key := snapshotResumeQualificationKey(t, cfg, opts, memoryMiB, workspaceMiB)
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

func snapshotResumeQualificationKey(
	t *testing.T,
	cfg *config.Config,
	opts runtimemanager.StartOpts,
	memoryMiB int,
	workspaceMiB int,
) SnapshotTemplateKey {
	t.Helper()
	artifactDir := filepath.Dir(cfg.MicroVMKernelPath)
	kernelDigest, err := fileSHA256(cfg.MicroVMKernelPath)
	if err != nil {
		t.Fatalf("digest kernel: %v", err)
	}
	rootfsDigest, err := fileSHA256(cfg.MicroVMRootfsPath)
	if err != nil {
		t.Fatalf("digest rootfs: %v", err)
	}
	sharedDigest, err := fileSHA256(cfg.MicroVMSharedImagePath)
	if err != nil {
		t.Fatalf("digest shared image: %v", err)
	}
	signedManifestDigest, err := fileSHA256(filepath.Join(artifactDir, "manifest.json"))
	if err != nil {
		t.Fatalf("digest signed manifest: %v", err)
	}
	cpuFingerprint, err := hostCPUCompatibilityFingerprint()
	if err != nil {
		t.Fatalf("host CPU fingerprint: %v", err)
	}
	features := []string{
		"streaming_exec",
		"pty_resize",
		"descriptor_pinned_filesystem",
		"activity_events",
		"port_proxy",
	}
	return SnapshotTemplateKey{
		ArtifactVersion:         "qualification",
		Architecture:            runtime.GOARCH,
		SigningKeyFingerprint:   cfg.MicroVMPublicKeySHA256,
		SignedManifestDigest:    "sha256:" + signedManifestDigest,
		KernelSHA256:            kernelDigest,
		KernelArgs:              effectiveKernelArgs(cfg, ""),
		SourceRootfsSHA256:      rootfsDigest,
		SharedImageSHA256:       sharedDigest,
		RuntimeBundleDigest:     opts.ImageManifestDigest,
		ToolchainBundleDigest:   opts.ToolchainManifestDigest,
		GuestBuildID:            opts.GuestBuildID,
		GuestProtocolGeneration: currentGuestProtocolGeneration,
		GuestFeatures:           features,
		FirecrackerVersion:      expectedFirecrackerVersionString(),
		HostCPUFingerprint:      cpuFingerprint,
		CPUTemplate:             cfg.MicroVMCPUTemplate,
		VCPUCount:               cfg.MicroVMVCPUs,
		MemorySizeMiB:           memoryMiB,
		WorkspaceSizeMiB:        workspaceMiB,
		ProcessLimit:            opts.SandboxPolicy.ProcessLimit,
		RuntimeClass:            string(runtimemanager.RuntimeClassToolExecutor),
		NetworkInterfaceID:      snapshotResumeNetworkInterfaceID,
		GuestControlVsockPort:   cfg.MicroVMGuestControlVsockPort,
		GuestProtocolVsockPort:  cfg.MicroVMGuestProtocolVsockPort,
		GuestCID:                3,
	}
}

func measureSnapshotResumeRung(
	t *testing.T,
	cfg *config.Config,
	template *AdmittedSnapshotTemplate,
	sourceWorkspace string,
	concurrency int,
) snapshotResumeQualificationRung {
	t.Helper()
	rung := snapshotResumeQualificationRung{Concurrency: concurrency, SharedMemoryInode: true}
	rungRoot, err := os.MkdirTemp(cfg.MicroVMRunDir, fmt.Sprintf("r%d-", concurrency))
	if err != nil {
		t.Fatalf("create resume rung root: %v", err)
	}
	defer os.RemoveAll(rungRoot)

	samples := make([]snapshotResumeQualificationSample, concurrency)
	failures := make([]error, concurrency)
	var wait sync.WaitGroup
	wait.Add(concurrency)
	for index := range concurrency {
		go func() {
			defer wait.Done()
			sample, err := runSnapshotResumeSample(
				t.Context(),
				cfg,
				template,
				sourceWorkspace,
				filepath.Join(rungRoot, fmt.Sprintf("%02d", index)),
			)
			samples[index] = sample
			failures[index] = err
		}()
	}
	wait.Wait()
	for index, err := range failures {
		if err != nil {
			t.Fatalf("resume sample %d at concurrency %d: %v", index, concurrency, err)
		}
	}
	rung.Samples = samples

	resume := make([]int64, 0, concurrency)
	total := make([]int64, 0, concurrency)
	for _, sample := range samples {
		resume = append(resume, sample.ResumeMillis)
		total = append(total, sample.TotalMillis)
		rung.AggregateReadBytes += sample.ReadBytes
		rung.SharedMemoryInode = rung.SharedMemoryInode && sample.SharedMemoryInode
	}
	rung.ResumeP50Millis = durationPercentile(resume, 50)
	rung.ResumeP95Millis = durationPercentile(resume, 95)
	rung.TotalP50Millis = durationPercentile(total, 50)
	rung.TotalP95Millis = durationPercentile(total, 95)
	if template.MemoryBytes > 0 {
		rung.MemoryImageMultiples = float64(rung.AggregateReadBytes) / float64(template.MemoryBytes)
	}
	return rung
}

func runSnapshotResumeSample(
	ctx context.Context,
	cfg *config.Config,
	template *AdmittedSnapshotTemplate,
	sourceWorkspace string,
	instanceDir string,
) (sample snapshotResumeQualificationSample, returnErr error) {
	startedAt := time.Now()
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		return sample, err
	}
	resolveStartedAt := time.Now()
	if err := template.VerifyStableIdentity(); err != nil {
		return sample, err
	}
	sample.TemplateResolveMillis = time.Since(resolveStartedAt).Milliseconds()

	workspacePath := filepath.Join(instanceDir, "attached-workspace.ext4")
	if err := reflinkOnlyFile(workspacePath, sourceWorkspace, 0o600); err != nil {
		return sample, err
	}
	manager := &Manager{cfg: cfg}
	stageStartedAt := time.Now()
	launch, err := manager.prepareSnapshotResumeLaunch(
		"resume-"+filepath.Base(instanceDir),
		instanceDir,
		template,
		workspacePath,
		cfg.MicroVMSharedImagePath,
		0,
		nil,
	)
	if err != nil {
		return sample, err
	}
	sample.StageMillis = time.Since(stageStartedAt).Milliseconds()
	// Every resume must open the one golden memory file, not a clone of it.
	// This is the property that makes the first resume's resident set every
	// later resume's cache hit, so it is proved rather than assumed.
	sample.SharedMemoryInode, err = sharesInode(launch.memoryResolvedPath, template.MemoryPath)
	if err != nil {
		return sample, err
	}

	logPath := instanceDir + ".log"
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return sample, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, logFile.Close())
	}()
	cmd := exec.Command(launch.executable, launch.args...)
	cmd.Dir = instanceDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	processStartedAt := time.Now()
	if err := cmd.Start(); err != nil {
		return sample, err
	}
	defer func() {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			returnErr = errors.Join(returnErr, err)
		}
		if err := cmd.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != -1 {
				returnErr = errors.Join(returnErr, err)
			}
		}
	}()
	if err := waitForUnixSocket(ctx, launch.socketPath, 10*time.Second); err != nil {
		return sample, err
	}
	sample.ProcessAPIReadyMillis = time.Since(processStartedAt).Milliseconds()
	before, err := readSnapshotProcessCounters(cmd.Process.Pid)
	if err != nil {
		return sample, err
	}

	loadStartedAt := time.Now()
	if err := (FirecrackerAPIClient{SocketPath: launch.socketPath, Timeout: 30 * time.Second}).LoadSnapshotWithOptions(
		ctx,
		snapshotResumeLoadRequest(launch, ""),
	); err != nil {
		return sample, err
	}
	sample.SnapshotLoadMillis = time.Since(loadStartedAt).Milliseconds()

	controlStartedAt := time.Now()
	controlClient := ControlClient{
		UDSPath: launch.vsockUDS,
		Port:    cfg.MicroVMGuestControlVsockPort,
		Timeout: 250 * time.Millisecond,
	}
	if err := waitForSnapshotControl(ctx, controlClient, 30*time.Second); err != nil {
		return sample, err
	}
	sample.FirstControlMillis = time.Since(controlStartedAt).Milliseconds()
	hardeningStartedAt := time.Now()
	if err := controlClient.HardenPostRestore(ctx, time.Now().UTC()); err != nil {
		return sample, err
	}
	sample.PostResumeHardeningMillis = time.Since(hardeningStartedAt).Milliseconds()
	sample.ResumeMillis = time.Since(processStartedAt).Milliseconds()
	sample.TotalMillis = time.Since(startedAt).Milliseconds()

	after, err := readSnapshotProcessCounters(cmd.Process.Pid)
	if err != nil {
		return sample, err
	}
	sample.ReadCharacters = counterDelta(after.readCharacters, before.readCharacters)
	sample.ReadBytes = counterDelta(after.readBytes, before.readBytes)
	sample.MajorFaults = counterDelta(after.majorFaults, before.majorFaults)
	return sample, nil
}

func requiredSnapshotConcurrencyRungs(t *testing.T) []int {
	t.Helper()
	raw := requiredEnv(t, "SECONDBOX_SNAPSHOT_RESUME_CONCURRENCY")
	var rungs []int
	for _, field := range strings.Split(raw, ",") {
		value := strings.TrimSpace(field)
		parsed := 0
		if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed < 1 {
			t.Fatalf("SECONDBOX_SNAPSHOT_RESUME_CONCURRENCY contains invalid value %q", field)
		}
		rungs = append(rungs, parsed)
	}
	if len(rungs) == 0 {
		t.Fatal("SECONDBOX_SNAPSHOT_RESUME_CONCURRENCY must contain a rung")
	}
	return rungs
}
