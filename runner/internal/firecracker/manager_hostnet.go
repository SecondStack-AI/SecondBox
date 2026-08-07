package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"net"
	"os"
	"strings"
	"time"
)

func (m *Manager) SetHostNetworkConfigurer(configurer HostNetworkConfigurer) {
	m.network = configurer
}

func (m *Manager) SetHostNetworkPolicyEnforcer(enforcer HostNetworkPolicyEnforcer) {
	m.networkPolicy = enforcer
}

func (m *Manager) networkPolicyCompileOptions() networkpolicy.CompileOptions {
	return networkpolicy.CompileOptions{
		MaximumPins:        m.cfg.NetworkPolicyMaximumDNSPins,
		MaximumTTL:         m.cfg.NetworkPolicyMaximumDNSTTL,
		RunnerAddresses:    append([]netip.Addr(nil), m.cfg.NetworkPolicyRunnerAddresses...),
		ManagementPrefixes: append([]netip.Prefix(nil), m.cfg.NetworkPolicyManagementCIDRs...),
		RunnerGateways:     cloneRunnerGateways(m.cfg.NetworkPolicyRunnerGateways),
	}
}

func cloneRunnerGateways(source map[string]netip.Addr) map[string]netip.Addr {
	result := make(map[string]netip.Addr, len(source))
	for domain, address := range source {
		result[domain] = address
	}
	return result
}

func (m *Manager) networkRequired(opts runtimemanager.StartOpts) bool {
	_ = opts
	return microVMNetworkRequired(m.cfg)
}

// microVMNetworkRequired reports whether this runner gives guests a network
// device. It is deployment configuration, not a per-start choice, which is why a
// snapshot template's recorded network shape follows from it: a template built
// on a runner without guest networking records no interface, and a resumed guest
// can never acquire one.
func microVMNetworkRequired(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if strings.TrimSpace(cfg.MicroVMBridgeName) != "" {
		return true
	}
	return strings.TrimSpace(cfg.MicroVMGuestIP) != ""
}

func (m *Manager) tapOwnerUID(jailerUID int) int {
	if m.cfg == nil || m.cfg.MicroVMAllowUnjailed {
		return os.Getuid()
	}
	return jailerUID
}

func tapNameForInstance(prefix, instanceID string) string {
	prefix = sanitizeTapPrefix(prefix)
	if prefix == "" {
		prefix = "agfc"
	}
	sum := sha256.Sum256([]byte(instanceID))
	return fmt.Sprintf("%s%x", prefix, sum[:])[:min(15, len(prefix)+10)]
}

func sanitizeTapPrefix(prefix string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(prefix) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 5 {
		return out[:5]
	}
	return out
}

func guestMACForInstance(instanceID string) string {
	sum := sha256.Sum256([]byte(instanceID))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

// reserveGuestIP allocates a per-VM guest IP from the configured bridge subnet.
// Without a bridge CIDR it uses the single explicitly configured guest IP.
func (m *Manager) reserveGuestIP(instanceID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.guestIPs == nil {
		m.guestIPs = map[string]string{}
	}
	cidr := strings.TrimSpace(m.cfg.MicroVMBridgeCIDR)
	if cidr == "" {
		ip := strings.TrimSpace(m.cfg.MicroVMGuestIP)
		m.guestIPs[instanceID] = ip
		return ip, nil
	}
	gwIP, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse bridge CIDR %q: %w", cidr, err)
	}
	gw4, base, mask := gwIP.To4(), ipnet.IP.To4(), ipnet.Mask
	if gw4 == nil || base == nil || len(mask) != 4 {
		return "", fmt.Errorf("microVM bridge CIDR %q must be IPv4", cidr)
	}
	used := make(map[string]bool, len(m.guestIPs))
	for _, ip := range m.guestIPs {
		used[ip] = true
	}
	maskU := binary.BigEndian.Uint32(mask)
	network := binary.BigEndian.Uint32(base) & maskU
	broadcast := network | ^maskU
	gwU := binary.BigEndian.Uint32(gw4)
	for host := network + 1; host < broadcast; host++ {
		if host == gwU {
			continue
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], host)
		s := net.IP(b[:]).String()
		if used[s] {
			continue
		}
		m.guestIPs[instanceID] = s
		return s, nil
	}
	return "", fmt.Errorf("no free guest IP available in %s", cidr)
}

func (m *Manager) releaseGuestIP(instanceID string) {
	m.mu.Lock()
	delete(m.guestIPs, instanceID)
	m.mu.Unlock()
}

func (m *Manager) guestIP(instanceID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.guestIPs[instanceID]
}

// guestIPBootArg renders the kernel ip= autoconfiguration argument for a per-VM
// guest IP. It is only emitted in bridge mode, where the gateway and netmask are
// known; single-IP mode relies on operator-provided SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS.
func guestIPBootArg(cfg *config.Config, guestIP string) string {
	guestIP = strings.TrimSpace(guestIP)
	cidr := strings.TrimSpace(cfg.MicroVMBridgeCIDR)
	if guestIP == "" || cidr == "" {
		return ""
	}
	gwIP, ipnet, err := net.ParseCIDR(cidr)
	if err != nil || ipnet.IP.To4() == nil {
		return ""
	}
	return fmt.Sprintf(
		"ip=%s::%s:%s::eth0:off:%s",
		guestIP,
		gwIP.String(),
		net.IP(ipnet.Mask).String(),
		gwIP.String(),
	)
}

// guestAddressCIDR renders a reserved guest address with the bridge's prefix
// length. A cold-booted guest receives the same pair through the kernel `ip=`
// argument's address and netmask fields; a resumed guest receives it in its
// assignment bind, because its kernel finished booting before this Sandbox
// existed. It is empty when this runner gives guests no network device.
func guestAddressCIDR(guestIP, bridgeCIDR string) string {
	address, err := netip.ParseAddr(strings.TrimSpace(guestIP))
	if err != nil {
		return ""
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(bridgeCIDR))
	if err != nil {
		return ""
	}
	return netip.PrefixFrom(address, prefix.Bits()).String()
}

func bridgeAddress(cidr string) netip.Addr {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil {
		return netip.Addr{}
	}
	return prefix.Addr()
}

func (m *Manager) joinInstanceNetworkCleanup(ctx context.Context, instanceID, tapName string, cause error) error {
	if err := m.cleanupNetworkChecked(ctx, instanceID, tapName); err != nil {
		return errors.Join(cause, fmt.Errorf("remove microVM network after launch failure: %w", err))
	}
	return cause
}

func (m *Manager) cleanupNetworkChecked(ctx context.Context, instanceID, tapName string) error {
	var cleanupErr error
	if m.networkPolicy != nil && strings.TrimSpace(instanceID) != "" {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := m.networkPolicy.Remove(cleanupCtx, instanceID)
		cancel()
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove host policy for %q: %w", instanceID, err))
		}
	}
	if err := m.cleanupTapChecked(ctx, tapName); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove TAP %q: %w", tapName, err))
	}
	return cleanupErr
}

// cleanupTapChecked removes an instance's tap and reports the outcome. It
// retries a bounded number of times to absorb transient host networking
// contention under load; the caller uses the returned error to decide
// whether the guest identity is safe to recycle.
func (m *Manager) cleanupTapChecked(ctx context.Context, tapName string) error {
	if m.network == nil || strings.TrimSpace(tapName) == "" {
		return nil
	}
	_ = ctx
	const attempts = 3
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := m.network.RemoveTap(cleanupCtx, tapName)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}
