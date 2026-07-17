package microvm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agentcy/internal/config"
	"agentcy/internal/harness"
	"agentcy/internal/runtimemanager"
	"golang.org/x/sys/unix"
)

const privilegedLauncherProtocolVersion = 1

const maxPrivilegedLauncherMessageBytes = 1 << 20
const maxPrivilegedLauncherResponseBytes = 4 << 20

const launcherSourceGuardTable = "agentcy_source_guard"

var launcherInstanceIDPattern = regexp.MustCompile(`^fc-[A-Za-z0-9][A-Za-z0-9_-]{2,180}$`)
var launcherPathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,180}$`)
var launcherHarnessCellIDPattern = regexp.MustCompile(`^hcell-[A-Za-z0-9][A-Za-z0-9_-]{2,90}$`)
var launcherEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type privilegedLauncherRequest struct {
	Version        int                              `json:"version"`
	Op             string                           `json:"op"`
	Launch         *privilegedLaunchRequest         `json:"launch,omitempty"`
	Tap            *TapConfig                       `json:"tap,omitempty"`
	Route          *TransparentRoute                `json:"route,omitempty"`
	HarnessPrepare *privilegedHarnessPrepareRequest `json:"harnessPrepare,omitempty"`
	HarnessExec    *privilegedHarnessExecRequest    `json:"harnessExec,omitempty"`
	ID             string                           `json:"id,omitempty"`
	TapName        string                           `json:"tapName,omitempty"`
}

type privilegedLaunchRequest struct {
	InstanceID    string                               `json:"instanceId"`
	AgentID       string                               `json:"agentId"`
	CompartmentID string                               `json:"compartmentId"`
	RootfsPath    string                               `json:"rootfsPath"`
	RootfsImage   string                               `json:"rootfsImage"`
	WorkspacePath string                               `json:"workspacePath"`
	SharedImage   string                               `json:"sharedImage,omitempty"`
	TapName       string                               `json:"tapName,omitempty"`
	GuestIP       string                               `json:"guestIp,omitempty"`
	SandboxPolicy *runtimemanager.SandboxRuntimePolicy `json:"sandboxPolicy,omitempty"`
}

type privilegedLauncherResponse struct {
	OK               bool                  `json:"ok"`
	Error            string                `json:"error,omitempty"`
	SocketPath       string                `json:"socketPath,omitempty"`
	VsockPath        string                `json:"vsockPath,omitempty"`
	JailRoot         string                `json:"jailRoot,omitempty"`
	LogPath          string                `json:"logPath,omitempty"`
	ResultPath       string                `json:"resultPath,omitempty"`
	Output           string                `json:"output,omitempty"`
	Running          bool                  `json:"running,omitempty"`
	ExecutionStarted bool                  `json:"executionStarted,omitempty"`
	ExitCode         int                   `json:"exitCode,omitempty"`
	Version          string                `json:"version,omitempty"`
	NetworkPosture   *NetworkPostureReport `json:"networkPosture,omitempty"`
}

type privilegedHarnessPrepareRequest struct {
	CellID    string                   `json:"cellId"`
	Namespace harness.NetworkNamespace `json:"namespace"`
}

type privilegedHarnessExecRequest struct {
	CellID      string                   `json:"cellId"`
	Namespace   harness.NetworkNamespace `json:"namespace"`
	Command     string                   `json:"command"`
	Args        []string                 `json:"args"`
	Env         []string                 `json:"env"`
	SeccompBPF  []byte                   `json:"seccompBpf"`
	MemoryBytes int64                    `json:"memoryBytes"`
	NanoCPUs    int64                    `json:"nanoCpus"`
	PidsLimit   int64                    `json:"pidsLimit"`
	MaxRuntime  int64                    `json:"maxRuntimeMillis"`
	IdleTimeout int64                    `json:"idleTimeoutMillis"`
}

type privilegedLauncherClient struct {
	socketPath string
}

func newPrivilegedLauncherClient(socketPath string) *privilegedLauncherClient {
	return &privilegedLauncherClient{socketPath: strings.TrimSpace(socketPath)}
}

func (c *privilegedLauncherClient) call(ctx context.Context, req privilegedLauncherRequest) (privilegedLauncherResponse, error) {
	if c == nil || strings.TrimSpace(c.socketPath) == "" {
		return privilegedLauncherResponse{}, fmt.Errorf("privileged launcher socket is not configured")
	}
	req.Version = privilegedLauncherProtocolVersion
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return privilegedLauncherResponse{}, fmt.Errorf("connect privileged launcher %s: %w", c.socketPath, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(req); err != nil {
		return privilegedLauncherResponse{}, fmt.Errorf("send privileged launcher request: %w", err)
	}
	var resp privilegedLauncherResponse
	decoder := json.NewDecoder(io.LimitReader(conn, maxPrivilegedLauncherResponseBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&resp); err != nil {
		return privilegedLauncherResponse{}, fmt.Errorf("read privileged launcher response: %w", err)
	}
	if !resp.OK {
		return resp, fmt.Errorf("privileged launcher %s: %s", req.Op, strings.TrimSpace(resp.Error))
	}
	return resp, nil
}

func (c *privilegedLauncherClient) Ping(ctx context.Context) error {
	resp, err := c.call(ctx, privilegedLauncherRequest{Op: "ping"})
	if err != nil {
		return err
	}
	if resp.Version != expectedFirecrackerVersionString() {
		return fmt.Errorf("privileged launcher firecracker version %q does not match %q", resp.Version, expectedFirecrackerVersionString())
	}
	if resp.NetworkPosture == nil {
		return fmt.Errorf("privileged launcher response is missing the required network posture report")
	}
	if err := resp.NetworkPosture.admissionError(); err != nil {
		return err
	}
	return nil
}

func (c *privilegedLauncherClient) Launch(ctx context.Context, req privilegedLaunchRequest) (privilegedLauncherResponse, error) {
	return c.call(ctx, privilegedLauncherRequest{Op: "launch", Launch: &req})
}

func (c *privilegedLauncherClient) Stop(ctx context.Context, instanceID string) error {
	_, err := c.call(ctx, privilegedLauncherRequest{Op: "stop", ID: instanceID})
	return err
}

func (c *privilegedLauncherClient) Running(ctx context.Context, instanceID string) (bool, error) {
	resp, err := c.call(ctx, privilegedLauncherRequest{Op: "running", ID: instanceID})
	return resp.Running, err
}

func (c *privilegedLauncherClient) ConfigureTap(ctx context.Context, cfg TapConfig) error {
	_, err := c.call(ctx, privilegedLauncherRequest{Op: "configure_tap", Tap: &cfg})
	return err
}

func (c *privilegedLauncherClient) RemoveTap(ctx context.Context, tapName string) error {
	_, err := c.call(ctx, privilegedLauncherRequest{Op: "remove_tap", TapName: tapName})
	return err
}

func (c *privilegedLauncherClient) RegisterTransparentRoute(ctx context.Context, route TransparentRoute) error {
	_, err := c.call(ctx, privilegedLauncherRequest{Op: "register_route", Route: &route})
	return err
}

func (c *privilegedLauncherClient) UnregisterContainer(instanceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.call(ctx, privilegedLauncherRequest{Op: "unregister_route", ID: instanceID}); err != nil {
		slog.Warn("privileged launcher retained transparent route cleanup state", "instance", instanceID, "error", err)
	}
}

type privilegedHarnessNetworkExecutor struct {
	client *privilegedLauncherClient
}

// NewPrivilegedHarnessNetworkExecutor returns the typed host-harness namespace
// client backed by the same peer-credentialed launcher used for Firecracker.
func NewPrivilegedHarnessNetworkExecutor(socketPath string) harness.PrivilegedNetworkExecutor {
	return &privilegedHarnessNetworkExecutor{client: newPrivilegedLauncherClient(socketPath)}
}

func (e *privilegedHarnessNetworkExecutor) Prepare(ctx context.Context, cellID string, namespace harness.NetworkNamespace) (string, error) {
	resp, err := e.client.call(ctx, privilegedLauncherRequest{
		Op: "prepare_harness_netns",
		HarnessPrepare: &privilegedHarnessPrepareRequest{
			CellID:    cellID,
			Namespace: namespace,
		},
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.ResultPath) == "" {
		return "", fmt.Errorf("privileged launcher omitted harness result path")
	}
	return resp.ResultPath, nil
}

func (e *privilegedHarnessNetworkExecutor) Execute(ctx context.Context, req harness.PrivilegedNetworkExecutionRequest) (harness.PrivilegedNetworkExecutionResult, error) {
	if len(req.Command.ExtraFiles) != 1 || strings.TrimSpace(req.Command.SeccompProfilePath) == "" {
		return harness.PrivilegedNetworkExecutionResult{}, fmt.Errorf("privileged harness execution requires exactly one seccomp profile")
	}
	seccompBPF, err := os.ReadFile(req.Command.SeccompProfilePath)
	if err != nil {
		return harness.PrivilegedNetworkExecutionResult{}, fmt.Errorf("read harness seccomp profile: %w", err)
	}
	resp, callErr := e.client.call(ctx, privilegedLauncherRequest{
		Op: "execute_harness_netns",
		HarnessExec: &privilegedHarnessExecRequest{
			CellID:      req.CellID,
			Namespace:   req.Namespace,
			Command:     req.Command.Command,
			Args:        append([]string(nil), req.Command.Args...),
			Env:         append([]string(nil), req.Command.Env...),
			SeccompBPF:  seccompBPF,
			MemoryBytes: req.Resources.MemoryBytes,
			NanoCPUs:    req.Resources.NanoCPUs,
			PidsLimit:   req.Resources.PidsLimit,
			MaxRuntime:  req.MaxRuntime.Milliseconds(),
			IdleTimeout: req.IdleTimeout.Milliseconds(),
		},
	})
	result := harness.PrivilegedNetworkExecutionResult{
		Started:  resp.ExecutionStarted,
		Output:   resp.Output,
		ExitCode: resp.ExitCode,
	}
	if callErr != nil && strings.TrimSpace(resp.Error) == "" {
		// Once the request is written, a transport loss cannot prove whether the
		// child crossed dispatch. Keep retry semantics conservative.
		result.Started = true
	}
	return result, callErr
}

func (e *privilegedHarnessNetworkExecutor) Remove(ctx context.Context, cellID string) error {
	_, err := e.client.call(ctx, privilegedLauncherRequest{Op: "remove_harness_netns", ID: cellID})
	return err
}

// PrivilegedLauncherConfig is loaded only by the root-owned launcher service.
// It contains no provider/database credentials and every filesystem/network
// value is treated as an allowlist, never as a request-controlled value.
type PrivilegedLauncherConfig struct {
	SocketPath             string
	SocketGID              int
	AllowedUID             int
	ManagerGID             int
	FirecrackerPath        string
	JailerPath             string
	ArtifactRoot           string
	KernelPath             string
	WorkspaceRoot          string
	RunRoot                string
	LogRoot                string
	JailRoot               string
	StateRoot              string
	BridgeName             string
	BridgeCIDR             string
	TapPrefix              string
	JailerUID              int
	JailerGID              int
	JailerCgroupVersion    int
	JailerParentCgroup     string
	MemoryMiB              int
	VCPUs                  int
	WorkspaceSizeMiB       int
	CPUTemplate            string
	KernelArgs             string
	TransparentHTTPPort    int
	HarnessCIDR            string
	HarnessProxyIP         string
	HarnessPlatformIP      string
	HarnessBubblewrap      string
	HarnessIPCommand       string
	HarnessSystemdRun      string
	HarnessSystemctl       string
	HarnessShell           string
	HarnessEnvCommand      string
	NftPath                string
	HarnessResultRoot      string
	HarnessMemoryBytes     int64
	HarnessNanoCPUs        int64
	HarnessPidsLimit       int64
	HarnessMaxRuntime      time.Duration
	HarnessIdleTimeout     time.Duration
	allowUnprivilegedTests bool
}

func LoadPrivilegedLauncherConfigFromEnv() (PrivilegedLauncherConfig, error) {
	intEnv := func(name string, fallback int) (int, error) {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			return fallback, nil
		}
		value, err := strconv.Atoi(raw)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer: %w", name, err)
		}
		return value, nil
	}
	allowedUID, err := intEnv("AG_VM_LAUNCHER_ALLOWED_UID", -1)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	socketGID, err := intEnv("AG_VM_LAUNCHER_SOCKET_GID", -1)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	managerGID, err := intEnv("AG_VM_LAUNCHER_MANAGER_GID", -1)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	jailerUID, err := intEnv("AG_MICROVM_JAILER_UID", -1)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	jailerGID, err := intEnv("AG_MICROVM_JAILER_GID", -1)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	cgroupVersion, err := intEnv("AG_MICROVM_JAILER_CGROUP_VERSION", 2)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	memoryMiB, err := intEnv("AG_MICROVM_MEMORY_MIB", 2048)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	vcpus, err := intEnv("AG_MICROVM_VCPUS", 2)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	workspaceSizeMiB, err := intEnv("AG_MICROVM_WORKSPACE_SIZE_MIB", 8192)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	transparentPort, err := intEnv("AG_EGRESS_PROXY_TRANSPARENT_HTTP_PORT", 0)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	harnessMemoryBytes, err := intEnv("AG_VM_LAUNCHER_HARNESS_MEMORY_BYTES", 2*1024*1024*1024)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	harnessNanoCPUs, err := intEnv("AG_VM_LAUNCHER_HARNESS_NANO_CPUS", 2_000_000_000)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	harnessPidsLimit, err := intEnv("AG_VM_LAUNCHER_HARNESS_PIDS_LIMIT", 256)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	harnessMaxRuntime, err := intEnv("AG_VM_LAUNCHER_HARNESS_MAX_RUNTIME_SECONDS", 600)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	harnessIdleTimeout, err := intEnv("AG_VM_LAUNCHER_HARNESS_IDLE_TIMEOUT_SECONDS", 300)
	if err != nil {
		return PrivilegedLauncherConfig{}, err
	}
	return PrivilegedLauncherConfig{
		SocketPath:          strings.TrimSpace(os.Getenv("AG_MICROVM_LAUNCHER_SOCKET")),
		SocketGID:           socketGID,
		AllowedUID:          allowedUID,
		ManagerGID:          managerGID,
		FirecrackerPath:     strings.TrimSpace(os.Getenv("AG_FIRECRACKER_PATH")),
		JailerPath:          strings.TrimSpace(os.Getenv("AG_FIRECRACKER_JAILER_PATH")),
		ArtifactRoot:        strings.TrimSpace(os.Getenv("AG_VM_LAUNCHER_ARTIFACT_ROOT")),
		KernelPath:          strings.TrimSpace(os.Getenv("AG_MICROVM_KERNEL_PATH")),
		WorkspaceRoot:       strings.TrimSpace(os.Getenv("AG_MICROVM_WORKSPACE_DIR")),
		RunRoot:             strings.TrimSpace(os.Getenv("AG_MICROVM_RUN_DIR")),
		LogRoot:             strings.TrimSpace(os.Getenv("AG_MICROVM_LOG_DIR")),
		JailRoot:            strings.TrimSpace(os.Getenv("AG_MICROVM_JAILER_CHROOT_BASE_DIR")),
		StateRoot:           strings.TrimSpace(os.Getenv("AG_VM_LAUNCHER_STATE_DIR")),
		BridgeName:          strings.TrimSpace(os.Getenv("AG_MICROVM_BRIDGE_NAME")),
		BridgeCIDR:          strings.TrimSpace(os.Getenv("AG_MICROVM_BRIDGE_CIDR")),
		TapPrefix:           strings.TrimSpace(os.Getenv("AG_MICROVM_TAP_PREFIX")),
		JailerUID:           jailerUID,
		JailerGID:           jailerGID,
		JailerCgroupVersion: cgroupVersion,
		JailerParentCgroup:  strings.TrimSpace(os.Getenv("AG_MICROVM_JAILER_PARENT_CGROUP")),
		MemoryMiB:           memoryMiB,
		VCPUs:               vcpus,
		WorkspaceSizeMiB:    workspaceSizeMiB,
		CPUTemplate:         normalizeLauncherCPUTemplate(os.Getenv("AG_MICROVM_CPU_TEMPLATE")),
		KernelArgs:          strings.TrimSpace(os.Getenv("AG_MICROVM_KERNEL_ARGS")),
		TransparentHTTPPort: transparentPort,
		HarnessCIDR:         strings.TrimSpace(os.Getenv("AG_HARNESS_NETNS_CIDR")),
		HarnessProxyIP:      strings.TrimSpace(os.Getenv("AG_VM_LAUNCHER_HARNESS_PROXY_IP")),
		HarnessPlatformIP:   strings.TrimSpace(os.Getenv("AG_VM_LAUNCHER_HARNESS_PLATFORM_IP")),
		HarnessBubblewrap:   strings.TrimSpace(envOr("AG_VM_LAUNCHER_HARNESS_BWRAP_PATH", "/usr/bin/bwrap")),
		HarnessIPCommand:    strings.TrimSpace(envOr("AG_VM_LAUNCHER_HARNESS_IP_PATH", "/usr/sbin/ip")),
		HarnessSystemdRun:   strings.TrimSpace(envOr("AG_VM_LAUNCHER_HARNESS_SYSTEMD_RUN_PATH", "/usr/bin/systemd-run")),
		HarnessSystemctl:    strings.TrimSpace(envOr("AG_VM_LAUNCHER_HARNESS_SYSTEMCTL_PATH", "/usr/bin/systemctl")),
		HarnessShell:        strings.TrimSpace(envOr("AG_VM_LAUNCHER_HARNESS_SHELL_PATH", "/bin/sh")),
		HarnessEnvCommand:   strings.TrimSpace(envOr("AG_VM_LAUNCHER_HARNESS_ENV_PATH", "/usr/bin/env")),
		NftPath:             strings.TrimSpace(envOr("AG_VM_LAUNCHER_NFT_PATH", "/usr/sbin/nft")),
		HarnessResultRoot:   strings.TrimSpace(os.Getenv("AG_VM_LAUNCHER_HARNESS_RESULT_DIR")),
		HarnessMemoryBytes:  int64(harnessMemoryBytes),
		HarnessNanoCPUs:     int64(harnessNanoCPUs),
		HarnessPidsLimit:    int64(harnessPidsLimit),
		HarnessMaxRuntime:   time.Duration(harnessMaxRuntime) * time.Second,
		HarnessIdleTimeout:  time.Duration(harnessIdleTimeout) * time.Second,
	}, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func normalizeLauncherCPUTemplate(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "none") || value == "" {
		return "None"
	}
	return value
}

func launcherRootUID(cfg PrivilegedLauncherConfig) int {
	if cfg.allowUnprivilegedTests {
		return os.Geteuid()
	}
	return 0
}

func ensureTrustedLauncherResultRoot(cfg PrivilegedLauncherConfig) error {
	parent := filepath.Dir(cfg.HarnessResultRoot)
	name := filepath.Base(cfg.HarnessResultRoot)
	relParent, err := filepath.Rel(string(os.PathSeparator), parent)
	if err != nil || relParent == "." || strings.HasPrefix(relParent, ".."+string(os.PathSeparator)) || name == "." {
		return fmt.Errorf("launcher harness result root is invalid")
	}
	rootFD, err := unix.Open(string(os.PathSeparator), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open filesystem root for launcher runtime: %w", err)
	}
	defer unix.Close(rootFD)
	parentFD, err := unix.Openat2(rootFD, relParent, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return fmt.Errorf("open trusted launcher runtime directory: %w", err)
	}
	defer unix.Close(parentFD)
	var parentStat unix.Stat_t
	if err := unix.Fstat(parentFD, &parentStat); err != nil {
		return fmt.Errorf("inspect launcher runtime directory: %w", err)
	}
	if parentStat.Mode&unix.S_IFMT != unix.S_IFDIR || int(parentStat.Uid) != launcherRootUID(cfg) || parentStat.Mode&0o022 != 0 {
		return fmt.Errorf("launcher runtime directory must be owned by the launcher identity and not group/world writable")
	}

	created := false
	if err := unix.Mkdirat(parentFD, name, 0o750); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("create launcher harness result root: %w", err)
		}
	} else {
		created = true
	}
	resultFD, err := unix.Openat2(parentFD, name, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		if created {
			_ = unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
		}
		return fmt.Errorf("open launcher harness result root without symlinks: %w", err)
	}
	defer unix.Close(resultFD)
	if created {
		if err := unix.Fchown(resultFD, launcherRootUID(cfg), cfg.ManagerGID); err != nil {
			_ = unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
			return fmt.Errorf("chown launcher harness result root: %w", err)
		}
		if err := unix.Fchmod(resultFD, 0o750); err != nil {
			_ = unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
			return fmt.Errorf("chmod launcher harness result root: %w", err)
		}
	}
	var resultStat unix.Stat_t
	if err := unix.Fstat(resultFD, &resultStat); err != nil {
		return fmt.Errorf("inspect launcher harness result root: %w", err)
	}
	if resultStat.Mode&unix.S_IFMT != unix.S_IFDIR || int(resultStat.Uid) != launcherRootUID(cfg) || int(resultStat.Gid) != cfg.ManagerGID || resultStat.Mode&0o777 != 0o750 {
		return fmt.Errorf("launcher harness result root ownership or mode does not match policy")
	}
	return nil
}

type privilegedLauncherState struct {
	InstanceID  string            `json:"instanceId"`
	TapName     string            `json:"tapName,omitempty"`
	GuestIP     string            `json:"guestIp,omitempty"`
	GuestMAC    string            `json:"guestMac,omitempty"`
	SourceGuard bool              `json:"sourceGuard"`
	Started     bool              `json:"started"`
	Route       *TransparentRoute `json:"route,omitempty"`
}

type privilegedHarnessState struct {
	CellID      string                   `json:"cellId"`
	Namespace   harness.NetworkNamespace `json:"namespace"`
	ResultPath  string                   `json:"resultPath"`
	SeccompPath string                   `json:"seccompPath"`
	UnitName    string                   `json:"unitName"`
	GuestMAC    string                   `json:"guestMac,omitempty"`
	SourceGuard string                   `json:"sourceGuard,omitempty"`
	Executing   bool                     `json:"executing"`
}

type PrivilegedLauncherServer struct {
	cfg             PrivilegedLauncherConfig
	mu              sync.Mutex
	network         IPTapConfigurer
	router          *IPTablesEgressRouter
	runHost         commandRunner
	postureFailures map[string]uint64
}

func NewPrivilegedLauncherServer(cfg PrivilegedLauncherConfig) (*PrivilegedLauncherServer, error) {
	if os.Geteuid() != 0 && !cfg.allowUnprivilegedTests {
		return nil, fmt.Errorf("privileged launcher server must run as root")
	}
	if err := validatePrivilegedLauncherConfig(&cfg); err != nil {
		return nil, err
	}
	server := &PrivilegedLauncherServer{
		cfg:             cfg,
		network:         IPTapConfigurer{},
		router:          NewIPTablesEgressRouter(),
		postureFailures: map[string]uint64{},
		runHost: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}
	if err := os.MkdirAll(cfg.StateRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create launcher state root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateRoot, "harness"), 0o700); err != nil {
		return nil, fmt.Errorf("create launcher harness state root: %w", err)
	}
	if err := ensureTrustedLauncherResultRoot(cfg); err != nil {
		return nil, err
	}
	if !cfg.allowUnprivilegedTests {
		liveIDs, err := liveLauncherInstanceIDs()
		if err != nil {
			return nil, fmt.Errorf("enumerate live launcher processes: %w", err)
		}
		if err := server.reconcileLauncherProcessState(liveIDs, killUntrackedLauncherProcess); err != nil {
			return nil, err
		}
	}
	if err := server.restoreTapSourceGuards(context.Background()); err != nil {
		return nil, err
	}
	if err := server.restoreRouteState(); err != nil {
		return nil, err
	}
	if err := server.recoverHarnessNetworkState(context.Background()); err != nil {
		return nil, err
	}
	return server, nil
}

func (s *PrivilegedLauncherServer) restoreRouteState() error {
	entries, err := os.ReadDir(s.cfg.StateRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		state, err := s.readState(id)
		if err != nil {
			return fmt.Errorf("restore launcher state %s: %w", entry.Name(), err)
		}
		if state.Route != nil {
			if !state.SourceGuard {
				return fmt.Errorf("restore launcher state %s: transparent route has no active source guard", entry.Name())
			}
			s.router.routes[id] = *state.Route
		}
	}
	return nil
}

func (s *PrivilegedLauncherServer) restoreTapSourceGuards(ctx context.Context) error {
	entries, err := os.ReadDir(s.cfg.StateRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		instanceID := strings.TrimSuffix(entry.Name(), ".json")
		state, err := s.readState(instanceID)
		if err != nil {
			return err
		}
		if state.TapName == "" {
			continue
		}
		wantMAC := guestMACForInstance(state.TapName)
		if !ipWithinCIDR(state.GuestIP, s.cfg.BridgeCIDR) || state.GuestMAC != wantMAC {
			if state.Started || state.Route != nil {
				return fmt.Errorf("launcher state %s lacks a valid guarded source identity", instanceID)
			}
			state.SourceGuard = false
			if err := s.writeState(state); err != nil {
				return err
			}
			continue
		}
		if out, err := s.hostCommand(ctx, s.cfg.HarnessIPCommand, "link", "show", state.TapName); err != nil {
			if state.Started || !missingNetworkResource(out) {
				return fmt.Errorf("inspect guarded tap %s: %w: %s", state.TapName, err, strings.TrimSpace(string(out)))
			}
			if err := s.cleanupMissingTapState(ctx, state); err != nil {
				return fmt.Errorf("reconcile missing tap %s: %w", state.TapName, err)
			}
			continue
		}
		if err := s.installSourceGuard(ctx, launcherSourceGuardChain(instanceID), state.TapName, state.GuestIP, state.GuestMAC); err != nil {
			return fmt.Errorf("restore source guard for %s: %w", instanceID, err)
		}
		state.SourceGuard = true
		if err := s.writeState(state); err != nil {
			return err
		}
	}
	return nil
}

func (s *PrivilegedLauncherServer) cleanupMissingTapState(ctx context.Context, state privilegedLauncherState) error {
	if state.Route != nil {
		s.router.routes[state.InstanceID] = *state.Route
		if err := s.router.UnregisterContainerContext(ctx, state.InstanceID); err != nil {
			return err
		}
		state.Route = nil
		if err := s.writeState(state); err != nil {
			return err
		}
	}
	if err := s.removeSourceGuard(ctx, launcherSourceGuardChain(state.InstanceID)); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Dir(s.jailerRoot(state.InstanceID))); err != nil {
		return err
	}
	if err := s.removeManagerSocketAliases(state.InstanceID); err != nil {
		return err
	}
	return removeLauncherStatePath(s.cfg.StateRoot, s.statePath(state.InstanceID))
}

func killUntrackedLauncherProcess(instanceID string) error {
	if err := signalFirecrackerByID(instanceID, syscall.SIGKILL); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		running, err := firecrackerProcessRunning(instanceID)
		if err != nil {
			return err
		}
		if !running {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("untracked launcher process remained alive after SIGKILL")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func liveLauncherInstanceIDs() (map[string]struct{}, error) {
	ids := make(map[string]struct{})
	_, err := firecrackerPIDsMatching(func(instanceID string) bool {
		if launcherInstanceIDPattern.MatchString(instanceID) {
			ids[instanceID] = struct{}{}
			return true
		}
		return false
	})
	return ids, err
}

func (s *PrivilegedLauncherServer) reconcileLauncherProcessState(liveIDs map[string]struct{}, killUntracked func(string) error) error {
	entries, err := os.ReadDir(s.cfg.StateRoot)
	if err != nil {
		return fmt.Errorf("read launcher state for process recovery: %w", err)
	}
	tracked := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		instanceID := strings.TrimSuffix(entry.Name(), ".json")
		state, err := s.readState(instanceID)
		if err != nil {
			return fmt.Errorf("recover launcher process state %s: %w", entry.Name(), err)
		}
		tracked[instanceID] = struct{}{}
		_, live := liveIDs[instanceID]
		if state.Started == live {
			continue
		}
		state.Started = live
		if err := s.writeState(state); err != nil {
			return fmt.Errorf("reconcile launcher process state %s: %w", instanceID, err)
		}
	}
	for instanceID := range liveIDs {
		if _, ok := tracked[instanceID]; ok {
			continue
		}
		if killUntracked == nil {
			return fmt.Errorf("live launcher process %s has no authoritative state", instanceID)
		}
		if err := killUntracked(instanceID); err != nil {
			return fmt.Errorf("kill untracked launcher process %s: %w", instanceID, err)
		}
	}
	return nil
}

func validatePrivilegedLauncherConfig(cfg *PrivilegedLauncherConfig) error {
	if cfg == nil {
		return fmt.Errorf("privileged launcher config is required")
	}
	if cfg.AllowedUID <= 0 {
		return fmt.Errorf("AG_VM_LAUNCHER_ALLOWED_UID must select a non-root identity")
	}
	if cfg.SocketGID <= 0 {
		return fmt.Errorf("AG_VM_LAUNCHER_SOCKET_GID must select a non-root group")
	}
	if cfg.ManagerGID <= 0 {
		return fmt.Errorf("AG_VM_LAUNCHER_MANAGER_GID must select a non-root group")
	}
	if cfg.JailerUID <= 0 || cfg.JailerGID <= 0 {
		return fmt.Errorf("AG_MICROVM_JAILER_UID/GID must select a non-root identity")
	}
	if cfg.MemoryMiB <= 0 || cfg.VCPUs <= 0 || cfg.WorkspaceSizeMiB <= 0 {
		return fmt.Errorf("microVM memory, vCPUs, and workspace size must be positive")
	}
	if cfg.TransparentHTTPPort < 0 || cfg.TransparentHTTPPort > 65535 {
		return fmt.Errorf("transparent HTTP port is invalid")
	}
	for name, value := range map[string]*string{
		"socket":          &cfg.SocketPath,
		"artifact root":   &cfg.ArtifactRoot,
		"kernel":          &cfg.KernelPath,
		"workspace root":  &cfg.WorkspaceRoot,
		"run root":        &cfg.RunRoot,
		"log root":        &cfg.LogRoot,
		"jail root":       &cfg.JailRoot,
		"state root":      &cfg.StateRoot,
		"harness results": &cfg.HarnessResultRoot,
	} {
		trimmed := strings.TrimSpace(*value)
		if trimmed == "" || !filepath.IsAbs(trimmed) {
			return fmt.Errorf("privileged launcher %s must be an absolute path", name)
		}
		*value = filepath.Clean(trimmed)
	}
	launcherRuntimeRoot := filepath.Dir(cfg.SocketPath)
	if filepath.Dir(cfg.HarnessResultRoot) != launcherRuntimeRoot {
		return fmt.Errorf("launcher harness result root must be an immediate child of the launcher runtime directory")
	}
	for name, value := range map[string]string{"firecracker": cfg.FirecrackerPath, "jailer": cfg.JailerPath} {
		if err := requireExecutable(name, value); err != nil {
			return err
		}
	}
	if err := ensureFirecrackerVersion(cfg.FirecrackerPath); err != nil {
		return err
	}
	if _, _, err := net.ParseCIDR(cfg.BridgeCIDR); err != nil {
		return fmt.Errorf("launcher bridge CIDR: %w", err)
	}
	if strings.TrimSpace(cfg.BridgeName) == "" {
		return fmt.Errorf("launcher bridge name is required")
	}
	if strings.TrimSpace(cfg.TapPrefix) == "" {
		return fmt.Errorf("launcher tap prefix is required")
	}
	if _, err := harness.DeriveNetworkNamespace("hcell-launcher-validation", cfg.HarnessCIDR); err != nil {
		return fmt.Errorf("launcher harness CIDR: %w", err)
	}
	if proxyIP, platformIP := net.ParseIP(cfg.HarnessProxyIP), net.ParseIP(cfg.HarnessPlatformIP); proxyIP == nil || proxyIP.To4() == nil || platformIP == nil || platformIP.To4() == nil {
		return fmt.Errorf("launcher harness proxy and platform IPs must be valid IPv4 addresses")
	}
	for name, value := range map[string]string{
		"harness bubblewrap":  cfg.HarnessBubblewrap,
		"harness ip":          cfg.HarnessIPCommand,
		"harness systemd-run": cfg.HarnessSystemdRun,
		"harness systemctl":   cfg.HarnessSystemctl,
		"harness shell":       cfg.HarnessShell,
		"harness env":         cfg.HarnessEnvCommand,
		"nft":                 cfg.NftPath,
	} {
		if err := requireExecutable(name, value); err != nil {
			return err
		}
	}
	if cfg.HarnessMemoryBytes <= 0 || cfg.HarnessNanoCPUs <= 0 || cfg.HarnessPidsLimit <= 0 || cfg.HarnessMaxRuntime <= 0 || cfg.HarnessIdleTimeout <= 0 || cfg.HarnessIdleTimeout > cfg.HarnessMaxRuntime {
		return fmt.Errorf("launcher harness resource and timeout policy is invalid")
	}
	kernel, err := filepath.EvalSymlinks(cfg.KernelPath)
	if err != nil {
		return fmt.Errorf("resolve launcher kernel: %w", err)
	}
	if err := ensureContainedRegularFile(cfg.ArtifactRoot, kernel); err != nil {
		return fmt.Errorf("launcher kernel: %w", err)
	}
	cfg.KernelPath = kernel
	return nil
}

func (s *PrivilegedLauncherServer) Serve(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("privileged launcher server is required")
	}
	for _, dir := range []string{filepath.Dir(s.cfg.SocketPath), s.cfg.StateRoot, s.cfg.JailRoot, s.cfg.LogRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create privileged launcher directory %s: %w", dir, err)
		}
	}
	if err := rejectActiveUnixSocket(s.cfg.SocketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen on privileged launcher socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(s.cfg.SocketPath)
	if err := os.Chown(s.cfg.SocketPath, 0, s.cfg.SocketGID); err != nil {
		return fmt.Errorf("chown privileged launcher socket: %w", err)
	}
	if err := os.Chmod(s.cfg.SocketPath, 0o660); err != nil {
		return fmt.Errorf("chmod privileged launcher socket: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept privileged launcher request: %w", err)
		}
		go s.serveConn(conn)
	}
}

func rejectActiveUnixSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect privileged launcher socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket path %s", path)
	}
	conn, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("privileged launcher socket %s is already active", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale privileged launcher socket: %w", err)
	}
	return nil
}

func (s *PrivilegedLauncherServer) serveConn(conn net.Conn) {
	defer conn.Close()
	if err := s.authorizePeer(conn); err != nil {
		_ = json.NewEncoder(conn).Encode(privilegedLauncherResponse{Error: err.Error()})
		return
	}
	requestDeadline := s.cfg.HarnessMaxRuntime + 2*time.Minute
	if requestDeadline < 10*time.Minute {
		requestDeadline = 10 * time.Minute
	}
	_ = conn.SetDeadline(time.Now().Add(requestDeadline))
	reader := bufio.NewReader(io.LimitReader(conn, maxPrivilegedLauncherMessageBytes))
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var req privilegedLauncherRequest
	if err := decoder.Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(privilegedLauncherResponse{Error: "invalid request: " + err.Error()})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopDisconnectWatch := watchLauncherPeerDisconnect(conn, cancel)
	resp := s.handle(ctx, req)
	stopDisconnectWatch()
	cancel()
	_ = json.NewEncoder(conn).Encode(resp)
}

func watchLauncherPeerDisconnect(conn net.Conn, cancel context.CancelFunc) func() {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return func() {}
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return func() {}
	}
	fd := -1
	if err := raw.Control(func(value uintptr) { fd = int(value) }); err != nil || fd < 0 {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		pollFDs := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLHUP | unix.POLLRDHUP | unix.POLLERR}}
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, err := unix.Poll(pollFDs, 100)
			if err != nil && !errors.Is(err, unix.EINTR) {
				cancel()
				return
			}
			if pollFDs[0].Revents&(unix.POLLHUP|unix.POLLRDHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
				cancel()
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func (s *PrivilegedLauncherServer) authorizePeer(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("launcher accepts Unix peers only")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return fmt.Errorf("inspect launcher peer: %w", err)
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return fmt.Errorf("inspect launcher peer fd: %w", err)
	}
	if credErr != nil {
		return fmt.Errorf("read launcher peer credential: %w", credErr)
	}
	if cred == nil || int(cred.Uid) != s.cfg.AllowedUID {
		return fmt.Errorf("launcher peer uid is not authorized")
	}
	return nil
}

func (s *PrivilegedLauncherServer) handle(ctx context.Context, req privilegedLauncherRequest) privilegedLauncherResponse {
	if req.Version != privilegedLauncherProtocolVersion {
		return privilegedLauncherResponse{Error: "unsupported protocol version"}
	}
	if req.Op == "execute_harness_netns" {
		resp, err := s.executeHarnessNetwork(ctx, req.HarnessExec)
		if err != nil {
			resp.Error = err.Error()
			return resp
		}
		resp.OK = true
		return resp
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var resp privilegedLauncherResponse
	var err error
	switch req.Op {
	case "ping":
		resp.Version = expectedFirecrackerVersionString()
		posture := s.networkPosture(ctx)
		resp.NetworkPosture = &posture
	case "configure_tap":
		err = s.configureTap(ctx, req.Tap)
	case "remove_tap":
		err = s.removeTap(ctx, req.TapName)
	case "register_route":
		err = s.registerRoute(ctx, req.Route)
	case "unregister_route":
		err = s.unregisterRoute(ctx, req.ID)
	case "launch":
		resp, err = s.launch(ctx, req.Launch)
	case "stop":
		err = s.stop(ctx, req.ID)
	case "running":
		resp.Running, err = s.running(req.ID)
	case "prepare_harness_netns":
		resp.ResultPath, err = s.prepareHarnessNetwork(ctx, req.HarnessPrepare)
	case "remove_harness_netns":
		err = s.removeHarnessNetwork(ctx, req.ID)
	default:
		err = fmt.Errorf("unsupported operation")
	}
	if err != nil {
		resp.OK = false
		resp.Error = err.Error()
		return resp
	}
	resp.OK = true
	return resp
}

func (s *PrivilegedLauncherServer) configureTap(ctx context.Context, cfg *TapConfig) error {
	if err := s.requireNetworkPosture(ctx); err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("tap config is required")
	}
	if err := validateLauncherInstanceID(cfg.InstanceID); err != nil {
		return err
	}
	wantTap := tapNameForInstance(s.cfg.TapPrefix, cfg.InstanceID)
	wantMAC := guestMACForInstance(wantTap)
	if cfg.TapName != wantTap || cfg.BridgeName != s.cfg.BridgeName || cfg.BridgeCIDR != s.cfg.BridgeCIDR || cfg.OwnerUID != s.cfg.JailerUID || !ipWithinCIDR(cfg.GuestIP, s.cfg.BridgeCIDR) {
		return fmt.Errorf("tap request does not match launcher policy")
	}
	if err := s.ensureGuestIdentityAvailable(ctx, cfg.InstanceID, cfg.GuestIP, wantMAC); err != nil {
		return err
	}
	state, err := s.readState(cfg.InstanceID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if state.Started || (state.TapName != "" && state.TapName != wantTap) {
		return fmt.Errorf("instance already owns launcher resources")
	}
	state.InstanceID = cfg.InstanceID
	state.TapName = wantTap
	state.GuestIP = cfg.GuestIP
	state.GuestMAC = wantMAC
	state.SourceGuard = false
	if err := s.writeState(state); err != nil {
		return err
	}
	if err := s.network.ConfigureTap(ctx, *cfg); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cleanupErr := s.removeTap(cleanupCtx, wantTap); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("cleanup tracked tap after configuration failure: %w", cleanupErr))
		}
		return err
	}
	if err := s.installSourceGuard(ctx, launcherSourceGuardChain(cfg.InstanceID), wantTap, cfg.GuestIP, wantMAC); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cleanupErr := s.removeTap(cleanupCtx, wantTap); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("cleanup tracked tap after source-guard failure: %w", cleanupErr))
		}
		return err
	}
	state.SourceGuard = true
	if err := s.writeState(state); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cleanupErr := s.removeTap(cleanupCtx, wantTap); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("cleanup tap after source-guard state failure: %w", cleanupErr))
		}
		return err
	}
	return nil
}

func (s *PrivilegedLauncherServer) ensureGuestIdentityAvailable(ctx context.Context, instanceID, guestIP, guestMAC string) error {
	entries, err := os.ReadDir(s.cfg.StateRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		otherID := strings.TrimSuffix(entry.Name(), ".json")
		if otherID == instanceID {
			continue
		}
		state, err := s.readState(otherID)
		if err != nil {
			return err
		}
		if state.GuestIP != guestIP && state.GuestMAC != guestMAC {
			continue
		}
		running, err := firecrackerProcessRunningFunc(otherID)
		if err != nil {
			return fmt.Errorf("inspect guest source identity owner %s: %w", otherID, err)
		}
		if running {
			return fmt.Errorf("guest source identity is already owned by another launcher instance")
		}
		if state.Route != nil {
			if err := s.unregisterRoute(ctx, otherID); err != nil {
				return fmt.Errorf("reclaim stopped guest source identity owner %s route: %w", otherID, err)
			}
		}
		if err := s.removeTap(ctx, state.TapName); err != nil {
			return fmt.Errorf("reclaim stopped guest source identity owner %s: %w", otherID, err)
		}
	}
	return nil
}

func (s *PrivilegedLauncherServer) removeTap(ctx context.Context, tapName string) error {
	tapName = strings.TrimSpace(tapName)
	state, err := s.findStateByTap(tapName)
	if err != nil {
		return err
	}
	if state.Started {
		running, runErr := firecrackerProcessRunning(state.InstanceID)
		if runErr != nil {
			return runErr
		}
		if running {
			return fmt.Errorf("refusing to remove tap for a running instance")
		}
	}
	if state.Route != nil {
		return fmt.Errorf("refusing to remove tap with an active egress route")
	}
	if err := s.removeSourceGuard(ctx, launcherSourceGuardChain(state.InstanceID)); err != nil {
		return err
	}
	if err := s.network.RemoveTap(ctx, tapName); err != nil {
		return err
	}
	if err := removeLauncherStatePath(s.cfg.StateRoot, s.statePath(state.InstanceID)); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Dir(s.jailerRoot(state.InstanceID))); err != nil {
		return fmt.Errorf("remove launcher jail: %w", err)
	}
	if err := s.removeManagerSocketAliases(state.InstanceID); err != nil {
		return fmt.Errorf("remove manager socket aliases: %w", err)
	}
	return nil
}

func (s *PrivilegedLauncherServer) registerRoute(ctx context.Context, route *TransparentRoute) error {
	if route == nil {
		return fmt.Errorf("route is required")
	}
	if err := validateLauncherInstanceID(route.InstanceID); err != nil {
		return err
	}
	state, err := s.readState(route.InstanceID)
	if err != nil {
		return err
	}
	if !state.Started || !state.SourceGuard || state.TapName == "" || route.InterfaceID != state.TapName || route.SourceIP != state.GuestIP {
		return fmt.Errorf("route does not match launcher-owned instance identity")
	}
	if route.HTTPPort != s.cfg.TransparentHTTPPort || route.HTTPPort == 0 {
		return fmt.Errorf("route port does not match launcher policy")
	}
	if !ipWithinCIDR(route.SourceIP, s.cfg.BridgeCIDR) {
		return fmt.Errorf("route source IP is outside launcher bridge CIDR")
	}
	copy := *route
	state.Route = &copy
	if err := s.writeState(state); err != nil {
		return err
	}
	if err := s.router.RegisterTransparentRoute(ctx, *route); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cleanupErr := s.router.UnregisterContainerContext(cleanupCtx, route.InstanceID)
		if cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("cleanup tracked route after registration failure: %w", cleanupErr))
		}
		state.Route = nil
		if stateErr := s.writeState(state); stateErr != nil {
			return errors.Join(err, fmt.Errorf("persist cleaned route state: %w", stateErr))
		}
		return err
	}
	return nil
}

func (s *PrivilegedLauncherServer) unregisterRoute(ctx context.Context, instanceID string) error {
	if err := validateLauncherInstanceID(instanceID); err != nil {
		return err
	}
	state, err := s.readState(instanceID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.Route != nil {
		if err := s.router.UnregisterContainerContext(ctx, instanceID); err != nil {
			return err
		}
		state.Route = nil
		return s.writeState(state)
	}
	return nil
}

func (s *PrivilegedLauncherServer) prepareHarnessNetwork(ctx context.Context, req *privilegedHarnessPrepareRequest) (string, error) {
	if req == nil {
		return "", fmt.Errorf("harness network request is required")
	}
	namespace, err := s.validateHarnessNamespace(req.CellID, req.Namespace)
	if err != nil {
		return "", err
	}
	if _, err := s.readHarnessState(req.CellID); err == nil {
		return "", fmt.Errorf("harness cell already owns launcher resources")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	states, err := s.listHarnessStates()
	if err != nil {
		return "", err
	}
	for _, state := range states {
		if state.Namespace.NamespaceName == namespace.NamespaceName || state.Namespace.HostIP == namespace.HostIP || state.Namespace.GuestIP == namespace.GuestIP {
			return "", fmt.Errorf("derived harness network collides with active cell %q", state.CellID)
		}
	}
	resultPath := filepath.Join(s.cfg.HarnessResultRoot, req.CellID+".json")
	seccompPath := filepath.Join(s.cfg.HarnessResultRoot, req.CellID+".bpf")
	state := privilegedHarnessState{
		CellID:      req.CellID,
		Namespace:   namespace,
		ResultPath:  resultPath,
		SeccompPath: seccompPath,
		UnitName:    harnessUnitName(namespace),
		GuestMAC:    launcherSourceMAC(req.CellID),
		SourceGuard: launcherSourceGuardChain(req.CellID),
	}
	for _, path := range []string{resultPath, seccompPath} {
		if err := ensureContainedPath(s.cfg.HarnessResultRoot, path); err != nil {
			return "", err
		}
		if err := rejectExistingLauncherPath(path); err != nil {
			return "", err
		}
	}
	if err := s.writeHarnessState(state); err != nil {
		return "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			if err := s.cleanupHarnessNetwork(context.Background(), state); err == nil {
				_ = removeLauncherStatePath(filepath.Join(s.cfg.StateRoot, "harness"), s.harnessStatePath(state.CellID))
			}
		}
	}()
	for _, fileSpec := range []struct {
		path string
		mode os.FileMode
		uid  int
		gid  int
	}{{resultPath, 0o660, s.cfg.AllowedUID, s.cfg.ManagerGID}, {seccompPath, 0o640, launcherRootUID(s.cfg), s.cfg.ManagerGID}} {
		file, err := os.OpenFile(fileSpec.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, fileSpec.mode)
		if err != nil {
			return "", fmt.Errorf("create harness exchange path: %w", err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(fileSpec.path)
			return "", err
		}
		if err := os.Chown(fileSpec.path, fileSpec.uid, fileSpec.gid); err != nil {
			_ = os.Remove(fileSpec.path)
			return "", fmt.Errorf("chown harness exchange path: %w", err)
		}
		if err := os.Chmod(fileSpec.path, fileSpec.mode); err != nil {
			_ = os.Remove(fileSpec.path)
			return "", fmt.Errorf("chmod harness exchange path: %w", err)
		}
	}
	commands := [][]string{
		{"netns", "add", namespace.NamespaceName},
		{"link", "add", namespace.HostVethName, "type", "veth", "peer", "name", namespace.GuestVethName},
		{"addr", "add", fmt.Sprintf("%s/%d", namespace.HostIP, namespace.PrefixLen), "dev", namespace.HostVethName},
		{"link", "set", namespace.HostVethName, "up"},
		{"link", "set", namespace.GuestVethName, "address", state.GuestMAC},
		{"link", "set", namespace.GuestVethName, "netns", namespace.NamespaceName},
		{"netns", "exec", namespace.NamespaceName, s.cfg.HarnessIPCommand, "addr", "add", fmt.Sprintf("%s/%d", namespace.GuestIP, namespace.PrefixLen), "dev", namespace.GuestVethName},
		{"netns", "exec", namespace.NamespaceName, s.cfg.HarnessIPCommand, "link", "set", "lo", "up"},
		{"netns", "exec", namespace.NamespaceName, s.cfg.HarnessIPCommand, "link", "set", namespace.GuestVethName, "up"},
		{"netns", "exec", namespace.NamespaceName, s.cfg.HarnessIPCommand, "route", "replace", namespace.ProxyIP + "/32", "via", namespace.HostIP},
	}
	if namespace.PlatformIP != namespace.ProxyIP {
		commands = append(commands, []string{"netns", "exec", namespace.NamespaceName, s.cfg.HarnessIPCommand, "route", "replace", namespace.PlatformIP + "/32", "via", namespace.HostIP})
	}
	for _, args := range commands {
		if out, err := s.hostHarnessIPCommand(ctx, args...); err != nil {
			return "", fmt.Errorf("prepare harness network: ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	if err := s.installSourceGuard(ctx, state.SourceGuard, namespace.HostVethName, namespace.GuestIP, state.GuestMAC); err != nil {
		return "", fmt.Errorf("prepare harness source guard: %w", err)
	}
	cleanup = false
	return resultPath, nil
}

// hostHarnessIPCommand asks PID 1 to invoke ip in the host mount namespace.
// The launcher service has a private mount namespace due to systemd filesystem
// hardening; named network namespace bind mounts created directly by it are
// otherwise invisible to the system manager that starts the harness unit.
func (s *PrivilegedLauncherServer) hostHarnessIPCommand(ctx context.Context, args ...string) ([]byte, error) {
	systemdArgs := []string{
		"--quiet", "--wait", "--pipe", "--collect", "--service-type=exec",
		"--property", "NoNewPrivileges=yes",
		"--property", "CapabilityBoundingSet=CAP_NET_ADMIN CAP_SYS_ADMIN",
		"--property", "AmbientCapabilities=CAP_NET_ADMIN CAP_SYS_ADMIN",
		"--property", "RestrictAddressFamilies=AF_UNIX AF_NETLINK",
		"--property", "UMask=0077",
		"--", s.cfg.HarnessIPCommand,
	}
	systemdArgs = append(systemdArgs, args...)
	return s.hostCommand(ctx, s.cfg.HarnessSystemdRun, systemdArgs...)
}

func (s *PrivilegedLauncherServer) executeHarnessNetwork(ctx context.Context, req *privilegedHarnessExecRequest) (privilegedLauncherResponse, error) {
	var resp privilegedLauncherResponse
	if req == nil {
		return resp, fmt.Errorf("harness execution request is required")
	}
	if _, err := s.validateHarnessNamespace(req.CellID, req.Namespace); err != nil {
		return resp, err
	}
	if err := s.validateHarnessExecutionRequest(req); err != nil {
		return resp, err
	}
	s.mu.Lock()
	state, err := s.readHarnessState(req.CellID)
	if err != nil {
		s.mu.Unlock()
		return resp, err
	}
	if state.Namespace != req.Namespace || state.Executing {
		s.mu.Unlock()
		return resp, fmt.Errorf("harness execution does not match prepared launcher state")
	}
	if err := validateHarnessStateBoundExecution(state, req); err != nil {
		s.mu.Unlock()
		return resp, err
	}
	seccompFile, err := os.OpenFile(state.SeccompPath, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		s.mu.Unlock()
		return resp, fmt.Errorf("open harness seccomp policy: %w", err)
	}
	if _, err := seccompFile.Write(req.SeccompBPF); err != nil {
		_ = seccompFile.Close()
		s.mu.Unlock()
		return resp, fmt.Errorf("write harness seccomp policy: %w", err)
	}
	if err := seccompFile.Close(); err != nil {
		s.mu.Unlock()
		return resp, fmt.Errorf("close harness seccomp policy: %w", err)
	}
	state.Executing = true
	if err := s.writeHarnessState(state); err != nil {
		s.mu.Unlock()
		return resp, err
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		current, readErr := s.readHarnessState(req.CellID)
		if readErr == nil {
			current.Executing = false
			_ = s.writeHarnessState(current)
		}
	}()

	args := s.harnessSystemdRunArgs(state, req)
	cmd := exec.Command(s.cfg.HarnessSystemdRun, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	output := newLauncherOutputBuffer()
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return resp, fmt.Errorf("start transient harness unit: %w", err)
	}
	resp.ExecutionStarted = true
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	err = s.waitHarnessUnit(ctx, state.UnitName, cmd.Process.Pid, waitCh, output.Activity(), time.Duration(req.IdleTimeout)*time.Millisecond, time.Duration(req.MaxRuntime)*time.Millisecond)
	resp.Output = strings.TrimSpace(output.String())
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			resp.ExitCode = exitErr.ExitCode()
		}
		return resp, err
	}
	return resp, nil
}

func (s *PrivilegedLauncherServer) removeHarnessNetwork(ctx context.Context, cellID string) error {
	if err := validateHarnessCellID(cellID); err != nil {
		return err
	}
	state, err := s.readHarnessState(cellID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.Executing {
		return fmt.Errorf("harness cell is still executing")
	}
	if err := s.cleanupHarnessNetwork(ctx, state); err != nil {
		return err
	}
	return removeLauncherStatePath(filepath.Join(s.cfg.StateRoot, "harness"), s.harnessStatePath(cellID))
}

func (s *PrivilegedLauncherServer) cleanupHarnessNetwork(ctx context.Context, state privilegedHarnessState) error {
	var cleanupErr error
	if state.UnitName != "" {
		if out, err := s.hostCommand(ctx, s.cfg.HarnessSystemctl, "kill", "--signal=SIGKILL", "--kill-whom=all", state.UnitName); err != nil && !missingSystemdUnit(out) {
			return fmt.Errorf("kill transient harness unit before removing source guard: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if err := s.waitHarnessUnitStopped(ctx, state.UnitName); err != nil {
			return err
		}
		_, _ = s.hostCommand(ctx, s.cfg.HarnessSystemctl, "reset-failed", state.UnitName)
	}
	guardChain := state.SourceGuard
	if guardChain == "" && state.CellID != "" {
		guardChain = launcherSourceGuardChain(state.CellID)
	}
	if err := s.removeSourceGuard(ctx, guardChain); err != nil {
		cleanupErr = err
	}
	if state.Namespace.NamespaceName != "" {
		if out, err := s.hostHarnessIPCommand(ctx, "netns", "delete", state.Namespace.NamespaceName); err != nil && !missingNetworkResource(out) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete harness namespace: %w: %s", err, strings.TrimSpace(string(out))))
		}
	}
	if state.Namespace.HostVethName != "" {
		if out, err := s.hostHarnessIPCommand(ctx, "link", "delete", state.Namespace.HostVethName); err != nil && !missingNetworkResource(out) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete harness veth: %w: %s", err, strings.TrimSpace(string(out))))
		}
	}
	for _, path := range []string{state.ResultPath, state.SeccompPath} {
		if strings.TrimSpace(path) != "" {
			_ = os.Remove(path)
		}
	}
	return cleanupErr
}

func (s *PrivilegedLauncherServer) waitHarnessUnitStopped(ctx context.Context, unitName string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := s.hostCommand(ctx, s.cfg.HarnessSystemctl, "show", "--property=ActiveState", "--value", unitName)
		if err != nil {
			if missingSystemdUnit(out) {
				return nil
			}
			return fmt.Errorf("confirm transient harness unit stopped: %w: %s", err, strings.TrimSpace(string(out)))
		}
		state := strings.TrimSpace(string(out))
		if state == "" || state == "inactive" || state == "failed" || state == "dead" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("transient harness unit %s remained %s after SIGKILL", unitName, state)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *PrivilegedLauncherServer) validateHarnessNamespace(cellID string, requested harness.NetworkNamespace) (harness.NetworkNamespace, error) {
	if err := validateHarnessCellID(cellID); err != nil {
		return harness.NetworkNamespace{}, err
	}
	derived, err := harness.DeriveNetworkNamespace(cellID, s.cfg.HarnessCIDR)
	if err != nil {
		return harness.NetworkNamespace{}, err
	}
	derived.ProxyIP = s.cfg.HarnessProxyIP
	derived.PlatformIP = s.cfg.HarnessPlatformIP
	if *derived != requested {
		return harness.NetworkNamespace{}, fmt.Errorf("harness namespace request does not match launcher policy")
	}
	return *derived, nil
}

func (s *PrivilegedLauncherServer) validateHarnessExecutionRequest(req *privilegedHarnessExecRequest) error {
	if filepath.Clean(req.Command) != filepath.Clean(s.cfg.HarnessBubblewrap) {
		return fmt.Errorf("harness execution command does not match launcher policy")
	}
	if len(req.SeccompBPF) == 0 || len(req.SeccompBPF) > 64*1024 {
		return fmt.Errorf("harness seccomp policy size is invalid")
	}
	maxRuntime := time.Duration(req.MaxRuntime) * time.Millisecond
	idleTimeout := time.Duration(req.IdleTimeout) * time.Millisecond
	if req.MemoryBytes <= 0 || req.MemoryBytes > s.cfg.HarnessMemoryBytes || req.NanoCPUs <= 0 || req.NanoCPUs > s.cfg.HarnessNanoCPUs || req.PidsLimit <= 0 || req.PidsLimit > s.cfg.HarnessPidsLimit || maxRuntime <= 0 || maxRuntime > s.cfg.HarnessMaxRuntime || idleTimeout <= 0 || idleTimeout > s.cfg.HarnessIdleTimeout || idleTimeout > maxRuntime {
		return fmt.Errorf("harness execution resources exceed or violate launcher policy")
	}
	if len(req.Args) == 0 || len(req.Args) > 4096 || len(req.Env) > 256 {
		return fmt.Errorf("harness execution argument or environment count is invalid")
	}
	if err := validateLauncherBubblewrapArgs(req.Args); err != nil {
		return err
	}
	total := 0
	for _, arg := range req.Args {
		total += len(arg)
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("harness execution argument contains NUL")
		}
	}
	seenEnv := make(map[string]struct{}, len(req.Env))
	for _, entry := range req.Env {
		total += len(entry)
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !launcherEnvKeyPattern.MatchString(key) || strings.ContainsRune(entry, 0) {
			return fmt.Errorf("harness execution environment is invalid")
		}
		if _, duplicate := seenEnv[key]; duplicate {
			return fmt.Errorf("harness execution environment repeats %s", key)
		}
		seenEnv[key] = struct{}{}
	}
	if total > 512*1024 {
		return fmt.Errorf("harness execution payload is too large")
	}
	for _, required := range []string{"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--disable-userns", "--assert-userns-disabled"} {
		if countSliceValue(req.Args, required) != 1 {
			return fmt.Errorf("harness bubblewrap request must contain exactly one %q", required)
		}
	}
	for flag, value := range map[string]string{"--uid": "65534", "--gid": "65534", "--seccomp": "3"} {
		if countSliceValue(req.Args, flag) != 1 || !sliceContainsPair(req.Args, flag, value) {
			return fmt.Errorf("harness bubblewrap request must contain exactly one %s %s", flag, value)
		}
	}
	for _, forbidden := range []string{"--cap-add", "--userns", "--userns2", "--pidns"} {
		if sliceContains(req.Args, forbidden) {
			return fmt.Errorf("harness bubblewrap request contains forbidden namespace/capability argument %q", forbidden)
		}
	}
	return nil
}

func validateLauncherBubblewrapArgs(args []string) error {
	requiredPrefix := []string{
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--uid", "65534",
		"--gid", "65534",
		"--disable-userns",
		"--assert-userns-disabled",
		"--die-with-parent",
		"--new-session",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--dir", "/run",
		"--setenv", "HOME", "/tmp",
	}
	if len(args) < len(requiredPrefix) {
		return fmt.Errorf("harness bubblewrap request omits the fixed sandbox prefix")
	}
	for i := range requiredPrefix {
		if args[i] != requiredPrefix[i] {
			return fmt.Errorf("harness bubblewrap request does not match the fixed sandbox prefix")
		}
	}
	optionArity := map[string]int{
		"--unshare-user": 0, "--unshare-pid": 0, "--unshare-ipc": 0, "--unshare-uts": 0,
		"--disable-userns": 0, "--assert-userns-disabled": 0, "--die-with-parent": 0, "--new-session": 0,
		"--uid": 1, "--gid": 1, "--proc": 1, "--dev": 1, "--tmpfs": 1, "--dir": 1, "--seccomp": 1,
		"--bind": 2, "--ro-bind": 2, "--setenv": 2,
	}
	seenSeccomp := false
	commandIndex := -1
	for i := 0; i < len(args); {
		arg := args[i]
		if arg == "--" {
			return fmt.Errorf("harness bubblewrap request may not terminate option parsing explicitly")
		}
		if !strings.HasPrefix(arg, "--") {
			commandIndex = i
			break
		}
		arity, ok := optionArity[arg]
		if !ok {
			return fmt.Errorf("harness bubblewrap request contains unsupported option %q", arg)
		}
		if i+arity >= len(args) {
			return fmt.Errorf("harness bubblewrap option %q is incomplete", arg)
		}
		if arg == "--seccomp" {
			seenSeccomp = true
		}
		i += 1 + arity
	}
	if commandIndex < 0 || !seenSeccomp {
		return fmt.Errorf("harness bubblewrap request requires seccomp before its command")
	}
	for _, arg := range args[commandIndex:] {
		switch arg {
		case "--uid", "--gid", "--seccomp", "--userns", "--userns2", "--pidns", "--cap-add":
			return fmt.Errorf("harness bubblewrap security option %q appears after its command", arg)
		}
	}
	return nil
}

func validateHarnessStateBoundExecution(state privilegedHarnessState, req *privilegedHarnessExecRequest) error {
	resultPath := ""
	for _, entry := range req.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "HARNESS_RESULT_PATH" {
			resultPath = value
			break
		}
	}
	if resultPath != state.ResultPath || !sliceContainsSequence(req.Args, "--bind", state.ResultPath, state.ResultPath) {
		return fmt.Errorf("harness result exchange does not match launcher state")
	}
	return nil
}

func (s *PrivilegedLauncherServer) harnessSystemdRunArgs(state privilegedHarnessState, req *privilegedHarnessExecRequest) []string {
	cpuQuota := float64(req.NanoCPUs) / 1e9 * 100
	properties := []string{
		"User=" + strconv.Itoa(s.cfg.AllowedUID),
		"Group=" + strconv.Itoa(s.cfg.ManagerGID),
		"SupplementaryGroups=",
		"NoNewPrivileges=yes",
		// The outer process is an unprivileged UID with no ambient capabilities.
		// Keep only the capabilities bubblewrap receives inside its new user
		// namespace; an empty bounding set prevents it from mounting /proc.
		"CapabilityBoundingSet=CAP_SYS_ADMIN CAP_SETUID CAP_SETGID CAP_SETFCAP",
		"AmbientCapabilities=",
		"NetworkNamespacePath=/run/netns/" + state.Namespace.NamespaceName,
		"MemoryMax=" + strconv.FormatInt(req.MemoryBytes, 10),
		"MemorySwapMax=0",
		"TasksMax=" + strconv.FormatInt(req.PidsLimit, 10),
		"CPUQuota=" + strconv.FormatFloat(cpuQuota, 'f', 2, 64) + "%",
		"RuntimeMaxSec=" + strconv.FormatInt((req.MaxRuntime+999)/1000, 10) + "s",
		"KillMode=mixed",
		"TimeoutStopSec=5s",
		"UMask=0077",
		"WorkingDirectory=/",
		"ProtectSystem=strict",
		"ReadWritePaths=" + state.ResultPath,
		"PrivateDevices=yes",
		"PrivateTmp=yes",
		"ProtectClock=yes",
		"ProtectControlGroups=yes",
		"ProtectHome=yes",
		"ProtectKernelModules=yes",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		"RestrictNamespaces=user pid ipc uts mnt",
		"RestrictRealtime=yes",
		"SystemCallArchitectures=native",
	}
	args := []string{"--quiet", "--wait", "--pipe", "--collect", "--service-type=exec", "--unit", state.UnitName}
	for _, property := range properties {
		args = append(args, "--property", property)
	}
	args = append(args, "--", s.cfg.HarnessEnvCommand, "-i", "--")
	args = append(args, req.Env...)
	args = append(args,
		s.cfg.HarnessShell,
		"-c",
		`seccomp="$1"; shift; exec "$@" 3<"$seccomp"`,
		"agentcy-harness-sandbox",
		state.SeccompPath,
		s.cfg.HarnessBubblewrap,
	)
	return append(args, req.Args...)
}

func (s *PrivilegedLauncherServer) waitHarnessUnit(ctx context.Context, unitName string, pid int, waitCh <-chan error, activity <-chan struct{}, idleTimeout, maxRuntime time.Duration) error {
	idleTimer := time.NewTimer(idleTimeout)
	defer idleTimer.Stop()
	maxTimer := time.NewTimer(maxRuntime)
	defer maxTimer.Stop()
	stop := func(reason error) error {
		_, _ = s.hostCommand(context.Background(), s.cfg.HarnessSystemctl, "kill", "--kill-whom=all", unitName)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		select {
		case <-waitCh:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("%w; transient unit did not exit", reason)
		}
		return reason
	}
	for {
		select {
		case err := <-waitCh:
			return err
		case <-activity:
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(idleTimeout)
		case <-idleTimer.C:
			return stop(fmt.Errorf("harness process idle timeout after %s", idleTimeout))
		case <-maxTimer.C:
			return stop(fmt.Errorf("harness process runtime exceeded %s", maxRuntime))
		case <-ctx.Done():
			return stop(ctx.Err())
		}
	}
}

func (s *PrivilegedLauncherServer) recoverHarnessNetworkState(ctx context.Context) error {
	states, err := s.listHarnessStates()
	if err != nil {
		return err
	}
	var recoveryErrs []error
	for _, state := range states {
		if err := s.cleanupHarnessNetwork(ctx, state); err != nil {
			recoveryErrs = append(recoveryErrs, fmt.Errorf("recover harness cell %s: %w", state.CellID, err))
			continue
		}
		if err := removeLauncherStatePath(filepath.Join(s.cfg.StateRoot, "harness"), s.harnessStatePath(state.CellID)); err != nil {
			recoveryErrs = append(recoveryErrs, err)
		}
	}
	return errors.Join(recoveryErrs...)
}

func (s *PrivilegedLauncherServer) listHarnessStates() ([]privilegedHarnessState, error) {
	root := filepath.Join(s.cfg.StateRoot, "harness")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	states := make([]privilegedHarnessState, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		cellID := strings.TrimSuffix(entry.Name(), ".json")
		state, err := s.readHarnessState(cellID)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

func (s *PrivilegedLauncherServer) harnessStatePath(cellID string) string {
	return filepath.Join(s.cfg.StateRoot, "harness", cellID+".json")
}

func (s *PrivilegedLauncherServer) readHarnessState(cellID string) (privilegedHarnessState, error) {
	if err := validateHarnessCellID(cellID); err != nil {
		return privilegedHarnessState{}, err
	}
	path := s.harnessStatePath(cellID)
	data, err := os.ReadFile(path)
	if err != nil {
		return privilegedHarnessState{}, err
	}
	var state privilegedHarnessState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return privilegedHarnessState{}, fmt.Errorf("decode harness launcher state: %w", err)
	}
	if state.CellID != cellID {
		return privilegedHarnessState{}, fmt.Errorf("harness launcher state identity mismatch")
	}
	if _, err := s.validateHarnessNamespace(cellID, state.Namespace); err != nil {
		return privilegedHarnessState{}, err
	}
	wantResult := filepath.Join(s.cfg.HarnessResultRoot, cellID+".json")
	wantSeccomp := filepath.Join(s.cfg.HarnessResultRoot, cellID+".bpf")
	if state.ResultPath != wantResult || state.SeccompPath != wantSeccomp || state.UnitName != harnessUnitName(state.Namespace) || state.GuestMAC != launcherSourceMAC(cellID) || state.SourceGuard != launcherSourceGuardChain(cellID) {
		return privilegedHarnessState{}, fmt.Errorf("harness launcher state paths do not match policy")
	}
	return state, nil
}

func (s *PrivilegedLauncherServer) writeHarnessState(state privilegedHarnessState) error {
	if _, err := s.validateHarnessNamespace(state.CellID, state.Namespace); err != nil {
		return err
	}
	wantResult := filepath.Join(s.cfg.HarnessResultRoot, state.CellID+".json")
	wantSeccomp := filepath.Join(s.cfg.HarnessResultRoot, state.CellID+".bpf")
	if state.ResultPath != wantResult || state.SeccompPath != wantSeccomp || state.UnitName != harnessUnitName(state.Namespace) || state.GuestMAC != launcherSourceMAC(state.CellID) || state.SourceGuard != launcherSourceGuardChain(state.CellID) {
		return fmt.Errorf("harness launcher state paths do not match policy")
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	root := filepath.Join(s.cfg.StateRoot, "harness")
	tmp, err := os.CreateTemp(root, ".harness-state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.harnessStatePath(state.CellID)); err != nil {
		return err
	}
	return syncDirectory(root)
}

func (s *PrivilegedLauncherServer) hostCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	if s.runHost != nil {
		return s.runHost(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (s *PrivilegedLauncherServer) networkPosture(ctx context.Context) NetworkPostureReport {
	// Direct protocol tests construct a minimal server without production
	// network configuration; validated production configs always name a bridge.
	if s.cfg.allowUnprivilegedTests || strings.TrimSpace(s.cfg.BridgeName) == "" {
		return NetworkPostureReport{Healthy: true, FailureCounts: clonePostureFailures(s.postureFailures)}
	}
	report := probeNetworkPosture(ctx, s.cfg, s.hostCommand)
	report.FailureCounts = clonePostureFailures(s.postureFailures)
	return report
}

func (s *PrivilegedLauncherServer) requireNetworkPosture(ctx context.Context) error {
	report := s.networkPosture(ctx)
	if report.Healthy {
		return nil
	}
	if s.postureFailures == nil {
		s.postureFailures = map[string]uint64{}
	}
	for _, invariant := range report.Missing {
		s.postureFailures[invariant]++
		slog.Error("refusing microVM launch because host network posture drifted", "invariant", invariant, "failures", s.postureFailures[invariant])
	}
	return report.admissionError()
}

func clonePostureFailures(source map[string]uint64) map[string]uint64 {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]uint64, len(source))
	for invariant, count := range source {
		result[invariant] = count
	}
	return result
}

func harnessUnitName(namespace harness.NetworkNamespace) string {
	return "agentcy-harness-" + strings.TrimPrefix(namespace.NamespaceName, "ag-") + ".service"
}

func validateHarnessCellID(cellID string) error {
	if !launcherHarnessCellIDPattern.MatchString(strings.TrimSpace(cellID)) {
		return fmt.Errorf("harness cell ID is invalid")
	}
	return nil
}

func rejectExistingLauncherPath(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("launcher exchange path already exists")
}

func launcherSourceGuardChain(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("sg_%x", sum[:16])
}

func launcherSourceMAC(identity string) string {
	sum := sha256.Sum256([]byte("source-guard:" + identity))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

func (s *PrivilegedLauncherServer) installSourceGuard(ctx context.Context, chain, iface, sourceIP, sourceMAC string) error {
	_, macErr := net.ParseMAC(sourceMAC)
	if strings.TrimSpace(chain) == "" || strings.TrimSpace(iface) == "" || net.ParseIP(sourceIP) == nil || macErr != nil {
		return fmt.Errorf("source guard identity is invalid")
	}
	if out, err := s.hostCommand(ctx, s.cfg.NftPath, "add", "table", "netdev", launcherSourceGuardTable); err != nil && !nftObjectExists(out) {
		return fmt.Errorf("create source-guard table: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := s.removeSourceGuard(ctx, chain); err != nil {
		return err
	}
	commands := [][]string{
		{"add", "chain", "netdev", launcherSourceGuardTable, chain, "{", "type", "filter", "hook", "ingress", "device", iface, "priority", "-500", ";", "policy", "accept", ";", "}"},
		{"add", "rule", "netdev", launcherSourceGuardTable, chain, "ether", "saddr", "!=", sourceMAC, "drop"},
		{"add", "rule", "netdev", launcherSourceGuardTable, chain, "ether", "type", "ip", "ip", "saddr", sourceIP, "accept"},
		{"add", "rule", "netdev", launcherSourceGuardTable, chain, "ether", "type", "arp", "arp", "saddr", "ip", sourceIP, "accept"},
		{"add", "rule", "netdev", launcherSourceGuardTable, chain, "drop"},
	}
	for _, args := range commands {
		if out, err := s.hostCommand(ctx, s.cfg.NftPath, args...); err != nil {
			_ = s.removeSourceGuard(context.Background(), chain)
			return fmt.Errorf("install source guard: nft %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (s *PrivilegedLauncherServer) removeSourceGuard(ctx context.Context, chain string) error {
	if strings.TrimSpace(chain) == "" {
		return nil
	}
	for _, args := range [][]string{
		{"flush", "chain", "netdev", launcherSourceGuardTable, chain},
		{"delete", "chain", "netdev", launcherSourceGuardTable, chain},
	} {
		if out, err := s.hostCommand(ctx, s.cfg.NftPath, args...); err != nil && !nftObjectMissing(out) {
			return fmt.Errorf("remove source guard: nft %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func nftObjectExists(output []byte) bool {
	message := strings.ToLower(string(output))
	return strings.Contains(message, "file exists") || strings.Contains(message, "already exists")
}

func nftObjectMissing(output []byte) bool {
	message := strings.ToLower(string(output))
	return strings.Contains(message, "no such file or directory") || strings.Contains(message, "does not exist")
}

func missingNetworkResource(output []byte) bool {
	message := string(output)
	return strings.Contains(message, "Cannot find device") || strings.Contains(message, "No such file") || strings.Contains(message, "does not exist")
}

func missingSystemdUnit(output []byte) bool {
	message := strings.ToLower(string(output))
	return strings.Contains(message, "not loaded") || strings.Contains(message, "not found") || strings.Contains(message, "not running") || strings.Contains(message, "not active")
}

func sliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countSliceValue(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func sliceContainsPair(values []string, first, second string) bool {
	return sliceContainsSequence(values, first, second)
}

func sliceContainsSequence(values []string, sequence ...string) bool {
	if len(sequence) == 0 || len(values) < len(sequence) {
		return false
	}
	for i := 0; i <= len(values)-len(sequence); i++ {
		matched := true
		for j := range sequence {
			if values[i+j] != sequence[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

const maxLauncherHarnessOutputBytes = 512 << 10

type launcherOutputBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	activity chan struct{}
}

func newLauncherOutputBuffer() *launcherOutputBuffer {
	return &launcherOutputBuffer{activity: make(chan struct{}, 1)}
}

func (b *launcherOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	if b.buf.Len() > maxLauncherHarnessOutputBytes {
		data := b.buf.Bytes()
		tail := append([]byte(nil), data[len(data)-maxLauncherHarnessOutputBytes:]...)
		b.buf.Reset()
		_, _ = b.buf.Write(tail)
	}
	b.mu.Unlock()
	select {
	case b.activity <- struct{}{}:
	default:
	}
	return n, err
}

func (b *launcherOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *launcherOutputBuffer) Activity() <-chan struct{} {
	return b.activity
}

func (s *PrivilegedLauncherServer) launch(ctx context.Context, req *privilegedLaunchRequest) (privilegedLauncherResponse, error) {
	if err := s.requireNetworkPosture(ctx); err != nil {
		return privilegedLauncherResponse{}, err
	}
	if req == nil {
		return privilegedLauncherResponse{}, fmt.Errorf("launch request is required")
	}
	if err := s.validateLaunchRequest(*req); err != nil {
		return privilegedLauncherResponse{}, err
	}
	state, err := s.readState(req.InstanceID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return privilegedLauncherResponse{}, err
	}
	if state.Started {
		return privilegedLauncherResponse{}, fmt.Errorf("instance is already started")
	}
	if req.TapName != "" && (state.TapName != req.TapName || state.GuestIP != req.GuestIP || !state.SourceGuard) {
		return privilegedLauncherResponse{}, fmt.Errorf("instance tap source identity was not enforced by launcher")
	}
	jailRoot := s.jailerRoot(req.InstanceID)
	if err := os.MkdirAll(jailRoot, 0o700); err != nil {
		return privilegedLauncherResponse{}, fmt.Errorf("create jail root: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = s.removeManagerSocketAliases(req.InstanceID)
			_ = os.RemoveAll(filepath.Dir(jailRoot))
		}
	}()
	if err := s.stageVerifiedLauncherLink(s.cfg.RunRoot, req.RootfsPath, filepath.Join(jailRoot, rootfsName), s.cfg.JailerUID, s.cfg.ManagerGID, 0o660, true); err != nil {
		return privilegedLauncherResponse{}, fmt.Errorf("stage rootfs: %w", err)
	}
	if err := s.stageVerifiedLauncherLink(s.cfg.WorkspaceRoot, req.WorkspacePath, filepath.Join(jailRoot, workspaceName), s.cfg.JailerUID, s.cfg.ManagerGID, 0o660, false); err != nil {
		return privilegedLauncherResponse{}, fmt.Errorf("stage workspace: %w", err)
	}
	if err := s.stageVerifiedLauncherCopy(s.cfg.ArtifactRoot, s.cfg.KernelPath, filepath.Join(jailRoot, kernelName), 0o600, s.cfg.JailerUID, s.cfg.JailerGID); err != nil {
		return privilegedLauncherResponse{}, fmt.Errorf("stage kernel: %w", err)
	}
	sharedName := ""
	if req.SharedImage != "" {
		sharedName = sharedImageName
		if err := s.stageVerifiedLauncherCopy(s.cfg.ArtifactRoot, req.SharedImage, filepath.Join(jailRoot, sharedImageName), 0o600, s.cfg.JailerUID, s.cfg.JailerGID); err != nil {
			return privilegedLauncherResponse{}, fmt.Errorf("stage shared image: %w", err)
		}
	}
	serverCfg := &config.Config{
		MicroVMKernelPath:  kernelName,
		MicroVMKernelArgs:  s.cfg.KernelArgs,
		MicroVMVCPUs:       s.cfg.VCPUs,
		MicroVMMemoryMiB:   s.cfg.MemoryMiB,
		MicroVMCPUTemplate: s.cfg.CPUTemplate,
		MicroVMBridgeCIDR:  s.cfg.BridgeCIDR,
	}
	fcConfig := buildFirecrackerConfigWithPolicy(serverCfg, kernelName, rootfsName, workspaceName, sharedName, vsockUDSName, req.TapName, req.GuestIP, req.SandboxPolicy)
	fcConfig.BootSource.KernelImagePath = kernelName
	configPath := filepath.Join(jailRoot, configName)
	data, err := json.MarshalIndent(fcConfig, "", "  ")
	if err != nil {
		return privilegedLauncherResponse{}, fmt.Errorf("marshal firecracker config: %w", err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		return privilegedLauncherResponse{}, fmt.Errorf("write firecracker config: %w", err)
	}
	if err := chownIfDifferent(configPath, s.cfg.JailerUID, s.cfg.JailerGID); err != nil {
		return privilegedLauncherResponse{}, fmt.Errorf("chown firecracker config: %w", err)
	}
	logPath := filepath.Join(s.cfg.LogRoot, req.InstanceID+".log")
	logFile, err := openLauncherLog(logPath, s.cfg.ManagerGID)
	if err != nil {
		return privilegedLauncherResponse{}, fmt.Errorf("open launcher log: %w", err)
	}
	memoryMiB := s.cfg.MemoryMiB
	if req.SandboxPolicy != nil {
		memoryMiB = req.SandboxPolicy.MemoryMiB
	}
	args := s.jailerArgsWithMemory(req.InstanceID, memoryMiB)
	args = append(args, "--", "--api-sock", firecrackerSockName, "--config-file", configName)
	cmd := exec.Command(s.cfg.JailerPath, args...)
	cmd.Dir = filepath.Join(s.cfg.RunRoot, req.InstanceID)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	state.InstanceID = req.InstanceID
	state.TapName = req.TapName
	state.GuestIP = req.GuestIP
	state.Started = true
	if err := s.writeState(state); err != nil {
		_ = logFile.Close()
		return privilegedLauncherResponse{}, fmt.Errorf("persist launch intent: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		state.Started = false
		if stateErr := s.writeState(state); stateErr != nil {
			return privilegedLauncherResponse{}, errors.Join(fmt.Errorf("start jailer: %w", err), fmt.Errorf("persist failed launch cleanup: %w", stateErr))
		}
		return privilegedLauncherResponse{}, fmt.Errorf("start jailer: %w", err)
	}
	abortStartedLaunch := func(cause error) error {
		_ = cmd.Process.Kill()
		_ = signalFirecrackerByID(req.InstanceID, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = logFile.Close()
		state.Started = false
		if stateErr := s.writeState(state); stateErr != nil {
			return errors.Join(cause, fmt.Errorf("persist failed launch cleanup: %w", stateErr))
		}
		return cause
	}
	socketPath := filepath.Join(jailRoot, firecrackerSockName)
	vsockPath := filepath.Join(jailRoot, vsockUDSName)
	if err := s.waitForManagerSockets(socketPath, vsockPath); err != nil {
		return privilegedLauncherResponse{}, abortStartedLaunch(err)
	}
	managerSocketPath, err := s.installManagerSocketAlias(req.InstanceID, socketPath, "api")
	if err != nil {
		return privilegedLauncherResponse{}, abortStartedLaunch(fmt.Errorf("install manager Firecracker socket alias: %w", err))
	}
	managerVsockPath, err := s.installManagerSocketAlias(req.InstanceID, vsockPath, "vsock")
	if err != nil {
		return privilegedLauncherResponse{}, abortStartedLaunch(fmt.Errorf("install manager vsock alias: %w", err))
	}
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
	}()
	cleanup = false
	return privilegedLauncherResponse{
		SocketPath: managerSocketPath,
		VsockPath:  managerVsockPath,
		JailRoot:   jailRoot,
		LogPath:    logPath,
	}, nil
}

func openLauncherLog(logPath string, managerGID int) (*os.File, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o640)
	if err != nil {
		return nil, err
	}
	if err := logFile.Chown(-1, managerGID); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("group log for manager: %w", err)
	}
	if err := logFile.Chmod(0o640); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("chmod log for manager: %w", err)
	}
	return logFile, nil
}

func (s *PrivilegedLauncherServer) waitForManagerSockets(paths ...string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		ready := true
		for _, path := range paths {
			info, err := os.Lstat(path)
			if err != nil || info.Mode()&os.ModeSocket == 0 {
				ready = false
				break
			}
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for launcher-owned Firecracker sockets")
		}
		time.Sleep(25 * time.Millisecond)
	}
	return s.grantManagerSocketAccess(paths...)
}

func (s *PrivilegedLauncherServer) grantManagerSocketAccess(paths ...string) error {
	seenDirs := map[string]struct{}{}
	for _, socketPath := range paths {
		if err := ensurePathWithinRoot(s.cfg.JailRoot, socketPath); err != nil {
			return err
		}
		for dir := filepath.Dir(socketPath); dir != s.cfg.JailRoot && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			seenDirs[dir] = struct{}{}
		}
	}
	seenDirs[s.cfg.JailRoot] = struct{}{}
	for dir := range seenDirs {
		if _, err := os.Stat(dir); err != nil {
			return fmt.Errorf("stat launcher socket directory %s: %w", dir, err)
		}
		if err := os.Chown(dir, -1, s.cfg.SocketGID); err != nil {
			return fmt.Errorf("group launcher socket directory %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o750); err != nil {
			return fmt.Errorf("chmod launcher socket directory %s: %w", dir, err)
		}
	}
	for _, socketPath := range paths {
		info, err := os.Lstat(socketPath)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("launcher socket %s is not a Unix socket", socketPath)
		}
		if err := os.Chown(socketPath, -1, s.cfg.SocketGID); err != nil {
			return fmt.Errorf("group launcher socket %s: %w", socketPath, err)
		}
		if err := os.Chmod(socketPath, 0o660); err != nil {
			return fmt.Errorf("chmod launcher socket %s: %w", socketPath, err)
		}
	}
	return nil
}

func ensurePathWithinRoot(root, path string) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("launcher path escapes configured jail root")
	}
	return nil
}

func (s *PrivilegedLauncherServer) validateLaunchRequest(req privilegedLaunchRequest) error {
	if err := validateLauncherInstanceID(req.InstanceID); err != nil {
		return err
	}
	if !launcherPathSegmentPattern.MatchString(req.AgentID) || !launcherPathSegmentPattern.MatchString(req.CompartmentID) {
		return fmt.Errorf("agent and compartment IDs must be single safe path segments")
	}
	wantRunRootfs := filepath.Join(s.cfg.RunRoot, req.InstanceID, rootfsName)
	if filepath.Clean(req.RootfsPath) != wantRunRootfs {
		return fmt.Errorf("rootfs path does not match launcher-derived run path")
	}
	if err := ensureContainedRegularFile(s.cfg.RunRoot, req.RootfsPath); err != nil {
		return fmt.Errorf("run rootfs: %w", err)
	}
	wantWorkspace := filepath.Join(s.cfg.WorkspaceRoot, req.AgentID, req.CompartmentID+"."+workspaceName)
	if filepath.Clean(req.WorkspacePath) != wantWorkspace {
		return fmt.Errorf("workspace path does not match launcher-derived identity")
	}
	if err := ensureContainedRegularFile(s.cfg.WorkspaceRoot, req.WorkspacePath); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if err := ensureContainedRegularFile(s.cfg.ArtifactRoot, req.RootfsImage); err != nil {
		return fmt.Errorf("rootfs image: %w", err)
	}
	if req.SharedImage != "" {
		if err := ensureContainedRegularFile(s.cfg.ArtifactRoot, req.SharedImage); err != nil {
			return fmt.Errorf("shared image: %w", err)
		}
	}
	if policy := req.SandboxPolicy; policy != nil {
		if policy.VCPUs < 1 || policy.MemoryMiB < 1 || policy.WorkspaceSizeMiB < 1 || policy.ProcessLimit < 1 {
			return fmt.Errorf("sandbox runtime policy values must be positive")
		}
		if policy.VCPUs > s.cfg.VCPUs || policy.MemoryMiB > s.cfg.MemoryMiB || policy.WorkspaceSizeMiB > s.cfg.WorkspaceSizeMiB {
			return fmt.Errorf("sandbox runtime policy exceeds launcher maxima")
		}
		if policy.SharedReadOnly != (req.SharedImage != "") {
			return fmt.Errorf("sandbox shared mount policy does not match launch drives")
		}
	}
	if req.TapName == "" || req.GuestIP == "" {
		if req.TapName != "" || req.GuestIP != "" {
			return fmt.Errorf("tap name and guest IP must be supplied together")
		}
		return nil
	}
	if req.TapName != tapNameForInstance(s.cfg.TapPrefix, req.InstanceID) {
		return fmt.Errorf("tap name does not match launcher policy")
	}
	if !ipWithinCIDR(req.GuestIP, s.cfg.BridgeCIDR) {
		return fmt.Errorf("guest IP is outside launcher bridge CIDR")
	}
	return nil
}

func (s *PrivilegedLauncherServer) stop(ctx context.Context, instanceID string) error {
	if err := validateLauncherInstanceID(instanceID); err != nil {
		return err
	}
	state, err := s.readState(instanceID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !state.Started {
		return nil
	}
	if err := signalFirecrackerByID(instanceID, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		running, err := firecrackerProcessRunning(instanceID)
		if err != nil {
			return err
		}
		if !running {
			state.Started = false
			return s.writeState(state)
		}
		if time.Now().After(deadline) {
			if err := signalFirecrackerByID(instanceID, syscall.SIGKILL); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *PrivilegedLauncherServer) running(instanceID string) (bool, error) {
	if err := validateLauncherInstanceID(instanceID); err != nil {
		return false, err
	}
	if _, err := s.readState(instanceID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return firecrackerProcessRunning(instanceID)
}

func (s *PrivilegedLauncherServer) jailerRoot(instanceID string) string {
	return filepath.Join(s.cfg.JailRoot, filepath.Base(s.cfg.FirecrackerPath), instanceID, "root")
}

func (s *PrivilegedLauncherServer) managerSocketAliasPath(instanceID, kind string) string {
	sum := sha256.Sum256([]byte(instanceID))
	return filepath.Join(filepath.Dir(s.cfg.SocketPath), fmt.Sprintf("vm-%x.%s", sum[:16], kind))
}

func (s *PrivilegedLauncherServer) installManagerSocketAlias(instanceID, target, kind string) (string, error) {
	if err := validateLauncherInstanceID(instanceID); err != nil {
		return "", err
	}
	if kind != "api" && kind != "vsock" {
		return "", fmt.Errorf("manager socket alias kind is invalid")
	}
	alias := s.managerSocketAliasPath(instanceID, kind)
	if len(alias) >= 108 {
		return "", fmt.Errorf("manager socket alias exceeds Linux sockaddr_un limit")
	}
	if info, err := os.Lstat(alias); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("manager socket alias path is not a symlink")
		}
		if err := os.Remove(alias); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Symlink(target, alias); err != nil {
		return "", err
	}
	return alias, nil
}

func (s *PrivilegedLauncherServer) removeManagerSocketAliases(instanceID string) error {
	var joined error
	for _, kind := range []string{"api", "vsock"} {
		alias := s.managerSocketAliasPath(instanceID, kind)
		info, err := os.Lstat(alias)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			joined = errors.Join(joined, err)
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			joined = errors.Join(joined, fmt.Errorf("manager socket alias path %s is not a symlink", alias))
			continue
		}
		if err := os.Remove(alias); err != nil && !errors.Is(err, os.ErrNotExist) {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func (s *PrivilegedLauncherServer) jailerArgsWithMemory(instanceID string, memoryMiB int) []string {
	args := []string{
		"--id", instanceID,
		"--exec-file", s.cfg.FirecrackerPath,
		"--uid", strconv.Itoa(s.cfg.JailerUID),
		"--gid", strconv.Itoa(s.cfg.JailerGID),
		"--chroot-base-dir", s.cfg.JailRoot,
		"--new-pid-ns",
		"--resource-limit", "no-file=4096",
	}
	if s.cfg.JailerCgroupVersion > 0 {
		args = append(args, "--cgroup-version", strconv.Itoa(s.cfg.JailerCgroupVersion))
		if s.cfg.JailerParentCgroup != "" {
			args = append(args, "--parent-cgroup", s.cfg.JailerParentCgroup)
		}
		args = append(args, "--cgroup", jailerMemoryCgroup(s.cfg.JailerCgroupVersion, memoryMiB))
	}
	return args
}

func (s *PrivilegedLauncherServer) statePath(instanceID string) string {
	return filepath.Join(s.cfg.StateRoot, instanceID+".json")
}

func (s *PrivilegedLauncherServer) readState(instanceID string) (privilegedLauncherState, error) {
	if err := validateLauncherInstanceID(instanceID); err != nil {
		return privilegedLauncherState{}, err
	}
	data, err := os.ReadFile(s.statePath(instanceID))
	if err != nil {
		return privilegedLauncherState{}, err
	}
	var state privilegedLauncherState
	if err := json.Unmarshal(data, &state); err != nil {
		return privilegedLauncherState{}, fmt.Errorf("decode launcher state: %w", err)
	}
	if state.InstanceID != instanceID {
		return privilegedLauncherState{}, fmt.Errorf("launcher state identity mismatch")
	}
	return state, nil
}

func (s *PrivilegedLauncherServer) writeState(state privilegedLauncherState) error {
	if err := validateLauncherInstanceID(state.InstanceID); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.cfg.StateRoot, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create launcher state: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.statePath(state.InstanceID)); err != nil {
		return fmt.Errorf("commit launcher state: %w", err)
	}
	return syncDirectory(s.cfg.StateRoot)
}

func removeLauncherStatePath(root, path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return syncDirectory(root)
}

func (s *PrivilegedLauncherServer) findStateByTap(tapName string) (privilegedLauncherState, error) {
	if tapName == "" || !strings.HasPrefix(tapName, sanitizeTapPrefix(s.cfg.TapPrefix)) {
		return privilegedLauncherState{}, fmt.Errorf("tap name is outside launcher policy")
	}
	entries, err := os.ReadDir(s.cfg.StateRoot)
	if err != nil {
		return privilegedLauncherState{}, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		state, err := s.readState(id)
		if err != nil {
			return privilegedLauncherState{}, err
		}
		if state.TapName == tapName {
			return state, nil
		}
	}
	return privilegedLauncherState{}, fmt.Errorf("tap is not owned by the launcher")
}

func validateLauncherInstanceID(instanceID string) error {
	if !launcherInstanceIDPattern.MatchString(strings.TrimSpace(instanceID)) {
		return fmt.Errorf("instance ID is invalid")
	}
	return nil
}

func ensureContainedPath(root, path string) error {
	root = filepath.Clean(strings.TrimSpace(root))
	path = filepath.Clean(strings.TrimSpace(path))
	if root == "" || path == "" || !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return fmt.Errorf("path and root must be absolute")
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path is outside configured root")
	}
	return nil
}

type verifiedLauncherFile struct {
	rootFD int
	file   *os.File
	root   string
	path   string
	rel    string
	stat   unix.Stat_t
	flags  int
}

func openVerifiedLauncherFile(root, path string, writable bool) (*verifiedLauncherFile, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	path = filepath.Clean(strings.TrimSpace(path))
	if err := ensureContainedPath(root, path); err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open launcher root without symlinks: %w", err)
	}
	flags := unix.O_RDONLY
	if writable {
		flags = unix.O_RDWR
	}
	how := &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(rootFD, rel, how)
	if err != nil {
		_ = unix.Close(rootFD)
		return nil, fmt.Errorf("open launcher source beneath trusted root: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		_ = unix.Close(rootFD)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		_ = unix.Close(rootFD)
		return nil, fmt.Errorf("launcher source must be a regular file")
	}
	return &verifiedLauncherFile{
		rootFD: rootFD,
		file:   os.NewFile(uintptr(fd), path),
		root:   root,
		path:   path,
		rel:    rel,
		stat:   stat,
		flags:  flags,
	}, nil
}

func (f *verifiedLauncherFile) Close() {
	if f == nil {
		return
	}
	if f.file != nil {
		_ = f.file.Close()
	}
	if f.rootFD >= 0 {
		_ = unix.Close(f.rootFD)
	}
}

func (f *verifiedLauncherFile) assertCurrentBinding() error {
	if f == nil || f.file == nil || f.rootFD < 0 {
		return fmt.Errorf("verified launcher source is closed")
	}
	how := &unix.OpenHow{
		Flags:   uint64(f.flags | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(f.rootFD, f.rel, how)
	if err != nil {
		return fmt.Errorf("reopen launcher source binding: %w", err)
	}
	defer unix.Close(fd)
	var current unix.Stat_t
	if err := unix.Fstat(fd, &current); err != nil {
		return err
	}
	if current.Dev != f.stat.Dev || current.Ino != f.stat.Ino {
		return fmt.Errorf("launcher source binding changed during staging")
	}
	return nil
}

func (s *PrivilegedLauncherServer) stageVerifiedLauncherLink(root, source, destination string, uid, gid int, mode os.FileMode, allowCopy bool) error {
	verified, err := openVerifiedLauncherFile(root, source, true)
	if err != nil {
		return err
	}
	defer verified.Close()
	return stageVerifiedLauncherLink(verified, destination, uid, gid, mode, allowCopy, s.cfg.allowUnprivilegedTests)
}

func stageVerifiedLauncherLink(verified *verifiedLauncherFile, destination string, uid, gid int, mode os.FileMode, allowCopy, allowTestProcFallback bool) error {
	destination = filepath.Clean(strings.TrimSpace(destination))
	destinationDir := filepath.Dir(destination)
	name := filepath.Base(destination)
	if name == "." || name == string(filepath.Separator) || !launcherPathSegmentPattern.MatchString(name) {
		return fmt.Errorf("launcher destination name is invalid")
	}
	dirFD, err := unix.Open(destinationDir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open launcher destination directory: %w", err)
	}
	defer unix.Close(dirFD)
	if err := unix.Unlinkat(dirFD, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove prior launcher destination: %w", err)
	}
	linked := false
	err = unix.Linkat(int(verified.file.Fd()), "", dirFD, name, unix.AT_EMPTY_PATH)
	if errors.Is(err, unix.EPERM) && allowTestProcFallback {
		fdPath := "/proc/self/fd/" + strconv.Itoa(int(verified.file.Fd()))
		err = unix.Linkat(unix.AT_FDCWD, fdPath, dirFD, name, unix.AT_SYMLINK_FOLLOW)
	}
	if err == nil {
		linked = true
	} else if !errors.Is(err, unix.EXDEV) || !allowCopy {
		return fmt.Errorf("link verified launcher source: %w", err)
	}
	cleanup := true
	sourceMutated := false
	defer func() {
		if cleanup {
			_ = unix.Unlinkat(dirFD, name, 0)
			if sourceMutated {
				_ = unix.Fchown(int(verified.file.Fd()), int(verified.stat.Uid), int(verified.stat.Gid))
				_ = unix.Fchmod(int(verified.file.Fd()), verified.stat.Mode&0o777)
			}
		}
	}()
	if linked {
		destFD, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("open staged launcher link: %w", err)
		}
		var destStat unix.Stat_t
		statErr := unix.Fstat(destFD, &destStat)
		_ = unix.Close(destFD)
		if statErr != nil {
			return statErr
		}
		if verified.stat.Dev != destStat.Dev || verified.stat.Ino != destStat.Ino {
			return fmt.Errorf("staged launcher link inode mismatch")
		}
		if err := verified.assertCurrentBinding(); err != nil {
			return err
		}
		if err := unix.Fchown(int(verified.file.Fd()), uid, gid); err != nil {
			return fmt.Errorf("chown verified launcher source: %w", err)
		}
		sourceMutated = true
		if err := unix.Fchmod(int(verified.file.Fd()), uint32(mode.Perm())); err != nil {
			return fmt.Errorf("chmod verified launcher source: %w", err)
		}
	} else {
		if err := copyVerifiedLauncherFileToDir(verified, dirFD, name, uid, gid, mode); err != nil {
			return err
		}
	}
	if err := verified.assertCurrentBinding(); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func (s *PrivilegedLauncherServer) stageVerifiedLauncherCopy(root, source, destination string, mode os.FileMode, uid, gid int) error {
	verified, err := openVerifiedLauncherFile(root, source, false)
	if err != nil {
		return err
	}
	defer verified.Close()
	destination = filepath.Clean(strings.TrimSpace(destination))
	dirFD, err := unix.Open(filepath.Dir(destination), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open launcher copy destination: %w", err)
	}
	defer unix.Close(dirFD)
	name := filepath.Base(destination)
	if !launcherPathSegmentPattern.MatchString(name) {
		return fmt.Errorf("launcher copy destination name is invalid")
	}
	if err := unix.Unlinkat(dirFD, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	if err := copyVerifiedLauncherFileToDir(verified, dirFD, name, uid, gid, mode); err != nil {
		return err
	}
	if err := verified.assertCurrentBinding(); err != nil {
		_ = unix.Unlinkat(dirFD, name, 0)
		return err
	}
	return nil
}

func copyVerifiedLauncherFileToDir(verified *verifiedLauncherFile, dirFD int, name string, uid, gid int, mode os.FileMode) error {
	dstFD, err := unix.Openat(dirFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("create staged launcher copy: %w", err)
	}
	destination := os.NewFile(uintptr(dstFD), name)
	cleanup := true
	defer func() {
		_ = destination.Close()
		if cleanup {
			_ = unix.Unlinkat(dirFD, name, 0)
		}
	}()
	if _, err := verified.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(destination, verified.file); err != nil {
		return fmt.Errorf("copy verified launcher source: %w", err)
	}
	if err := unix.Fchown(dstFD, uid, gid); err != nil {
		return fmt.Errorf("chown staged launcher copy: %w", err)
	}
	if err := unix.Fchmod(dstFD, uint32(mode.Perm())); err != nil {
		return fmt.Errorf("chmod staged launcher copy: %w", err)
	}
	if err := destination.Sync(); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func ensureContainedRegularFile(root, path string) error {
	root = filepath.Clean(strings.TrimSpace(root))
	path = filepath.Clean(strings.TrimSpace(path))
	if root == "" || path == "" || !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return fmt.Errorf("path and root must be absolute")
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path is outside configured root")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve configured root: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	resolvedRel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || resolvedRel == "." || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("resolved path escapes configured root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("path must be a non-symlink regular file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open path without following symlinks: %w", err)
	}
	return file.Close()
}

func ipWithinCIDR(ipString, cidr string) bool {
	ip := net.ParseIP(strings.TrimSpace(ipString))
	_, network, err := net.ParseCIDR(strings.TrimSpace(cidr))
	return err == nil && ip != nil && network.Contains(ip)
}
