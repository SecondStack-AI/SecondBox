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
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
	"golang.org/x/sys/unix"
)

const snapshotLoadQualificationSchemaVersion = 1

type snapshotLoadQualificationReport struct {
	SchemaVersion         int                              `json:"schemaVersion"`
	SourceCommit          string                           `json:"sourceCommit"`
	SourceTreeDirty       bool                             `json:"sourceTreeDirty"`
	GoVersion             string                           `json:"goVersion"`
	ComputeBackendVersion string                           `json:"firecrackerVersion"`
	HostKernel            string                           `json:"hostKernel"`
	HostCPU               string                           `json:"hostCpu"`
	WorkspaceFilesystem   string                           `json:"workspaceFilesystem"`
	MemoryBackend         string                           `json:"memoryBackend"`
	CompletedAt           string                           `json:"completedAt"`
	WarmIterations        int                              `json:"warmIterations"`
	Shapes                []snapshotLoadQualificationShape `json:"shapes"`
	GatePassed            bool                             `json:"gatePassed"`
	GateFailures          []string                         `json:"gateFailures,omitempty"`
}

type snapshotLoadQualificationShape struct {
	MemoryMiB                 int                               `json:"memoryMiB"`
	MemoryFileBytes           int64                             `json:"memoryFileBytes"`
	Samples                   []snapshotLoadQualificationSample `json:"samples"`
	WarmSnapshotLoadP50Millis int64                             `json:"warmSnapshotLoadP50Milliseconds"`
	WarmSnapshotLoadP95Millis int64                             `json:"warmSnapshotLoadP95Milliseconds"`
	WarmTotalP50Millis        int64                             `json:"warmTotalP50Milliseconds"`
	WarmTotalP95Millis        int64                             `json:"warmTotalP95Milliseconds"`
	FullCopyObserved          bool                              `json:"fullCopyObserved"`
}

type snapshotLoadQualificationSample struct {
	CacheState                string `json:"cacheState"`
	ProcessAPIReadyMillis     int64  `json:"processApiReadyMilliseconds"`
	SnapshotLoadMillis        int64  `json:"snapshotLoadMilliseconds"`
	FirstControlMillis        int64  `json:"firstControlMilliseconds"`
	PostResumeHardeningMillis int64  `json:"postResumeHardeningMilliseconds"`
	TotalMillis               int64  `json:"totalMilliseconds"`
	ReadCharacters            uint64 `json:"readCharacters"`
	WriteCharacters           uint64 `json:"writeCharacters"`
	ReadBytes                 uint64 `json:"readBytes"`
	WriteBytes                uint64 `json:"writeBytes"`
	MajorFaults               uint64 `json:"majorFaults"`
	FullCopyObserved          bool   `json:"fullCopyObserved"`
}

type snapshotProcessCounters struct {
	readCharacters  uint64
	writeCharacters uint64
	readBytes       uint64
	writeBytes      uint64
	majorFaults     uint64
}

func TestSmokeSnapshotResumeLoadMeasurement(t *testing.T) {
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_SNAPSHOT_LOAD") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_SNAPSHOT_LOAD=1 to measure snapshot load")
	}
	shapes := requiredSnapshotMemoryShapes(t)
	warmIterations := requiredPositiveEnvInt(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_WARM_ITERATIONS")
	outputPath := requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_OUTPUT")
	if !filepath.IsAbs(outputPath) || filepath.Clean(outputPath) != outputPath {
		t.Fatalf("SECONDBOX_SNAPSHOT_QUALIFICATION_OUTPUT must be a clean absolute path")
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SECONDBOX_SNAPSHOT_QUALIFICATION_OUTPUT must be absent: %v", err)
	}

	report := snapshotLoadQualificationReport{
		SchemaVersion:         snapshotLoadQualificationSchemaVersion,
		SourceCommit:          requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_SOURCE_COMMIT"),
		SourceTreeDirty:       requiredEnvBool(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_SOURCE_TREE_DIRTY"),
		GoVersion:             runtime.Version(),
		ComputeBackendVersion: firecrackerQualificationVersion(t),
		HostKernel:            requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_HOST_KERNEL"),
		HostCPU:               requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_HOST_CPU"),
		WorkspaceFilesystem:   requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_FILESYSTEM"),
		MemoryBackend:         "File",
		WarmIterations:        warmIterations,
		GatePassed:            true,
	}
	for _, memoryMiB := range shapes {
		shape := measureSnapshotLoadShape(t, memoryMiB, warmIterations)
		report.Shapes = append(report.Shapes, shape)
		if shape.WarmSnapshotLoadP95Millis > 70 {
			report.GatePassed = false
			report.GateFailures = append(report.GateFailures, fmt.Sprintf(
				"memoryMiB=%d warm snapshot load p95=%dms exceeds 70ms",
				memoryMiB,
				shape.WarmSnapshotLoadP95Millis,
			))
		}
		if shape.FullCopyObserved {
			report.GatePassed = false
			report.GateFailures = append(report.GateFailures, fmt.Sprintf(
				"memoryMiB=%d performed memory-image-sized process I/O",
				memoryMiB,
			))
		}
	}
	report.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeSnapshotLoadQualificationReport(outputPath, report); err != nil {
		t.Fatalf("write snapshot-load qualification report: %v", err)
	}
	if !report.GatePassed {
		t.Fatalf("snapshot-load feasibility gate failed: %s", strings.Join(report.GateFailures, "; "))
	}
}

func measureSnapshotLoadShape(t *testing.T, memoryMiB int, warmIterations int) snapshotLoadQualificationShape {
	t.Helper()
	workDir := shortSmokeDir(t)
	workspaceMiB := requiredPositiveEnvInt(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_MIB")
	cfg := snapshotLoadQualificationConfig(t, workDir, memoryMiB, workspaceMiB)
	manager, err := New(cfg)
	if err != nil {
		t.Fatalf("new %d MiB snapshot source manager: %v", memoryMiB, err)
	}
	workspaceStore, err := workspacestore.New(t.Context(), workspacestore.Config{
		Root:                  cfg.RunnerWorkspaceRoot,
		TemplateCapacityBytes: int64(workspaceMiB) << 20,
		FormatterKind:         workspacestore.FormatterMke2fs,
	})
	if err != nil {
		t.Fatalf("new %d MiB snapshot WorkspaceStore: %v", memoryMiB, err)
	}
	if err := manager.SetWorkspaceStore(workspaceStore); err != nil {
		t.Fatalf("bind %d MiB snapshot WorkspaceStore: %v", memoryMiB, err)
	}

	workspaceID := fmt.Sprintf("snapshot-load-%d", memoryMiB)
	if _, err := workspaceStore.Create(t.Context(), workspacestore.CreateWorkspaceRequest{
		Mutation: workspacestore.Mutation{
			OperationID:  "create-" + workspaceID,
			WorkspaceID:  workspaceID,
			FencingToken: []byte("01234567890123456789012345678901"),
		},
		CapacityBytes: int64(workspaceMiB) << 20,
	}); err != nil {
		t.Fatalf("create %d MiB snapshot Workspace: %v", memoryMiB, err)
	}
	attachment, err := workspaceStore.Open(t.Context(), workspaceID, 1)
	if err != nil {
		t.Fatalf("open %d MiB snapshot Workspace: %v", memoryMiB, err)
	}
	opts := smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone:            "UTC",
		CompartmentID:       fmt.Sprintf("cmp_snapshot_%d", memoryMiB),
		WorkspaceAttachment: attachment,
	})
	instanceID, err := manager.createAndStart(t.Context(), "snapshotload", opts)
	if err != nil {
		_ = attachment.Close()
		t.Fatalf("start %d MiB snapshot source: %v\n%s", memoryMiB, err, latestSmokeLog(t, workDir))
	}
	inst := manager.lookup(instanceID)
	if inst == nil || inst.cmd == nil || inst.cmd.Process == nil {
		t.Fatalf("snapshot source %q has no Firecracker process", instanceID)
	}

	snapshotDir := filepath.Join(workDir, "snapshot")
	manifest, err := createPausedSnapshotForQualification(t.Context(), inst, snapshotDir)
	if err != nil {
		_ = manager.Remove(context.Background(), instanceID)
		t.Fatalf("create %d MiB snapshot: %v\n%s", memoryMiB, err, smokeLogPath(t, inst.logPath))
	}
	if err := syscall.Kill(-inst.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("terminate %d MiB snapshot source: %v", memoryMiB, err)
	}
	select {
	case <-inst.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("snapshot source %q did not terminate", instanceID)
	}

	memoryInfo, err := os.Stat(manifest.MemFilePath)
	if err != nil {
		t.Fatalf("stat %d MiB memory snapshot: %v", memoryMiB, err)
	}
	shape := snapshotLoadQualificationShape{
		MemoryMiB:       memoryMiB,
		MemoryFileBytes: memoryInfo.Size(),
	}
	restoreRoot, err := os.MkdirTemp(cfg.MicroVMRunDir, fmt.Sprintf("slq-%d-", memoryMiB))
	if err != nil {
		t.Fatalf("create %d MiB short restore root: %v", memoryMiB, err)
	}
	defer os.RemoveAll(restoreRoot)
	for sampleIndex := 0; sampleIndex <= warmIterations; sampleIndex++ {
		cacheState := "warm"
		if sampleIndex == 0 {
			cacheState = "cold"
			if err := evictSnapshotFileCache(manifest.MemFilePath); err != nil {
				t.Fatalf("evict %d MiB snapshot memory cache: %v", memoryMiB, err)
			}
		}
		restoreDir := filepath.Join(restoreRoot, fmt.Sprintf("%02d", sampleIndex))
		sample, err := runSnapshotLoadSample(
			t.Context(),
			cfg,
			manifest,
			restoreDir,
			cacheState,
			memoryInfo.Size(),
		)
		if err != nil {
			t.Fatalf(
				"measure %d MiB %s snapshot sample %d: %v\n%s",
				memoryMiB,
				cacheState,
				sampleIndex,
				err,
				smokeLogPath(t, restoreDir+".log"),
			)
		}
		shape.Samples = append(shape.Samples, sample)
		shape.FullCopyObserved = shape.FullCopyObserved || sample.FullCopyObserved
	}
	warmLoad, warmTotal := make([]int64, 0, warmIterations), make([]int64, 0, warmIterations)
	for _, sample := range shape.Samples {
		if sample.CacheState != "warm" {
			continue
		}
		warmLoad = append(warmLoad, sample.SnapshotLoadMillis)
		warmTotal = append(warmTotal, sample.TotalMillis)
	}
	shape.WarmSnapshotLoadP50Millis = durationPercentile(warmLoad, 50)
	shape.WarmSnapshotLoadP95Millis = durationPercentile(warmLoad, 95)
	shape.WarmTotalP50Millis = durationPercentile(warmTotal, 50)
	shape.WarmTotalP95Millis = durationPercentile(warmTotal, 95)
	return shape
}

func createPausedSnapshotForQualification(
	ctx context.Context,
	inst *instance,
	outDir string,
) (GoldenSnapshotManifest, error) {
	if inst == nil {
		return GoldenSnapshotManifest{}, fmt.Errorf("snapshot source instance is required")
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return GoldenSnapshotManifest{}, fmt.Errorf("create snapshot output directory: %w", err)
	}
	client := inst.apiClient(30 * time.Second)
	if err := client.Pause(ctx); err != nil {
		return GoldenSnapshotManifest{}, fmt.Errorf("pause qualification source: %w", err)
	}
	manifest := GoldenSnapshotManifest{
		SnapshotPath: filepath.Join(outDir, "vmstate.snap"),
		MemFilePath:  filepath.Join(outDir, "memory.snap"),
	}
	if err := client.CreateFullSnapshot(ctx, manifest.SnapshotPath, manifest.MemFilePath); err != nil {
		return GoldenSnapshotManifest{}, fmt.Errorf("create paused qualification snapshot: %w", err)
	}
	for label, path := range map[string]string{
		"snapshot": manifest.SnapshotPath,
		"memory":   manifest.MemFilePath,
	} {
		if _, err := os.Stat(path); err != nil {
			return GoldenSnapshotManifest{}, fmt.Errorf("stat qualification %s: %w", label, err)
		}
	}
	return manifest, nil
}

func snapshotLoadQualificationConfig(t *testing.T, workDir string, memoryMiB int, workspaceMiB int) *config.Config {
	t.Helper()
	runDir := filepath.Join(
		requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_RUN_ROOT"),
		fmt.Sprintf("m%d", memoryMiB),
	)
	return &config.Config{
		FirecrackerPath:        requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		MicroVMKernelPath:      requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:      requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMSharedImagePath: requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
		MicroVMPublicKeyPath:   requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"),
		MicroVMPublicKeySHA256: requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"),
		RunnerWorkspaceRoot:    filepath.Join(workDir, "workspaces"),
		MicroVMRunDir:          runDir,
		MicroVMLogDir:          filepath.Join(workDir, "logs"),
		// The qualifications build their own cache under the same root, so the
		// Manager's configured root is the one they then admit templates through
		// and the jailed gate's single-filesystem requirement covers both.
		MicroVMSnapshotTemplateCacheRoot: filepath.Join(runDir, "templates"),
		MicroVMKernelArgs:                requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		MicroVMMemoryMiB:                 memoryMiB,
		MicroVMVCPUs:                     1,
		MicroVMWorkspaceSizeMiB:          workspaceMiB,
		MicroVMAllowUnjailed:             true,
	}
}

func runSnapshotLoadSample(
	ctx context.Context,
	cfg *config.Config,
	manifest GoldenSnapshotManifest,
	restoreDir string,
	cacheState string,
	memoryFileBytes int64,
) (sample snapshotLoadQualificationSample, returnErr error) {
	if err := os.MkdirAll(restoreDir, 0o700); err != nil {
		return sample, err
	}
	apiSocket := filepath.Join(restoreDir, firecrackerSockName)
	vsockPath := filepath.Join(restoreDir, vsockUDSName)
	if err := checkUnixSocketPath("snapshot qualification API", apiSocket, "SECONDBOX_RUNNER_QUALIFICATION_TEMP_ROOT"); err != nil {
		return sample, err
	}
	if err := checkUnixSocketPath("snapshot qualification vsock", vsockPath, "SECONDBOX_RUNNER_QUALIFICATION_TEMP_ROOT"); err != nil {
		return sample, err
	}
	logPath := restoreDir + ".log"
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return sample, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, logFile.Close())
	}()
	restoreID := "snapshot-load-" + filepath.Base(restoreDir)
	cmd := exec.Command(cfg.FirecrackerPath, "--id", restoreID, "--api-sock", apiSocket)
	cmd.Dir = restoreDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	startedAt := time.Now()
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
	if err := waitForUnixSocket(ctx, apiSocket, 5*time.Second); err != nil {
		return sample, err
	}
	sample.CacheState = cacheState
	sample.ProcessAPIReadyMillis = time.Since(startedAt).Milliseconds()
	before, err := readSnapshotProcessCounters(cmd.Process.Pid)
	if err != nil {
		return sample, err
	}

	loadStartedAt := time.Now()
	if err := (FirecrackerAPIClient{SocketPath: apiSocket, Timeout: 10 * time.Second}).LoadSnapshotWithOptions(
		ctx,
		snapshotLoadRequest{
			SnapshotPath: manifest.SnapshotPath,
			MemBackend: &memoryBackend{
				BackendPath: manifest.MemFilePath,
				BackendType: "File",
			},
			ResumeVM:      true,
			VsockOverride: &vsockOverride{UDSPath: vsockPath},
			ClockRealtime: true,
		},
	); err != nil {
		return sample, err
	}
	sample.SnapshotLoadMillis = time.Since(loadStartedAt).Milliseconds()

	controlStartedAt := time.Now()
	controlClient := ControlClient{
		UDSPath: vsockPath,
		Port:    cfg.MicroVMGuestControlVsockPort,
		Timeout: 25 * time.Millisecond,
	}
	if err := waitForSnapshotControl(ctx, controlClient, 5*time.Second); err != nil {
		return sample, err
	}
	sample.FirstControlMillis = time.Since(controlStartedAt).Milliseconds()
	hardeningStartedAt := time.Now()
	if err := controlClient.HardenPostRestore(ctx, time.Now().UTC()); err != nil {
		return sample, err
	}
	sample.PostResumeHardeningMillis = time.Since(hardeningStartedAt).Milliseconds()
	sample.TotalMillis = time.Since(startedAt).Milliseconds()
	after, err := readSnapshotProcessCounters(cmd.Process.Pid)
	if err != nil {
		return sample, err
	}
	sample.ReadCharacters = counterDelta(after.readCharacters, before.readCharacters)
	sample.WriteCharacters = counterDelta(after.writeCharacters, before.writeCharacters)
	sample.ReadBytes = counterDelta(after.readBytes, before.readBytes)
	sample.WriteBytes = counterDelta(after.writeBytes, before.writeBytes)
	sample.MajorFaults = counterDelta(after.majorFaults, before.majorFaults)
	sample.FullCopyObserved = observedFullMemoryCopy(cacheState, memoryFileBytes, sample)
	return sample, nil
}

func observedFullMemoryCopy(
	cacheState string,
	memoryFileBytes int64,
	sample snapshotLoadQualificationSample,
) bool {
	fullCopyThreshold := uint64(memoryFileBytes) * 8 / 10
	if sample.ReadCharacters >= fullCopyThreshold {
		return true
	}
	return cacheState == "warm" && sample.ReadBytes >= fullCopyThreshold
}

func requiredSnapshotMemoryShapes(t *testing.T) []int {
	t.Helper()
	raw := requiredEnv(t, "SECONDBOX_SNAPSHOT_QUALIFICATION_MEMORY_MIB")
	seen := map[int]struct{}{}
	var shapes []int
	for _, field := range strings.Split(raw, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || value <= 0 {
			t.Fatalf("SECONDBOX_SNAPSHOT_QUALIFICATION_MEMORY_MIB contains invalid value %q", field)
		}
		if _, exists := seen[value]; exists {
			t.Fatalf("SECONDBOX_SNAPSHOT_QUALIFICATION_MEMORY_MIB repeats %d", value)
		}
		seen[value] = struct{}{}
		shapes = append(shapes, value)
	}
	if len(shapes) == 0 {
		t.Fatal("SECONDBOX_SNAPSHOT_QUALIFICATION_MEMORY_MIB must contain a shape")
	}
	sort.Ints(shapes)
	return shapes
}

func requiredEnvBool(t *testing.T, name string) bool {
	t.Helper()
	value, err := strconv.ParseBool(requiredEnv(t, name))
	if err != nil {
		t.Fatalf("%s must be true or false", name)
	}
	return value
}

func firecrackerQualificationVersion(t *testing.T) string {
	t.Helper()
	path := requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH")
	output, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("read qualification Firecracker version: %v: %s", err, strings.TrimSpace(string(output)))
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Firecracker v") {
			return line
		}
	}
	t.Fatalf("qualification Firecracker returned no version line: %s", strings.TrimSpace(string(output)))
	return ""
}

func evictSnapshotFileCache(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_DONTNEED)
}

func waitForSnapshotControl(ctx context.Context, client ControlClient, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	lastHealthy := false
	for {
		heartbeat, err := client.Heartbeat(ctx)
		lastErr = err
		lastHealthy = heartbeat.Healthy
		if err == nil && heartbeat.Healthy {
			return nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("timed out waiting for restored guest control: %w", lastErr)
			}
			return fmt.Errorf("timed out waiting for restored guest control: healthy=%t", lastHealthy)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func readSnapshotProcessCounters(pid int) (snapshotProcessCounters, error) {
	var counters snapshotProcessCounters
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "io"))
	if err != nil {
		return counters, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return counters, err
		}
		switch fields[0] {
		case "rchar:":
			counters.readCharacters = value
		case "wchar:":
			counters.writeCharacters = value
		case "read_bytes:":
			counters.readBytes = value
		case "write_bytes:":
			counters.writeBytes = value
		}
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return counters, err
	}
	closingParenthesis := strings.LastIndexByte(string(stat), ')')
	if closingParenthesis < 0 {
		return counters, fmt.Errorf("process stat lacks command terminator")
	}
	fields := strings.Fields(string(stat[closingParenthesis+1:]))
	if len(fields) <= 9 {
		return counters, fmt.Errorf("process stat has %d fields after command", len(fields))
	}
	counters.majorFaults, err = strconv.ParseUint(fields[9], 10, 64)
	if err != nil {
		return counters, err
	}
	return counters, nil
}

func counterDelta(after uint64, before uint64) uint64 {
	if after < before {
		return 0
	}
	return after - before
}

func durationPercentile(values []int64, percentile int) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i int, j int) bool { return sorted[i] < sorted[j] })
	index := (len(sorted)*percentile + 99) / 100
	if index < 1 {
		index = 1
	}
	if index > len(sorted) {
		index = len(sorted)
	}
	return sorted[index-1]
}

func TestDurationPercentileNearestRank(t *testing.T) {
	tests := []struct {
		name       string
		values     []int64
		percentile int
		want       int64
	}{
		{name: "empty", percentile: 95, want: 0},
		{name: "median odd", values: []int64{30, 10, 20}, percentile: 50, want: 20},
		{name: "p95 small sample is maximum", values: []int64{5, 1, 4, 2, 3}, percentile: 95, want: 5},
		{name: "does not mutate", values: []int64{3, 1, 2}, percentile: 50, want: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := append([]int64(nil), test.values...)
			if got := durationPercentile(test.values, test.percentile); got != test.want {
				t.Fatalf("durationPercentile(%v, %d) = %d, want %d", test.values, test.percentile, got, test.want)
			}
			for index := range before {
				if test.values[index] != before[index] {
					t.Fatalf("durationPercentile mutated input: got %v want %v", test.values, before)
				}
			}
		})
	}
}

func TestReadSnapshotProcessCountersSelf(t *testing.T) {
	counters, err := readSnapshotProcessCounters(os.Getpid())
	if err != nil {
		t.Fatalf("read current process counters: %v", err)
	}
	if counters.readCharacters == 0 {
		t.Fatal("current process read-character counter is zero")
	}
}

func TestObservedFullMemoryCopy(t *testing.T) {
	const memoryBytes = int64(100)
	tests := []struct {
		name       string
		cacheState string
		sample     snapshotLoadQualificationSample
		want       bool
	}{
		{name: "explicit reads", cacheState: "cold", sample: snapshotLoadQualificationSample{ReadCharacters: 80}, want: true},
		{name: "cold demand faults", cacheState: "cold", sample: snapshotLoadQualificationSample{ReadBytes: 100}, want: false},
		{name: "warm block reads", cacheState: "warm", sample: snapshotLoadQualificationSample{ReadBytes: 80}, want: true},
		{name: "small warm working set", cacheState: "warm", sample: snapshotLoadQualificationSample{ReadBytes: 79}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := observedFullMemoryCopy(test.cacheState, memoryBytes, test.sample); got != test.want {
				t.Fatalf("observedFullMemoryCopy() = %t, want %t", got, test.want)
			}
		})
	}
}

func writeSnapshotLoadQualificationReport(path string, report snapshotLoadQualificationReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
