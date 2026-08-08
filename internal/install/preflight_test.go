package install

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeInfo struct{ mode fs.FileMode }

func (fakeInfo) Name() string           { return "device" }
func (fakeInfo) Size() int64            { return 0 }
func (info fakeInfo) Mode() fs.FileMode { return info.mode }
func (fakeInfo) ModTime() time.Time     { return time.Time{} }
func (fakeInfo) IsDir() bool            { return false }
func (fakeInfo) Sys() any               { return nil }

type fakeFilesystem struct {
	files       map[string]string
	lstatErrors map[string]error
	openErrors  map[string]error
	stats       map[string][2]int64
}

func (probe *fakeFilesystem) ReadFile(path string) ([]byte, error) {
	value, ok := probe.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(value), nil
}
func (probe *fakeFilesystem) Lstat(path string) (fs.FileInfo, error) {
	if err := probe.lstatErrors[path]; err != nil {
		return nil, err
	}
	return fakeInfo{mode: fs.ModeDevice}, nil
}
func (probe *fakeFilesystem) OpenReadWrite(path string) error { return probe.openErrors[path] }
func (probe *fakeFilesystem) StatFS(path string) (int64, int64, error) {
	value, ok := probe.stats[path]
	if !ok {
		return 0, 0, fs.ErrNotExist
	}
	return value[0], value[1], nil
}

type fakeProcess struct {
	results map[string]CommandResult
	errors  map[string]error
	missing map[string]bool
}

func (probe *fakeProcess) Run(_ context.Context, name string, args ...string) (CommandResult, error) {
	key := name + " " + strings.Join(args, " ")
	return probe.results[key], probe.errors[key]
}
func (probe *fakeProcess) LookPath(name string) (string, error) {
	if probe.missing[name] {
		return "", fs.ErrNotExist
	}
	return "/usr/bin/" + name, nil
}

type fakeNetwork struct {
	lookupErr error
	headErr   error
	status    int
}

func (probe fakeNetwork) LookupHost(context.Context, string) ([]string, error) {
	return []string{"192.0.2.1"}, probe.lookupErr
}
func (probe fakeNetwork) Head(context.Context, string) (int, error) {
	return probe.status, probe.headErr
}

type fakeClock struct{ now time.Time }

func (clock fakeClock) Now() time.Time { return clock.now }

type fakeUsers struct {
	assigned map[int64]bool
	err      error
}

func (users fakeUsers) AssignedUIDs() (map[int64]bool, error) { return users.assigned, users.err }

type fakeUsersWithRanges struct {
	fakeUsers
	ranges []UIDRange
}

func (users fakeUsersWithRanges) ReservedIDRanges() ([]UIDRange, error) { return users.ranges, nil }

func qualifiedProbes() PreflightProbes {
	files := map[string]string{"/etc/machine-id": "host-1\n", "/sys/fs/cgroup/cgroup.controllers": "cpu memory pids io\n", "/proc/filesystems": "nodev\tbtrfs\n\txfs\n", "/proc/cpuinfo": "processor: 0\nflags : fpu vmx sse\n", "/proc/meminfo": "MemTotal:       33554432 kB\n", "/proc/self/mountinfo": "22 1 8:1 / / rw - ext4 /dev/root rw\n23 1 8:2 / /srv/workspace rw - xfs /dev/sdb rw\n", "/etc/resolv.conf": "nameserver 192.0.2.53\n"}
	process := &fakeProcess{results: map[string]CommandResult{"uname -r": {Stdout: "6.12.0"}, "systemd --version": {Stdout: "systemd 257"}, "systemctl is-system-running": {Stdout: "running"}, "docker version --format {{.Server.Version}}": {Stdout: "27.5.1"}, "docker compose version --short": {Stdout: "2.32.4"}, "docker compose ls --format json": {Stdout: "[]"}, "ip -o route show": {Stdout: "default via 192.0.2.1 dev eth0\n192.0.2.0/24 dev eth0"}, "ss -H -lntu": {Stdout: "tcp LISTEN 0 128 127.0.0.1:22"}}, errors: map[string]error{}, missing: map[string]bool{}}
	return PreflightProbes{Filesystem: &fakeFilesystem{files: files, lstatErrors: map[string]error{}, openErrors: map[string]error{}, stats: map[string][2]int64{"/srv/workspace": {200 << 30, 250 << 30}}}, Process: process, Network: fakeNetwork{status: 200}, Clock: fakeClock{now: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}, Users: fakeUsers{assigned: map[int64]bool{0: true, 1000: true}}, LookupEnv: func(string) (string, bool) { return "", false }, OS: "linux", Architecture: "amd64", CPUCount: 8, InvokingUID: 1000, InvokingGID: 1000}
}

func findingByID(t *testing.T, facts HostFacts, id string) Finding {
	t.Helper()
	for _, finding := range facts.Findings {
		if finding.ID == id {
			return finding
		}
	}
	t.Fatalf("finding %q absent", id)
	return Finding{}
}

func TestQualifiedPreflightAggregatesReadOnlyFacts(t *testing.T) {
	facts, err := Preflight(context.Background(), qualifiedProbes())
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.Validate(); err != nil {
		t.Fatal(err)
	}
	if HasBlockingFindings(facts) {
		t.Fatalf("qualified findings blocked: %#v", facts.Findings)
	}
	if len(facts.Devices) != 1 || facts.Devices[0].Identity != "8:2" {
		t.Fatalf("devices = %#v", facts.Devices)
	}
	if len(facts.CandidateUIDRanges) == 0 {
		t.Fatal("no UID range")
	}
	report := RenderPreflight(facts)
	for _, text := range []string{"SecondBox single-host preflight", "PASS", "workspace_filesystem", "machine-id:host-1"} {
		if !strings.Contains(report, text) {
			t.Fatalf("report lacks %q:\n%s", text, report)
		}
	}
}

func TestPreflightAcceptsSystemdDegradedExitStatusAsWarning(t *testing.T) {
	probes := qualifiedProbes()
	process := probes.Process.(*fakeProcess)
	process.results["systemctl is-system-running"] = CommandResult{Stdout: "degraded"}
	process.errors["systemctl is-system-running"] = errors.New("systemd reports degraded")
	facts, err := Preflight(context.Background(), probes)
	if err != nil {
		t.Fatal(err)
	}
	if finding := findingByID(t, facts, "systemd"); finding.Class != FindingWarning {
		t.Fatalf("degraded systemd finding = %#v", finding)
	}
}

func TestPreflightRejectsNonDefaultDockerSelection(t *testing.T) {
	for name, value := range map[string]string{"DOCKER_HOST-remote": "tcp://198.51.100.10:2376", "DOCKER_HOST-local": "unix:///run/docker.sock", "DOCKER_CONTEXT": "production"} {
		t.Run(name, func(t *testing.T) {
			probes := qualifiedProbes()
			probes.LookupEnv = func(candidate string) (string, bool) {
				variable := name
				if strings.HasPrefix(name, "DOCKER_HOST-") {
					variable = "DOCKER_HOST"
				}
				return value, candidate == variable
			}
			facts, err := Preflight(context.Background(), probes)
			if err != nil {
				t.Fatal(err)
			}
			if finding := findingByID(t, facts, "docker_context"); finding.Class != FindingBlocked {
				t.Fatalf("remote Docker finding = %#v", finding)
			}
		})
	}
}

func TestPreflightWorkspaceCandidateRequiresSandboxMinimum(t *testing.T) {
	for _, test := range []struct {
		name      string
		available int64
		want      FindingClass
	}{{"below", MinimumWorkspaceBytes - 1, FindingRemediable}, {"exact", MinimumWorkspaceBytes, FindingPass}} {
		t.Run(test.name, func(t *testing.T) {
			probes := qualifiedProbes()
			probes.Filesystem.(*fakeFilesystem).stats["/srv/workspace"] = [2]int64{test.available, 250 << 30}
			facts, err := Preflight(context.Background(), probes)
			if err != nil {
				t.Fatal(err)
			}
			if finding := findingByID(t, facts, "workspace_filesystem"); finding.Class != test.want {
				t.Fatalf("workspace finding = %#v", finding)
			}
		})
	}
}

func TestPreflightResolvesStubDNSAndAvoidsSubordinateIDs(t *testing.T) {
	probes := qualifiedProbes()
	filesystem := probes.Filesystem.(*fakeFilesystem)
	filesystem.files["/etc/resolv.conf"] = "nameserver 127.0.0.53\n"
	process := probes.Process.(*fakeProcess)
	process.results["resolvectl dns"] = CommandResult{Stdout: "Global: 192.0.2.54\nLink 2 (eth0): 2001:db8::53"}
	probes.Users = fakeUsersWithRanges{fakeUsers: fakeUsers{assigned: map[int64]bool{0: true, 1000: true}}, ranges: []UIDRange{{Start: 200000, Count: 64}}}
	facts, err := Preflight(context.Background(), probes)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(facts.DNSUpstreams, "192.0.2.54") || !slices.Contains(facts.DNSUpstreams, "2001:db8::53") {
		t.Fatalf("resolved upstreams = %#v", facts.DNSUpstreams)
	}
	if len(facts.CandidateUIDRanges) == 0 || rangesOverlap(facts.CandidateUIDRanges[0], UIDRange{Start: 200000, Count: 64}) {
		t.Fatalf("candidate ranges overlap subordinate allocation: %#v", facts.CandidateUIDRanges)
	}
}

func TestPreflightClassifiesAllFindingsWithoutShortCircuit(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PreflightProbes)
		want   map[string]FindingClass
	}{
		{"unsupported architecture", func(p *PreflightProbes) { p.Architecture = "arm64" }, map[string]FindingClass{"platform": FindingBlocked}},
		{"inactive systemd", func(p *PreflightProbes) {
			p.Process.(*fakeProcess).results["systemctl is-system-running"] = CommandResult{Stdout: "offline"}
			p.Process.(*fakeProcess).errors["systemctl is-system-running"] = errors.New("exit 1")
		}, map[string]FindingClass{"systemd": FindingBlocked}},
		{"docker permission and compose missing", func(p *PreflightProbes) {
			process := p.Process.(*fakeProcess)
			process.results["docker version --format {{.Server.Version}}"] = CommandResult{Stderr: "permission denied"}
			process.errors["docker version --format {{.Server.Version}}"] = errors.New("exit 1")
			process.errors["docker compose version --short"] = errors.New("exit 1")
		}, map[string]FindingClass{"docker": FindingNeedsAction, "compose": FindingNeedsAction}},
		{"cgroup v1", func(p *PreflightProbes) {
			delete(p.Filesystem.(*fakeFilesystem).files, "/sys/fs/cgroup/cgroup.controllers")
		}, map[string]FindingClass{"cgroup": FindingBlocked}},
		{"KVM and TUN permission", func(p *PreflightProbes) {
			filesystem := p.Filesystem.(*fakeFilesystem)
			filesystem.openErrors["/dev/kvm"] = fs.ErrPermission
			filesystem.openErrors["/dev/net/tun"] = fs.ErrPermission
		}, map[string]FindingClass{"kvm": FindingNeedsAction, "tun": FindingNeedsAction}},
		{"low resources and port conflict", func(p *PreflightProbes) {
			p.CPUCount = 2
			p.Filesystem.(*fakeFilesystem).files["/proc/meminfo"] = "MemTotal: 4194304 kB\n"
			p.Process.(*fakeProcess).results["ss -H -lntu"] = CommandResult{Stdout: "tcp LISTEN 0 128 127.0.0.1:8080"}
		}, map[string]FindingClass{"memory": FindingNeedsAction, "cpu": FindingNeedsAction, "port_conflicts": FindingRemediable}},
		{"missing utilities and offline", func(p *PreflightProbes) {
			p.Process.(*fakeProcess).missing["systemd-analyze"] = true
			p.Network = fakeNetwork{lookupErr: errors.New("offline")}
		}, map[string]FindingClass{"utilities": FindingNeedsAction, "dns_github_com": FindingNeedsAction, "dns_ghcr_io": FindingNeedsAction}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probes := qualifiedProbes()
			test.mutate(&probes)
			facts, err := Preflight(context.Background(), probes)
			if err != nil {
				t.Fatal(err)
			}
			for id, class := range test.want {
				if got := findingByID(t, facts, id).Class; got != class {
					t.Errorf("%s class = %s, want %s", id, got, class)
				}
			}
			if len(facts.Findings) < 15 {
				t.Fatalf("preflight stopped early with %d findings", len(facts.Findings))
			}
		})
	}
}

func TestPreflightReportsMissingDeviceSeparately(t *testing.T) {
	probes := qualifiedProbes()
	probes.Filesystem.(*fakeFilesystem).lstatErrors["/dev/kvm"] = fs.ErrNotExist
	facts, err := Preflight(context.Background(), probes)
	if err != nil {
		t.Fatal(err)
	}
	if findingByID(t, facts, "kvm").Class != FindingBlocked {
		t.Fatal("missing KVM was not blocked")
	}
	if findingByID(t, facts, "tun").Class != FindingPass {
		t.Fatal("TUN finding was lost")
	}
}
