//go:build linux && listener_qualification

package firecracker

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

// This network-only qualification needs an isolated namespace and /dev/net/tun.
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
	for _, failStartup := range []bool{false, true} {
		expiry := time.Now().Add(time.Minute)
		guard, err := runtimemanager.NewAttributedExecutionGuard("assignment", expiry)
		if err != nil {
			t.Fatal(err)
		}
		opts := runtimemanager.StartOpts{AttributedExecution: guard, ExecutionNetwork: &runtimemanager.AttributedExecutionNetwork{
			GatewaySocket: gateway.Addr().String(), MaximumConnections: 2,
			CompileOptions: networkpolicy.CompileOptions{MaximumPins: 1, MaximumTTL: time.Second},
			Attribution:    egressattribution.ExecutionAttribution{TenantRef: "tenant", SubjectRef: "subject", SandboxID: "sandbox", InstanceID: "instance", AssignmentID: "assignment", Generation: 1, AuthorizationRef: "authorization", ExpiresAt: expiry},
		}}
		startupErr := errors.New("injected post-network startup failure")
		if failStartup {
			opts.StartupProgress = func(runtimemanager.StartupStage) error { return startupErr }
		}
		host, err := m.reserveInstanceHost(t.Context(), t.Context(), "sandbox", "instance", opts)
		if failStartup {
			if !errors.Is(err, startupErr) || host != nil {
				t.Fatalf("failed launch result: host=%v err=%v", host, err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := host.joinNetworkCleanup(t.Context(), nil); err != nil {
					t.Error(err)
				}
				host.release()
			})
			if host.guestIP != "10.254.72.2" || host.executionForwarder == nil {
				t.Fatalf("wrong network reservation: %+v", host)
			}
			endpoint := host.executionForwarder.ListenerAddress()
			if connection, err := net.DialTimeout("tcp", endpoint.String(), 300*time.Millisecond); err == nil {
				connection.Close()
				t.Fatal("host reached private execution listener")
			}
			host.executionForwarder.Revoke()
			if err := host.executionForwarder.Wait(); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			host.executionForwarder = nil
			recovered := &Manager{cfg: m.cfg, network: m.network, networkPolicy: m.networkPolicy}
			if err := recovered.sweepStartupOrphans(t.Context()); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command("nft", "list", "tables").CombinedOutput()
			if err != nil || strings.Contains(string(output), "sbx_exec_") {
				t.Fatalf("restart left execution rules: %v: %s", err, output)
			}
			if err := host.joinNetworkCleanup(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			host.release()
		}
		if len(m.guestIPs) != 0 {
			t.Fatalf("guest identity leaked: %+v", m.guestIPs)
		}
		output, err := exec.Command("nft", "list", "tables").CombinedOutput()
		if err != nil || strings.Contains(string(output), "sbx_exec_") || strings.Contains(string(output), "secondbox_") {
			t.Fatalf("execution firewall cleanup: %v: %s", err, output)
		}
	}
}
