//go:build linux

package gvisor

import (
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
)

func TestNetworkForIndexShapesSlots(t *testing.T) {
	first := networkForIndex(0, 0)
	if first.hostAddress != "169.254.104.1" || first.guestAddress != "169.254.104.2" ||
		first.hostVeth != "gvh0-0" || first.namespaceName != "sbxgv0-0" {
		t.Fatalf("slot 0 = %+v", first)
	}
	seventh := networkForIndex(0, 7)
	if seventh.hostAddress != "169.254.104.29" || seventh.guestAddress != "169.254.104.30" {
		t.Fatalf("slot 7 = %+v", seventh)
	}
}

func TestNetworkProfilesSeparateRunnersOnOneHost(t *testing.T) {
	relocated := networkForIndex(1, 0)
	if relocated.hostAddress != "169.254.105.1" || relocated.guestAddress != "169.254.105.2" ||
		relocated.hostVeth != "gvh1-0" || relocated.guestVeth != "gvg1-0" ||
		relocated.namespaceName != "sbxgv1-0" {
		t.Fatalf("profile 1 slot 0 = %+v", relocated)
	}
	if dnsAddressForProfile(0) != "169.254.99.53" || dnsAddressForProfile(1) != "169.254.99.54" {
		t.Fatalf("profile DNS addresses = %q, %q", dnsAddressForProfile(0), dnsAddressForProfile(1))
	}
	shared := networkForIndex(0, 0)
	if shared.hostVeth == relocated.hostVeth || shared.hostAddress == relocated.hostAddress ||
		shared.namespaceName == relocated.namespaceName {
		t.Fatalf("profiles collide: %+v vs %+v", shared, relocated)
	}
	last := networkForIndex(15, maximumNetworkSlots-1)
	if last.hostAddress != "198.18.191.253" || last.guestAddress != "198.18.191.254" {
		t.Fatalf("final slot = %+v", last)
	}
}

func TestNetworkSlotBankBoundaries(t *testing.T) {
	for _, test := range []struct {
		index       uint32
		host, guest string
	}{
		{62, "169.254.104.249", "169.254.104.250"},
		{63, "169.254.104.253", "169.254.104.254"},
		{64, "169.254.120.1", "169.254.120.2"},
		{127, "169.254.120.253", "169.254.120.254"},
		{128, "169.254.136.1", "169.254.136.2"},
		{191, "169.254.136.253", "169.254.136.254"},
		{192, "169.254.152.1", "169.254.152.2"},
		{255, "169.254.152.253", "169.254.152.254"},
		{256, "198.18.0.1", "198.18.0.2"},
		{319, "198.18.0.253", "198.18.0.254"},
		{320, "198.18.16.1", "198.18.16.2"},
		{959, "198.18.160.253", "198.18.160.254"},
		{960, "198.18.176.1", "198.18.176.2"},
		{999, "198.18.176.157", "198.18.176.158"},
		{1023, "198.18.176.253", "198.18.176.254"},
	} {
		network := networkForIndex(0, test.index)
		if network.hostAddress != test.host || network.guestAddress != test.guest {
			t.Errorf("slot %d = %+v", test.index, network)
		}
	}
}

func TestAllNetworkSlotsAreDisjointAndPreserveLegacyMappings(t *testing.T) {
	policy, err := networkpolicy.Compile(networkpolicy.Policy{
		Mode:         networkpolicy.ModeAllowList,
		Destinations: []networkpolicy.Destination{{Protocol: networkpolicy.ProtocolTCP, Domain: "allowed.test", Port: 443}},
	}, networkpolicy.CompileOptions{MaximumPins: 1, MaximumTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	subnets := make(map[netip.Prefix]bool)
	names := make(map[string]bool)
	for profile := uint32(0); profile < maximumNetworkProfiles; profile++ {
		for index := uint32(0); index < maximumNetworkSlots; index++ {
			network := networkForIndex(profile, index)
			host := netip.MustParseAddr(network.hostAddress)
			guest := netip.MustParseAddr(network.guestAddress)
			subnet := netip.PrefixFrom(host, 30).Masked()
			if subnets[subnet] || !subnet.Contains(guest) || host.Next() != guest || subnet.Addr().Next() != host {
				t.Fatalf("invalid or overlapping /30: %+v (%s)", network, subnet)
			}
			subnets[subnet] = true
			for _, address := range []netip.Addr{host, guest} {
				if decision := policy.AuthorizeIP(networkpolicy.ProtocolTCP, address, 443); decision.Allowed || decision.Reason != networkpolicy.ReasonProtectedDestination {
					t.Fatalf("slot address %s escaped protected destinations: %+v", address, decision)
				}
				if _, decision := policy.PinDNS(networkpolicy.ProtocolTCP, "allowed.test", 443, []netip.Addr{address}, time.Minute, time.Now()); decision.Allowed || decision.Reason != networkpolicy.ReasonProtectedDNSAnswer {
					t.Fatalf("slot address %s escaped protected DNS answers: %+v", address, decision)
				}
			}
			for _, name := range []string{network.hostVeth, network.guestVeth, network.namespaceName} {
				if names[name] || len(name) > 15 {
					t.Fatalf("duplicate or overlong link/namespace name %q", name)
				}
				names[name] = true
			}
			if index < 63 && (network.hostAddress != fmt.Sprintf("169.254.%d.%d", 104+profile, index*4+1) ||
				network.guestAddress != fmt.Sprintf("169.254.%d.%d", 104+profile, index*4+2)) {
				t.Fatalf("legacy slot moved: %+v", network)
			}
			if index < 256 && (network.hostAddress != fmt.Sprintf("169.254.%d.%d", 104+profile+16*(index/64), (index%64)*4+1) ||
				network.guestAddress != fmt.Sprintf("169.254.%d.%d", 104+profile+16*(index/64), (index%64)*4+2)) {
				t.Fatalf("256-slot mapping moved: %+v", network)
			}
			if index >= 256 && !netip.MustParsePrefix("198.18.0.0/16").Contains(host) {
				t.Fatalf("expanded slot outside benchmarking pool: %+v", network)
			}
			for other := uint32(0); other < maximumNetworkProfiles; other++ {
				if subnet.Contains(netip.MustParseAddr(dnsAddressForProfile(other))) {
					t.Fatalf("slot overlaps DNS profile %d: %+v", other, network)
				}
			}
			for _, reserved := range []string{"169.254.0.0/24", "169.254.255.0/24", "169.254.169.0/24", "169.254.170.0/24", "198.18.192.0/18", "198.19.0.0/16"} {
				if netip.MustParsePrefix(reserved).Contains(host) {
					t.Fatalf("slot overlaps reserved range %s: %+v", reserved, network)
				}
			}
		}
	}
}

func TestNetworkSlotAllocatorReleasesAndBounds(t *testing.T) {
	backend := &AssignmentBackend{}
	first, err := backend.acquireNetworkSlot()
	if err != nil || first.index != 0 {
		t.Fatalf("first slot = %+v, %v", first, err)
	}
	second, err := backend.acquireNetworkSlot()
	if err != nil || second.index != 1 {
		t.Fatalf("second slot = %+v, %v", second, err)
	}
	backend.releaseNetworkSlot(first)
	reused, err := backend.acquireNetworkSlot()
	if err != nil || reused.index != 0 {
		t.Fatalf("reused slot = %+v, %v", reused, err)
	}
	for index := uint32(2); index < maximumNetworkSlots; index++ {
		network, err := backend.acquireNetworkSlot()
		if err != nil || network.index != index {
			t.Fatalf("slot %d = %+v, %v", index, network, err)
		}
	}
	if _, err := backend.acquireNetworkSlot(); err == nil {
		t.Fatal("allocator exceeded its address space")
	}
	last := networkForIndex(0, maximumNetworkSlots-1)
	backend.releaseNetworkSlot(last)
	if network, err := backend.acquireNetworkSlot(); err != nil || network.index != last.index || network.hostAddress != last.hostAddress {
		t.Fatalf("reuse final slot = %+v, %v", network, err)
	}
}

func TestNetworkSlotAllocatorConcurrent(t *testing.T) {
	backend := &AssignmentBackend{}
	results := make(chan instanceNetwork, maximumNetworkSlots)
	var workers sync.WaitGroup
	for range maximumNetworkSlots {
		workers.Go(func() {
			network, err := backend.acquireNetworkSlot()
			if err != nil {
				t.Error(err)
				return
			}
			results <- network
		})
	}
	workers.Wait()
	close(results)
	seen := make(map[uint32]bool)
	for network := range results {
		if seen[network.index] {
			t.Fatalf("concurrent allocation duplicated slot %d", network.index)
		}
		seen[network.index] = true
	}
	if len(seen) != maximumNetworkSlots {
		t.Fatalf("allocated %d slots", len(seen))
	}
}

func TestRenderInetPolicyFailsClosed(t *testing.T) {
	compiled, err := networkpolicy.Compile(networkpolicy.Policy{
		Mode: networkpolicy.ModeAllowList,
		Destinations: []networkpolicy.Destination{
			{Protocol: networkpolicy.ProtocolTCP, Domain: "allowed.test", Port: 8080},
			{Protocol: networkpolicy.ProtocolHTTPS, Prefix: netip.MustParsePrefix("93.184.216.0/24"), Port: 443},
		},
	}, networkpolicy.CompileOptions{MaximumPins: 8, MaximumTTL: 37 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	pins := map[int][]netip.Addr{0: {netip.MustParseAddr("198.51.100.7")}}
	script := renderInetPolicy(
		"sbx_test", "gvh3", "169.254.104.14",
		netip.MustParseAddr(dnsAddressForProfile(0)),
		compiled.AllowsDNS(),
		compiled.ProtectedPrefixes(),
		compiled.Destinations(),
		compiled.RunnerGatewayDestinations(),
		pins,
	)
	for _, required := range []string{
		`add table inet sbx_test`,
		`add chain inet sbx_test forward { type filter hook forward priority -10; policy accept; }`,
		`add rule inet sbx_test forward iifname "gvh3" ip saddr != 169.254.104.14 drop`,
		`add rule inet sbx_test forward oifname "gvh3" ct state established,related accept`,
		`ip daddr 169.254.99.53 udp dport 53 ct mark set 0x53425801 accept`,
		`add rule inet sbx_test input iifname "gvh3" drop`,
		`ip daddr 198.51.100.7 tcp dport 8080 ct mark set 0x53425801 accept`,
		`ip daddr 93.184.216.0/24 tcp dport 443 ct mark set 0x53425801 accept`,
		`add rule inet sbx_test forward iifname "gvh3" drop`,
		`add rule inet sbx_test forward oifname "gvh3" drop`,
		`add table ip sbx_testnat`,
		`add rule ip sbx_testnat postrouting ip saddr 169.254.104.14 oifname != "gvh3" masquerade`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("rendered policy lacks %q\n%s", required, script)
		}
	}
	// Every compiled protected class renders as an explicit forward drop.
	if len(compiled.ProtectedPrefixes()) == 0 {
		t.Fatal("compiled policy carries no protected classes")
	}
	for _, prefix := range compiled.ProtectedPrefixes() {
		if !strings.Contains(script, "daddr "+prefix.String()+" drop") {
			t.Errorf("rendered policy does not drop protected class %s\n%s", prefix, script)
		}
	}
	// The final forward drop must come after every accept.
	lastAccept := strings.LastIndex(script, "accept\n")
	finalDrop := strings.LastIndex(script, `add rule inet sbx_test forward iifname "gvh3" drop`)
	if finalDrop < lastAccept {
		t.Errorf("default drop precedes an accept\n%s", script)
	}
	if deleteInetPolicyTables("sbx_test") != "delete table inet sbx_test\ndelete table ip sbx_testnat\n" {
		t.Errorf("delete rendering = %q", deleteInetPolicyTables("sbx_test"))
	}
}

func TestRenderInetPolicyDenyAll(t *testing.T) {
	compiled, err := networkpolicy.Compile(networkpolicy.Policy{Mode: networkpolicy.ModeDenyAll},
		networkpolicy.CompileOptions{MaximumPins: 8, MaximumTTL: 37 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	script := renderInetPolicy(
		"sbx_deny", "gvh0", "169.254.104.2",
		netip.MustParseAddr(dnsAddressForProfile(0)),
		compiled.AllowsDNS(),
		compiled.ProtectedPrefixes(),
		compiled.Destinations(),
		compiled.RunnerGatewayDestinations(),
		nil,
	)
	if strings.Count(script, "tcp dport") > 1 {
		// Only the DNS TCP admission may appear; no destination accepts exist.
		t.Errorf("deny-all rendered destination accepts\n%s", script)
	}
	if !strings.Contains(script, `add rule inet sbx_deny forward iifname "gvh0" drop`) {
		t.Errorf("deny-all lacks the default drop\n%s", script)
	}
}
