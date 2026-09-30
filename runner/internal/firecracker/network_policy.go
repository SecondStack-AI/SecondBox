package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/netip"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
)

// PolicyNetworkConfig binds one immutable assignment policy to one host TAP.
type PolicyNetworkConfig struct {
	InstanceID string
	TapName    string
	GuestIP    string
	DNSAddress netip.Addr
	Policy     *networkpolicy.CompiledPolicy
	OnFailure  func(error)
}

// HostNetworkPolicyEnforcer owns fail-closed host firewall state per instance.
type HostNetworkPolicyEnforcer interface {
	Ready(context.Context) error
	Install(context.Context, PolicyNetworkConfig) error
	Remove(context.Context, string) error
	Close() error
	ExecutionListenerPolicyEnforcer
}

// ExecutionListenerPolicyEnforcer admits one attributed exec's listener into an
// installed Instance policy for exactly that exec's window. Instance teardown
// fences admission before it sweeps the Instance's listeners.
type ExecutionListenerPolicyEnforcer interface {
	AllowExecutionListener(context.Context, string, netip.AddrPort) error
	RevokeExecutionListener(context.Context, string, netip.AddrPort) error
	FenceExecutionListeners(string)
	ExecutionListenerAdmission(string) error
}

type nftScriptRunner func(context.Context, string, []string, string) ([]byte, error)

// PolicyScriptRenderer produces the complete nftables script for one guest
// interface and policy state. The Firecracker enforcer's default is the
// bridge-family TAP rendering below; other backends supply a rendering for
// their own interface topology while sharing the enforcer's fail-closed
// lifecycle and DNS pin state machine.
type PolicyScriptRenderer func(
	table string,
	interfaceName string,
	guestIP string,
	dnsAddress netip.Addr,
	allowDNS bool,
	protected []netip.Prefix,
	destinations []networkpolicy.Destination,
	runnerGateways []networkpolicy.RunnerGatewayDestination,
	pins map[int][]netip.Addr,
) string

// NewNetworkPolicyEnforcer builds an enforcer whose scripts come from the
// given renderer and table-deletion form; nil values keep the Firecracker
// bridge rendering.
func NewNetworkPolicyEnforcer(
	nftPath string,
	dnsListen netip.Addr,
	dnsUpstream netip.AddrPort,
	render PolicyScriptRenderer,
	deleteTable func(table string) string,
) *NFTablesNetworkPolicyEnforcer {
	return &NFTablesNetworkPolicyEnforcer{
		nftPath: nftPath, dnsListen: dnsListen, dnsUpstream: dnsUpstream,
		render: render, deleteTable: deleteTable,
	}
}

// NFTablesNetworkPolicyEnforcer renders one isolated nftables table per VM.
// The table accepts established replies, explicit CIDR destinations, and
// runner-resolved DNS pins, then drops all other ingress and egress.
type NFTablesNetworkPolicyEnforcer struct {
	run         nftScriptRunner
	nftPath     string
	now         func() time.Time
	dnsListen   netip.Addr
	dnsUpstream netip.AddrPort
	dnsProxy    *runnerDNSProxy
	render      PolicyScriptRenderer
	deleteTable func(table string) string

	mutationMu sync.Mutex
	mu         sync.Mutex
	instances  map[string]nftPolicyInstance
}

type nftPolicyInstance struct {
	table  string
	cfg    PolicyNetworkConfig
	pins   map[int][]netip.Addr
	expiry map[int]time.Time
	// executionListeners are live attributed exec endpoints on the Runner
	// side of this Instance's interface.
	executionListeners map[netip.AddrPort]struct{}
	// listenersFenced refuses new listeners once teardown has begun.
	listenersFenced bool
	cancel          context.CancelFunc
	ctx             context.Context
}

// runnerGatewayDestinations adds each live attributed exec listener to the
// policy's logical gateway exceptions, in a stable order.
func (instance nftPolicyInstance) runnerGatewayDestinations() []networkpolicy.RunnerGatewayDestination {
	gateways := instance.cfg.Policy.RunnerGatewayDestinations()
	listeners := make([]netip.AddrPort, 0, len(instance.executionListeners))
	for listener := range instance.executionListeners {
		listeners = append(listeners, listener)
	}
	sort.Slice(listeners, func(left, right int) bool {
		return listeners[left].Compare(listeners[right]) < 0
	})
	for _, listener := range listeners {
		gateways = append(gateways, networkpolicy.RunnerGatewayDestination{
			Destination: networkpolicy.Destination{Protocol: networkpolicy.ProtocolTCP, Port: listener.Port()},
			Address:     listener.Addr(),
		})
	}
	return gateways
}

// replacementScript atomically replaces the Instance table with its current state.
func (e *NFTablesNetworkPolicyEnforcer) replacementScript(instance nftPolicyInstance) string {
	return e.deletePolicyTable(instance.table) + e.renderPolicy(
		instance.table, instance.cfg.TapName, instance.cfg.GuestIP, instance.cfg.DNSAddress,
		instance.cfg.Policy.AllowsDNS(), instance.cfg.Policy.ProtectedPrefixes(),
		instance.cfg.Policy.Destinations(), instance.runnerGatewayDestinations(), instance.pins,
	)
}

const allowedConnectionMark = "0x53425801"

// deletePolicyTable renders the family-correct table deletion; other
// renderers pair with their own families through DeleteTable.
func (e *NFTablesNetworkPolicyEnforcer) deletePolicyTable(table string) string {
	if e.deleteTable != nil {
		return e.deleteTable(table)
	}
	return fmt.Sprintf("delete table bridge %s\n", table)
}

func (e *NFTablesNetworkPolicyEnforcer) renderPolicy(
	table, interfaceName, guestIP string,
	dnsAddress netip.Addr,
	allowDNS bool,
	protected []netip.Prefix,
	destinations []networkpolicy.Destination,
	runnerGateways []networkpolicy.RunnerGatewayDestination,
	pins map[int][]netip.Addr,
) string {
	render := e.render
	if render == nil {
		render = renderNFTPolicy
	}
	return render(table, interfaceName, guestIP, dnsAddress, allowDNS, protected, destinations, runnerGateways, pins)
}

func (e *NFTablesNetworkPolicyEnforcer) Ready(ctx context.Context) error {
	if strings.TrimSpace(e.nftPath) == "" || !e.dnsListen.IsValid() || !e.dnsUpstream.IsValid() {
		return fmt.Errorf("SecondBox nftables and runner DNS settings are incomplete")
	}
	if output, err := e.command(ctx, e.nftPath, []string{"list", "tables"}, ""); err != nil {
		return fmt.Errorf("probe nftables policy enforcement: %w: %s", err, strings.TrimSpace(string(output)))
	}
	e.mu.Lock()
	if e.dnsProxy == nil {
		e.dnsProxy = &runnerDNSProxy{
			listen: e.dnsListen, upstream: e.dnsUpstream, observe: e.ObserveDNSAnswer,
			onFailure: e.handleDNSProxyFailure,
		}
	}
	proxy := e.dnsProxy
	e.mu.Unlock()
	if err := proxy.Start(); err != nil {
		return err
	}
	return proxy.Health()
}

func (e *NFTablesNetworkPolicyEnforcer) Close() error {
	e.mu.Lock()
	proxy := e.dnsProxy
	e.mu.Unlock()
	if proxy == nil {
		return nil
	}
	return proxy.Close()
}

func (e *NFTablesNetworkPolicyEnforcer) handleDNSProxyFailure(proxyErr error) {
	e.mu.Lock()
	configs := make([]PolicyNetworkConfig, 0, len(e.instances))
	for _, instance := range e.instances {
		configs = append(configs, instance.cfg)
	}
	e.mu.Unlock()
	for _, cfg := range configs {
		e.reportFailure(cfg, fmt.Errorf("runner DNS proxy failed: %w", proxyErr))
	}
}

func (e *NFTablesNetworkPolicyEnforcer) Install(ctx context.Context, cfg PolicyNetworkConfig) error {
	e.mutationMu.Lock()
	defer e.mutationMu.Unlock()
	instanceID := strings.TrimSpace(cfg.InstanceID)
	tapName := strings.TrimSpace(cfg.TapName)
	if instanceID == "" || tapName == "" || cfg.Policy == nil {
		return fmt.Errorf("SecondBox host network policy requires instance, TAP, and compiled policy")
	}
	guestIP, err := netip.ParseAddr(strings.TrimSpace(cfg.GuestIP))
	if err != nil || !guestIP.Is4() {
		return fmt.Errorf("SecondBox host network policy requires an IPv4 guest address")
	}
	cfg.GuestIP = guestIP.String()
	if strings.TrimSpace(e.nftPath) == "" {
		return fmt.Errorf("SecondBox host network policy nft executable is unavailable")
	}
	if e.dnsListen.IsValid() {
		if !e.dnsUpstream.IsValid() {
			return fmt.Errorf("SecondBox runner DNS upstream is unavailable")
		}
		e.mu.Lock()
		if e.dnsProxy == nil {
			e.dnsProxy = &runnerDNSProxy{
				listen:    e.dnsListen,
				upstream:  e.dnsUpstream,
				observe:   e.ObserveDNSAnswer,
				onFailure: e.handleDNSProxyFailure,
			}
		}
		proxy := e.dnsProxy
		e.mu.Unlock()
		if err := proxy.Start(); err != nil {
			return fmt.Errorf("start runner DNS proxy: %w", err)
		}
		if err := proxy.Health(); err != nil {
			return fmt.Errorf("runner DNS proxy health: %w", err)
		}
	}
	destinations := cfg.Policy.Destinations()
	runnerGateways := cfg.Policy.RunnerGatewayDestinations()
	table := nftTableName(instanceID)
	script := e.renderPolicy(
		table,
		tapName,
		cfg.GuestIP,
		cfg.DNSAddress,
		cfg.Policy.AllowsDNS(),
		cfg.Policy.ProtectedPrefixes(),
		destinations,
		runnerGateways,
		nil,
	)
	output, err := e.command(ctx, e.nftPath, []string{"-f", "-"}, script)
	if err != nil {
		return fmt.Errorf("install nftables policy for %s: %w: %s", instanceID, err, strings.TrimSpace(string(output)))
	}

	instanceContext, cancelRefresh := context.WithCancel(context.Background())
	e.mu.Lock()
	if e.instances == nil {
		e.instances = make(map[string]nftPolicyInstance)
	}
	if existing, found := e.instances[instanceID]; found {
		existing.cancel()
	}
	e.instances[instanceID] = nftPolicyInstance{
		table:  table,
		cfg:    cfg,
		pins:   make(map[int][]netip.Addr),
		expiry: make(map[int]time.Time),
		cancel: cancelRefresh,
		ctx:    instanceContext,
	}
	e.mu.Unlock()
	return nil
}

// ObserveDNSAnswer admits addresses only after this Sandbox queried an exact
// allowed domain through the runner DNS proxy.
func (e *NFTablesNetworkPolicyEnforcer) ObserveDNSAnswer(
	ctx context.Context,
	guestIP string,
	domain string,
	addresses []netip.Addr,
	ttl time.Duration,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// A resolver commonly sends A and AAAA queries concurrently. Serialize the
	// read-modify-replace transaction so neither answer can replace the nft table
	// or stored pin set derived from stale state.
	e.mutationMu.Lock()
	var enforcementFailure error
	var failureConfig PolicyNetworkConfig
	defer func() {
		e.mutationMu.Unlock()
		if enforcementFailure != nil {
			e.reportFailure(failureConfig, enforcementFailure)
		}
	}()
	now := time.Now().UTC()
	if e.now != nil {
		now = e.now().UTC()
	}
	e.mu.Lock()
	var instanceID string
	var instance nftPolicyInstance
	for candidateID, candidate := range e.instances {
		if candidate.cfg.GuestIP == guestIP {
			instanceID, instance = candidateID, candidate
			break
		}
	}
	if instanceID == "" {
		e.mu.Unlock()
		return fmt.Errorf("SecondBox DNS query source %q has no Sandbox policy", guestIP)
	}
	instance.pins = clonePolicyPins(instance.pins)
	instance.expiry = clonePolicyExpiry(instance.expiry)
	destinations := instance.cfg.Policy.Destinations()
	family := uint8(0)
	for _, address := range addresses {
		candidate := uint8(6)
		if address.Is4() {
			candidate = 4
		}
		if family != 0 && candidate != family {
			e.mu.Unlock()
			return fmt.Errorf("observed DNS answer mixes IPv4 and IPv6 records")
		}
		family = candidate
	}
	matched := false
	var expiresAt time.Time
	for index, destination := range destinations {
		if destination.Domain != strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), ".")) {
			continue
		}
		pin, decision := instance.cfg.Policy.PinDNSForAddressFamily(
			destination.Protocol,
			destination.Domain,
			destination.Port,
			family,
			addresses,
			ttl,
			now,
		)
		if !decision.Allowed {
			e.mu.Unlock()
			return fmt.Errorf("pin observed domain %q: %s", destination.Domain, decision.Reason)
		}
		instance.pins[index] = mergePolicyAddresses(instance.pins[index], pin.Addresses)
		instance.expiry[index] = pin.ExpiresAt
		expiresAt = pin.ExpiresAt
		matched = true
	}
	if !matched {
		e.mu.Unlock()
		return fmt.Errorf("SecondBox DNS query domain %q is not allowed", domain)
	}
	script := e.replacementScript(instance)
	e.mu.Unlock()
	updateContext, cancelUpdate := context.WithTimeout(instance.ctx, 5*time.Second)
	defer cancelUpdate()
	output, err := e.command(updateContext, e.nftPath, []string{"-f", "-"}, script)
	if err != nil {
		if instance.ctx.Err() != nil {
			return fmt.Errorf("update observed DNS pin for %s: policy removed", instanceID)
		}
		policyErr := fmt.Errorf("update observed DNS pin for %s: %w: %s", instanceID, err, strings.TrimSpace(string(output)))
		failureConfig = instance.cfg
		enforcementFailure = policyErr
		return policyErr
	}
	e.mu.Lock()
	if current, found := e.instances[instanceID]; found && current.ctx == instance.ctx {
		e.instances[instanceID] = instance
	}
	e.mu.Unlock()
	go e.expireDNSPin(instanceID, domain, expiresAt)
	return nil
}

func mergePolicyAddresses(left, right []netip.Addr) []netip.Addr {
	seen := make(map[netip.Addr]bool, len(left)+len(right))
	merged := make([]netip.Addr, 0, len(left)+len(right))
	for _, addresses := range [][]netip.Addr{left, right} {
		for _, address := range addresses {
			if seen[address] {
				continue
			}
			seen[address] = true
			merged = append(merged, address)
		}
	}
	return merged
}

func clonePolicyPins(source map[int][]netip.Addr) map[int][]netip.Addr {
	cloned := make(map[int][]netip.Addr, len(source))
	for index, addresses := range source {
		cloned[index] = append([]netip.Addr(nil), addresses...)
	}
	return cloned
}

func clonePolicyExpiry(source map[int]time.Time) map[int]time.Time {
	cloned := make(map[int]time.Time, len(source))
	for index, expiry := range source {
		cloned[index] = expiry
	}
	return cloned
}

func (e *NFTablesNetworkPolicyEnforcer) expireDNSPin(instanceID, domain string, expiresAt time.Time) {
	timer := time.NewTimer(time.Until(expiresAt))
	defer timer.Stop()
	e.mu.Lock()
	instance, found := e.instances[instanceID]
	e.mu.Unlock()
	if !found {
		return
	}
	select {
	case <-timer.C:
	case <-instance.ctx.Done():
		return
	}
	e.mutationMu.Lock()
	var enforcementFailure error
	defer func() {
		e.mutationMu.Unlock()
		if enforcementFailure != nil {
			e.reportFailure(instance.cfg, enforcementFailure)
		}
	}()
	e.mu.Lock()
	instance, found = e.instances[instanceID]
	if !found {
		e.mu.Unlock()
		return
	}
	instance.pins = clonePolicyPins(instance.pins)
	instance.expiry = clonePolicyExpiry(instance.expiry)
	// The observed domain arrives as the query's FQDN with a trailing dot;
	// destinations store the normalized form. Comparing raw values here left
	// expired pins rendered forever.
	normalizedDomain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	for index, destination := range instance.cfg.Policy.Destinations() {
		if destination.Domain == normalizedDomain && !instance.expiry[index].After(expiresAt) {
			delete(instance.pins, index)
			delete(instance.expiry, index)
		}
	}
	script := e.replacementScript(instance)
	e.mu.Unlock()
	if output, err := e.command(instance.ctx, e.nftPath, []string{"-f", "-"}, script); err != nil {
		if instance.ctx.Err() != nil {
			return
		}
		enforcementFailure = fmt.Errorf("expire DNS pin for %s: %w: %s", instanceID, err, strings.TrimSpace(string(output)))
		return
	}
	e.mu.Lock()
	if current, found := e.instances[instanceID]; found && current.ctx == instance.ctx {
		e.instances[instanceID] = instance
	}
	e.mu.Unlock()
}

func (e *NFTablesNetworkPolicyEnforcer) AllowExecutionListener(ctx context.Context, instanceID string, listener netip.AddrPort) error {
	return e.updateExecutionListener(ctx, instanceID, listener, true)
}

// RevokeExecutionListener is a no-op once the Instance policy is removed. A
// failed revocation cannot prove enforcement and terminates the Instance.
func (e *NFTablesNetworkPolicyEnforcer) RevokeExecutionListener(ctx context.Context, instanceID string, listener netip.AddrPort) error {
	return e.updateExecutionListener(ctx, instanceID, listener, false)
}

// FenceExecutionListeners refuses every later listener for the Instance. Its
// installed policy is unchanged; the caller then sweeps the live listeners.
func (e *NFTablesNetworkPolicyEnforcer) FenceExecutionListeners(instanceID string) {
	e.mutationMu.Lock()
	defer e.mutationMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if instance, found := e.instances[instanceID]; found {
		instance.listenersFenced = true
		e.instances[instanceID] = instance
	}
}

// ExecutionListenerAdmission reports whether the Instance still admits new
// listeners. Forwarder startup checks it under the listener table lock, so a
// startup that follows a teardown sweep installs nothing.
func (e *NFTablesNetworkPolicyEnforcer) ExecutionListenerAdmission(instanceID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if instance, found := e.instances[instanceID]; !found || instance.listenersFenced {
		return fmt.Errorf("SecondBox attributed exec listener has no admitting policy for %s", instanceID)
	}
	return nil
}

func (e *NFTablesNetworkPolicyEnforcer) updateExecutionListener(ctx context.Context, instanceID string, listener netip.AddrPort, allow bool) error {
	if !listener.Addr().Is4() || listener.Port() == 0 {
		return fmt.Errorf("SecondBox attributed exec listener must be an IPv4 endpoint with a nonzero port")
	}
	e.mutationMu.Lock()
	var enforcementFailure error
	var failureConfig PolicyNetworkConfig
	defer func() {
		e.mutationMu.Unlock()
		if enforcementFailure != nil {
			e.reportFailure(failureConfig, enforcementFailure)
		}
	}()
	e.mu.Lock()
	instance, found := e.instances[instanceID]
	if !found || (allow && instance.listenersFenced) {
		e.mu.Unlock()
		if allow {
			return fmt.Errorf("SecondBox attributed exec listener has no admitting policy for %s", instanceID)
		}
		return nil
	}
	listeners := make(map[netip.AddrPort]struct{}, len(instance.executionListeners)+1)
	for existing := range instance.executionListeners {
		listeners[existing] = struct{}{}
	}
	if allow {
		listeners[listener] = struct{}{}
	} else {
		delete(listeners, listener)
	}
	instance.executionListeners = listeners
	script := e.replacementScript(instance)
	e.mu.Unlock()
	updateContext, cancelUpdate := context.WithTimeout(ctx, 5*time.Second)
	defer cancelUpdate()
	output, err := e.command(updateContext, e.nftPath, []string{"-f", "-"}, script)
	if err != nil {
		policyErr := fmt.Errorf("update attributed exec listener for %s: %w: %s", instanceID, err, strings.TrimSpace(string(output)))
		if !allow && instance.ctx.Err() == nil {
			// The atomic script left the previous table, which still admits
			// the revoked endpoint.
			failureConfig = instance.cfg
			enforcementFailure = policyErr
		}
		return policyErr
	}
	e.mu.Lock()
	if current, found := e.instances[instanceID]; found && current.ctx == instance.ctx {
		current.executionListeners = listeners
		e.instances[instanceID] = current
	}
	e.mu.Unlock()
	return nil
}

func (e *NFTablesNetworkPolicyEnforcer) reportFailure(cfg PolicyNetworkConfig, err error) {
	if cfg.OnFailure != nil {
		cfg.OnFailure(err)
	}
}

func (e *NFTablesNetworkPolicyEnforcer) Remove(ctx context.Context, instanceID string) error {
	e.mutationMu.Lock()
	defer e.mutationMu.Unlock()
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return nil
	}
	e.mu.Lock()
	instance := e.instances[instanceID]
	e.mu.Unlock()
	table := instance.table
	if table == "" {
		table = nftTableName(instanceID)
	}
	if instance.cancel != nil {
		instance.cancel()
	}
	script := e.deletePolicyTable(table)
	output, err := e.command(ctx, e.nftPath, []string{"-f", "-"}, script)
	if err != nil && !strings.Contains(string(output), "No such file or directory") {
		return fmt.Errorf("remove nftables policy for %s: %w: %s", instanceID, err, strings.TrimSpace(string(output)))
	}
	// An already-missing table counts as removed, and its in-memory entry
	// must go with it: a retained canceled entry would answer DNS decisions
	// for whichever Instance later reuses this slot's guest address.
	e.mu.Lock()
	delete(e.instances, instanceID)
	e.mu.Unlock()
	return nil
}

func (e *NFTablesNetworkPolicyEnforcer) command(
	ctx context.Context,
	name string,
	args []string,
	stdin string,
) ([]byte, error) {
	if e.run != nil {
		return e.run(ctx, name, args, stdin)
	}
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = strings.NewReader(stdin)
	return command.CombinedOutput()
}

// PolicyTableName reports the per-Instance policy table name so backend
// renderers pairing extra family tables with it can sweep their residuals.
func PolicyTableName(instanceID string) string { return nftTableName(instanceID) }

func nftTableName(instanceID string) string {
	digest := sha256.Sum256([]byte(instanceID))
	var result strings.Builder
	result.WriteString("secondbox_")
	for _, character := range strings.ToLower(instanceID) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			result.WriteRune(character)
		} else {
			result.WriteByte('_')
		}
	}
	name := strings.Trim(result.String(), "_")
	if len(name) > 31 {
		name = name[:31]
	}
	return fmt.Sprintf("%s_%x", name, digest[:8])
}

func renderNFTPolicy(
	table string,
	tapName string,
	guestIP string,
	dnsAddress netip.Addr,
	allowDNS bool,
	protected []netip.Prefix,
	destinations []networkpolicy.Destination,
	runnerGateways []networkpolicy.RunnerGatewayDestination,
	pins map[int][]netip.Addr,
) string {
	var script bytes.Buffer
	fmt.Fprintf(&script, "add table bridge %s\n", table)
	fmt.Fprintf(&script, "add chain bridge %s ingress { type filter hook prerouting priority -10; policy accept; }\n", table)
	fmt.Fprintf(&script, "add chain bridge %s egress { type filter hook postrouting priority -10; policy accept; }\n", table)
	fmt.Fprintf(
		&script,
		"add rule bridge %s ingress iifname %q ip saddr != %s drop\n",
		table,
		tapName,
		guestIP,
	)
	fmt.Fprintf(
		&script,
		"add rule bridge %s egress oifname %q ct state established,related accept\n",
		table,
		tapName,
	)
	if dnsAddress.Is4() {
		fmt.Fprintf(
			&script,
			"add rule bridge %s ingress iifname %q arp daddr ip %s accept\n",
			table,
			tapName,
			dnsAddress,
		)
		fmt.Fprintf(
			&script,
			"add rule bridge %s egress oifname %q arp saddr ip %s accept\n",
			table,
			tapName,
			dnsAddress,
		)
	}
	if allowDNS && dnsAddress.IsValid() {
		family := "ip"
		if dnsAddress.Is6() {
			family = "ip6"
		}
		for _, protocol := range []string{"udp", "tcp"} {
			fmt.Fprintf(
				&script,
				"add rule bridge %s ingress iifname %q %s daddr %s %s dport 53 ct mark set %s accept\n",
				table, tapName, family, dnsAddress, protocol, allowedConnectionMark,
			)
		}
	}
	for _, gateway := range runnerGateways {
		if gateway.Address.Is4() {
			fmt.Fprintf(
				&script,
				"add rule bridge %s ingress iifname %q arp daddr ip %s accept\n",
				table,
				tapName,
				gateway.Address,
			)
			fmt.Fprintf(
				&script,
				"add rule bridge %s egress oifname %q arp saddr ip %s accept\n",
				table,
				tapName,
				gateway.Address,
			)
		}
		renderNFTAllow(
			&script,
			table,
			tapName,
			gateway.Address.String(),
			gateway.Address.Is6(),
			gateway.Destination,
		)
	}
	for _, prefix := range protected {
		family := "ip"
		if prefix.Addr().Is6() {
			family = "ip6"
		}
		fmt.Fprintf(
			&script,
			"add rule bridge %s ingress iifname %q %s daddr %s drop\n",
			table,
			tapName,
			family,
			prefix,
		)
	}
	for index, destination := range destinations {
		if destination.Prefix.IsValid() {
			renderNFTAllow(&script, table, tapName, destination.Prefix.String(), destination.Prefix.Addr().Is6(), destination)
			continue
		}
		addresses := append([]netip.Addr(nil), pins[index]...)
		sort.Slice(addresses, func(left, right int) bool {
			return addresses[left].Compare(addresses[right]) < 0
		})
		for _, address := range addresses {
			renderNFTAllow(&script, table, tapName, address.String(), address.Is6(), destination)
		}
	}
	fmt.Fprintf(&script, "add rule bridge %s ingress iifname %q drop\n", table, tapName)
	fmt.Fprintf(&script, "add rule bridge %s egress oifname %q drop\n", table, tapName)
	return script.String()
}

func renderNFTAllow(
	script *bytes.Buffer,
	table string,
	tapName string,
	target string,
	ipv6 bool,
	destination networkpolicy.Destination,
) {
	family := "ip"
	if ipv6 {
		family = "ip6"
	}
	fmt.Fprintf(
		script,
		"add rule bridge %s ingress iifname %q %s daddr %s tcp dport %d ct mark set %s accept\n",
		table,
		tapName,
		family,
		target,
		destination.Port,
		allowedConnectionMark,
	)
}
