package firecracker

import (
	"testing"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

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
