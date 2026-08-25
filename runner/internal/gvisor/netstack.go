//go:build linux

package gvisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
)

// The gVisor network path: each Instance gets a private network namespace
// holding one routed veth; runsc attaches its netstack to that device with
// --network=sandbox. The host side enforces the shared fail-closed compiled
// policy through the extracted enforcer with an inet-family rendering, the
// runner DNS proxy pins exact-domain answers, and NAT masquerades egress.
// The agent's Unix-socket transport never touches this path.

const (
	// dnsListenAddress is the runner DNS proxy address, held by a
	// runner-owned dummy interface and reachable from every Instance
	// namespace through its veth; policy admits only this input.
	dnsListenAddress = "169.254.99.53"
	dnsInterfaceName = "sbxgv-dns"
	// instanceSubnetBase carves link-local /30 subnets per Instance index:
	// host side .1, guest side .2.
	instanceSubnetBase   = "169.254.104."
	maximumNetworkslots  = 255
	allowedInetMark      = "0x53425801"
	natTableSuffix       = "nat"
	guestResolvConfName  = "resolv.conf"
	guestVethNamePrefix  = "gvg"
	hostVethNamePrefix   = "gvh"
	namespaceNamePrefix  = "sbxgv"
	namespaceRuntimePath = "/var/run/netns/"
)

type instanceNetwork struct {
	index         uint32
	namespaceName string
	hostVeth      string
	guestVeth     string
	hostAddress   string
	guestAddress  string
}

func (network instanceNetwork) namespacePath() string {
	return namespaceRuntimePath + network.namespaceName
}

func networkForIndex(index uint32) instanceNetwork {
	base := index * 4
	return instanceNetwork{
		index:         index,
		namespaceName: fmt.Sprintf("%s%d", namespaceNamePrefix, index),
		hostVeth:      fmt.Sprintf("%s%d", hostVethNamePrefix, index),
		guestVeth:     fmt.Sprintf("%s%d", guestVethNamePrefix, index),
		hostAddress:   fmt.Sprintf("%s%d", instanceSubnetBase, base+1),
		guestAddress:  fmt.Sprintf("%s%d", instanceSubnetBase, base+2),
	}
}

func (backend *AssignmentBackend) acquireNetworkSlot() (instanceNetwork, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.networkSlots == nil {
		backend.networkSlots = make(map[uint32]bool)
	}
	for index := uint32(0); index < maximumNetworkslots; index++ {
		if !backend.networkSlots[index] {
			backend.networkSlots[index] = true
			return networkForIndex(index), nil
		}
	}
	return instanceNetwork{}, fmt.Errorf("SecondBox gVisor network slots are exhausted")
}

func (backend *AssignmentBackend) releaseNetworkSlot(network instanceNetwork) {
	backend.mu.Lock()
	delete(backend.networkSlots, network.index)
	backend.mu.Unlock()
}

func ipCommand(ctx context.Context, arguments ...string) error {
	output, err := exec.CommandContext(ctx, "ip", arguments...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w: %s", strings.Join(arguments, " "), err, bytes.TrimSpace(output))
	}
	return nil
}

// createInstanceNetwork builds the per-Instance namespace with its routed
// veth; runsc later scrapes the namespace-side address and default route.
func createInstanceNetwork(ctx context.Context, network instanceNetwork) error {
	steps := [][]string{
		{"netns", "add", network.namespaceName},
		{"link", "add", network.hostVeth, "type", "veth", "peer", "name", network.guestVeth},
		{"link", "set", network.guestVeth, "netns", network.namespaceName},
		{"addr", "add", network.hostAddress + "/30", "dev", network.hostVeth},
		{"link", "set", network.hostVeth, "up"},
		{"-n", network.namespaceName, "addr", "add", network.guestAddress + "/30", "dev", network.guestVeth},
		{"-n", network.namespaceName, "link", "set", "lo", "up"},
		{"-n", network.namespaceName, "link", "set", network.guestVeth, "up"},
		{"-n", network.namespaceName, "route", "add", "default", "via", network.hostAddress},
	}
	for _, step := range steps {
		if err := ipCommand(ctx, step...); err != nil {
			return errors.Join(err, destroyInstanceNetwork(context.Background(), network))
		}
	}
	return nil
}

func destroyInstanceNetwork(ctx context.Context, network instanceNetwork) error {
	// Deleting the namespace removes the moved veth end; deleting the host
	// veth removes the pair when the namespace was never populated.
	var joined error
	if err := exec.CommandContext(ctx, "ip", "link", "delete", network.hostVeth).Run(); err == nil {
		joined = nil
	}
	if output, err := exec.CommandContext(ctx, "ip", "netns", "delete", network.namespaceName).CombinedOutput(); err != nil &&
		!strings.Contains(string(output), "No such file") {
		joined = errors.Join(joined, fmt.Errorf("delete namespace %s: %w: %s",
			network.namespaceName, err, bytes.TrimSpace(output)))
	}
	return joined
}

// ensureHostNetworkPlumbing prepares the runner-owned pieces every Instance
// shares: the DNS dummy interface and IPv4 forwarding.
func ensureHostNetworkPlumbing(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "ip", "link", "show", dnsInterfaceName).Run(); err != nil {
		if err := ipCommand(ctx, "link", "add", dnsInterfaceName, "type", "dummy"); err != nil &&
			!strings.Contains(err.Error(), "File exists") {
			return err
		}
	}
	if err := ipCommand(ctx, "addr", "replace", dnsListenAddress+"/32", "dev", dnsInterfaceName); err != nil {
		return err
	}
	if err := ipCommand(ctx, "link", "set", dnsInterfaceName, "up"); err != nil {
		return err
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable IPv4 forwarding: %w", err)
	}
	return nil
}

// systemDNSUpstream discovers the host resolver for the runner DNS proxy.
func systemDNSUpstream() (netip.AddrPort, error) {
	content, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("read host resolver configuration: %w", err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			address, err := netip.ParseAddr(fields[1])
			if err == nil && address.Is4() {
				return netip.AddrPortFrom(address, 53), nil
			}
		}
	}
	return netip.AddrPort{}, fmt.Errorf("host resolver configuration names no IPv4 nameserver")
}

// renderInetPolicy is the routed-veth rendering of the shared compiled
// policy: forward-hook enforcement for guest egress, input-hook admission of
// only the runner DNS proxy, protected-prefix drops before any accept, and a
// NAT table masquerading guest egress. It fails closed: everything from the
// guest interface not explicitly accepted is dropped.
func renderInetPolicy(
	table string,
	interfaceName string,
	guestIP string,
	dnsAddress netip.Addr,
	protected []netip.Prefix,
	destinations []networkpolicy.Destination,
	runnerGateways []networkpolicy.RunnerGatewayDestination,
	pins map[int][]netip.Addr,
) string {
	var script bytes.Buffer
	fmt.Fprintf(&script, "add table inet %s\n", table)
	fmt.Fprintf(&script, "add chain inet %s forward { type filter hook forward priority 0; policy accept; }\n", table)
	fmt.Fprintf(&script, "add chain inet %s input { type filter hook input priority 0; policy accept; }\n", table)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %q ip saddr != %s drop\n", table, interfaceName, guestIP)
	fmt.Fprintf(&script, "add rule inet %s input iifname %q ip saddr != %s drop\n", table, interfaceName, guestIP)
	fmt.Fprintf(&script, "add rule inet %s forward oifname %q ct state established,related accept\n", table, interfaceName)
	if dnsAddress.IsValid() {
		family := "ip"
		if dnsAddress.Is6() {
			family = "ip6"
		}
		for _, protocol := range []string{"udp", "tcp"} {
			fmt.Fprintf(&script,
				"add rule inet %s input iifname %q %s daddr %s %s dport 53 ct mark set %s accept\n",
				table, interfaceName, family, dnsAddress, protocol, allowedInetMark)
		}
	}
	fmt.Fprintf(&script, "add rule inet %s input iifname %q drop\n", table, interfaceName)
	for _, gateway := range runnerGateways {
		renderInetAllow(&script, table, interfaceName, gateway.Address.String(), gateway.Address.Is6(), gateway.Destination)
	}
	for _, prefix := range protected {
		family := "ip"
		if prefix.Addr().Is6() {
			family = "ip6"
		}
		fmt.Fprintf(&script, "add rule inet %s forward iifname %q %s daddr %s drop\n",
			table, interfaceName, family, prefix)
	}
	for index, destination := range destinations {
		if destination.Prefix.IsValid() {
			renderInetAllow(&script, table, interfaceName, destination.Prefix.String(), destination.Prefix.Addr().Is6(), destination)
			continue
		}
		addresses := append([]netip.Addr(nil), pins[index]...)
		sort.Slice(addresses, func(left, right int) bool {
			return addresses[left].Compare(addresses[right]) < 0
		})
		for _, address := range addresses {
			renderInetAllow(&script, table, interfaceName, address.String(), address.Is6(), destination)
		}
	}
	fmt.Fprintf(&script, "add rule inet %s forward iifname %q drop\n", table, interfaceName)
	fmt.Fprintf(&script, "add rule inet %s forward oifname %q drop\n", table, interfaceName)

	fmt.Fprintf(&script, "add table ip %s%s\n", table, natTableSuffix)
	fmt.Fprintf(&script, "add chain ip %s%s postrouting { type nat hook postrouting priority 100 ; }\n", table, natTableSuffix)
	fmt.Fprintf(&script, "add rule ip %s%s postrouting ip saddr %s oifname != %q masquerade\n",
		table, natTableSuffix, guestIP, interfaceName)
	return script.String()
}

func renderInetAllow(
	script *bytes.Buffer,
	table string,
	interfaceName string,
	target string,
	ipv6 bool,
	destination networkpolicy.Destination,
) {
	family := "ip"
	if ipv6 {
		family = "ip6"
	}
	fmt.Fprintf(script,
		"add rule inet %s forward iifname %q %s daddr %s tcp dport %d ct mark set %s accept\n",
		table, interfaceName, family, target, destination.Port, allowedInetMark)
}

// deleteInetPolicyTables removes both per-Instance tables atomically with
// any replacement rendering that follows in the same script.
func deleteInetPolicyTables(table string) string {
	return fmt.Sprintf("delete table inet %s\ndelete table ip %s%s\n", table, table, natTableSuffix)
}

// writeGuestResolvConf points the sandbox resolver at the runner DNS proxy.
func writeGuestResolvConf(instanceDir string) (string, error) {
	path := instanceDir + "/" + guestResolvConfName
	content := "nameserver " + dnsListenAddress + "\noptions timeout:2 attempts:2\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write guest resolver configuration: %w", err)
	}
	return path, nil
}
