package firecracker

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

type failingExecutionForwarder struct {
	closeErr error
	closed   int
}

func (f *failingExecutionForwarder) ListenerAddress() netip.AddrPort {
	return netip.MustParseAddrPort("10.0.0.1:41000")
}
func (f *failingExecutionForwarder) Done() <-chan struct{}       { return nil }
func (f *failingExecutionForwarder) Wait() error                 { return context.Canceled }
func (f *failingExecutionForwarder) Revoke()                     {}
func (f *failingExecutionForwarder) Close(context.Context) error { f.closed++; return f.closeErr }

func TestFailedHostReservationReleasesGuestIP(t *testing.T) {
	m := &Manager{cfg: &config.Config{MicroVMRunDir: t.TempDir(), MicroVMBridgeCIDR: "10.0.0.1/24", MicroVMBridgeName: "testbr", MicroVMAllowUnjailed: true}, guestIPs: map[string]string{}}
	host, err := m.reserveInstanceHost(t.Context(), t.Context(), "sandbox", "instance", runtimemanager.StartOpts{})
	if err == nil || host != nil {
		t.Fatal("missing network configurer must reject startup")
	}
	if len(m.guestIPs) != 0 {
		t.Fatalf("failed reservation leaked guest IP: %+v", m.guestIPs)
	}
}

func TestExecutionForwarderCleanupRetainsGuestIdentityOnFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		m := &Manager{cfg: &config.Config{MicroVMAllowUnjailed: true}, guestIPs: map[string]string{"instance": "10.0.0.2"}}
		forwarder := &failingExecutionForwarder{}
		if failure {
			forwarder.closeErr = errors.New("forwarder cleanup failed")
		}
		directory := t.TempDir()
		host := &instanceHostReservation{manager: m, id: "instance", guestIP: "10.0.0.2", releaseIP: true, executionForwarder: forwarder, dir: directory, cleanupDir: true}
		cause := errors.New("launch failed")
		if err := host.joinNetworkCleanup(t.Context(), cause); !errors.Is(err, cause) || errors.Is(err, forwarder.closeErr) != failure {
			t.Fatalf("wrong cleanup result: %v", err)
		}
		host.release()
		if _, err := os.Stat(directory); (err == nil) != failure {
			t.Fatalf("recovery directory retention: failure=%t err=%v", failure, err)
		}
		if forwarder.closed == 0 || (len(m.guestIPs) != 0) != failure {
			t.Fatalf("identity release did not follow forwarder cleanup: calls=%d IPs=%v", forwarder.closed, m.guestIPs)
		}
	}
}

func TestAttributedHostReservationRequiresCompleteNetworkBinding(t *testing.T) {
	m := &Manager{cfg: &config.Config{MicroVMAllowUnjailed: true}}
	for _, opts := range []runtimemanager.StartOpts{
		{AttributedExecution: &runtimemanager.AttributedExecutionGuard{}},
		{ExecutionNetwork: &runtimemanager.AttributedExecutionNetwork{}},
		{AttributedExecution: &runtimemanager.AttributedExecutionGuard{}, ExecutionNetwork: &runtimemanager.AttributedExecutionNetwork{}, TemplateMode: true},
	} {
		if _, err := m.reserveInstanceHost(t.Context(), t.Context(), "sandbox", "instance", opts); err == nil {
			t.Fatal("incomplete attributed network setup accepted")
		}
	}
}
