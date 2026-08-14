package microvmguest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func templateNetworkIdentity() AssignmentNetworkIdentity {
	return AssignmentNetworkIdentity{
		Interface:   "eth0",
		MACAddress:  "02:9f:1c:44:5e:07",
		AddressCIDR: "198.18.7.13/24",
		Gateway:     "198.18.7.1",
		Nameserver:  "198.18.7.1",
	}
}

func TestAssignmentNetworkIdentityResolvesCompleteIdentity(t *testing.T) {
	resolved, err := templateNetworkIdentity().resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.name != "eth0" {
		t.Fatalf("interface = %q", resolved.name)
	}
	if resolved.hardware.String() != "02:9f:1c:44:5e:07" {
		t.Fatalf("hardware address = %q", resolved.hardware)
	}
	if resolved.address.String() != "198.18.7.13" || resolved.prefixBits != 24 {
		t.Fatalf("address = %s/%d", resolved.address, resolved.prefixBits)
	}
	if resolved.broadcast.String() != "198.18.7.255" {
		t.Fatalf("broadcast = %s", resolved.broadcast)
	}
	if resolved.gateway.String() != "198.18.7.1" {
		t.Fatalf("gateway = %s", resolved.gateway)
	}
	if resolved.nameserver.String() != "198.18.7.1" {
		t.Fatalf("nameserver = %s", resolved.nameserver)
	}
}

func TestAssignmentNetworkIdentityBroadcastFollowsPrefix(t *testing.T) {
	for _, testCase := range []struct {
		cidr      string
		broadcast string
	}{
		{"10.0.0.5/8", "10.255.255.255"},
		{"172.20.3.9/16", "172.20.255.255"},
		{"198.18.7.13/24", "198.18.7.255"},
		{"192.168.4.130/28", "192.168.4.143"},
		{"192.168.4.130/30", "192.168.4.131"},
	} {
		identity := templateNetworkIdentity()
		identity.AddressCIDR = testCase.cidr
		identity.Gateway = testCase.broadcast
		identity.Nameserver = testCase.broadcast
		resolved, err := identity.resolve()
		if err != nil {
			t.Fatalf("resolve %s: %v", testCase.cidr, err)
		}
		if resolved.broadcast.String() != testCase.broadcast {
			t.Fatalf("%s broadcast = %s, want %s", testCase.cidr, resolved.broadcast, testCase.broadcast)
		}
	}
}

func TestAssignmentNetworkIdentityRejectsUnusableValues(t *testing.T) {
	cases := map[string]func(*AssignmentNetworkIdentity){
		"blank interface":     func(i *AssignmentNetworkIdentity) { i.Interface = "  " },
		"overlong interface":  func(i *AssignmentNetworkIdentity) { i.Interface = strings.Repeat("e", 16) },
		"blank mac":           func(i *AssignmentNetworkIdentity) { i.MACAddress = "" },
		"malformed mac":       func(i *AssignmentNetworkIdentity) { i.MACAddress = "02:9f:1c:44:5e" },
		"eui-64 mac":          func(i *AssignmentNetworkIdentity) { i.MACAddress = "02:9f:1c:44:5e:07:08:09" },
		"group mac":           func(i *AssignmentNetworkIdentity) { i.MACAddress = "03:9f:1c:44:5e:07" },
		"blank address":       func(i *AssignmentNetworkIdentity) { i.AddressCIDR = "" },
		"bare address":        func(i *AssignmentNetworkIdentity) { i.AddressCIDR = "198.18.7.13" },
		"ipv6 address":        func(i *AssignmentNetworkIdentity) { i.AddressCIDR = "fd00::1/64" },
		"host prefix":         func(i *AssignmentNetworkIdentity) { i.AddressCIDR = "198.18.7.13/32" },
		"blank gateway":       func(i *AssignmentNetworkIdentity) { i.Gateway = "" },
		"ipv6 gateway":        func(i *AssignmentNetworkIdentity) { i.Gateway = "fd00::1" },
		"off-link gateway":    func(i *AssignmentNetworkIdentity) { i.Gateway = "198.19.7.1" },
		"gateway is this vm":  func(i *AssignmentNetworkIdentity) { i.Gateway = "198.18.7.13" },
		"malformed gateway":   func(i *AssignmentNetworkIdentity) { i.Gateway = "198.18.7.300" },
		"gateway with prefix": func(i *AssignmentNetworkIdentity) { i.Gateway = "198.18.7.1/24" },
		"blank nameserver":    func(i *AssignmentNetworkIdentity) { i.Nameserver = "" },
		"off-gateway nameserver": func(i *AssignmentNetworkIdentity) {
			i.Nameserver = "198.18.7.2"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			identity := templateNetworkIdentity()
			mutate(&identity)
			if _, err := identity.resolve(); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

type fakeNetworkConfigurer struct {
	configured int
	identity   AssignmentNetworkIdentity
	err        error
}

func (f *fakeNetworkConfigurer) Configure(_ context.Context, identity AssignmentNetworkIdentity) error {
	f.configured++
	f.identity = identity
	return f.err
}

func newNetworkTemplateGuestServer(t *testing.T) (Server, *fakeWorkspaceMounter, *fakeNetworkConfigurer) {
	t.Helper()
	mounter := &fakeWorkspaceMounter{}
	networker := &fakeNetworkConfigurer{}
	return Server{
		WorkspaceDir:      t.TempDir(),
		RuntimePrivateDir: t.TempDir(),
		Hardener:          &fakeHardener{},
		Mounter:           mounter,
		Networker:         networker,
		Assignment:        NewAssignmentGate(),
	}, mounter, networker
}

func TestAssignmentBindInstallsNetworkIdentityOnce(t *testing.T) {
	server, mounter, networker := newNetworkTemplateGuestServer(t)
	postTemplateControl(t, server, "/restore/harden", RestoreHardenRequest{})
	request := templateBindRequest()
	identity := templateNetworkIdentity()
	request.Network = &identity
	if recorder := postTemplateControl(t, server, "/assignment/bind", request); recorder.Code != http.StatusOK {
		t.Fatalf("bind status = %d body %s", recorder.Code, recorder.Body.String())
	}
	if mounter.mounts != 1 {
		t.Fatalf("workspace mounts = %d", mounter.mounts)
	}
	if networker.configured != 1 {
		t.Fatalf("network installs = %d", networker.configured)
	}
	if networker.identity != identity {
		t.Fatalf("installed identity = %+v", networker.identity)
	}
	// The one permitted bind already happened, so the second cannot reconfigure
	// the interface.
	if recorder := postTemplateControl(t, server, "/assignment/bind", request); recorder.Code != http.StatusConflict {
		t.Fatalf("second bind status = %d", recorder.Code)
	}
	if networker.configured != 1 {
		t.Fatalf("network installs after second bind = %d", networker.configured)
	}
}

func TestAssignmentBindWithoutNetworkInstallsNone(t *testing.T) {
	server, mounter, networker := newNetworkTemplateGuestServer(t)
	postTemplateControl(t, server, "/restore/harden", RestoreHardenRequest{})
	if recorder := postTemplateControl(t, server, "/assignment/bind", templateBindRequest()); recorder.Code != http.StatusOK {
		t.Fatalf("bind status = %d body %s", recorder.Code, recorder.Body.String())
	}
	if mounter.mounts != 1 {
		t.Fatalf("workspace mounts = %d", mounter.mounts)
	}
	if networker.configured != 0 {
		t.Fatalf("network installs = %d without a network identity", networker.configured)
	}
}

func TestAssignmentBindLeavesGuestUnboundWhenNetworkInstallFails(t *testing.T) {
	server, mounter, networker := newNetworkTemplateGuestServer(t)
	networker.err = errors.New("rtnetlink rejected")
	postTemplateControl(t, server, "/restore/harden", RestoreHardenRequest{})
	request := templateBindRequest()
	identity := templateNetworkIdentity()
	request.Network = &identity
	recorder := postTemplateControl(t, server, "/assignment/bind", request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("bind status = %d body %s", recorder.Code, recorder.Body.String())
	}
	if mounter.mounts != 1 {
		t.Fatalf("workspace mounts = %d", mounter.mounts)
	}
	if _, bound := server.Assignment.Identity(); bound {
		t.Fatal("a guest whose network identity failed to install must stay unbound")
	}
}

// The network install is the step that brings the link up, so it must be the
// last thing the bind does. A guest that acquired a live interface before its
// Workspace was mounted could emit frames while still incomplete.
func TestAssignmentBindConfiguresNetworkAfterWorkspaceMount(t *testing.T) {
	var order []string
	mounter := &orderRecordingMounter{record: func() { order = append(order, "mount") }}
	networker := &orderRecordingNetworker{record: func() { order = append(order, "network") }}
	server := Server{
		WorkspaceDir:      t.TempDir(),
		RuntimePrivateDir: t.TempDir(),
		Hardener:          &fakeHardener{},
		Mounter:           mounter,
		Networker:         networker,
		Assignment:        NewAssignmentGate(),
	}
	postTemplateControl(t, server, "/restore/harden", RestoreHardenRequest{})
	request := templateBindRequest()
	identity := templateNetworkIdentity()
	request.Network = &identity
	if recorder := postTemplateControl(t, server, "/assignment/bind", request); recorder.Code != http.StatusOK {
		t.Fatalf("bind status = %d body %s", recorder.Code, recorder.Body.String())
	}
	if len(order) != 2 || order[0] != "mount" || order[1] != "network" {
		t.Fatalf("bind install order = %v", order)
	}
}

type orderRecordingMounter struct{ record func() }

func (m *orderRecordingMounter) Mount(context.Context, string, bool) error {
	m.record()
	return nil
}

type orderRecordingNetworker struct{ record func() }

func (n *orderRecordingNetworker) Configure(context.Context, AssignmentNetworkIdentity) error {
	n.record()
	return nil
}
