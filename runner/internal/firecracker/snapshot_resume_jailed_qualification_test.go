package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
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
	NetworkRebinding    jailedResumeNetworkFinding `json:"networkRebinding"`
	Rungs               []jailedSnapshotResumeRung `json:"concurrencyRungs"`
	GatePassed          bool                       `json:"gatePassed"`
	GateFailures        []string                   `json:"gateFailures,omitempty"`
}

// jailedResumeNetworkFinding records what the pinned VMM actually does with a
// snapshotted network interface. The plan asserted the shape from the API
// specification; these are the measurements that settle it.
type jailedResumeNetworkFinding struct {
	// TemplateRecordsInterface is whether the captured VM state has a network
	// device at all. Everything below only means something when it is true.
	TemplateRecordsInterface bool   `json:"templateRecordsNetworkInterface"`
	TemplateInterfaceID      string `json:"templateNetworkInterfaceId"`
	TemplateGuestMAC         string `json:"templateGuestMac"`
	// RebindsToPerInstanceTap is proved by resuming with an override naming a
	// TAP that did not exist when the template was captured, after the
	// template's own TAP has been destroyed. A load that opened the recorded
	// device name could not succeed.
	RebindsToPerInstanceTap bool `json:"snapshotLoadRebindsInterfaceToPerInstanceTap"`
	// AbsentInterfaceOverrideError is the VMM's reply to an override naming an
	// interface the VM state never recorded. It is the direct evidence that an
	// override rebinds rather than adds.
	AbsentInterfaceOverrideError string `json:"absentInterfaceOverrideError"`
	// PostRestoreInterfaceCreateError is the VMM's reply to PUT
	// /network-interfaces after a successful load, closing the only other way an
	// interface could have been added.
	PostRestoreInterfaceCreateError string `json:"postRestoreInterfaceCreateError"`
}

type jailedSnapshotResumeRung struct {
	Concurrency                int                                `json:"concurrency"`
	WallClockMillis            int64                              `json:"wallClockMilliseconds"`
	Instances                  []jailedSnapshotResumeSample       `json:"instances"`
	Stages                     []jailedSnapshotResumeStageSummary `json:"stages"`
	GoldenMemoryInode          string                             `json:"goldenMemoryInode"`
	GoldenMemoryInodeShared    bool                               `json:"goldenMemoryInodeShared"`
	GoldenMemoryLinkCount      uint64                             `json:"goldenMemoryLinkCount"`
	RootfsChildrenAreDistinct  bool                               `json:"rootfsChildrenAreDistinct"`
	WorkspacesAreDistinct      bool                               `json:"workspacesAreDistinct"`
	FirecrackerReadBytesTotal  uint64                             `json:"firecrackerReadBytesTotal"`
	FirecrackerReadCharsTotal  uint64                             `json:"firecrackerReadCharactersTotal"`
	FullMemoryCopyObserved     bool                               `json:"fullMemoryCopyObserved"`
	GuestMACsAreDistinct       bool                               `json:"guestMacsAreDistinct"`
	GuestAddressesAreDistinct  bool                               `json:"guestAddressesAreDistinct"`
	EveryGuestReachedItsOwnTap bool                               `json:"everyGuestReachedItsOwnTap"`
	NoTemplateMACOnTheBridge   bool                               `json:"noTemplateMacOnTheBridge"`
}

type jailedSnapshotResumeSample struct {
	InstanceID string `json:"instanceId"`
	JailerUID  int    `json:"jailerUid"`
	// The network identity this Instance was given, and what the host observed
	// after the guest first transmitted through its own TAP.
	TapName                string `json:"tapName"`
	GuestIP                string `json:"guestIp"`
	GuestMAC               string `json:"guestMac"`
	ObservedInGuestMAC     string `json:"observedInGuestMac"`
	ObservedInGuestRoute   string `json:"observedInGuestDefaultRoute"`
	ObservedNeighbourMAC   string `json:"observedHostNeighbourMac"`
	ObservedForwardingPort string `json:"observedBridgeForwardingPort"`
	// NetworkSetupMicros is the host-side half: guest IP reservation, TAP
	// creation with this Instance's jailer UID, and a fail-closed policy install,
	// all of which complete before the snapshot is loaded. The guest-side half is
	// inside assignmentBindMicroseconds.
	NetworkSetupMicros     int64 `json:"hostNetworkSetupMicroseconds"`
	TemplateIdentityMicros int64 `json:"templateIdentityMicroseconds"`
	StageMicros            int64 `json:"stageMicroseconds"`
	ProcessStartMicros     int64 `json:"processStartMicroseconds"`
	SnapshotLoadMicros     int64 `json:"snapshotLoadMicroseconds"`
	FirstControlMicros     int64 `json:"firstControlMicroseconds"`
	HardenMicros           int64 `json:"postResumeHardeningMicroseconds"`
	BindMicros             int64 `json:"assignmentBindMicroseconds"`
	GuestHandshakeMicros   int64 `json:"guestHandshakeMicroseconds"`
	ResumeTotalMicros      int64 `json:"resumeTotalMicroseconds"`
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

	report.NetworkRebinding = measureSnapshotResumeNetworkFindings(t, cfg, template, store, memoryMiB, workspaceMiB)

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
		if !rung.GuestMACsAreDistinct || !rung.GuestAddressesAreDistinct {
			report.GatePassed = false
			report.GateFailures = append(report.GateFailures, fmt.Sprintf(
				"concurrency=%d resumed Instances shared a MAC or an address",
				concurrency,
			))
		}
		if !rung.EveryGuestReachedItsOwnTap {
			report.GatePassed = false
			report.GateFailures = append(report.GateFailures, fmt.Sprintf(
				"concurrency=%d guest frames arrived on another Instance's TAP",
				concurrency,
			))
		}
		if !rung.NoTemplateMACOnTheBridge {
			report.GatePassed = false
			report.GateFailures = append(report.GateFailures, fmt.Sprintf(
				"concurrency=%d leaked the template's captured MAC onto the bridge",
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
	defer releaseManagerNetworkPolicy(t, manager)
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
	}
	assertJailedResumeNetworkIdentities(t, cfg, instances, &rung)
	for _, inst := range instances {
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
	id              string
	sandboxID       string
	compartment     string
	jailerUID       int
	runDir          string
	logPath         string
	logFile         *os.File
	workspaceID     string
	attachment      workspacestore.ComputeAttachment
	launch          snapshotResumeLaunch
	cmd             *exec.Cmd
	session         *GuestProtocolSession
	guestIP         string
	tapName         string
	tapConfigured   bool
	policyInstalled bool
	sample          jailedSnapshotResumeSample
	err             error
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
	launch, err := inst.startResumeProcess(ctx, manager, cfg, template, workspaceImage.Name(), policy)
	if err != nil {
		return err
	}

	loadStartedAt := time.Now()
	// The TAP the template was captured against no longer exists; only the
	// override can make this load succeed.
	if err := resumeSnapshotTemplate(ctx, launch, inst.tapName, 30*time.Second, 120*time.Second); err != nil {
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
		Network: &AssignmentNetworkIdentity{
			Interface:   snapshotResumeNetworkInterfaceID,
			MACAddress:  guestMACForInstance(inst.tapName),
			AddressCIDR: guestAddressCIDR(inst.guestIP, cfg.MicroVMBridgeCIDR),
			Gateway:     bridgeAddress(cfg.MicroVMBridgeCIDR).String(),
		},
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
	// Everything above is the resume. What follows is the evidence that the
	// resumed guest's interface is its own, collected after the timed path so it
	// cannot inflate a stage.
	return inst.observeGuestNetworkIdentity(ctx, bridgeAddress(cfg.MicroVMBridgeCIDR).String())
}

// startResumeProcess creates everything the host owns and starts the jailed
// process, stopping just short of the load. The order is the one the production
// path must also hold: the guest address, the TAP owned by this Instance's
// jailer UID, and a fail-closed policy all exist before the snapshot is loaded.
// A resumed guest's interface is captured link-down and stays down until its
// assignment bind, so no frame can leave it before the policy governing it is
// installed.
func (inst *jailedResumeInstance) startResumeProcess(
	ctx context.Context,
	manager *Manager,
	cfg *config.Config,
	template *AdmittedSnapshotTemplate,
	workspacePath string,
	policy *runtimemanager.SandboxRuntimePolicy,
) (snapshotResumeLaunch, error) {
	networkStartedAt := time.Now()
	guestIP, err := manager.reserveGuestIP(inst.id)
	if err != nil {
		return snapshotResumeLaunch{}, fmt.Errorf("reserve resume guest IP: %w", err)
	}
	inst.guestIP = guestIP
	inst.tapName = tapNameForInstance(cfg.MicroVMTapPrefix, inst.id)
	if err := manager.network.ConfigureTap(ctx, TapConfig{
		SandboxID:  inst.sandboxID,
		InstanceID: inst.id,
		TapName:    inst.tapName,
		GuestIP:    guestIP,
		BridgeName: cfg.MicroVMBridgeName,
		BridgeCIDR: cfg.MicroVMBridgeCIDR,
		OwnerUID:   manager.tapOwnerUID(inst.jailerUID),
	}); err != nil {
		return snapshotResumeLaunch{}, fmt.Errorf("configure resume TAP: %w", err)
	}
	inst.tapConfigured = true
	if manager.networkPolicy == nil || manager.defaultNetworkPolicy == nil {
		return snapshotResumeLaunch{}, fmt.Errorf("host network policy enforcement is required for a jailed resume gate")
	}
	if err := manager.networkPolicy.Install(ctx, PolicyNetworkConfig{
		InstanceID: inst.id,
		TapName:    inst.tapName,
		GuestIP:    guestIP,
		DNSAddress: bridgeAddress(cfg.MicroVMBridgeCIDR),
		Policy:     manager.defaultNetworkPolicy,
		OnFailure:  func(error) {},
	}); err != nil {
		return snapshotResumeLaunch{}, fmt.Errorf("install resume host network policy: %w", err)
	}
	inst.policyInstalled = true
	inst.sample.NetworkSetupMicros = time.Since(networkStartedAt).Microseconds()
	inst.sample.TapName = inst.tapName
	inst.sample.GuestIP = guestIP
	inst.sample.GuestMAC = guestMACForInstance(inst.tapName)

	stageStartedAt := time.Now()
	launch, err := manager.prepareSnapshotResumeLaunch(
		inst.id,
		inst.runDir,
		template,
		workspacePath,
		"",
		inst.jailerUID,
		policy,
	)
	if err != nil {
		return snapshotResumeLaunch{}, fmt.Errorf("prepare jailed resume launch: %w", err)
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
		return snapshotResumeLaunch{}, fmt.Errorf("start jailer: %w", err)
	}
	inst.cmd = cmd
	if err := waitForUnixSocket(ctx, launch.socketPath, 30*time.Second); err != nil {
		return snapshotResumeLaunch{}, fmt.Errorf("wait for jailed Firecracker API socket: %w", err)
	}
	inst.sample.ProcessStartMicros = time.Since(processStartedAt).Microseconds()
	return launch, nil
}

// observeGuestNetworkIdentity reads the interface state the guest itself sees
// and then makes it transmit, so the host can record which TAP its frames
// arrive on.
func (inst *jailedResumeInstance) observeGuestNetworkIdentity(ctx context.Context, gateway string) error {
	hardware, err := inst.runGuestShell(ctx, "cat /sys/class/net/"+snapshotResumeNetworkInterfaceID+"/address")
	if err != nil {
		return fmt.Errorf("read resumed guest hardware address: %w", err)
	}
	inst.sample.ObservedInGuestMAC = hardware
	// /proc/net/route renders the default route as a zero destination with the
	// gateway in host byte order. Reading it with the shell alone proves the route
	// the bind installed without any userspace networking tool, which is the point:
	// the guest image has none.
	route, err := inst.runGuestShell(
		ctx,
		`while read -r iface dest gw rest; do `+
			`if [ "$dest" = 00000000 ] && [ "$gw" != 00000000 ]; then echo "$iface $gw"; fi; `+
			`done < /proc/net/route`,
	)
	if err != nil {
		return fmt.Errorf("read resumed guest default route: %w", err)
	}
	inst.sample.ObservedInGuestRoute = route
	// The default-deny policy drops the echo request, but it permits the ARP
	// exchange with the gateway that must happen first. That exchange is what
	// puts this guest's address and MAC into the host's neighbour table and its
	// MAC into the bridge forwarding database against its own TAP port.
	if _, err := inst.runGuestShell(
		ctx,
		fmt.Sprintf("ping -c 2 -W 1 %s >/dev/null 2>&1; exit 0", gateway),
	); err != nil {
		return fmt.Errorf("transmit from resumed guest: %w", err)
	}
	return nil
}

func (inst *jailedResumeInstance) runGuestShell(ctx context.Context, script string) (string, error) {
	result, err := inst.session.ExecuteBuffered(ctx, "jailed-resume-"+inst.id, &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Shell{Shell: script},
		OutputLimitBytes: 64 * 1024,
		DeadlineUnixMs:   uint64(time.Now().Add(30 * time.Second).UnixMilli()),
	})
	if err != nil {
		return "", err
	}
	if result.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED ||
		result.Terminal.GetExitCode() != 0 {
		return "", fmt.Errorf("guest shell %q ended %v: %s", script, result.Terminal, strings.TrimSpace(string(result.Stderr)))
	}
	return strings.TrimSpace(string(result.Stdout)), nil
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
	// The policy and the TAP are released only after the VMM is gone, in that
	// order: nothing may remove the rules governing a link a resumed guest could
	// still be using.
	if inst.policyInstalled {
		joined = errors.Join(joined, manager.networkPolicy.Remove(context.Background(), inst.id))
		inst.policyInstalled = false
	}
	if inst.tapConfigured {
		joined = errors.Join(joined, manager.cleanupTapChecked(context.Background(), inst.tapName))
		inst.tapConfigured = false
	}
	if inst.guestIP != "" {
		manager.releaseGuestIP(inst.id)
		inst.guestIP = ""
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

// measureSnapshotResumeNetworkFindings settles what the pinned VMM will and will
// not do with a snapshotted network interface.
//
// The API specification already says a network override changes "the backing TAP
// device of a network interface", that PUT /network-interfaces is pre-boot only,
// and that no endpoint hotplugs one. These are the measurements behind those
// sentences, taken against the exact binary the runner ships, because the whole
// template shape depends on the answer: an interface a template did not capture
// is an interface no Sandbox resumed from it can ever have.
func measureSnapshotResumeNetworkFindings(
	t *testing.T,
	cfg *config.Config,
	template *AdmittedSnapshotTemplate,
	store *workspacestore.Store,
	memoryMiB int,
	workspaceMiB int,
) jailedResumeNetworkFinding {
	t.Helper()
	finding := jailedResumeNetworkFinding{
		TemplateRecordsInterface: template.Manifest.Key.HasNetworkDevice(),
		TemplateInterfaceID:      template.Manifest.Key.NetworkInterfaceID,
		TemplateGuestMAC:         template.Manifest.Key.TemplateGuestMAC,
	}
	if !finding.TemplateRecordsInterface {
		t.Fatal("the jailed resume gate requires a template captured with a network device")
	}
	manager, err := New(cfg)
	if err != nil {
		t.Fatalf("new network-finding manager: %v", err)
	}
	defer releaseManagerNetworkPolicy(t, manager)
	opts := smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone:      "UTC",
		CompartmentID: "cmp_resume_netfind",
	})
	policy := &runtimemanager.SandboxRuntimePolicy{
		VCPUs:             cfg.MicroVMVCPUs,
		CPUMillis:         cfg.MicroVMVCPUs * 1000,
		MemoryMiB:         memoryMiB,
		WorkspaceSizeMiB:  workspaceMiB,
		ProcessLimit:      opts.SandboxPolicy.ProcessLimit,
		WorkspaceWritable: true,
	}

	// An override naming an interface the VM state never recorded. If an override
	// could add one, this would succeed.
	absent := newJailedResumeInstance(t, manager, store, cfg, 0, 0, workspaceMiB)
	defer func() {
		if err := absent.shutdown(manager); err != nil {
			t.Errorf("release absent-interface finding instance: %v", err)
		}
	}()
	absentLaunch, err := absent.startResumeProcess(t.Context(), manager, cfg, template, absent.attachment.Image().Name(), policy)
	if err != nil {
		t.Fatalf("start absent-interface finding instance: %v", err)
	}
	absentRequest := snapshotResumeLoadRequest(absentLaunch, absent.tapName)
	absentRequest.NetworkOverrides = []networkOverride{{
		IfaceID:     unrecordedNetworkInterfaceID,
		HostDevName: absent.tapName,
	}}
	if err := waitForUnixSocket(t.Context(), absentLaunch.socketPath, 30*time.Second); err != nil {
		t.Fatalf("wait for absent-interface finding API socket: %v", err)
	}
	absentErr := (FirecrackerAPIClient{SocketPath: absentLaunch.socketPath, Timeout: 120 * time.Second}).
		LoadSnapshotWithOptions(t.Context(), absentRequest)
	if absentErr == nil {
		t.Fatalf(
			"the VMM accepted a network override for %q, which the template never recorded",
			unrecordedNetworkInterfaceID,
		)
	}
	finding.AbsentInterfaceOverrideError = absentErr.Error()

	// A successful resume, then the only other way an interface could arrive.
	rebound := newJailedResumeInstance(t, manager, store, cfg, 1, 0, workspaceMiB)
	defer func() {
		if err := rebound.shutdown(manager); err != nil {
			t.Errorf("release rebinding finding instance: %v", err)
		}
	}()
	reboundLaunch, err := rebound.startResumeProcess(t.Context(), manager, cfg, template, rebound.attachment.Image().Name(), policy)
	if err != nil {
		t.Fatalf("start rebinding finding instance: %v", err)
	}
	// The TAP the template was captured against was destroyed with the template
	// source VM, so a load that opened the recorded device name could not
	// succeed. This one names a TAP created minutes later.
	if err := resumeSnapshotTemplate(t.Context(), reboundLaunch, rebound.tapName, 30*time.Second, 120*time.Second); err != nil {
		t.Fatalf("resume onto a per-Instance TAP: %v\n%s", err, smokeLogPath(t, rebound.logPath))
	}
	finding.RebindsToPerInstanceTap = true
	createErr := (FirecrackerAPIClient{SocketPath: reboundLaunch.socketPath, Timeout: 30 * time.Second}).
		putJSON(t.Context(), "/network-interfaces/"+unrecordedNetworkInterfaceID, networkIface{
			IfaceID:     unrecordedNetworkInterfaceID,
			HostDevName: rebound.tapName,
			GuestMAC:    snapshotTemplateGuestMAC,
		}, nil)
	if createErr == nil {
		t.Fatal("the VMM created a network interface after a snapshot load")
	}
	finding.PostRestoreInterfaceCreateError = createErr.Error()

	t.Logf(
		"network finding: rebinding to a per-Instance TAP succeeds; override for %q -> %v; post-restore create -> %v",
		unrecordedNetworkInterfaceID,
		absentErr,
		createErr,
	)
	return finding
}

// unrecordedNetworkInterfaceID is an interface identifier no template captures.
// It is the probe for "can a resumed guest be given a new interface", and the
// answer is what decides whether templates must be built with one.
const unrecordedNetworkInterfaceID = "eth1"

// assertJailedResumeNetworkIdentities is the empirical half of the network
// finding. Every resumed Instance was given a TAP that did not exist when the
// template was captured, so a load that opened the recorded device name could
// not have succeeded at all. What remains to prove is that the rebinding is
// per-Instance: each guest carries its own MAC and address, and each guest's
// frames arrive on its own TAP rather than on a shared link.
func assertJailedResumeNetworkIdentities(
	t *testing.T,
	cfg *config.Config,
	instances []*jailedResumeInstance,
	rung *jailedSnapshotResumeRung,
) {
	t.Helper()
	rung.GuestMACsAreDistinct = true
	rung.GuestAddressesAreDistinct = true
	rung.EveryGuestReachedItsOwnTap = true
	rung.NoTemplateMACOnTheBridge = true
	macs := map[string]string{}
	addresses := map[string]string{}
	for _, inst := range instances {
		expectedMAC := guestMACForInstance(inst.tapName)
		if inst.sample.ObservedInGuestMAC != expectedMAC {
			t.Fatalf(
				"instance %s reports hardware address %q inside the guest, want the bound %q",
				inst.id,
				inst.sample.ObservedInGuestMAC,
				expectedMAC,
			)
		}
		if inst.sample.ObservedInGuestMAC == snapshotTemplateGuestMAC {
			t.Fatalf("instance %s still carries the template's captured MAC", inst.id)
		}
		wantRoute := snapshotResumeNetworkInterfaceID + " " + littleEndianHexAddress(t, bridgeAddress(cfg.MicroVMBridgeCIDR).String())
		if inst.sample.ObservedInGuestRoute != wantRoute {
			t.Fatalf(
				"instance %s default route is %q, want %q",
				inst.id,
				inst.sample.ObservedInGuestRoute,
				wantRoute,
			)
		}
		if owner, exists := macs[inst.sample.ObservedInGuestMAC]; exists {
			t.Logf("instances %s and %s share MAC %s", owner, inst.id, inst.sample.ObservedInGuestMAC)
			rung.GuestMACsAreDistinct = false
		}
		macs[inst.sample.ObservedInGuestMAC] = inst.id
		if owner, exists := addresses[inst.guestIP]; exists {
			t.Logf("instances %s and %s share address %s", owner, inst.id, inst.guestIP)
			rung.GuestAddressesAreDistinct = false
		}
		addresses[inst.guestIP] = inst.id
	}
	neighbours := hostNeighbourTable(t, cfg.MicroVMBridgeName)
	forwarding := hostBridgeForwardingTable(t, cfg.MicroVMBridgeName)
	for _, inst := range instances {
		inst.sample.ObservedNeighbourMAC = neighbours[inst.guestIP]
		inst.sample.ObservedForwardingPort = forwarding[inst.sample.ObservedInGuestMAC]
		if inst.sample.ObservedNeighbourMAC != inst.sample.ObservedInGuestMAC {
			t.Fatalf(
				"host learned %s at %q, want the guest's own %q",
				inst.guestIP,
				inst.sample.ObservedNeighbourMAC,
				inst.sample.ObservedInGuestMAC,
			)
		}
		if inst.sample.ObservedForwardingPort != inst.tapName {
			t.Logf(
				"instance %s frames arrived on %q, not its own TAP %q",
				inst.id,
				inst.sample.ObservedForwardingPort,
				inst.tapName,
			)
			rung.EveryGuestReachedItsOwnTap = false
		}
	}
	if _, leaked := forwarding[snapshotTemplateGuestMAC]; leaked {
		rung.NoTemplateMACOnTheBridge = false
	}
}

// hostNeighbourTable maps guest address to learned MAC. The entry exists because
// the resumed guest ARPed for the gateway, which the default-deny policy permits
// and nothing else does.
func hostNeighbourTable(t *testing.T, bridgeName string) map[string]string {
	t.Helper()
	table := map[string]string{}
	forEachIPRoute2Record(t, []string{"ip", "-json", "neigh", "show", "dev", bridgeName}, func(record map[string]any) {
		address, _ := record["dst"].(string)
		hardware, _ := record["lladdr"].(string)
		if address != "" && hardware != "" {
			table[address] = hardware
		}
	})
	return table
}

// hostBridgeForwardingTable maps a learned MAC to the bridge port it was learned
// on. It is the direct observation that a resumed guest's frames arrive on its
// own TAP: the bridge records the port, not the guest.
func hostBridgeForwardingTable(t *testing.T, bridgeName string) map[string]string {
	t.Helper()
	table := map[string]string{}
	forEachIPRoute2Record(t, []string{"bridge", "-json", "fdb", "show", "br", bridgeName}, func(record map[string]any) {
		hardware, _ := record["mac"].(string)
		port, _ := record["ifname"].(string)
		if hardware == "" || port == "" || port == bridgeName {
			return
		}
		if _, exists := table[hardware]; !exists {
			table[hardware] = port
		}
	})
	return table
}

func forEachIPRoute2Record(t *testing.T, command []string, visit func(map[string]any)) {
	t.Helper()
	out, err := exec.Command(command[0], command[1:]...).Output()
	if err != nil {
		t.Fatalf("%s: %v", strings.Join(command, " "), err)
	}
	var records []map[string]any
	if err := json.Unmarshal(out, &records); err != nil {
		t.Fatalf("decode %s output %q: %v", strings.Join(command, " "), string(out), err)
	}
	for _, record := range records {
		visit(record)
	}
}

// littleEndianHexAddress renders an IPv4 address the way /proc/net/route does,
// which is host byte order printed as eight uppercase hex digits.
func littleEndianHexAddress(t *testing.T, address string) string {
	t.Helper()
	parsed, err := netip.ParseAddr(address)
	if err != nil || !parsed.Is4() {
		t.Fatalf("parse IPv4 address %q: %v", address, err)
	}
	octets := parsed.As4()
	return fmt.Sprintf("%02X%02X%02X%02X", octets[3], octets[2], octets[1], octets[0])
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
	// The template is captured with a network device and every Instance resumes
	// onto its own TAP, because a resumed guest can never be given an interface
	// the capture did not record. Bridge networking is therefore part of the gate
	// rather than an option in it.
	cfg.MicroVMBridgeName = requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_BRIDGE_NAME")
	cfg.MicroVMBridgeCIDR = requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_BRIDGE_CIDR")
	cfg.MicroVMTapPrefix = requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_TAP_PREFIX")
	cfg.NetworkPolicyNFTPath = requiredEnv(t, "SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH")
	cfg.NetworkPolicyMaximumDNSPins = requiredPositiveEnvInt(t, "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS")
	cfg.NetworkPolicyMaximumDNSTTL = requiredDurationEnv(t, "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL")
	cfg.NetworkPolicyRunnerAddresses = []netip.Addr{bridgeAddress(cfg.MicroVMBridgeCIDR)}
	// The gate runs in an isolated network namespace and resolves nothing, but the
	// policy enforcer starts the runner's DNS proxy and refuses to run without an
	// explicit upstream. Stating one keeps the enforcer the same fail-closed
	// component a production runner installs.
	dnsUpstream, err := netip.ParseAddrPort(requiredEnv(t, "SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM"))
	if err != nil {
		t.Fatalf("SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM must be host:port: %v", err)
	}
	cfg.NetworkPolicyDNSUpstream = dnsUpstream
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

// releaseManagerNetworkPolicy stops a Manager's runner DNS proxy. A production
// runner has exactly one Manager for its whole lifetime; this gate builds several
// in sequence, and each one's enforcer binds the bridge address on port 53, so
// the previous one has to let go before the next can start.
func releaseManagerNetworkPolicy(t *testing.T, manager *Manager) {
	t.Helper()
	if manager == nil {
		return
	}
	enforcer, ok := manager.networkPolicy.(*NFTablesNetworkPolicyEnforcer)
	if !ok {
		return
	}
	if err := enforcer.Close(); err != nil {
		t.Errorf("close runner DNS proxy: %v", err)
	}
}

func requiredDurationEnv(t *testing.T, key string) time.Duration {
	t.Helper()
	value, err := time.ParseDuration(requiredEnv(t, key))
	if err != nil || value <= 0 {
		t.Fatalf("%s must be a positive Go duration: %v", key, err)
	}
	return value
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
