package firecracker

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
)

func TestNFTablesNetworkPolicyEnforcerInstallsDefaultDenyAndExplicitAllows(t *testing.T) {
	compiled, err := networkpolicy.Compile(networkpolicy.Policy{
		Mode: networkpolicy.ModeAllowList,
		Destinations: []networkpolicy.Destination{
			{
				Protocol: networkpolicy.ProtocolHTTPS,
				Prefix:   netip.MustParsePrefix("8.8.8.0/24"),
				Port:     443,
			},
			{
				Protocol: networkpolicy.ProtocolHTTPS,
				Domain:   "api.example.com",
				Port:     8443,
			},
		},
	}, networkpolicy.CompileOptions{
		MaximumPins: 4,
		MaximumTTL:  time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	var scripts []string
	enforcer := &NFTablesNetworkPolicyEnforcer{
		run: func(_ context.Context, name string, args []string, stdin string) ([]byte, error) {
			if name != "/usr/sbin/nft" ||
				len(args) != 2 ||
				args[0] != "-f" ||
				args[1] != "-" {
				t.Fatalf("unexpected command: %s %v", name, args)
			}
			scripts = append(scripts, stdin)
			return nil, nil
		},
		nftPath: "/usr/sbin/nft",
		now:     time.Now,
	}
	err = enforcer.Install(context.Background(), PolicyNetworkConfig{
		InstanceID: "fc-test-1",
		TapName:    "sbtap1",
		GuestIP:    "10.20.0.2",
		DNSAddress: netip.MustParseAddr("10.20.0.1"),
		Policy:     compiled,
	})
	if err != nil {
		t.Fatalf("install policy: %v", err)
	}
	t.Cleanup(func() {
		_ = enforcer.Remove(context.Background(), "fc-test-1")
	})

	if len(scripts) != 1 {
		t.Fatalf("nft install scripts = %d, want 1", len(scripts))
	}
	script := scripts[0]
	for _, required := range []string{
		"table bridge secondbox_fc_test_1",
		`iifname "sbtap1" arp daddr ip 10.20.0.1 accept`,
		`oifname "sbtap1" arp saddr ip 10.20.0.1 accept`,
		`iifname "sbtap1" ip daddr 8.8.8.0/24 tcp dport 443 ct mark set 0x53425801 accept`,
		`iifname "sbtap1" ip daddr 10.20.0.1 udp dport 53 ct mark set 0x53425801 accept`,
		`iifname "sbtap1" drop`,
		`oifname "sbtap1" drop`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("nft script missing %q:\n%s", required, script)
		}
	}
	if strings.Contains(script, "93.184.216.34") {
		t.Fatalf("unobserved domain was pre-authorized:\n%s", script)
	}
	if err := enforcer.ObserveDNSAnswer(
		context.Background(),
		"10.20.0.2",
		"api.example.com",
		[]netip.Addr{netip.MustParseAddr("93.184.216.34")},
		time.Minute,
	); err != nil {
		t.Fatalf("observe DNS answer: %v", err)
	}
	if !strings.Contains(
		scripts[len(scripts)-1],
		`ip daddr 93.184.216.34 tcp dport 8443 ct mark set 0x53425801 accept`,
	) {
		t.Fatalf("observed DNS pin was not installed:\n%s", scripts[len(scripts)-1])
	}
}

func TestNFTablesNetworkPolicyPlacesExactRunnerGatewayBeforeProtectedDrops(t *testing.T) {
	gatewayAddress := netip.MustParseAddr("198.18.43.1")
	compiled, err := networkpolicy.Compile(networkpolicy.Policy{
		Mode: networkpolicy.ModeAllowList,
		Destinations: []networkpolicy.Destination{{
			Protocol: networkpolicy.ProtocolHTTP,
			Domain:   "platform-gateway.internal",
			Port:     18080,
		}},
	}, networkpolicy.CompileOptions{
		MaximumPins:        4,
		MaximumTTL:         time.Minute,
		RunnerAddresses:    []netip.Addr{gatewayAddress},
		ManagementPrefixes: []netip.Prefix{netip.MustParsePrefix("198.18.43.0/24")},
		RunnerGateways: map[string]netip.Addr{
			"platform-gateway.internal": gatewayAddress,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var script string
	enforcer := &NFTablesNetworkPolicyEnforcer{
		run: func(_ context.Context, _ string, _ []string, stdin string) ([]byte, error) {
			script = stdin
			return nil, nil
		},
		nftPath: "/usr/sbin/nft",
	}
	if err := enforcer.Install(context.Background(), PolicyNetworkConfig{
		InstanceID: "gateway-test",
		TapName:    "sbtap1",
		GuestIP:    "198.18.43.2",
		DNSAddress: gatewayAddress,
		Policy:     compiled,
	}); err != nil {
		t.Fatal(err)
	}
	allow := `ip daddr 198.18.43.1 tcp dport 18080 ct mark set 0x53425801 accept`
	protectedDrop := `ip daddr 198.18.43.0/24 drop`
	allowIndex := strings.Index(script, allow)
	dropIndex := strings.Index(script, protectedDrop)
	if allowIndex < 0 || dropIndex < 0 || allowIndex >= dropIndex {
		t.Fatalf("Runner gateway allow must precede protected drop:\n%s", script)
	}
	if strings.Count(script, allow) != 1 {
		t.Fatalf("Runner gateway tuple must be admitted exactly once:\n%s", script)
	}
}

func TestNFTablesNetworkPolicyKeepsUnsolicitedInboundClosedWithoutPortSessions(t *testing.T) {
	compiled, err := networkpolicy.Compile(
		networkpolicy.Policy{Mode: networkpolicy.ModeDenyAll},
		networkpolicy.CompileOptions{MaximumPins: 1, MaximumTTL: time.Minute},
	)
	if err != nil {
		t.Fatal(err)
	}
	var script string
	enforcer := &NFTablesNetworkPolicyEnforcer{
		run: func(_ context.Context, _ string, _ []string, stdin string) ([]byte, error) {
			script = stdin
			return nil, nil
		},
		nftPath: "/usr/sbin/nft",
	}
	if err := enforcer.Install(context.Background(), PolicyNetworkConfig{
		InstanceID: "fc-no-port-session",
		TapName:    "sbtap-closed",
		GuestIP:    "10.20.0.4",
		Policy:     compiled,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = enforcer.Remove(context.Background(), "fc-no-port-session") })

	for _, required := range []string{
		`iifname "sbtap-closed" drop`,
		`oifname "sbtap-closed" drop`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("default-deny policy lacks published-port isolation %q:\n%s", required, script)
		}
	}
	for _, forbidden := range []string{"dnat", "redirect", "tcp dport 8080 accept"} {
		if strings.Contains(strings.ToLower(script), forbidden) {
			t.Fatalf("policy created unsupported published-port behavior %q:\n%s", forbidden, script)
		}
	}
}

func TestNFTablesNetworkPolicyEnforcerRejectsProtectedDNSAnswerBeforeMutation(t *testing.T) {
	compiled, err := networkpolicy.Compile(networkpolicy.Policy{
		Mode: networkpolicy.ModeAllowList,
		Destinations: []networkpolicy.Destination{{
			Protocol: networkpolicy.ProtocolHTTPS,
			Domain:   "metadata.example",
			Port:     443,
		}},
	}, networkpolicy.CompileOptions{
		MaximumPins: 2,
		MaximumTTL:  time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	enforcer := &NFTablesNetworkPolicyEnforcer{
		run: func(context.Context, string, []string, string) ([]byte, error) {
			called = true
			return nil, nil
		},
		nftPath: "/usr/sbin/nft",
		now:     time.Now,
	}
	err = enforcer.Install(context.Background(), PolicyNetworkConfig{
		InstanceID: "fc-test-2",
		TapName:    "sbtap2",
		GuestIP:    "10.20.0.3",
		Policy:     compiled,
	})
	if err != nil {
		t.Fatalf("install policy: %v", err)
	}
	err = enforcer.ObserveDNSAnswer(
		context.Background(),
		"10.20.0.3",
		"metadata.example",
		[]netip.Addr{netip.MustParseAddr("169.254.169.254")},
		time.Minute,
	)
	if err == nil || !strings.Contains(err.Error(), string(networkpolicy.ReasonProtectedDNSAnswer)) {
		t.Fatalf("install error = %v, want protected DNS answer", err)
	}
	if !called {
		t.Fatal("initial default-deny policy was not installed")
	}
}

func TestNFTablesNetworkPolicyEnforcerFailsHardAndRemovesState(t *testing.T) {
	compiled, err := networkpolicy.Compile(networkpolicy.Policy{
		Mode: networkpolicy.ModeDenyAll,
	}, networkpolicy.CompileOptions{
		MaximumPins: 1,
		MaximumTTL:  time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("nft unavailable")
	enforcer := &NFTablesNetworkPolicyEnforcer{
		run: func(context.Context, string, []string, string) ([]byte, error) {
			return []byte("permission denied"), wantErr
		},
		nftPath: "/usr/sbin/nft",
		now:     time.Now,
	}
	err = enforcer.Install(context.Background(), PolicyNetworkConfig{
		InstanceID: "fc-test-3",
		TapName:    "sbtap3",
		GuestIP:    "10.20.0.4",
		Policy:     compiled,
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("install error = %v, want %v", err, wantErr)
	}
}

func TestNFTablesNetworkPolicyPinsAreSandboxScopedAndExpire(t *testing.T) {
	compile := func() *networkpolicy.CompiledPolicy {
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
		return policy
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
	for _, cfg := range []PolicyNetworkConfig{
		{InstanceID: "fc-a", TapName: "tap-a", GuestIP: "10.0.0.2", Policy: compile()},
		{InstanceID: "fc-b", TapName: "tap-b", GuestIP: "10.0.0.3", Policy: compile()},
	} {
		if err := enforcer.Install(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = enforcer.Remove(context.Background(), cfg.InstanceID) })
	}
	if err := enforcer.ObserveDNSAnswer(
		context.Background(), "10.0.0.2", "api.example.com",
		[]netip.Addr{netip.MustParseAddr("93.184.216.34")}, 25*time.Millisecond,
	); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	pinScript := scripts[len(scripts)-1]
	mu.Unlock()
	if !strings.Contains(pinScript, `iifname "tap-a" ip daddr 93.184.216.34`) ||
		strings.Contains(pinScript, `iifname "tap-b" ip daddr 93.184.216.34`) {
		t.Fatalf("pin crossed Sandbox boundary:\n%s", pinScript)
	}
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	expiryScript := scripts[len(scripts)-1]
	mu.Unlock()
	if strings.Contains(expiryScript, "93.184.216.34") {
		t.Fatalf("expired pin remained authorized:\n%s", expiryScript)
	}
}

func TestNFTTableNamesUseCollisionResistantInstanceIdentity(t *testing.T) {
	left := nftTableName(strings.Repeat("a", 80) + "-one")
	right := nftTableName(strings.Repeat("a", 80) + "_one")
	if left == right {
		t.Fatalf("distinct instance IDs collided at nft table %q", left)
	}
}

func TestNFTablesNetworkPolicyUpdateFailureIsObservable(t *testing.T) {
	compiled, err := networkpolicy.Compile(networkpolicy.Policy{
		Mode: networkpolicy.ModeAllowList,
		Destinations: []networkpolicy.Destination{{
			Protocol: networkpolicy.ProtocolHTTPS,
			Domain:   "api.example.com",
			Port:     443,
		}},
	}, networkpolicy.CompileOptions{MaximumPins: 1, MaximumTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	failed := make(chan error, 1)
	enforcer := &NFTablesNetworkPolicyEnforcer{
		nftPath: "/usr/sbin/nft",
		run: func(context.Context, string, []string, string) ([]byte, error) {
			calls++
			if calls == 2 {
				return []byte("atomic update rejected"), errors.New("nft update failed")
			}
			return nil, nil
		},
	}
	if err := enforcer.Install(context.Background(), PolicyNetworkConfig{
		InstanceID: "fc-failure",
		TapName:    "tap-failure",
		GuestIP:    "10.0.0.9",
		Policy:     compiled,
		OnFailure: func(err error) {
			failed <- err
		},
	}); err != nil {
		t.Fatal(err)
	}
	err = enforcer.ObserveDNSAnswer(
		context.Background(), "10.0.0.9", "api.example.com",
		[]netip.Addr{netip.MustParseAddr("93.184.216.34")}, time.Minute,
	)
	if err == nil {
		t.Fatal("expected nft update failure")
	}
	select {
	case failure := <-failed:
		if !strings.Contains(failure.Error(), "atomic update rejected") {
			t.Fatalf("observable failure = %v", failure)
		}
	case <-time.After(time.Second):
		t.Fatal("nft update failure was not reported")
	}
}
