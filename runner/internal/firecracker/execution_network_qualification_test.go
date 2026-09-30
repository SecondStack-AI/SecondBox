//go:build linux && listener_qualification

package firecracker

import (
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/egressforwarder"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

// This network-only qualification needs an isolated namespace and /dev/net/tun.
// An attributed exec window opens and closes on an ordinary reserved Instance
// network without touching the Instance; startup sweeps a crashed window.
func TestFirecrackerExecutionNetworkQualification(t *testing.T) {
	gateway, err := net.ListenUnix("unix", &net.UnixAddr{Name: shortUnixSocketPath(t, "gateway.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	m := &Manager{cfg: &config.Config{
		MicroVMRunDir: t.TempDir(), MicroVMLogDir: t.TempDir(), MicroVMAllowUnjailed: true,
		MicroVMBridgeName: "execbr", MicroVMBridgeCIDR: "10.254.72.1/29", MicroVMTapPrefix: "extap",
		NetworkPolicyNFTPath: "/usr/sbin/nft",
	}, network: IPTapConfigurer{}, guestIPs: map[string]string{}}
	m.networkPolicy = NewNetworkPolicyEnforcer(m.cfg.NetworkPolicyNFTPath, netip.Addr{}, netip.AddrPort{}, nil, nil)
	t.Cleanup(func() {
		if output, err := exec.Command("ip", "link", "delete", "execbr").CombinedOutput(); err != nil {
			t.Errorf("bridge cleanup: %v: %s", err, output)
		}
	})
	denyAll, err := networkpolicy.Compile(networkpolicy.Policy{Mode: networkpolicy.ModeDenyAll}, networkpolicy.CompileOptions{MaximumPins: 1, MaximumTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	host, err := m.reserveInstanceHost(t.Context(), t.Context(), "sandbox", "instance", runtimemanager.StartOpts{NetworkPolicy: denyAll})
	if err != nil {
		t.Fatal(err)
	}
	openWindow := func(reference string) *AttributedExecWindow {
		t.Helper()
		expiry := time.Now().Add(time.Minute)
		window, err := OpenAttributedExecWindow(t.Context(), AttributedExecWindowConfig{
			NFTPath: m.cfg.NetworkPolicyNFTPath, Policy: m.networkPolicy, PolicyInstanceID: host.id,
			Gateway: AttributedExecGateway{Socket: gateway.Addr().String(), MaximumConnections: 2},
			Fence:   &runnerprotocol.AssignmentFence{AssignmentId: "assignment", SandboxId: "sandbox", InstanceId: "instance", SandboxGeneration: 1},
			Open: &runnerprotocol.ExecOpen{DeadlineUnixMs: uint64(expiry.UnixMilli()), AttributedExecution: &runnerprotocol.AttributedExecution{
				TenantRef: "tenant", SubjectRef: "subject", AuthorizationRef: reference, ExpiresAtUnixMs: uint64(expiry.UnixMilli()),
			}},
			Listener: egressforwarder.ExecutionListenerPolicy{
				GuestInterface: host.tapName, BridgeInterface: m.cfg.MicroVMBridgeName,
				GuestAddress: netip.MustParseAddr(host.guestIP), ListenerAddress: netip.MustParseAddrPort("10.254.72.1:0"),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return window
	}
	nftTables := func() string {
		t.Helper()
		output, err := exec.Command("nft", "list", "tables").CombinedOutput()
		if err != nil {
			t.Fatalf("list nftables: %v: %s", err, output)
		}
		return string(output)
	}
	policyTable := func() string {
		t.Helper()
		output, err := exec.Command("nft", "list", "table", "bridge", nftTableName(host.id)).CombinedOutput()
		if err != nil {
			t.Fatalf("list Instance policy: %v: %s", err, output)
		}
		return string(output)
	}
	first := openWindow("authorization-1")
	second := openWindow("authorization-2")
	if first.Gateway() == second.Gateway() || strings.Count(nftTables(), "sbx_exec_") != 4 {
		t.Fatalf("concurrent windows must own distinct listeners and tables: %s %s\n%s", first.Gateway(), second.Gateway(), nftTables())
	}
	for _, window := range []*AttributedExecWindow{first, second} {
		if !strings.Contains(policyTable(), "tcp dport "+strconv.Itoa(int(window.Gateway().Port()))) {
			t.Fatalf("window listener %s missing from Instance policy:\n%s", window.Gateway(), policyTable())
		}
		if connection, err := net.DialTimeout("tcp", window.Gateway().String(), 300*time.Millisecond); err == nil {
			connection.Close()
			t.Fatal("host reached a private execution listener")
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if rules := policyTable(); strings.Contains(rules, "tcp dport "+strconv.Itoa(int(first.Gateway().Port()))) ||
		!strings.Contains(rules, "tcp dport "+strconv.Itoa(int(second.Gateway().Port()))) || strings.Count(nftTables(), "sbx_exec_") != 2 {
		t.Fatalf("closing one window must remove only its rules:\n%s\n%s", rules, nftTables())
	}
	// A Runner crash leaves the second window's rules behind; startup sweeps them
	// with the orphaned Instance network.
	recovered := &Manager{cfg: m.cfg, network: m.network, networkPolicy: m.networkPolicy}
	if err := recovered.sweepStartupOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if tables := nftTables(); strings.Contains(tables, "sbx_exec_") || strings.Contains(tables, "secondbox_") {
		t.Fatalf("restart left execution rules: %s", tables)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("closing a window after its Instance policy was swept = %v", err)
	}
	if err := host.joinNetworkCleanup(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	host.release()
	if len(m.guestIPs) != 0 {
		t.Fatalf("guest identity leaked: %+v", m.guestIPs)
	}
}
