//go:build linux

package gvisor

import (
	"net/netip"
	"strings"
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
	last := networkForIndex(15, maximumNetworkslots-1)
	if last.hostAddress != "169.254.119.249" || last.guestAddress != "169.254.119.250" {
		t.Fatalf("final slot = %+v", last)
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
