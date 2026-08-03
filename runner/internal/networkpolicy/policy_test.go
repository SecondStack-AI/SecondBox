package networkpolicy

import (
	"net/netip"
	"testing"
	"time"
)

func TestCompileRejectsImplicitOrUnsafePolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy Policy
	}{
		{name: "unknown mode", policy: Policy{Mode: "permit"}},
		{
			name: "deny all with destination",
			policy: Policy{
				Mode:         ModeDenyAll,
				Destinations: []Destination{{Protocol: ProtocolHTTPS, Domain: "example.com", Port: 443}},
			},
		},
		{
			name: "domain and cidr",
			policy: Policy{
				Mode: ModeAllowList,
				Destinations: []Destination{{
					Protocol: ProtocolHTTPS,
					Domain:   "example.com",
					Prefix:   netip.MustParsePrefix("203.0.113.0/24"),
					Port:     443,
				}},
			},
		},
		{
			name: "unsafe cidr",
			policy: Policy{
				Mode: ModeAllowList,
				Destinations: []Destination{{
					Protocol: ProtocolTCP,
					Prefix:   netip.MustParsePrefix("10.0.0.0/8"),
					Port:     443,
				}},
			},
		},
		{
			name: "wildcard domain",
			policy: Policy{
				Mode: ModeAllowList,
				Destinations: []Destination{{
					Protocol: ProtocolHTTPS,
					Domain:   "*.example.com",
					Port:     443,
				}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Compile(test.policy, CompileOptions{
				MaximumPins: 64,
				MaximumTTL:  time.Minute,
			}); err == nil {
				t.Fatal("Compile succeeded")
			}
		})
	}
}

func TestDenyAllRejectsEveryDestination(t *testing.T) {
	compiled, err := Compile(Policy{Mode: ModeDenyAll}, CompileOptions{
		MaximumPins: 64,
		MaximumTTL:  time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := compiled.AuthorizeIP(ProtocolHTTPS, netip.MustParseAddr("93.184.216.34"), 443)
	if decision.Allowed || decision.Reason != ReasonPolicyDenyAll {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestIPPolicyRejectsMetadataSSRFAndPrivateRanges(t *testing.T) {
	protected := []string{
		"0.0.0.0",
		"10.1.2.3",
		"100.64.0.1",
		"100.100.100.200",
		"127.0.0.1",
		"169.254.169.254",
		"169.254.170.2",
		"172.16.0.1",
		"192.168.0.1",
		"198.18.0.1",
		"224.0.0.1",
		"::",
		"::1",
		"fc00::1",
		"fd00:ec2::254",
		"fe80::1",
		"ff02::1",
	}
	for _, rawAddress := range protected {
		t.Run(rawAddress, func(t *testing.T) {
			address := netip.MustParseAddr(rawAddress)
			bits := 128
			if address.Is4() {
				bits = 32
			}
			_, err := Compile(Policy{
				Mode: ModeAllowList,
				Destinations: []Destination{{
					Protocol: ProtocolTCP,
					Prefix:   netip.PrefixFrom(address, bits),
					Port:     443,
				}},
			}, CompileOptions{MaximumPins: 64, MaximumTTL: time.Minute})
			if err == nil {
				t.Fatal("Compile accepted a protected destination")
			}
		})
	}
}

func TestIPPolicyRequiresExactProtocolPortAndCIDR(t *testing.T) {
	compiled, err := Compile(Policy{
		Mode: ModeAllowList,
		Destinations: []Destination{{
			Protocol: ProtocolTCP,
			Prefix:   netip.MustParsePrefix("93.184.216.0/24"),
			Port:     443,
		}},
	}, CompileOptions{MaximumPins: 64, MaximumTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		protocol Protocol
		address  string
		port     uint16
		allowed  bool
		reason   Reason
	}{
		{ProtocolTCP, "93.184.216.34", 443, true, ReasonAllowedCIDR},
		{ProtocolHTTP, "93.184.216.34", 443, false, ReasonNoMatchingRule},
		{ProtocolTCP, "93.184.216.34", 80, false, ReasonNoMatchingRule},
		{ProtocolTCP, "93.184.217.34", 443, false, ReasonNoMatchingRule},
	}
	for _, test := range tests {
		decision := compiled.AuthorizeIP(test.protocol, netip.MustParseAddr(test.address), test.port)
		if decision.Allowed != test.allowed || decision.Reason != test.reason {
			t.Errorf("%s %s:%d decision = %#v", test.protocol, test.address, test.port, decision)
		}
	}
}

func TestRunnerAndManagementDestinationsRemainForbidden(t *testing.T) {
	compiled, err := Compile(Policy{
		Mode: ModeAllowList,
		Destinations: []Destination{{
			Protocol: ProtocolTCP,
			Prefix:   netip.MustParsePrefix("8.8.8.0/24"),
			Port:     443,
		}},
	}, CompileOptions{
		MaximumPins:        64,
		MaximumTTL:         time.Minute,
		RunnerAddresses:    []netip.Addr{netip.MustParseAddr("8.8.8.8")},
		ManagementPrefixes: []netip.Prefix{netip.MustParsePrefix("8.8.8.16/28")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"8.8.8.8", "8.8.8.20"} {
		decision := compiled.AuthorizeIP(ProtocolTCP, netip.MustParseAddr(address), 443)
		if decision.Allowed || decision.Reason != ReasonProtectedDestination {
			t.Errorf("%s decision = %#v", address, decision)
		}
	}
	if decision := compiled.AuthorizeIP(ProtocolTCP, netip.MustParseAddr("8.8.8.40"), 443); !decision.Allowed {
		t.Fatalf("public decision = %#v", decision)
	}
}

func TestRunnerGatewayAllowsOnlyOperatorBoundDomainProtocolAndPort(t *testing.T) {
	gatewayAddress := netip.MustParseAddr("198.18.43.1")
	compiled, err := Compile(Policy{
		Mode: ModeAllowList,
		Destinations: []Destination{{
			Protocol: ProtocolHTTP,
			Domain:   "platform-gateway.internal",
			Port:     18080,
		}},
	}, CompileOptions{
		MaximumPins:        64,
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
	gateways := compiled.RunnerGatewayDestinations()
	if len(gateways) != 1 ||
		gateways[0].Address != gatewayAddress ||
		gateways[0].Destination.Port != 18080 {
		t.Fatalf("Runner gateways = %#v", gateways)
	}
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	if decision := compiled.AuthorizePinned(
		ProtocolHTTP,
		"platform-gateway.internal",
		gatewayAddress,
		18080,
		now,
	); !decision.Allowed || decision.Reason != ReasonAllowedRunnerGateway {
		t.Fatalf("gateway decision = %#v", decision)
	}
	for _, test := range []struct {
		domain   string
		protocol Protocol
		port     uint16
	}{
		{"other.internal", ProtocolHTTP, 18080},
		{"platform-gateway.internal", ProtocolHTTPS, 18080},
		{"platform-gateway.internal", ProtocolHTTP, 18081},
	} {
		decision := compiled.AuthorizePinned(
			test.protocol,
			test.domain,
			gatewayAddress,
			test.port,
			now,
		)
		if decision.Allowed {
			t.Fatalf("unexpected gateway admission for %#v: %#v", test, decision)
		}
	}
	if decision := compiled.AuthorizeIP(
		ProtocolHTTP,
		gatewayAddress,
		18080,
	); decision.Allowed || decision.Reason != ReasonProtectedDestination {
		t.Fatalf("direct IP gateway decision = %#v", decision)
	}
}

func TestDNSResolutionPinsPublicAnswersPerSandbox(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	compiled, err := Compile(Policy{
		Mode: ModeAllowList,
		Destinations: []Destination{{
			Protocol: ProtocolHTTPS,
			Domain:   "Example.COM.",
			Port:     443,
		}},
	}, CompileOptions{MaximumPins: 64, MaximumTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	answer, decision := compiled.PinDNS(
		ProtocolHTTPS,
		"example.com",
		443,
		[]netip.Addr{netip.MustParseAddr("93.184.216.34")},
		30*time.Second,
		now,
	)
	if !decision.Allowed || decision.Reason != ReasonAllowedDomain {
		t.Fatalf("pin decision = %#v", decision)
	}
	if answer.Domain != "example.com" || !answer.ExpiresAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("answer = %#v", answer)
	}
	if decision := compiled.AuthorizePinned(
		ProtocolHTTPS,
		"example.com.",
		netip.MustParseAddr("93.184.216.34"),
		443,
		now.Add(time.Second),
	); !decision.Allowed || decision.Reason != ReasonAllowedPinnedDNS {
		t.Fatalf("pinned decision = %#v", decision)
	}
	if decision := compiled.AuthorizePinned(
		ProtocolHTTPS,
		"example.com",
		netip.MustParseAddr("93.184.216.34"),
		443,
		now.Add(31*time.Second),
	); decision.Allowed || decision.Reason != ReasonDNSPinExpired {
		t.Fatalf("expired decision = %#v", decision)
	}
}

func TestDNSResolutionRejectsMetadataSSRFAndUnlistedDomains(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	compiled, err := Compile(Policy{
		Mode: ModeAllowList,
		Destinations: []Destination{{
			Protocol: ProtocolHTTPS,
			Domain:   "example.com",
			Port:     443,
		}},
	}, CompileOptions{MaximumPins: 64, MaximumTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for _, protectedAnswer := range []string{
		"169.254.169.254",
		"100.100.100.200",
		"fd00:ec2::254",
	} {
		if _, decision := compiled.PinDNS(
			ProtocolHTTPS,
			"example.com",
			443,
			[]netip.Addr{netip.MustParseAddr(protectedAnswer)},
			30*time.Second,
			now,
		); decision.Allowed || decision.Reason != ReasonProtectedDNSAnswer {
			t.Fatalf("protected answer %s decision = %#v", protectedAnswer, decision)
		}
	}
	if _, decision := compiled.PinDNS(
		ProtocolHTTPS,
		"example.com",
		443,
		[]netip.Addr{netip.MustParseAddr("93.184.216.34")},
		30*time.Second,
		now,
	); !decision.Allowed {
		t.Fatalf("first public answer decision = %#v", decision)
	}
	rotated, decision := compiled.PinDNS(
		ProtocolHTTPS,
		"example.com",
		443,
		[]netip.Addr{netip.MustParseAddr("93.184.216.35")},
		30*time.Second,
		now.Add(time.Second),
	)
	if !decision.Allowed ||
		len(rotated.Addresses) != 2 ||
		rotated.Addresses[0] != netip.MustParseAddr("93.184.216.34") ||
		rotated.Addresses[1] != netip.MustParseAddr("93.184.216.35") ||
		!rotated.ExpiresAt.Equal(now.Add(31*time.Second)) {
		t.Fatalf("rotated answer = %#v decision = %#v", rotated, decision)
	}
	if decision := compiled.AuthorizePinned(
		ProtocolHTTPS,
		"example.com",
		netip.MustParseAddr("93.184.216.34"),
		443,
		now.Add(30*time.Second+500*time.Millisecond),
	); !decision.Allowed {
		t.Fatalf("refreshed pin decision = %#v", decision)
	}
	if _, decision := compiled.PinDNS(
		ProtocolHTTPS,
		"other.example.com",
		443,
		[]netip.Addr{netip.MustParseAddr("93.184.216.34")},
		30*time.Second,
		now,
	); decision.Allowed || decision.Reason != ReasonNoMatchingRule {
		t.Fatalf("unlisted domain decision = %#v", decision)
	}
}

func TestDNSPinAddressUnionIsBoundedWithoutTruncation(t *testing.T) {
	compiled, err := Compile(Policy{
		Mode: ModeAllowList,
		Destinations: []Destination{{
			Protocol: ProtocolHTTPS,
			Domain:   "example.com",
			Port:     443,
		}},
	}, CompileOptions{MaximumPins: 1, MaximumTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	addresses := make([]netip.Addr, 0, maximumPinAddresses)
	for index := 1; index <= maximumPinAddresses; index++ {
		addresses = append(addresses, netip.AddrFrom4([4]byte{8, 0, 0, byte(index)}))
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if pin, decision := compiled.PinDNS(
		ProtocolHTTPS,
		"example.com",
		443,
		addresses,
		time.Minute,
		now,
	); !decision.Allowed || len(pin.Addresses) != maximumPinAddresses {
		t.Fatalf("maximum-size pin = %#v decision = %#v", pin, decision)
	}
	overflowAddress := netip.MustParseAddr("8.0.0.65")
	if _, decision := compiled.PinDNS(
		ProtocolHTTPS,
		"example.com",
		443,
		[]netip.Addr{overflowAddress},
		time.Minute,
		now.Add(time.Second),
	); decision.Allowed || decision.Reason != ReasonPinCapacityExhausted {
		t.Fatalf("overflow decision = %#v", decision)
	}
	if decision := compiled.AuthorizePinned(
		ProtocolHTTPS,
		"example.com",
		overflowAddress,
		443,
		now.Add(2*time.Second),
	); decision.Allowed || decision.Reason != ReasonDNSRebinding {
		t.Fatalf("truncated overflow address decision = %#v", decision)
	}
	if decision := compiled.AuthorizePinned(
		ProtocolHTTPS,
		"example.com",
		addresses[0],
		443,
		now.Add(2*time.Second),
	); !decision.Allowed {
		t.Fatalf("existing pin changed after overflow: %#v", decision)
	}
}

func TestDNSPinsAreBoundedAndTTLIsExplicit(t *testing.T) {
	compiled, err := Compile(Policy{
		Mode: ModeAllowList,
		Destinations: []Destination{
			{Protocol: ProtocolHTTPS, Domain: "one.example", Port: 443},
			{Protocol: ProtocolHTTPS, Domain: "two.example", Port: 443},
		},
	}, CompileOptions{MaximumPins: 1, MaximumTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if _, decision := compiled.PinDNS(
		ProtocolHTTPS,
		"one.example",
		443,
		[]netip.Addr{netip.MustParseAddr("8.8.8.8")},
		0,
		now,
	); decision.Reason != ReasonInvalidDNSAnswer {
		t.Fatalf("zero TTL decision = %#v", decision)
	}
	if answer, decision := compiled.PinDNS(
		ProtocolHTTPS,
		"one.example",
		443,
		[]netip.Addr{netip.MustParseAddr("8.8.8.8")},
		5*time.Minute,
		now,
	); !decision.Allowed || !answer.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("capped answer = %#v decision = %#v", answer, decision)
	}
	if _, decision := compiled.PinDNS(
		ProtocolHTTPS,
		"two.example",
		443,
		[]netip.Addr{netip.MustParseAddr("1.1.1.1")},
		time.Minute,
		now,
	); decision.Allowed || decision.Reason != ReasonPinCapacityExhausted {
		t.Fatalf("capacity decision = %#v", decision)
	}
}
