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
	// dnsListenBase anchors the runner DNS proxy address: profile N binds
	// 169.254.99.(53+N) on the shared dummy interface, reachable from every
	// Instance namespace through its veth; policy admits only this input.
	// Profiles keep multiple runners on one host network namespace apart:
	// each gets its own proxy address, /30 slot space, and link names.
	dnsListenBase    = 53
	dnsListenPrefix  = "169.254.99."
	dnsInterfaceName = "sbxgv-dns"
	// instanceSubnetPrefix carves a link-local /24 per profile at third
	// octet 104+profile, then /30 subnets per Instance index inside it:
	// host side .1, guest side .2.
	instanceSubnetPrefix    = "169.254."
	instanceSubnetBaseOctet = 104
	maximumNetworkProfiles  = 16
	maximumNetworkslots     = 63
	allowedInetMark         = "0x53425801"
	natTableSuffix          = "nat"
	guestVethNamePrefix     = "gvg"
	hostVethNamePrefix      = "gvh"
	namespaceNamePrefix     = "sbxgv"
	guestResolvConfName     = "resolv.conf"
	namespaceRuntimePath    = "/var/run/netns/"
)

func dnsAddressForProfile(profile uint32) string {
	return fmt.Sprintf("%s%d", dnsListenPrefix, dnsListenBase+profile)
}

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

func networkForIndex(profile, index uint32) instanceNetwork {
	subnet := fmt.Sprintf("%s%d.", instanceSubnetPrefix, instanceSubnetBaseOctet+profile)
	base := index * 4
	return instanceNetwork{
		index:         index,
		namespaceName: fmt.Sprintf("%s%d-%d", namespaceNamePrefix, profile, index),
		hostVeth:      fmt.Sprintf("%s%d-%d", hostVethNamePrefix, profile, index),
		guestVeth:     fmt.Sprintf("%s%d-%d", guestVethNamePrefix, profile, index),
		hostAddress:   fmt.Sprintf("%s%d", subnet, base+1),
		guestAddress:  fmt.Sprintf("%s%d", subnet, base+2),
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
			return networkForIndex(backend.config.NetworkProfile, index), nil
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
	// veth removes the pair when the namespace was never populated. Only a
	// confirmed already-absent resource is tolerated; every other failure
	// surfaces so the slot stays leaked instead of colliding later.
	var joined error
	if output, err := exec.CommandContext(ctx, "ip", "link", "delete", network.hostVeth).CombinedOutput(); err != nil &&
		!strings.Contains(string(output), "Cannot find device") {
		joined = errors.Join(joined, fmt.Errorf("delete host veth %s: %w: %s",
			network.hostVeth, err, bytes.TrimSpace(output)))
	}
	if output, err := exec.CommandContext(ctx, "ip", "netns", "delete", network.namespaceName).CombinedOutput(); err != nil &&
		!strings.Contains(string(output), "No such file") {
		joined = errors.Join(joined, fmt.Errorf("delete namespace %s: %w: %s",
			network.namespaceName, err, bytes.TrimSpace(output)))
	}
	return joined
}

// reconcileStaleNetworks removes this network profile's leftovers from an
// earlier runner generation: instance namespaces, host veths, and the
// per-Instance policy tables whose rules reference the profile's links. None
// of those can be live before this backend launches compute, and other
// profiles' resources belong to other runners.
func reconcileStaleNetworks(ctx context.Context, profile uint32) error {
	var joined error
	namespacePrefix := fmt.Sprintf("%s%d-", namespaceNamePrefix, profile)
	if output, err := exec.CommandContext(ctx, "ip", "netns", "list").Output(); err != nil {
		joined = errors.Join(joined, fmt.Errorf("list network namespaces: %w", err))
	} else {
		for _, line := range strings.Split(string(output), "\n") {
			name := strings.Fields(line)
			if len(name) > 0 && strings.HasPrefix(name[0], namespacePrefix) {
				joined = errors.Join(joined, ipCommand(ctx, "netns", "delete", name[0]))
			}
		}
	}
	vethPrefix := fmt.Sprintf("%s%d-", hostVethNamePrefix, profile)
	if output, err := exec.CommandContext(ctx, "ip", "-o", "link", "show").Output(); err != nil {
		joined = errors.Join(joined, fmt.Errorf("list links: %w", err))
	} else {
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			name := strings.TrimSuffix(fields[1], ":")
			name, _, _ = strings.Cut(name, "@")
			if strings.HasPrefix(name, vethPrefix) {
				joined = errors.Join(joined, ipCommand(ctx, "link", "delete", name))
			}
		}
	}
	if output, err := exec.CommandContext(ctx, "nft", "list", "tables").Output(); err != nil {
		joined = errors.Join(joined, fmt.Errorf("list nftables tables: %w", err))
	} else {
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[0] != "table" {
				continue
			}
			family, name := fields[1], fields[2]
			// The inet policy table and its ip NAT twin are swept
			// independently: an orphaned NAT-only table (its inet twin
			// already gone) must be found through its own family listing.
			switch {
			case family == "inet" && strings.HasPrefix(name, "secondbox_"):
			case family == "ip" && strings.HasPrefix(name, "secondbox_") && strings.HasSuffix(name, natTableSuffix):
			default:
				continue
			}
			listing, listErr := exec.CommandContext(ctx, "nft", "list", "table", family, name).Output()
			if listErr != nil || !strings.Contains(string(listing), `"`+vethPrefix) {
				continue
			}
			if deleteOutput, deleteErr := exec.CommandContext(ctx, "nft", "delete", "table", family, name).CombinedOutput(); deleteErr != nil &&
				!strings.Contains(string(deleteOutput), "No such file") {
				joined = errors.Join(joined, fmt.Errorf("delete stale policy table %s: %w: %s",
					name, deleteErr, bytes.TrimSpace(deleteOutput)))
			}
		}
	}
	if joined != nil {
		return fmt.Errorf("reconcile stale profile networks: %w", joined)
	}
	return nil
}

// ensureHostNetworkPlumbing prepares the runner-owned pieces every Instance
// shares: the DNS dummy interface, IPv4 forwarding, and admission through a
// coexisting Docker firewall.
func ensureHostNetworkPlumbing(ctx context.Context, dnsAddress string) error {
	if err := exec.CommandContext(ctx, "ip", "link", "show", dnsInterfaceName).Run(); err != nil {
		if err := ipCommand(ctx, "link", "add", dnsInterfaceName, "type", "dummy"); err != nil &&
			!strings.Contains(err.Error(), "File exists") {
			return err
		}
	}
	if err := ipCommand(ctx, "addr", "replace", dnsAddress+"/32", "dev", dnsInterfaceName); err != nil {
		return err
	}
	if err := ipCommand(ctx, "link", "set", dnsInterfaceName, "up"); err != nil {
		return err
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable IPv4 forwarding: %w", err)
	}
	return ensureDockerForwardAdmission(ctx)
}

// ensureDockerForwardAdmission lets policy-accepted flows through a Docker
// firewall sharing the host. Docker sets its own forward-hook chain to a drop
// policy, and every forward-hook chain must accept a packet independently, so
// the runner inserts one rule into Docker's designated DOCKER-USER extension
// chain admitting exactly the connections the runner's fail-closed tables
// have already marked. Hosts without Docker have no such chain and need no
// admission.
func ensureDockerForwardAdmission(ctx context.Context) error {
	listing, err := exec.CommandContext(ctx, "nft", "list", "chain", "ip", "filter", "DOCKER-USER").CombinedOutput()
	if err != nil {
		// Only a genuinely absent chain means Docker does not manage this
		// host's forward hook; permission, cancellation, or nftables
		// failures must fail readiness rather than admit Sandboxes whose
		// forwarding Docker will silently drop.
		text := string(listing)
		if strings.Contains(text, "No such file or directory") ||
			strings.Contains(text, "does not exist") {
			return nil
		}
		return fmt.Errorf("inspect the Docker firewall admission chain: %w: %s", err, bytes.TrimSpace(listing))
	}
	if strings.Contains(string(listing), "ct mark "+allowedInetMark) && strings.Contains(string(listing), "accept") {
		return nil
	}
	script := fmt.Sprintf("insert rule ip filter DOCKER-USER ct mark %s counter accept\n", allowedInetMark)
	command := exec.CommandContext(ctx, "nft", "-f", "-")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("admit runner flows through the Docker firewall: %w: %s", err, bytes.TrimSpace(output))
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
	allowDNS bool,
	protected []netip.Prefix,
	destinations []networkpolicy.Destination,
	runnerGateways []networkpolicy.RunnerGatewayDestination,
	pins map[int][]netip.Addr,
) string {
	var script bytes.Buffer
	fmt.Fprintf(&script, "add table inet %s\n", table)
	// Priority -10 runs these chains before a coexisting Docker firewall's
	// priority-0 chains, so accepted connections carry their ct mark by the
	// time DOCKER-USER admission evaluates the same packet.
	fmt.Fprintf(&script, "add chain inet %s forward { type filter hook forward priority -10; policy accept; }\n", table)
	fmt.Fprintf(&script, "add chain inet %s input { type filter hook input priority -10; policy accept; }\n", table)
	fmt.Fprintf(&script, "add rule inet %s forward iifname %q ip saddr != %s drop\n", table, interfaceName, guestIP)
	fmt.Fprintf(&script, "add rule inet %s input iifname %q ip saddr != %s drop\n", table, interfaceName, guestIP)
	fmt.Fprintf(&script, "add rule inet %s forward oifname %q ct state established,related accept\n", table, interfaceName)
	if allowDNS && dnsAddress.IsValid() {
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

// removeResidualPolicyTables deletes each per-Instance policy table family
// independently, tolerating only a genuinely absent table: a partial pair
// left by an aborted atomic delete must not survive slot release.
func removeResidualPolicyTables(ctx context.Context, table string) error {
	var joined error
	for _, arguments := range [][]string{
		{"delete", "table", "inet", table},
		{"delete", "table", "ip", table + natTableSuffix},
	} {
		if output, err := exec.CommandContext(ctx, "nft", arguments...).CombinedOutput(); err != nil &&
			!strings.Contains(string(output), "No such file") {
			joined = errors.Join(joined, fmt.Errorf("delete residual policy table %s: %w: %s",
				arguments[3], err, bytes.TrimSpace(output)))
		}
	}
	return joined
}

// writeGuestResolvConf points the sandbox resolver at the runner DNS proxy.
func writeGuestResolvConf(instanceDir, dnsAddress string) (string, error) {
	path := instanceDir + "/" + guestResolvConfName
	content := "nameserver " + dnsAddress + "\noptions timeout:2 attempts:2\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write guest resolver configuration: %w", err)
	}
	return path, nil
}
