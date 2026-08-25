package firecracker

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
)

// TestValidateDNSResponseForwardsStrictlyEmptyAnswerUnpinned pins the strict
// resolver compatibility contract: an empty NOERROR or NXDOMAIN response
// (commonly the AAAA half of a dual-family query) is forwarded with nothing
// to pin, while any answer carrying unrelated records stays rejected.
func TestValidateDNSResponseForwardsStrictlyEmptyAnswerUnpinned(t *testing.T) {
	name := dnsmessage.MustNewName("allowed.test.")
	question := dnsmessage.Question{Name: name, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}
	queryMessage := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 7, RecursionDesired: true},
		Questions: []dnsmessage.Question{question},
	}
	packedQuery, err := queryMessage.Pack()
	if err != nil {
		t.Fatal(err)
	}
	validated, err := validateDNSQuery(packedQuery)
	if err != nil {
		t.Fatal(err)
	}
	response := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID: 7, Response: true, RecursionDesired: true, RecursionAvailable: true,
		},
		Questions: []dnsmessage.Question{question},
	}
	packedResponse, err := response.Pack()
	if err != nil {
		t.Fatal(err)
	}
	addresses, ttl, err := validateDNSResponse(validated, packedResponse)
	if err != nil {
		t.Fatalf("strictly empty answer was rejected: %v", err)
	}
	if len(addresses) != 0 || ttl != 0 {
		t.Fatalf("strictly empty answer pinned %v ttl=%v", addresses, ttl)
	}
}

// TestNFTablesNetworkPolicyPinExpiryHandlesFQDNObservations pins the
// regression where the proxy's trailing-dot FQDN never matched the
// normalized destination domain at expiry, leaving expired pins rendered
// forever.
func TestNFTablesNetworkPolicyPinExpiryHandlesFQDNObservations(t *testing.T) {
	policy, err := networkpolicy.Compile(networkpolicy.Policy{
		Mode: networkpolicy.ModeAllowList,
		Destinations: []networkpolicy.Destination{{
			Protocol: networkpolicy.ProtocolHTTPS,
			Domain:   "api.example.com",
			Port:     443,
		}},
	}, networkpolicy.CompileOptions{MaximumPins: 2, MaximumTTL: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var scripts []string
	enforcer := &NFTablesNetworkPolicyEnforcer{
		nftPath: "/usr/sbin/nft",
		run: func(_ context.Context, _ string, _ []string, stdin string) ([]byte, error) {
			mu.Lock()
			scripts = append(scripts, stdin)
			mu.Unlock()
			return nil, nil
		},
	}
	cfg := PolicyNetworkConfig{InstanceID: "fc-fqdn", TapName: "tap-f", GuestIP: "10.0.0.9", Policy: policy}
	if err := enforcer.Install(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = enforcer.Remove(context.Background(), cfg.InstanceID) })
	// The proxy observes the query name as an FQDN with a trailing dot.
	if err := enforcer.ObserveDNSAnswer(
		context.Background(), "10.0.0.9", "api.example.com.",
		[]netip.Addr{netip.MustParseAddr("93.184.216.34")}, 25*time.Millisecond,
	); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		latest := scripts[len(scripts)-1]
		mu.Unlock()
		if !strings.Contains(latest, "93.184.216.34") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("FQDN-observed pin remained authorized after expiry:\n%s", latest)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
