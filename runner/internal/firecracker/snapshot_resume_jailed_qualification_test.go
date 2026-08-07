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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

const jailedSnapshotResumeQualificationSchemaVersion = 1

type jailedSnapshotResumeReport struct {
	SchemaVersion       int                        `json:"schemaVersion"`
	SourceCommit        string                     `json:"sourceCommit"`
	SourceTreeDirty     bool                       `json:"sourceTreeDirty"`
	GoVersion           string                     `json:"goVersion"`
	FirecrackerVersion  string                     `json:"firecrackerVersion"`
	HostKernel          string                     `json:"hostKernel"`
	HostCPU             string                     `json:"hostCpu"`
	WorkspaceFilesystem string                     `json:"workspaceFilesystem"`
	CompletedAt         string                     `json:"completedAt"`
	Jailed              bool                       `json:"jailedResume"`
	IdentityNeutral     bool                       `json:"identityNeutralTemplate"`
	MemoryMiB           int                        `json:"memoryMiB"`
	WorkspaceMiB        int                        `json:"workspaceMiB"`
	TemplateID          string                     `json:"templateId"`
	MemoryFileBytes     int64                      `json:"memoryFileBytes"`
	RootfsFileBytes     int64                      `json:"rootfsFileBytes"`
	VMStateFileBytes    int64                      `json:"vmStateFileBytes"`
	TemplateBuildMillis int64                      `json:"templateBuildMilliseconds"`
	AdmissionMillis     int64                      `json:"cacheAdmissionMilliseconds"`
	Rungs               []jailedSnapshotResumeRung `json:"concurrencyRungs"`
	GatePassed          bool                       `json:"gatePassed"`
	GateFailures        []string                   `json:"gateFailures,omitempty"`
}

type jailedSnapshotResumeRung struct {
	Concurrency               int                                `json:"concurrency"`
	WallClockMillis           int64                              `json:"wallClockMilliseconds"`
	Instances                 []jailedSnapshotResumeSample       `json:"instances"`
	Stages                    []jailedSnapshotResumeStageSummary `json:"stages"`
	GoldenMemoryInode         string                             `json:"goldenMemoryInode"`
	GoldenMemoryInodeShared   bool                               `json:"goldenMemoryInodeShared"`
	GoldenMemoryLinkCount     uint64                             `json:"goldenMemoryLinkCount"`
	RootfsChildrenAreDistinct bool                               `json:"rootfsChildrenAreDistinct"`
	WorkspacesAreDistinct     bool                               `json:"workspacesAreDistinct"`
	FirecrackerReadBytesTotal uint64                             `json:"firecrackerReadBytesTotal"`
	FirecrackerReadCharsTotal uint64                             `json:"firecrackerReadCharactersTotal"`
	FullMemoryCopyObserved    bool                               `json:"fullMemoryCopyObserved"`
}

type jailedSnapshotResumeSample struct {
	InstanceID             string `json:"instanceId"`
	JailerUID              int    `json:"jailerUid"`
	TemplateIdentityMicros int64  `json:"templateIdentityMicroseconds"`
	StageMicros            int64  `json:"stageMicroseconds"`
	ProcessStartMicros     int64  `json:"processStartMicroseconds"`
	SnapshotLoadMicros     int64  `json:"snapshotLoadMicroseconds"`
	FirstControlMicros     int64  `json:"firstControlMicroseconds"`
	HardenMicros           int64  `json:"postResumeHardeningMicroseconds"`
	BindMicros             int64  `json:"assignmentBindMicroseconds"`
	GuestHandshakeMicros   int64  `json:"guestHandshakeMicroseconds"`
	ResumeTotalMicros      int64  `json:"resumeTotalMicroseconds"`
}

type jailedSnapshotResumeStageSummary struct {
	Stage     string `json:"stage"`
	P50Micros int64  `json:"p50Microseconds"`
	P95Micros int64  `json:"p95Microseconds"`
	MaxMicros int64  `json:"maxMicroseconds"`
}

// TestSmokeJailedSnapshotResume is the gate the whole snapshot-resume plan waits
// on. It builds an identity-neutral template under the jailer, admits it through
// the runner-local cache, and then resumes real Instances from it at rising
// concurrency, timing every stage.
//
// It must run jailed and it must run as root, and neither is a preference.
// Firecracker opens every block device at the path the VM state recorded, during
// the load itself, so a restored Instance receives its own disks only when those
// paths are chroot-relative names it resolves inside its own jail. An unjailed
// measurement describes Instances sharing the template source's disks and is not
// evidence of anything. prepareSnapshotResumeLaunch refuses one before staging a
// file; this gate proves the jailed path it demands actually works.
//
// The assertion that matters as much as the latency is inode sharing: every
// Instance's jail must hold a hard link to the one golden memory file. One inode
// is one page cache, which is why the first resume's resident set is every later
// resume's cache hit, and why 16 concurrent resumes do not read 16 memory images.
func TestSmokeJailedSnapshotResume(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_SNAPSHOT_RESUME_JAILED") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_SNAPSHOT_RESUME_JAILED=1 to qualify jailed snapshot resume")
	}
	if os.Geteuid() != 0 {
		t.Fatal("jailed snapshot-resume qualification must run as root: the jailer chroots, creates device nodes, chowns, and drops UID")
	}
	memoryMiB := requiredPositiveEnvInt(t, "SECONDBOX_SNAPSHOT_RESUME_JAILED_MEMORY_MIB")
	workspaceMiB := requiredPositiveEnvInt(t, "SECONDBOX_SNAPSHOT_RESUME_JAILED_WORKSPACE_MIB")
	rungs := requiredConcurrencyRungs(t, "SECONDBOX_SNAPSHOT_RESUME_JAILED_CONCURRENCY")
	outputPath := requiredEnv(t, "SECONDBOX_SNAPSHOT_RESUME_JAILED_OUTPUT")
	if !filepath.IsAbs(outputPath) || filepath.Clean(outputPath) != outputPath {
		t.Fatal("SECONDBOX_SNAPSHOT_RESUME_JAILED_OUTPUT must be a clean absolute path")
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SECONDBOX_SNAPSHOT_RESUME_JAILED_OUTPUT must be absent: %v", err)
	}

	report := jailedSnapshotResumeReport{
		SchemaVersion:       jailedSnapshotResumeQualificationSchemaVersion,
		SourceCommit:        requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_SOURCE_COMMIT"),
		SourceTreeDirty:     requiredEnvBool(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_SOURCE_TREE_DIRTY"),
		GoVersion:           runtime.Version(),
		FirecrackerVersion:  firecrackerQualificationVersion(t),
		HostKernel:          requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_HOST_KERNEL"),
		HostCPU:             requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_HOST_CPU"),
		WorkspaceFilesystem: requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_FILESYSTEM"),
		Jailed:              true,
		IdentityNeutral:     true,
		MemoryMiB:           memoryMiB,
		WorkspaceMiB:        workspaceMiB,
		GatePassed:          true,
	}

	workDir := shortSmokeDir(t)
	// One jailer UID per resident Instance, plus one for the template source that
	// boots before any of them.
	cfg := jailedResumeQualificationConfig(t, workDir, memoryMiB, workspaceMiB, rungs[len(rungs)-1]+1)
	cacheRoot := filepath.Join(cfg.MicroVMRunDir, "templates")
	requireSingleFilesystem(t, map[string]string{
		"template cache root":    filepath.Dir(cacheRoot),
		"jailer chroot base dir": cfg.MicroVMJailerChrootBaseDir,
		"qualification work dir": workDir,
	})
	cache, err := NewSnapshotTemplateCache(cacheRoot)
	if err != nil {
		t.Fatalf("new template cache: %v", err)
	}

	buildStartedAt := time.Now()
	key, manifest := buildSnapshotResumeTemplate(t, cfg, cache, memoryMiB, workspaceMiB)
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

	store, err := workspacestore.New(t.Context(), workspacestore.Config{
		Root:                  filepath.Join(workDir, "resume-workspaces"),
		TemplateCapacityBytes: int64(workspaceMiB) << 20,
	})
	if err != nil {
		t.Fatalf("new resume WorkspaceStore: %v", err)
	}

	for _, concurrency := range rungs {
		rung := measureJailedResumeRung(t, cfg, template, store, concurrency, memoryMiB, workspaceMiB)
		report.Rungs = append(report.Rungs, rung)
		if !rung.GoldenMemoryInodeShared {
			report.GatePassed = false
			report.GateFailures = append(report.GateFailures, fmt.Sprintf(
				"concurrency=%d resumed Instances did not share one golden memory inode",
				concurrency,
			))
		}
		if rung.GoldenMemoryLinkCount != uint64(concurrency)+1 {
			report.GatePassed = false
			report.GateFailures = append(report.GateFailures, fmt.Sprintf(
				"concurrency=%d golden memory link count is %d, expected %d",
				concurrency,
				rung.GoldenMemoryLinkCount,
				concurrency+1,
			))
		}
		if !rung.RootfsChildrenAreDistinct {
			report.GatePassed = false
			report.GateFailures = append(report.GateFailures, fmt.Sprintf(
				"concurrency=%d resumed Instances shared one rootfs inode; the restored guest writes to it",
				concurrency,
			))
		}
		if !rung.WorkspacesAreDistinct {
			report.GatePassed = false
			report.GateFailures = append(report.GateFailures, fmt.Sprintf(
				"concurrency=%d resumed Instances shared one Workspace image",
				concurrency,
			))
		}
		if rung.FullMemoryCopyObserved {
			report.GatePassed = false
			report.GateFailures = append(report.GateFailures, fmt.Sprintf(
				"concurrency=%d performed memory-image-sized process I/O per Instance",
				concurrency,
			))
		}
	}

	report.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("encode jailed resume qualification report: %v", err)
	}
	if err := os.WriteFile(outputPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write jailed resume qualification report: %v", err)
	}
	if err := syncDirectory(filepath.Dir(outputPath)); err != nil {
		t.Fatalf("sync jailed resume qualification report directory: %v", err)
	}
	if !report.GatePassed {
		t.Fatalf("jailed snapshot-resume gate failed: %s", strings.Join(report.GateFailures, "; "))
	}
}

// measureJailedResumeRung resumes concurrency Instances from one admitted
// template at the same time, times every stage of each, and proves the sharing
// and isolation properties while they are all still running.
func measureJailedResumeRung(
	t *testing.T,
	cfg *config.Config,
	template *AdmittedSnapshotTemplate,
	store *workspacestore.Store,
	concurrency int,
	memoryMiB int,
	workspaceMiB int,
) jailedSnapshotResumeRung {
	t.Helper()
	rung := jailedSnapshotResumeRung{Concurrency: concurrency}
	// A fresh Manager per rung starts with an empty jailer-UID map, so each rung
	// allocates from the same range the previous rung released.
	manager, err := New(cfg)
	if err != nil {
		t.Fatalf("new concurrency-%d resume manager: %v", concurrency, err)
	}
	opts := smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone:      "UTC",
		CompartmentID: "cmp_jailed_resume",
	})

	instances := make([]*jailedResumeInstance, concurrency)
	for index := range instances {
		instances[index] = newJailedResumeInstance(t, manager, store, cfg, index, concurrency, workspaceMiB)
	}
	// Every rung releases its Instances before the next one starts. A rung that
	// outlived itself would leave its jail links on the golden memory file, so the
	// next rung's link count would not be its own concurrency, and its resumed
	// Instances would share jailer UIDs with still-running ones.
	defer func() {
		for _, inst := range instances {
			if err := inst.shutdown(manager); err != nil {
				t.Errorf("release concurrency-%d resume instance %s: %v", concurrency, inst.id, err)
			}
		}
	}()

	startedAt := time.Now()
	var waitGroup sync.WaitGroup
	for _, inst := range instances {
		waitGroup.Add(1)
		go func(inst *jailedResumeInstance) {
			defer waitGroup.Done()
			inst.err = inst.resume(t.Context(), manager, cfg, template, opts, memoryMiB, workspaceMiB)
		}(inst)
	}
	waitGroup.Wait()
	rung.WallClockMillis = time.Since(startedAt).Milliseconds()
	for _, inst := range instances {
		if inst.err != nil {
			t.Fatalf(
				"resume concurrency-%d instance %s: %v\n%s",
				concurrency,
				inst.id,
				inst.err,
				smokeLogPath(t, inst.logPath),
			)
		}
		rung.Instances = append(rung.Instances, inst.sample)
	}

	goldenIdentity, err := trustedMicroVMArtifactIdentityFor(template.MemoryPath)
	if err != nil {
		t.Fatalf("stat golden memory file: %v", err)
	}
	rung.GoldenMemoryInode = fmt.Sprintf("%d:%d", goldenIdentity.dev, goldenIdentity.ino)
	rung.GoldenMemoryLinkCount = linkCountOf(t, template.MemoryPath)
	rung.GoldenMemoryInodeShared = true
	rung.RootfsChildrenAreDistinct = true
	rung.WorkspacesAreDistinct = true
	rootfsInodes := map[string]string{}
	workspaceInodes := map[string]string{}
	for _, inst := range instances {
		shared, err := sharesInode(filepath.Join(inst.launch.jailRoot, snapshotResumeMemoryName), template.MemoryPath)
		if err != nil {
			t.Fatalf("compare instance %s golden memory inode: %v", inst.id, err)
		}
		if !shared {
			rung.GoldenMemoryInodeShared = false
		}
		rootfsInode := inodeKey(t, filepath.Join(inst.launch.jailRoot, snapshotTemplateRootfsName))
		if owner, exists := rootfsInodes[rootfsInode]; exists {
			t.Logf("instances %s and %s share rootfs inode %s", owner, inst.id, rootfsInode)
			rung.RootfsChildrenAreDistinct = false
		}
		rootfsInodes[rootfsInode] = inst.id
		workspaceInode := inodeKey(t, filepath.Join(inst.launch.jailRoot, workspaceName))
		if owner, exists := workspaceInodes[workspaceInode]; exists {
			t.Logf("instances %s and %s share Workspace inode %s", owner, inst.id, workspaceInode)
			rung.WorkspacesAreDistinct = false
		}
		workspaceInodes[workspaceInode] = inst.id

		pids, err := firecrackerPIDs(inst.id)
		if err != nil {
			t.Fatalf("enumerate instance %s Firecracker processes: %v", inst.id, err)
		}
		if len(pids) == 0 {
			t.Fatalf("instance %s has no running Firecracker process after resume", inst.id)
		}
		for _, pid := range pids {
			counters, err := readSnapshotProcessCounters(pid)
			if err != nil {
				t.Fatalf("read instance %s process counters: %v", inst.id, err)
			}
			rung.FirecrackerReadBytesTotal += counters.readBytes
			rung.FirecrackerReadCharsTotal += counters.readCharacters
		}
	}
	// One memory image per Instance would be the failure the plan names: page
	// cache is per inode, so Instances that did not share the golden inode would
	// each fault in their own copy.
	fullCopyThreshold := uint64(template.MemoryBytes) * uint64(concurrency) * 8 / 10
	rung.FullMemoryCopyObserved = rung.FirecrackerReadBytesTotal >= fullCopyThreshold ||
		rung.FirecrackerReadCharsTotal >= fullCopyThreshold

	rung.Stages = summarizeJailedResumeStages(rung.Instances)
	t.Logf(
		"concurrency %d: resume total p50 %d us p95 %d us; load p50 %d us; harden p50 %d us; bind p50 %d us; handshake p50 %d us; golden inode %s shared=%t links=%d",
		concurrency,
		stagePercentile(rung.Stages, "resume_total", 50),
		stagePercentile(rung.Stages, "resume_total", 95),
		stagePercentile(rung.Stages, "snapshot_load", 50),
		stagePercentile(rung.Stages, "post_resume_hardening", 50),
		stagePercentile(rung.Stages, "assignment_bind", 50),
		stagePercentile(rung.Stages, "guest_handshake", 50),
		rung.GoldenMemoryInode,
		rung.GoldenMemoryInodeShared,
		rung.GoldenMemoryLinkCount,
	)
	return rung
}

// jailedResumeInstance is one resumed Instance and every resource it holds. The
// gate composes already-landed primitives around it and performs no Manager
// surgery.
type jailedResumeInstance struct {
	id          string
	sandboxID   string
	compartment string
	jailerUID   int
	runDir      string
	logPath     string
	logFile     *os.File
	workspaceID string
	attachment  workspacestore.ComputeAttachment
	launch      snapshotResumeLaunch
	cmd         *exec.Cmd
	session     *GuestProtocolSession
	sample      jailedSnapshotResumeSample
	err         error
}

func newJailedResumeInstance(
	t *testing.T,
	manager *Manager,
	store *workspacestore.Store,
	cfg *config.Config,
	index int,
	concurrency int,
	workspaceMiB int,
) *jailedResumeInstance {
	t.Helper()
	sandboxID := fmt.Sprintf("jr%dx%02d", concurrency, index)
	compartment := fmt.Sprintf("cmp_jr_%d_%02d", concurrency, index)
	id, err := newInstanceID(sandboxID, compartment)
	if err != nil {
		t.Fatalf("allocate resume instance id: %v", err)
	}
	jailerUID, err := manager.allocateJailerUID(id)
	if err != nil {
		t.Fatalf("allocate resume jailer UID: %v", err)
	}
	workspaceID := fmt.Sprintf("jailed-resume-%d-%02d", concurrency, index)
	if _, err := store.Create(t.Context(), workspacestore.CreateWorkspaceRequest{
		Mutation: workspacestore.Mutation{
			OperationID:  "create-" + workspaceID,
			WorkspaceID:  workspaceID,
			FencingToken: []byte("01234567890123456789012345678901"),
		},
		CapacityBytes: int64(workspaceMiB) << 20,
	}); err != nil {
		t.Fatalf("create resume Workspace %q: %v", workspaceID, err)
	}
	// The resume path forks below WorkspaceStore.Open exactly where cold start
	// does: the exclusive writer lock and the generation fence are held before any
	// resume resource is created.
	attachment, err := store.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatalf("open resume Workspace %q: %v", workspaceID, err)
	}
	runDir := filepath.Join(cfg.MicroVMRunDir, id)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("create resume run dir: %v", err)
	}
	logPath := filepath.Join(cfg.MicroVMLogDir, id+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open resume microVM log: %v", err)
	}
	return &jailedResumeInstance{
		id:          id,
		sandboxID:   sandboxID,
		compartment: compartment,
		jailerUID:   jailerUID,
		runDir:      runDir,
		logPath:     logPath,
		logFile:     logFile,
		workspaceID: workspaceID,
		attachment:  attachment,
	}
}

func (inst *jailedResumeInstance) resume(
	ctx context.Context,
	manager *Manager,
	cfg *config.Config,
	template *AdmittedSnapshotTemplate,
	opts runtimemanager.StartOpts,
	memoryMiB int,
	workspaceMiB int,
) error {
	totalStartedAt := time.Now()
	inst.sample.InstanceID = inst.id
	inst.sample.JailerUID = inst.jailerUID

	// Template lookup on the start path is a stable-identity check, never a
	// rehash. It is measured on its own because the budget allows 0-3 ms for it.
	identityStartedAt := time.Now()
	if err := template.VerifyStableIdentity(); err != nil {
		return fmt.Errorf("verify template identity: %w", err)
	}
	inst.sample.TemplateIdentityMicros = time.Since(identityStartedAt).Microseconds()

	workspaceImage := inst.attachment.Image()
	if workspaceImage == nil || inst.attachment.Generation() != 1 {
		return fmt.Errorf("resolved Workspace attachment generation is stale")
	}
	policy := &runtimemanager.SandboxRuntimePolicy{
		VCPUs:             cfg.MicroVMVCPUs,
		CPUMillis:         cfg.MicroVMVCPUs * 1000,
		MemoryMiB:         memoryMiB,
		WorkspaceSizeMiB:  workspaceMiB,
		ProcessLimit:      opts.SandboxPolicy.ProcessLimit,
		WorkspaceWritable: true,
	}
	stageStartedAt := time.Now()
	launch, err := manager.prepareSnapshotResumeLaunch(
		inst.id,
		inst.runDir,
		template,
		workspaceImage.Name(),
		"",
		inst.jailerUID,
		policy,
	)
	if err != nil {
		return fmt.Errorf("prepare jailed resume launch: %w", err)
	}
	inst.launch = launch
	inst.sample.StageMicros = time.Since(stageStartedAt).Microseconds()

	processStartedAt := time.Now()
	cmd := exec.Command(launch.executable, launch.args...)
	cmd.Dir = inst.runDir
	cmd.Stdout = inst.logFile
	cmd.Stderr = inst.logFile
	cmd.Env = append(os.Environ(), launch.environment...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start jailer: %w", err)
	}
	inst.cmd = cmd
	if err := waitForUnixSocket(ctx, launch.socketPath, 30*time.Second); err != nil {
		return fmt.Errorf("wait for jailed Firecracker API socket: %w", err)
	}
	inst.sample.ProcessStartMicros = time.Since(processStartedAt).Microseconds()

	loadStartedAt := time.Now()
	if err := resumeSnapshotTemplate(ctx, launch, "", 30*time.Second, 120*time.Second); err != nil {
		return err
	}
	inst.sample.SnapshotLoadMicros = time.Since(loadStartedAt).Microseconds()

	pollClient := ControlClient{
		UDSPath: launch.vsockUDS,
		Port:    cfg.MicroVMGuestControlVsockPort,
		Timeout: 500 * time.Millisecond,
	}
	controlStartedAt := time.Now()
	if err := waitForSnapshotControl(ctx, pollClient, 30*time.Second); err != nil {
		return err
	}
	inst.sample.FirstControlMicros = time.Since(controlStartedAt).Microseconds()

	controlClient := ControlClient{
		UDSPath: launch.vsockUDS,
		Port:    cfg.MicroVMGuestControlVsockPort,
		Timeout: 30 * time.Second,
	}
	hardenStartedAt := time.Now()
	if err := controlClient.HardenPostRestore(ctx, time.Now().UTC()); err != nil {
		return fmt.Errorf("harden resumed guest: %w", err)
	}
	inst.sample.HardenMicros = time.Since(hardenStartedAt).Microseconds()

	bindStartedAt := time.Now()
	if err := controlClient.BindAssignment(ctx, AssignmentBindRequest{
		InstanceID:              inst.compartment,
		SandboxID:               inst.sandboxID,
		SandboxGeneration:       1,
		GuestBuildID:            opts.GuestBuildID,
		ImageManifestDigest:     opts.ImageManifestDigest,
		ToolchainManifestDigest: opts.ToolchainManifestDigest,
		HeartbeatIntervalMs:     uint64(cfg.MicroVMGuestHeartbeatInterval / time.Millisecond),
		WorkspaceWritable:       true,
	}); err != nil {
		return fmt.Errorf("bind resumed guest assignment: %w", err)
	}
	inst.sample.BindMicros = time.Since(bindStartedAt).Microseconds()

	handshakeStartedAt := time.Now()
	session, err := NegotiateGuestProtocol(ctx, GuestProtocolNegotiation{
		UDSPath:                         launch.vsockUDS,
		Port:                            cfg.MicroVMGuestProtocolVsockPort,
		InstanceID:                      inst.compartment,
		SandboxID:                       inst.sandboxID,
		SandboxGeneration:               1,
		ExpectedGuestBuildID:            opts.GuestBuildID,
		ExpectedImageManifestDigest:     opts.ImageManifestDigest,
		ExpectedToolchainManifestDigest: opts.ToolchainManifestDigest,
		RequestedFeatures: []guestv1.GuestFeature{
			guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
			guestv1.GuestFeature_GUEST_FEATURE_PTY_RESIZE,
			guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
			guestv1.GuestFeature_GUEST_FEATURE_ACTIVITY_EVENTS,
			guestv1.GuestFeature_GUEST_FEATURE_PORT_PROXY,
		},
		MandatoryFeatures: []guestv1.GuestFeature{
			guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC,
			guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM,
		},
	})
	if err != nil {
		return fmt.Errorf("negotiate resumed guest protocol: %w", err)
	}
	inst.session = session
	inst.sample.GuestHandshakeMicros = time.Since(handshakeStartedAt).Microseconds()
	inst.sample.ResumeTotalMicros = time.Since(totalStartedAt).Microseconds()
	return nil
}

// shutdown releases everything the Instance holds, in the order a real teardown
// must: guest stream, VMM process, staged jail files, then the Workspace writer
// lock. Nothing may release the writer lock while a resumed VM could still write.
func (inst *jailedResumeInstance) shutdown(manager *Manager) error {
	var joined error
	if inst.session != nil {
		joined = errors.Join(joined, inst.session.Close())
		inst.session = nil
	}
	if inst.cmd != nil && inst.cmd.Process != nil {
		if err := syscall.Kill(-inst.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			joined = errors.Join(joined, err)
		}
		if err := inst.cmd.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				joined = errors.Join(joined, err)
			}
		}
		inst.cmd = nil
	}
	if inst.logFile != nil {
		joined = errors.Join(joined, inst.logFile.Close())
		inst.logFile = nil
	}
	if inst.launch.jailRoot != "" {
		joined = errors.Join(joined, os.RemoveAll(inst.launch.jailRoot))
		inst.launch.jailRoot = ""
	}
	if inst.runDir != "" {
		joined = errors.Join(joined, os.RemoveAll(inst.runDir))
		inst.runDir = ""
	}
	if inst.attachment != nil {
		joined = errors.Join(joined, inst.attachment.Close())
		inst.attachment = nil
	}
	if inst.jailerUID != 0 {
		joined = errors.Join(joined, manager.releaseJailerUID(inst.id, inst.jailerUID))
		inst.jailerUID = 0
	}
	return joined
}

func jailedResumeQualificationConfig(
	t *testing.T,
	workDir string,
	memoryMiB int,
	workspaceMiB int,
	jailerUIDCount int,
) *config.Config {
	t.Helper()
	cfg := snapshotLoadQualificationConfig(t, workDir, memoryMiB, workspaceMiB)
	// Snapshot resume is jailed by construction, so this gate cannot be run with
	// the test-only unjailed escape hatch the other qualifications use.
	cfg.MicroVMAllowUnjailed = false
	cfg.JailerPath = requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH")
	cfg.MicroVMJailerChrootBaseDir = requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT")
	cfg.MicroVMJailerUIDStart = requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_START")
	cfg.MicroVMJailerUIDCount = jailerUIDCount
	cfg.MicroVMJailerGID = requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID")
	cfg.MicroVMJailerCgroupVersion = requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION")
	cfg.MicroVMJailerParentCgroup = requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT")
	if err := os.MkdirAll(cfg.MicroVMJailerChrootBaseDir, 0o700); err != nil {
		t.Fatalf("create jailer chroot base dir: %v", err)
	}
	return cfg
}

// requireSingleFilesystem fails with an actionable message rather than letting a
// resume fail deep inside staging. The golden memory file is hard-linked into
// every jail and the sealed rootfs is reflinked per Instance; both require one
// filesystem, and both are the mechanism rather than an optimization.
func requireSingleFilesystem(t *testing.T, roots map[string]string) {
	t.Helper()
	devices := map[uint64][]string{}
	for label, path := range roots {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create %s %q: %v", label, path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s %q: %v", label, path, err)
		}
		device, _, _, ok := fileStatIdentity(info)
		if !ok {
			t.Fatalf("unsupported stat metadata for %s %q", label, path)
		}
		devices[device] = append(devices[device], fmt.Sprintf("%s=%s", label, path))
	}
	if len(devices) > 1 {
		var described []string
		for device, labels := range devices {
			described = append(described, fmt.Sprintf("device %d: %s", device, strings.Join(labels, ", ")))
		}
		t.Fatalf(
			"jailed snapshot resume requires one filesystem for the template cache, the jailer chroot base dir, and the run dirs: %s",
			strings.Join(described, "; "),
		)
	}
}

func requiredConcurrencyRungs(t *testing.T, key string) []int {
	t.Helper()
	raw := requiredEnv(t, key)
	var rungs []int
	seen := map[int]struct{}{}
	for _, field := range strings.Split(raw, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || value <= 0 {
			t.Fatalf("%s contains invalid concurrency %q", key, field)
		}
		if _, exists := seen[value]; exists {
			t.Fatalf("%s repeats concurrency %d", key, value)
		}
		seen[value] = struct{}{}
		if len(rungs) > 0 && value <= rungs[len(rungs)-1] {
			t.Fatalf("%s must be strictly ascending", key)
		}
		rungs = append(rungs, value)
	}
	if len(rungs) == 0 {
		t.Fatalf("%s must contain at least one concurrency rung", key)
	}
	return rungs
}

func linkCountOf(t *testing.T, path string) uint64 {
	t.Helper()
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		t.Fatalf("stat %q link count: %v", path, err)
	}
	return uint64(stat.Nlink)
}

func inodeKey(t *testing.T, path string) string {
	t.Helper()
	identity, err := trustedMicroVMArtifactIdentityFor(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return fmt.Sprintf("%d:%d", identity.dev, identity.ino)
}

func summarizeJailedResumeStages(samples []jailedSnapshotResumeSample) []jailedSnapshotResumeStageSummary {
	stages := []struct {
		name  string
		value func(jailedSnapshotResumeSample) int64
	}{
		{"template_identity", func(s jailedSnapshotResumeSample) int64 { return s.TemplateIdentityMicros }},
		{"jail_staging", func(s jailedSnapshotResumeSample) int64 { return s.StageMicros }},
		{"process_start", func(s jailedSnapshotResumeSample) int64 { return s.ProcessStartMicros }},
		{"snapshot_load", func(s jailedSnapshotResumeSample) int64 { return s.SnapshotLoadMicros }},
		{"first_control_response", func(s jailedSnapshotResumeSample) int64 { return s.FirstControlMicros }},
		{"post_resume_hardening", func(s jailedSnapshotResumeSample) int64 { return s.HardenMicros }},
		{"assignment_bind", func(s jailedSnapshotResumeSample) int64 { return s.BindMicros }},
		{"guest_handshake", func(s jailedSnapshotResumeSample) int64 { return s.GuestHandshakeMicros }},
		{"resume_total", func(s jailedSnapshotResumeSample) int64 { return s.ResumeTotalMicros }},
	}
	summaries := make([]jailedSnapshotResumeStageSummary, 0, len(stages))
	for _, stage := range stages {
		values := make([]int64, 0, len(samples))
		for _, sample := range samples {
			values = append(values, stage.value(sample))
		}
		summaries = append(summaries, jailedSnapshotResumeStageSummary{
			Stage:     stage.name,
			P50Micros: durationPercentile(values, 50),
			P95Micros: durationPercentile(values, 95),
			MaxMicros: durationPercentile(values, 100),
		})
	}
	return summaries
}

func stagePercentile(summaries []jailedSnapshotResumeStageSummary, stage string, percentile int) int64 {
	for _, summary := range summaries {
		if summary.Stage != stage {
			continue
		}
		if percentile == 95 {
			return summary.P95Micros
		}
		return summary.P50Micros
	}
	return 0
}
