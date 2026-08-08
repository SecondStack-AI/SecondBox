package install

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type CommandResult struct {
	Stdout string
	Stderr string
}
type FilesystemProbe interface {
	ReadFile(string) ([]byte, error)
	Lstat(string) (fs.FileInfo, error)
	StatFS(string) (available, total int64, err error)
	OpenReadWrite(string) error
}
type ProcessProbe interface {
	Run(context.Context, string, ...string) (CommandResult, error)
	LookPath(string) (string, error)
}
type NetworkProbe interface {
	LookupHost(context.Context, string) ([]string, error)
	Head(context.Context, string) (int, error)
}
type ClockProbe interface{ Now() time.Time }
type UserProbe interface {
	AssignedUIDs() (map[int64]bool, error)
}
type UserRangeProbe interface {
	ReservedIDRanges() ([]UIDRange, error)
}
type PreflightProbes struct {
	Filesystem   FilesystemProbe
	Process      ProcessProbe
	Network      NetworkProbe
	Clock        ClockProbe
	Users        UserProbe
	LookupEnv    func(string) (string, bool)
	OS           string
	Architecture string
	CPUCount     int
	InvokingUID  int64
	InvokingGID  int64
}

type systemFilesystemProbe struct{}

func (systemFilesystemProbe) ReadFile(path string) ([]byte, error)   { return os.ReadFile(path) }
func (systemFilesystemProbe) Lstat(path string) (fs.FileInfo, error) { return os.Lstat(path) }
func (systemFilesystemProbe) OpenReadWrite(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return file.Close()
}
func (systemFilesystemProbe) StatFS(path string) (int64, int64, error) {
	var value syscall.Statfs_t
	if err := syscall.Statfs(path, &value); err != nil {
		return 0, 0, err
	}
	return int64(value.Bavail) * int64(value.Bsize), int64(value.Blocks) * int64(value.Bsize), nil
}

type systemProcessProbe struct{}

func (systemProcessProbe) Run(ctx context.Context, name string, args ...string) (CommandResult, error) {
	command := exec.CommandContext(ctx, name, args...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return CommandResult{Stdout: strings.TrimSpace(stdout.String()), Stderr: strings.TrimSpace(stderr.String())}, err
}
func (systemProcessProbe) LookPath(name string) (string, error) { return exec.LookPath(name) }

type systemNetworkProbe struct{ client *http.Client }

func (systemNetworkProbe) LookupHost(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, name)
}
func (probe systemNetworkProbe) Head(ctx context.Context, location string) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, location, nil)
	if err != nil {
		return 0, err
	}
	response, err := probe.client.Do(request)
	if err != nil {
		return 0, err
	}
	closeErr := response.Body.Close()
	return response.StatusCode, closeErr
}

type systemClockProbe struct{}

func (systemClockProbe) Now() time.Time { return time.Now() }

type systemUserProbe struct{ filesystem FilesystemProbe }

func (probe systemUserProbe) AssignedUIDs() (map[int64]bool, error) {
	result := map[int64]bool{}
	for _, database := range []struct {
		name  string
		field int
	}{{"passwd", 2}, {"group", 2}} {
		content, err := exec.Command("getent", database.name).Output()
		if err != nil {
			return nil, fmt.Errorf("inspect NSS %s database: %w", database.name, err)
		}
		scanner := bufio.NewScanner(strings.NewReader(string(content)))
		for scanner.Scan() {
			fields := strings.Split(scanner.Text(), ":")
			if len(fields) <= database.field {
				continue
			}
			id, err := strconv.ParseInt(fields[database.field], 10, 64)
			if err == nil {
				result[id] = true
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (probe systemUserProbe) ReservedIDRanges() ([]UIDRange, error) {
	result := []UIDRange{}
	for _, path := range []string{"/etc/subuid", "/etc/subgid"} {
		content, err := probe.filesystem.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(strings.NewReader(string(content)))
		for scanner.Scan() {
			fields := strings.Split(scanner.Text(), ":")
			if len(fields) != 3 {
				continue
			}
			start, startErr := strconv.ParseInt(fields[1], 10, 64)
			count, countErr := strconv.ParseInt(fields[2], 10, 64)
			if startErr != nil || countErr != nil || start < 0 || count <= 0 {
				return nil, fmt.Errorf("invalid subordinate ID range in %s", path)
			}
			result = append(result, UIDRange{Start: start, Count: count})
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func SystemPreflightProbes() PreflightProbes {
	filesystem := systemFilesystemProbe{}
	return PreflightProbes{Filesystem: filesystem, Process: systemProcessProbe{}, Network: systemNetworkProbe{client: &http.Client{Timeout: 5 * time.Second}}, Clock: systemClockProbe{}, Users: systemUserProbe{filesystem: filesystem}, LookupEnv: os.LookupEnv, OS: runtime.GOOS, Architecture: runtime.GOARCH, CPUCount: runtime.NumCPU(), InvokingUID: int64(os.Getuid()), InvokingGID: int64(os.Getgid())}
}

func Preflight(ctx context.Context, probes PreflightProbes) (HostFacts, error) {
	if probes.Filesystem == nil || probes.Process == nil || probes.Network == nil || probes.Clock == nil || probes.Users == nil || probes.LookupEnv == nil {
		return HostFacts{}, installerError("preflight requires filesystem, process, network, clock, and user probes", nil)
	}
	facts := HostFacts{SchemaVersion: HostFactsSchema, ObservedAt: probes.Clock.Now().UTC(), OS: probes.OS, Architecture: probes.Architecture, InvokingUID: probes.InvokingUID, InvokingGID: probes.InvokingGID, CPUCount: probes.CPUCount, Devices: []DeviceFact{}, ListeningPorts: []PortFact{}, Routes: []RouteFact{}, DNSUpstreams: []string{}, AssignedUIDs: []int64{}, ReservedIDRanges: []UIDRange{}, CandidateUIDRanges: []UIDRange{}, Utilities: map[string]string{}, Findings: []Finding{}}
	add := func(id string, class FindingClass, summary, detail, remedy string) {
		facts.Findings = append(facts.Findings, Finding{ID: id, Class: class, Summary: summary, Detail: detail, Remedy: remedy})
	}
	machineID, err := probes.Filesystem.ReadFile("/etc/machine-id")
	if err != nil || strings.TrimSpace(string(machineID)) == "" {
		add("host_identity", FindingBlocked, "Host identity is unavailable", errorText(err), "Restore /etc/machine-id before installing.")
	} else {
		facts.HostIdentity = "machine-id:" + strings.TrimSpace(string(machineID))
		add("host_identity", FindingPass, "Host identity observed", facts.HostIdentity, "")
	}
	if facts.OS != "linux" || facts.Architecture != "amd64" {
		add("platform", FindingBlocked, "Linux amd64 is required", facts.OS+"/"+facts.Architecture, "Use a Linux amd64 host for the v1 Runner/guest matrix.")
	} else {
		add("platform", FindingPass, "Linux amd64 host", facts.OS+"/"+facts.Architecture, "")
	}
	if result, runErr := probes.Process.Run(ctx, "uname", "-r"); runErr != nil {
		add("kernel", FindingBlocked, "Kernel version is unavailable", result.Stderr, "Repair the host command environment.")
	} else {
		facts.KernelVersion = result.Stdout
		add("kernel", FindingPass, "Kernel observed", facts.KernelVersion, "")
	}
	preflightSystemd(ctx, probes, &facts, add)
	preflightDocker(ctx, probes, &facts, add)
	preflightCgroup(probes, &facts, add)
	preflightDevices(probes, &facts, add)
	preflightCPUAndMemory(probes, &facts, add)
	preflightMounts(probes, &facts, add)
	preflightNetwork(ctx, probes, &facts, add)
	preflightUsers(probes, &facts, add)
	preflightUtilities(probes, &facts, add)
	if facts.HostIdentity == "" {
		facts.HostIdentity = "unavailable"
	}
	if facts.KernelVersion == "" {
		facts.KernelVersion = "unavailable"
	}
	return facts, nil
}

func preflightSystemd(ctx context.Context, p PreflightProbes, f *HostFacts, add func(string, FindingClass, string, string, string)) {
	version, versionErr := p.Process.Run(ctx, "systemctl", "--version")
	active, activeErr := p.Process.Run(ctx, "systemctl", "is-system-running")
	if versionErr != nil || (activeErr != nil && active.Stdout != "degraded") || (active.Stdout != "running" && active.Stdout != "degraded") {
		add("systemd", FindingBlocked, "systemd is not the active service manager", strings.TrimSpace(version.Stdout+" "+active.Stdout+" "+active.Stderr), "Boot the host under systemd before installing.")
		return
	}
	f.SystemdVersion = strings.Split(version.Stdout, "\n")[0]
	class := FindingPass
	if active.Stdout == "degraded" {
		class = FindingWarning
	}
	add("systemd", class, "systemd is active", f.SystemdVersion+" ("+active.Stdout+")", "")
}
func preflightDocker(ctx context.Context, p PreflightProbes, f *HostFacts, add func(string, FindingClass, string, string, string)) {
	for _, name := range []string{"DOCKER_HOST", "DOCKER_CONTEXT"} {
		value, present := p.LookupEnv(name)
		value = strings.TrimSpace(value)
		allowed := value == "" || (name == "DOCKER_CONTEXT" && value == "default")
		if present && !allowed {
			add("docker_context", FindingBlocked, "Remote or non-default Docker selection is not supported", name+" is explicitly set", "Unset DOCKER_HOST and DOCKER_CONTEXT, then use the host-local default Docker socket.")
			return
		}
	}
	docker, err := p.Process.Run(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		detail := strings.TrimSpace(docker.Stdout + " " + docker.Stderr)
		class := FindingNeedsAction
		if strings.Contains(strings.ToLower(detail), "permission") {
			detail = "Docker permission denied: " + detail
		}
		add("docker", class, "Docker Engine is unavailable", detail, "Install Docker Engine or grant the invoking user access, then rerun --check.")
	} else {
		f.DockerVersion = docker.Stdout
		add("docker", FindingPass, "Docker Engine is reachable", docker.Stdout, "")
	}
	compose, composeErr := p.Process.Run(ctx, "docker", "compose", "version", "--short")
	if composeErr != nil {
		add("compose", FindingNeedsAction, "Docker Compose v2 is unavailable", compose.Stderr, "Install the Docker Compose v2 plugin.")
	} else {
		f.ComposeVersion = compose.Stdout
		add("compose", FindingPass, "Docker Compose v2 is available", compose.Stdout, "")
	}
	projects, projectsErr := p.Process.Run(ctx, "docker", "compose", "ls", "--format", "json")
	if projectsErr != nil {
		add("compose_projects", FindingWarning, "Existing Compose projects could not be inspected", projects.Stderr, "Review Docker access before installation.")
	} else {
		f.Utilities["composeProjects"] = projects.Stdout
		add("compose_projects", FindingPass, "Existing Compose projects inspected", boundedDetail(projects.Stdout), "")
	}
}
func preflightCgroup(p PreflightProbes, f *HostFacts, add func(string, FindingClass, string, string, string)) {
	content, err := p.Filesystem.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		f.CgroupVersion = 1
		add("cgroup", FindingBlocked, "cgroup v2 is required", errorText(err), "Boot with the unified cgroup v2 hierarchy.")
		return
	}
	f.CgroupVersion = 2
	f.CgroupControllers = strings.Fields(string(content))
	required := []string{"cpu", "memory", "pids"}
	missing := []string{}
	for _, name := range required {
		if !slices.Contains(f.CgroupControllers, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		add("cgroup", FindingBlocked, "Required cgroup v2 controllers are absent", strings.Join(missing, ","), "Enable the missing controllers.")
	} else {
		add("cgroup", FindingPass, "cgroup v2 controllers are available", strings.Join(f.CgroupControllers, ","), "")
	}
}
func preflightDevices(p PreflightProbes, f *HostFacts, add func(string, FindingClass, string, string, string)) {
	for _, item := range []struct {
		id, path string
		target   *bool
	}{{"kvm", "/dev/kvm", &f.KVMAccessible}, {"tun", "/dev/net/tun", &f.TUNAccessible}} {
		info, err := p.Filesystem.Lstat(item.path)
		if err != nil || info.Mode()&os.ModeDevice == 0 {
			add(item.id, FindingBlocked, item.path+" is unavailable", errorText(err), "Expose the host device to the installer and Runner.")
			continue
		}
		openErr := p.Filesystem.OpenReadWrite(item.path)
		if openErr != nil {
			add(item.id, FindingNeedsAction, item.path+" is permission denied", openErr.Error(), "Grant the invoking user access; privileged apply will recheck as root.")
			continue
		}
		*item.target = true
		add(item.id, FindingPass, item.path+" is accessible", "", "")
	}
	filesystems, err := p.Filesystem.ReadFile("/proc/filesystems")
	f.BtrfsSupported = err == nil && strings.Contains(string(filesystems), "btrfs")
	if f.BtrfsSupported {
		add("btrfs", FindingPass, "Btrfs kernel support is available", "", "")
	} else {
		add("btrfs", FindingWarning, "Btrfs kernel support was not observed", errorText(err), "Use a qualified kernel or an existing dedicated XFS workspace.")
	}
}
func preflightCPUAndMemory(p PreflightProbes, f *HostFacts, add func(string, FindingClass, string, string, string)) {
	cpu, err := p.Filesystem.ReadFile("/proc/cpuinfo")
	text := string(cpu)
	if err == nil && (strings.Contains(text, " vmx ") || strings.Contains(text, " svm ") || strings.Contains(text, "\nflags") && (strings.Contains(text, "vmx") || strings.Contains(text, "svm"))) {
		f.Virtualization = "hardware"
		add("virtualization", FindingPass, "CPU virtualization is available", "", "")
	} else {
		add("virtualization", FindingBlocked, "CPU virtualization is unavailable", errorText(err), "Enable VT-x/AMD-V and nested virtualization when applicable.")
	}
	memory, err := p.Filesystem.ReadFile("/proc/meminfo")
	if err == nil {
		for _, line := range strings.Split(string(memory), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "MemTotal:" {
				kb, _ := strconv.ParseInt(fields[1], 10, 64)
				f.MemoryBytes = kb * 1024
				break
			}
		}
	}
	if f.MemoryBytes < MinimumHostMemoryBytes {
		add("memory", FindingNeedsAction, "Host memory is below 12 GiB", strconv.FormatInt(f.MemoryBytes, 10)+" bytes", "Use a host with enough memory for control services and the durable-coding microVM.")
	} else {
		add("memory", FindingPass, "Host memory is sufficient", strconv.FormatInt(f.MemoryBytes, 10)+" bytes", "")
	}
	if f.CPUCount < MinimumHostCPUCount {
		add("cpu", FindingNeedsAction, "Fewer than six logical CPUs detected", strconv.Itoa(f.CPUCount), "Use a host with capacity for control services and the durable-coding microVM.")
	} else {
		add("cpu", FindingPass, "CPU capacity observed", strconv.Itoa(f.CPUCount)+" logical CPUs", "")
	}
}
func preflightMounts(p PreflightProbes, f *HostFacts, add func(string, FindingClass, string, string, string)) {
	content, err := p.Filesystem.ReadFile("/proc/self/mountinfo")
	if err != nil {
		add("filesystems", FindingNeedsAction, "Mounted filesystems could not be inspected", err.Error(), "Restore /proc and rerun preflight.")
		return
	}
	rootDevice := ""
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		separator := slices.Index(fields, "-")
		if len(fields) < 6 || separator < 0 || separator+2 >= len(fields) {
			continue
		}
		device, mountpoint, filesystem := fields[2], decodeMount(fields[4]), fields[separator+1]
		if mountpoint == "/" {
			rootDevice = device
		}
		if filesystem != "xfs" && filesystem != "btrfs" {
			continue
		}
		available, total, statErr := p.Filesystem.StatFS(mountpoint)
		if statErr != nil {
			continue
		}
		f.Devices = append(f.Devices, DeviceFact{Path: mountpoint, Identity: device, Filesystem: filesystem, SizeBytes: total, AvailableBytes: available, Mountpoint: mountpoint})
	}
	candidates := 0
	for _, device := range f.Devices {
		if device.Mountpoint != "/" && device.Identity != rootDevice && device.AvailableBytes >= MinimumWorkspaceBytes {
			candidates++
		}
	}
	if candidates == 0 {
		add("workspace_filesystem", FindingRemediable, "No dedicated XFS/Btrfs workspace candidate was found", "The installer can create a bounded Btrfs filesystem image.", "Choose the filesystem-image option or mount a dedicated filesystem.")
	} else {
		add("workspace_filesystem", FindingPass, "Dedicated workspace candidates found", strconv.Itoa(candidates), "")
	}
}
func preflightNetwork(ctx context.Context, p PreflightProbes, f *HostFacts, add func(string, FindingClass, string, string, string)) {
	routes, routeErr := p.Process.Run(ctx, "ip", "-o", "route", "show")
	if routeErr == nil {
		for _, line := range strings.Split(routes.Stdout, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			route := RouteFact{Destination: fields[0]}
			for index, value := range fields {
				if value == "dev" && index+1 < len(fields) {
					route.Interface = fields[index+1]
				}
				if value == "via" && index+1 < len(fields) {
					route.Gateway = fields[index+1]
				}
			}
			f.Routes = append(f.Routes, route)
		}
	} else {
		add("routes", FindingNeedsAction, "Host routes could not be inspected", routes.Stderr, "Install the ip utility and rerun preflight.")
	}
	ports, portErr := p.Process.Run(ctx, "ss", "-H", "-lntu")
	if portErr == nil {
		for _, line := range strings.Split(ports.Stdout, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 5 {
				continue
			}
			address := fields[4]
			host, rawPort, err := net.SplitHostPort(address)
			if err != nil {
				last := strings.LastIndex(address, ":")
				if last < 0 {
					continue
				}
				host, rawPort = address[:last], address[last+1:]
			}
			port, _ := strconv.Atoi(rawPort)
			f.ListeningPorts = append(f.ListeningPorts, PortFact{Address: host, Port: port, Protocol: fields[0]})
		}
	} else {
		add("listening_ports", FindingNeedsAction, "Listening sockets could not be inspected", ports.Stderr, "Install ss and rerun preflight.")
	}
	conflicts := []string{}
	for _, port := range f.ListeningPorts {
		if slices.Contains([]int{8080, 9443, 9444, 5432, 9000, 9001}, port.Port) {
			conflicts = append(conflicts, strconv.Itoa(port.Port))
		}
	}
	if len(conflicts) > 0 {
		add("port_conflicts", FindingRemediable, "Proposed loopback ports are occupied", strings.Join(conflicts, ","), "The installer will propose collision-free replacements for review.")
	} else {
		add("port_conflicts", FindingPass, "Proposed loopback ports are unused", "", "")
	}
	resolv, resolvErr := p.Filesystem.ReadFile("/etc/resolv.conf")
	if resolvErr == nil {
		for _, line := range strings.Split(string(resolv), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || fields[0] != "nameserver" {
				continue
			}
			address := net.ParseIP(fields[1])
			if address != nil && !address.IsLoopback() && !address.IsUnspecified() {
				f.DNSUpstreams = append(f.DNSUpstreams, fields[1])
			}
		}
	}
	if len(f.DNSUpstreams) == 0 {
		resolved, resolvedErr := p.Process.Run(ctx, "resolvectl", "dns")
		if resolvedErr == nil {
			for _, line := range strings.Split(resolved.Stdout, "\n") {
				_, values, found := strings.Cut(line, ":")
				if !found {
					continue
				}
				for _, value := range strings.Fields(values) {
					address := net.ParseIP(strings.Trim(value, "[]"))
					if address != nil && !address.IsLoopback() && !address.IsUnspecified() {
						f.DNSUpstreams = append(f.DNSUpstreams, strings.Trim(value, "[]"))
					}
				}
			}
		}
	}
	if len(f.DNSUpstreams) == 0 {
		add("dns_upstream", FindingNeedsAction, "No non-loopback DNS upstream was observed", errorText(resolvErr), "Configure a non-loopback DNS upstream for microVM networking.")
	} else {
		add("dns_upstream", FindingPass, "Non-loopback DNS upstream observed", strings.Join(f.DNSUpstreams, ","), "")
	}
	for _, host := range []string{"github.com", "ghcr.io"} {
		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, dnsErr := p.Network.LookupHost(lookupCtx, host)
		cancel()
		if dnsErr != nil {
			add("dns_"+strings.ReplaceAll(host, ".", "_"), FindingNeedsAction, "Release DNS is unreachable", dnsErr.Error(), "Restore DNS/network access and retry.")
			continue
		}
		headCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, headErr := p.Network.Head(headCtx, "https://"+host+"/")
		cancel()
		if headErr != nil || status >= 500 {
			add("https_"+strings.ReplaceAll(host, ".", "_"), FindingNeedsAction, "Release HTTPS is unreachable", errorText(headErr), "Restore outbound HTTPS and retry.")
		} else {
			add("https_"+strings.ReplaceAll(host, ".", "_"), FindingPass, "Release HTTPS is reachable", strconv.Itoa(status), "")
		}
	}
}
func preflightUsers(p PreflightProbes, f *HostFacts, add func(string, FindingClass, string, string, string)) {
	assigned, err := p.Users.AssignedUIDs()
	if err != nil {
		add("uids", FindingNeedsAction, "Assigned host UIDs could not be inspected", err.Error(), "Repair account database access.")
		return
	}
	for uid := range assigned {
		f.AssignedUIDs = append(f.AssignedUIDs, uid)
	}
	slices.Sort(f.AssignedUIDs)
	if rangeProbe, ok := p.Users.(UserRangeProbe); ok {
		reserved, err := rangeProbe.ReservedIDRanges()
		if err != nil {
			add("uids", FindingNeedsAction, "Subordinate host ID ranges could not be inspected", err.Error(), "Repair /etc/subuid and /etc/subgid access.")
			return
		}
		f.ReservedIDRanges = reserved
	}
	for start := int64(200000); start < 400000 && len(f.CandidateUIDRanges) < 3; start += 64 {
		free := true
		for uid := start; uid < start+64; uid++ {
			if assigned[uid] {
				free = false
				break
			}
		}
		if slices.ContainsFunc(f.ReservedIDRanges, func(reserved UIDRange) bool { return rangesOverlap(UIDRange{Start: start, Count: 64}, reserved) }) {
			free = false
		}
		if free {
			f.CandidateUIDRanges = append(f.CandidateUIDRanges, UIDRange{Start: start, Count: 64})
		}
	}
	if len(f.CandidateUIDRanges) == 0 {
		add("uids", FindingBlocked, "No unassigned jailer UID range was found", "", "Free a contiguous unassigned UID range.")
	} else {
		add("uids", FindingPass, "Unassigned jailer UID ranges found", strconv.Itoa(len(f.CandidateUIDRanges)), "")
	}
}
func preflightUtilities(p PreflightProbes, f *HostFacts, add func(string, FindingClass, string, string, string)) {
	missing := []string{}
	for _, name := range []string{"docker", "systemctl", "systemd-analyze", "ip", "ss"} {
		path, err := p.Process.LookPath(name)
		if err != nil {
			missing = append(missing, name)
		} else {
			f.Utilities[name] = path
		}
	}
	if len(missing) > 0 {
		add("utilities", FindingNeedsAction, "Required host utilities are missing", strings.Join(missing, ","), "Install the listed host utilities before installation.")
	} else {
		add("utilities", FindingPass, "Required host utilities are available", "", "")
	}
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
func boundedDetail(value string) string {
	if len(value) > 512 {
		return value[:512] + "…"
	}
	return value
}
func decodeMount(value string) string {
	return strings.NewReplacer(`\040`, ` `, `\011`, `\t`, `\134`, `\`).Replace(value)
}

func HasBlockingFindings(facts HostFacts) bool {
	for _, finding := range facts.Findings {
		if finding.Class == FindingBlocked || finding.Class == FindingNeedsAction {
			return true
		}
	}
	return false
}

func RenderPreflight(facts HostFacts) string {
	var result strings.Builder
	fmt.Fprintf(&result, "SecondBox single-host preflight\nHost: %s  Platform: %s/%s  Observed: %s\n\n", facts.HostIdentity, facts.OS, facts.Architecture, facts.ObservedAt.Format(time.RFC3339))
	for _, finding := range facts.Findings {
		fmt.Fprintf(&result, "%-12s  %-24s  %s\n", strings.ToUpper(string(finding.Class)), finding.ID, finding.Summary)
		if finding.Detail != "" {
			fmt.Fprintf(&result, "              %s\n", strings.ReplaceAll(finding.Detail, "\n", " "))
		}
		if finding.Remedy != "" {
			fmt.Fprintf(&result, "              Remedy: %s\n", finding.Remedy)
		}
	}
	return result.String()
}
