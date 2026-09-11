package egressforwarder

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

var executionInterfaceName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,15}$`)

type ExecutionListenerPolicy struct {
	InstanceID      string
	GuestInterface  string
	BridgeInterface string
	GuestAddress    netip.Addr
	ListenerAddress netip.AddrPort
}

// RenderExecutionListenerPolicy reserves the endpoint across host input and
// output. Bridged guests also need a bridge input check before L3 delivery.
func RenderExecutionListenerPolicy(policy ExecutionListenerPolicy) (string, error) {
	if strings.TrimSpace(policy.InstanceID) == "" || !executionInterfaceName.MatchString(policy.GuestInterface) ||
		!executionListenerIPv4(policy.GuestAddress) ||
		!executionListenerIPv4(policy.ListenerAddress.Addr()) ||
		policy.ListenerAddress.Port() == 0 || policy.GuestAddress == policy.ListenerAddress.Addr() {
		return "", fmt.Errorf("attributed listener policy requires an Instance, interface, and distinct unicast IPv4 endpoints")
	}
	if policy.BridgeInterface != "" && (!executionInterfaceName.MatchString(policy.BridgeInterface) || policy.BridgeInterface == policy.GuestInterface) {
		return "", fmt.Errorf("attributed listener bridge interface is invalid")
	}
	table := executionListenerTable(policy.GuestInterface)
	inputInterface := policy.GuestInterface
	if policy.BridgeInterface != "" {
		inputInterface = policy.BridgeInterface
	}
	endpoint := fmt.Sprintf("ip daddr %s tcp dport %d", policy.ListenerAddress.Addr(), policy.ListenerAddress.Port())
	var rules bytes.Buffer
	fmt.Fprintf(&rules, "add table inet %s\n", table)
	fmt.Fprintf(&rules, "add chain inet %s input { type filter hook input priority -20; policy accept; }\n", table)
	fmt.Fprintf(&rules, "add chain inet %s output { type filter hook output priority -20; policy accept; }\n", table)
	fmt.Fprintf(&rules, "add rule inet %s input %s iifname != %q drop\n", table, endpoint, inputInterface)
	fmt.Fprintf(&rules, "add rule inet %s input %s ip saddr != %s drop\n", table, endpoint, policy.GuestAddress)
	fmt.Fprintf(&rules, "add rule inet %s output %s drop\n", table, endpoint)
	if policy.BridgeInterface != "" {
		fmt.Fprintf(&rules, "add table bridge %s\n", table)
		fmt.Fprintf(&rules, "add chain bridge %s input { type filter hook input priority -20; policy accept; }\n", table)
		fmt.Fprintf(&rules, "add rule bridge %s input %s iifname != %q drop\n", table, endpoint, policy.GuestInterface)
		fmt.Fprintf(&rules, "add rule bridge %s input %s ip saddr != %s drop\n", table, endpoint, policy.GuestAddress)
	}
	return rules.String(), nil
}

func executionListenerIPv4(address netip.Addr) bool {
	return address.Is4() && (address.IsGlobalUnicast() || address.IsLinkLocalUnicast())
}

func executionListenerTable(guestInterface string) string {
	digest := sha256.Sum256([]byte(guestInterface))
	return fmt.Sprintf("sbx_exec_%x", digest[:8])
}
