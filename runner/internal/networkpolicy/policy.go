// Package networkpolicy validates immutable runner egress policy and owns
// per-Sandbox DNS destination pins.
package networkpolicy

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mode is the closed outbound policy mode.
type Mode string

const (
	ModeDenyAll   Mode = "deny_all"
	ModeAllowList Mode = "allow_list"
)

// maximumPinAddresses accommodates normal CDN answer rotation while bounding
// per-domain host firewall growth; 64 is ample for a 4 KiB DNS response without
// allowing repeated observations to grow the rules indefinitely.
const maximumPinAddresses = 64

// Protocol is the provider-neutral destination protocol.
type Protocol string

const (
	ProtocolTCP   Protocol = "tcp"
	ProtocolHTTP  Protocol = "http"
	ProtocolHTTPS Protocol = "https"
)

// Reason is a stable network admission decision.
type Reason string

const (
	ReasonAllowedCIDR          Reason = "allowed_cidr"
	ReasonAllowedDomain        Reason = "allowed_domain"
	ReasonAllowedRunnerGateway Reason = "allowed_runner_gateway"
	ReasonAllowedPinnedDNS     Reason = "allowed_pinned_dns"
	ReasonPolicyDenyAll        Reason = "policy_deny_all"
	ReasonNoMatchingRule       Reason = "no_matching_rule"
	ReasonProtectedDestination Reason = "protected_destination"
	ReasonProtectedDNSAnswer   Reason = "protected_dns_answer"
	ReasonInvalidDNSAnswer     Reason = "invalid_dns_answer"
	ReasonDNSPinMissing        Reason = "dns_pin_missing"
	ReasonDNSPinExpired        Reason = "dns_pin_expired"
	ReasonDNSRebinding         Reason = "dns_rebinding"
	ReasonPinCapacityExhausted Reason = "pin_capacity_exhausted"
)

// Policy is one immutable profile-resolved outbound policy.
type Policy struct {
	Mode         Mode
	Destinations []Destination
}

// Destination permits one exact domain or CIDR for one protocol and port.
type Destination struct {
	Protocol Protocol
	Domain   string
	Prefix   netip.Prefix
	Port     uint16
}

// CompileOptions are explicit runner-local bounds and protected destinations.
type CompileOptions struct {
	MaximumPins        int
	MaximumTTL         time.Duration
	RunnerAddresses    []netip.Addr
	ManagementPrefixes []netip.Prefix
	RunnerGateways     map[string]netip.Addr
}

// Decision is safe fixed-shape admission evidence.
type Decision struct {
	Allowed bool
	Reason  Reason
}

// DNSPin is one accepted normalized answer set.
type DNSPin struct {
	Domain    string
	Addresses []netip.Addr
	ExpiresAt time.Time
}

// RunnerGatewayDestination is one operator-bound logical gateway tuple.
type RunnerGatewayDestination struct {
	Destination Destination
	Address     netip.Addr
}

type pinKey struct {
	protocol Protocol
	domain   string
	port     uint16
	family   uint8
}

// CompiledPolicy is scoped to one Sandbox assignment and must not be shared
// across Sandboxes.
type CompiledPolicy struct {
	mode               Mode
	destinations       []Destination
	maximumPins        int
	maximumTTL         time.Duration
	runnerAddresses    map[netip.Addr]struct{}
	managementPrefixes []netip.Prefix
	runnerGateways     map[string]netip.Addr

	mu   sync.Mutex
	pins map[pinKey]DNSPin
}

// RunnerGatewayDestinations returns Profile destinations explicitly bound to Runner-local gateways.
func (policy *CompiledPolicy) RunnerGatewayDestinations() []RunnerGatewayDestination {
	if policy == nil {
		return nil
	}
	result := make([]RunnerGatewayDestination, 0, len(policy.runnerGateways))
	for _, destination := range policy.destinations {
		address, found := policy.runnerGateways[destination.Domain]
		if !found {
			continue
		}
		result = append(result, RunnerGatewayDestination{
			Destination: destination,
			Address:     address,
		})
	}
	return result
}

// Destinations returns the validated immutable destination rules used by host
// enforcement. The returned slice does not share storage with the policy.
func (policy *CompiledPolicy) Destinations() []Destination {
	if policy == nil {
		return nil
	}
	return append([]Destination(nil), policy.destinations...)
}

// Mode returns the validated closed policy mode.
func (policy *CompiledPolicy) Mode() Mode {
	if policy == nil {
		return ""
	}
	return policy.mode
}

// AllowsDNS reports whether this policy has a domain destination that requires
// the Runner-controlled resolver. CIDR-only and deny-all policies do not.
func (policy *CompiledPolicy) AllowsDNS() bool {
	if policy == nil || policy.mode == ModeDenyAll {
		return false
	}
	for _, destination := range policy.destinations {
		if destination.Domain != "" {
			return true
		}
	}
	return false
}

// ProtectedPrefixes returns every host-enforced destination class, including
// runner addresses and operator-declared management networks.
func (policy *CompiledPolicy) ProtectedPrefixes() []netip.Prefix {
	if policy == nil {
		return nil
	}
	prefixes := append([]netip.Prefix(nil), protectedPrefixes...)
	prefixes = append(prefixes, policy.managementPrefixes...)
	for address := range policy.runnerAddresses {
		bits := 128
		if address.Is4() {
			bits = 32
		}
		prefixes = append(prefixes, netip.PrefixFrom(address, bits))
	}
	return prefixes
}

var protectedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// Compile validates a resolved policy before the runner accepts an assignment.
func Compile(policy Policy, options CompileOptions) (*CompiledPolicy, error) {
	if options.MaximumPins < 1 {
		return nil, fmt.Errorf("SecondBox network policy maximum DNS pins must be positive")
	}
	if options.MaximumTTL <= 0 {
		return nil, fmt.Errorf("SecondBox network policy maximum DNS TTL must be positive")
	}
	if len(policy.Destinations) > 128 {
		return nil, fmt.Errorf("SecondBox network policy exceeds 128 destinations")
	}
	switch policy.Mode {
	case ModeDenyAll:
		if len(policy.Destinations) != 0 {
			return nil, fmt.Errorf("SecondBox deny-all network policy cannot contain destinations")
		}
	case ModeAllowList:
	default:
		return nil, fmt.Errorf("SecondBox network policy mode %q is unsupported", policy.Mode)
	}

	destinations := make([]Destination, 0, len(policy.Destinations))
	for index, destination := range policy.Destinations {
		validated, err := validateDestination(destination)
		if err != nil {
			return nil, fmt.Errorf("SecondBox network policy destination %d: %w", index, err)
		}
		destinations = append(destinations, validated)
	}
	runnerAddresses := make(map[netip.Addr]struct{}, len(options.RunnerAddresses))
	for _, address := range options.RunnerAddresses {
		address = normalizeAddress(address)
		if !address.IsValid() {
			return nil, fmt.Errorf("SecondBox network policy runner address is invalid")
		}
		runnerAddresses[address] = struct{}{}
	}
	managementPrefixes := make([]netip.Prefix, 0, len(options.ManagementPrefixes))
	for _, prefix := range options.ManagementPrefixes {
		normalized, err := normalizePrefix(prefix)
		if err != nil {
			return nil, fmt.Errorf("SecondBox network policy management prefix: %w", err)
		}
		managementPrefixes = append(managementPrefixes, normalized)
	}
	runnerGateways := make(map[string]netip.Addr, len(options.RunnerGateways))
	for rawDomain, rawAddress := range options.RunnerGateways {
		domain, err := normalizeDomain(rawDomain)
		if err != nil {
			return nil, fmt.Errorf("SecondBox network policy Runner gateway domain: %w", err)
		}
		address := normalizeAddress(rawAddress)
		if !address.IsValid() {
			return nil, fmt.Errorf("SecondBox network policy Runner gateway %q address is invalid", domain)
		}
		if !isProtectedAddress(address, runnerAddresses, managementPrefixes) {
			return nil, fmt.Errorf(
				"SecondBox network policy Runner gateway %q address %s is not a protected Runner destination",
				domain,
				address,
			)
		}
		if _, duplicate := runnerGateways[domain]; duplicate {
			return nil, fmt.Errorf("SecondBox network policy Runner gateway domain %q is duplicated", domain)
		}
		runnerGateways[domain] = address
	}
	return &CompiledPolicy{
		mode:               policy.Mode,
		destinations:       destinations,
		maximumPins:        options.MaximumPins,
		maximumTTL:         options.MaximumTTL,
		runnerAddresses:    runnerAddresses,
		managementPrefixes: managementPrefixes,
		runnerGateways:     runnerGateways,
		pins:               make(map[pinKey]DNSPin),
	}, nil
}

func validateDestination(destination Destination) (Destination, error) {
	if !validProtocol(destination.Protocol) {
		return Destination{}, fmt.Errorf("protocol %q is unsupported", destination.Protocol)
	}
	if destination.Port == 0 {
		return Destination{}, fmt.Errorf("port must be positive")
	}
	if destination.Port == 53 || destination.Port == 853 {
		return Destination{}, fmt.Errorf("port %d is reserved for runner-controlled DNS", destination.Port)
	}
	hasDomain := strings.TrimSpace(destination.Domain) != ""
	hasPrefix := destination.Prefix.IsValid()
	if hasDomain == hasPrefix {
		return Destination{}, fmt.Errorf("exactly one domain or CIDR is required")
	}
	if hasDomain {
		domain, err := normalizeDomain(destination.Domain)
		if err != nil {
			return Destination{}, err
		}
		destination.Domain = domain
		destination.Prefix = netip.Prefix{}
		return destination, nil
	}
	prefix, err := normalizePrefix(destination.Prefix)
	if err != nil {
		return Destination{}, err
	}
	for _, protected := range protectedPrefixes {
		if prefixesOverlap(prefix, protected) {
			return Destination{}, fmt.Errorf("CIDR %s intersects protected destination class %s", prefix, protected)
		}
	}
	destination.Prefix = prefix
	destination.Domain = ""
	return destination, nil
}

func validProtocol(protocol Protocol) bool {
	switch protocol {
	case ProtocolTCP, ProtocolHTTP, ProtocolHTTPS:
		return true
	default:
		return false
	}
}

func normalizeDomain(raw string) (string, error) {
	domain := strings.ToLower(strings.TrimSpace(raw))
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" || len(domain) > 253 {
		return "", fmt.Errorf("domain length is invalid")
	}
	if strings.ContainsAny(domain, "*:/") {
		return "", fmt.Errorf("domain %q must be one exact DNS name", raw)
	}
	if _, err := netip.ParseAddr(domain); err == nil {
		return "", fmt.Errorf("domain %q is an IP address; use a CIDR destination", raw)
	}
	labels := strings.Split(domain, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 ||
			label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("domain %q contains an invalid label", raw)
		}
		for _, character := range label {
			if (character >= 'a' && character <= 'z') ||
				(character >= '0' && character <= '9') ||
				character == '-' {
				continue
			}
			return "", fmt.Errorf("domain %q must be an ASCII DNS name", raw)
		}
	}
	return domain, nil
}

func normalizeAddress(address netip.Addr) netip.Addr {
	if address.Is4In6() {
		return address.Unmap()
	}
	return address
}

func normalizePrefix(prefix netip.Prefix) (netip.Prefix, error) {
	if !prefix.IsValid() {
		return netip.Prefix{}, fmt.Errorf("CIDR is invalid")
	}
	if prefix.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("IPv4-mapped IPv6 CIDRs are unsupported")
	}
	return prefix.Masked(), nil
}

func prefixesOverlap(left, right netip.Prefix) bool {
	if left.Addr().BitLen() != right.Addr().BitLen() {
		return false
	}
	return left.Contains(right.Addr()) || right.Contains(left.Addr())
}

// AuthorizeIP evaluates a direct IP destination. Domain rules never authorize
// direct IP use.
func (policy *CompiledPolicy) AuthorizeIP(
	protocol Protocol,
	address netip.Addr,
	port uint16,
) Decision {
	if policy.mode == ModeDenyAll {
		return Decision{Reason: ReasonPolicyDenyAll}
	}
	address = normalizeAddress(address)
	if policy.isProtected(address) {
		return Decision{Reason: ReasonProtectedDestination}
	}
	for _, destination := range policy.destinations {
		if destination.Domain == "" &&
			destination.Protocol == protocol &&
			destination.Port == port &&
			destination.Prefix.Contains(address) {
			return Decision{Allowed: true, Reason: ReasonAllowedCIDR}
		}
	}
	return Decision{Reason: ReasonNoMatchingRule}
}

// PinDNS validates an answer from the runner-controlled resolver and retains a
// bounded union of addresses observed before the pin expires.
func (policy *CompiledPolicy) PinDNS(
	protocol Protocol,
	rawDomain string,
	port uint16,
	addresses []netip.Addr,
	ttl time.Duration,
	now time.Time,
) (DNSPin, Decision) {
	return policy.pinDNS(protocol, rawDomain, port, 0, addresses, ttl, now)
}

// PinDNSForAddressFamily keeps IPv4 and IPv6 answer sets independent so a
// normal A query followed by AAAA is not treated as DNS rebinding.
func (policy *CompiledPolicy) PinDNSForAddressFamily(
	protocol Protocol,
	rawDomain string,
	port uint16,
	family uint8,
	addresses []netip.Addr,
	ttl time.Duration,
	now time.Time,
) (DNSPin, Decision) {
	if family != 4 && family != 6 {
		return DNSPin{}, Decision{Reason: ReasonInvalidDNSAnswer}
	}
	return policy.pinDNS(protocol, rawDomain, port, family, addresses, ttl, now)
}

func (policy *CompiledPolicy) pinDNS(
	protocol Protocol,
	rawDomain string,
	port uint16,
	family uint8,
	addresses []netip.Addr,
	ttl time.Duration,
	now time.Time,
) (DNSPin, Decision) {
	if policy.mode == ModeDenyAll {
		return DNSPin{}, Decision{Reason: ReasonPolicyDenyAll}
	}
	domain, err := normalizeDomain(rawDomain)
	if err != nil || !policy.hasDomainRule(protocol, domain, port) {
		return DNSPin{}, Decision{Reason: ReasonNoMatchingRule}
	}
	if ttl <= 0 || len(addresses) == 0 {
		return DNSPin{}, Decision{Reason: ReasonInvalidDNSAnswer}
	}
	normalized := make([]netip.Addr, 0, len(addresses))
	seen := make(map[netip.Addr]struct{}, len(addresses))
	for _, address := range addresses {
		address = normalizeAddress(address)
		if !address.IsValid() {
			return DNSPin{}, Decision{Reason: ReasonInvalidDNSAnswer}
		}
		if family == 4 && !address.Is4() || family == 6 && !address.Is6() {
			return DNSPin{}, Decision{Reason: ReasonInvalidDNSAnswer}
		}
		if policy.isProtected(address) {
			return DNSPin{}, Decision{Reason: ReasonProtectedDNSAnswer}
		}
		if _, duplicate := seen[address]; duplicate {
			continue
		}
		seen[address] = struct{}{}
		normalized = append(normalized, address)
	}
	if len(normalized) == 0 {
		return DNSPin{}, Decision{Reason: ReasonInvalidDNSAnswer}
	}
	sort.Slice(normalized, func(left, right int) bool {
		return normalized[left].Compare(normalized[right]) < 0
	})
	if ttl > policy.maximumTTL {
		ttl = policy.maximumTTL
	}
	now = now.UTC()
	key := pinKey{protocol: protocol, domain: domain, port: port, family: family}
	policy.mu.Lock()
	defer policy.mu.Unlock()
	policy.removeExpiredPinsLocked(now)
	if existing, found := policy.pins[key]; found {
		normalized = mergeAddresses(existing.Addresses, normalized)
	} else if len(policy.pins) >= policy.maximumPins {
		return DNSPin{}, Decision{Reason: ReasonPinCapacityExhausted}
	}
	if len(normalized) > maximumPinAddresses {
		return DNSPin{}, Decision{Reason: ReasonPinCapacityExhausted}
	}
	pin := DNSPin{
		Domain:    domain,
		Addresses: append([]netip.Addr(nil), normalized...),
		ExpiresAt: now.Add(ttl),
	}
	policy.pins[key] = pin
	return clonePin(pin), Decision{Allowed: true, Reason: ReasonAllowedDomain}
}

// AuthorizePinned proves a connection target against the accepted DNS answer
// for this Sandbox policy instance.
func (policy *CompiledPolicy) AuthorizePinned(
	protocol Protocol,
	rawDomain string,
	address netip.Addr,
	port uint16,
	now time.Time,
) Decision {
	if policy.mode == ModeDenyAll {
		return Decision{Reason: ReasonPolicyDenyAll}
	}
	domain, err := normalizeDomain(rawDomain)
	if err != nil || !policy.hasDomainRule(protocol, domain, port) {
		return Decision{Reason: ReasonNoMatchingRule}
	}
	address = normalizeAddress(address)
	if policy.isRunnerGateway(protocol, domain, address, port) {
		return Decision{Allowed: true, Reason: ReasonAllowedRunnerGateway}
	}
	if policy.isProtected(address) {
		return Decision{Reason: ReasonProtectedDestination}
	}
	key := pinKey{protocol: protocol, domain: domain, port: port}
	policy.mu.Lock()
	defer policy.mu.Unlock()
	pin, found := policy.pins[key]
	if !found {
		return Decision{Reason: ReasonDNSPinMissing}
	}
	if !now.UTC().Before(pin.ExpiresAt) {
		delete(policy.pins, key)
		return Decision{Reason: ReasonDNSPinExpired}
	}
	for _, pinned := range pin.Addresses {
		if pinned == address {
			return Decision{Allowed: true, Reason: ReasonAllowedPinnedDNS}
		}
	}
	return Decision{Reason: ReasonDNSRebinding}
}

func (policy *CompiledPolicy) isRunnerGateway(
	protocol Protocol,
	domain string,
	address netip.Addr,
	port uint16,
) bool {
	gatewayAddress, found := policy.runnerGateways[domain]
	return found &&
		gatewayAddress == address &&
		policy.hasDomainRule(protocol, domain, port)
}

func (policy *CompiledPolicy) hasDomainRule(protocol Protocol, domain string, port uint16) bool {
	for _, destination := range policy.destinations {
		if destination.Domain == domain &&
			destination.Protocol == protocol &&
			destination.Port == port {
			return true
		}
	}
	return false
}

func (policy *CompiledPolicy) isProtected(address netip.Addr) bool {
	return isProtectedAddress(address, policy.runnerAddresses, policy.managementPrefixes)
}

func isProtectedAddress(
	address netip.Addr,
	runnerAddresses map[netip.Addr]struct{},
	managementPrefixes []netip.Prefix,
) bool {
	if !address.IsValid() {
		return true
	}
	if _, found := runnerAddresses[address]; found {
		return true
	}
	for _, prefix := range managementPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	for _, prefix := range protectedPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func (policy *CompiledPolicy) removeExpiredPinsLocked(now time.Time) {
	for key, pin := range policy.pins {
		if !now.Before(pin.ExpiresAt) {
			delete(policy.pins, key)
		}
	}
}

func mergeAddresses(left, right []netip.Addr) []netip.Addr {
	seen := make(map[netip.Addr]struct{}, len(left)+len(right))
	merged := make([]netip.Addr, 0, len(left)+len(right))
	for _, addresses := range [][]netip.Addr{left, right} {
		for _, address := range addresses {
			if _, found := seen[address]; found {
				continue
			}
			seen[address] = struct{}{}
			merged = append(merged, address)
		}
	}
	sort.Slice(merged, func(left, right int) bool {
		return merged[left].Compare(merged[right]) < 0
	})
	return merged
}

func clonePin(pin DNSPin) DNSPin {
	pin.Addresses = append([]netip.Addr(nil), pin.Addresses...)
	return pin
}
