package microvm

import (
	"agentcy/internal/config"
	"agentcy/internal/egressproxy"
	"agentcy/internal/runtimemanager"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"
)

func (m *Manager) SetSourceBindingRegistrar(registrar SourceBindingRegistrar) {
	m.sourceBindings = registrar
}

func (m *Manager) SetHostNetworkConfigurer(configurer HostNetworkConfigurer) {
	m.network = configurer
}

func (m *Manager) SetHostEgressRouter(router HostEgressRouter) {
	m.egressRouter = router
}

func (m *Manager) networkRequired(opts runtimemanager.StartOpts) bool {
	if strings.TrimSpace(m.cfg.MicroVMBridgeName) != "" {
		return true
	}
	if strings.TrimSpace(m.cfg.MicroVMGuestIP) != "" {
		return true
	}
	return opts.ProxyEgress != nil && opts.ProxyEgress.Enabled
}

func (m *Manager) tapOwnerUID() int {
	if m.cfg == nil || m.cfg.MicroVMAllowUnjailed {
		return os.Getuid()
	}
	return m.cfg.MicroVMJailerUID
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

// reserveGuestIP allocates a per-VM guest IP. When a bridge CIDR is configured it
// picks the next free host address in that subnet (skipping the gateway), giving
// each microVM a distinct source identity for the egress proxy and a distinct
// target for the service proxies. Without a bridge CIDR it falls back to
// the single configured guest IP (only correct with one concurrent VM).
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
// known; single-IP mode relies on operator-provided AG_MICROVM_KERNEL_ARGS.
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
	return fmt.Sprintf("ip=%s::%s:%s::eth0:off", guestIP, gwIP.String(), net.IP(ipnet.Mask).String())
}

func (m *Manager) registerSourceBinding(ctx context.Context, agentID, instanceID string, opts runtimemanager.StartOpts) (string, error) {
	if m.sourceBindings == nil {
		return "", nil
	}
	if opts.ProxyEgress == nil || !opts.ProxyEgress.Enabled {
		return "", nil
	}
	sourceIP := strings.TrimSpace(m.guestIP(instanceID))
	if sourceIP == "" {
		return "", fmt.Errorf("register proxy source binding: no guest IP reserved for %s (set AG_MICROVM_BRIDGE_CIDR or AG_MICROVM_GUEST_IP)", instanceID)
	}
	contextToken := ""
	if strings.TrimSpace(m.cfg.AgentRuntimeAuthSecret) != "" {
		var err error
		contextToken, err = egressproxy.MintContextToken(m.cfg.AgentRuntimeAuthSecret, egressproxy.ContextTokenClaims{
			AgentID:           agentID,
			CompartmentID:     normalizeRuntimeCompartmentID(opts.CompartmentID),
			PlaceID:           strings.TrimSpace(opts.ProxyEgress.PlaceID),
			CredentialSubject: strings.TrimSpace(opts.ActorPrincipal),
			PlatformUserID:    strings.TrimSpace(opts.RuntimeActorContext.PlatformUserID),
			AgentSessionID:    strings.TrimSpace(opts.RuntimeActorContext.SessionID),
			RequestID:         strings.TrimSpace(opts.RuntimeActorContext.RequestID),
			TurnID:            strings.TrimSpace(opts.RuntimeActorContext.WakeTurnID),
			EgressID:          instanceID,
		}, time.Now().UTC(), egressproxy.ContextTokenTTL)
		if err != nil {
			return "", fmt.Errorf("mint proxy egress context token: %w", err)
		}
	}
	binding := egressproxy.SourceBinding{
		AgentID:             agentID,
		CompartmentID:       normalizeRuntimeCompartmentID(opts.CompartmentID),
		ContainerID:         instanceID,
		SourceIP:            sourceIP,
		Generation:          instanceID,
		ContextToken:        contextToken,
		AllowlistEnabled:    opts.ProxyEgress.AllowlistEnabled,
		AllowedHosts:        append([]string(nil), opts.ProxyEgress.AllowedHosts...),
		ToolCredentialHosts: append([]string(nil), opts.ProxyEgress.ToolCredentialHosts...),
	}
	if err := m.sourceBindings.Register(binding); err != nil {
		return "", fmt.Errorf("register proxy source binding: %w", err)
	}
	if opts.ProxyEgress.TransparentHTTPPort > 0 && m.egressRouter != nil {
		tapName := ""
		if inst := m.lookup(instanceID); inst != nil {
			tapName = inst.tapName
		}
		if err := m.egressRouter.RegisterTransparentRoute(ctx, TransparentRoute{
			AgentID:     agentID,
			InstanceID:  instanceID,
			SourceIP:    sourceIP,
			HTTPPort:    opts.ProxyEgress.TransparentHTTPPort,
			InterfaceID: tapName,
		}); err != nil {
			m.unregisterSourceBinding(instanceID)
			return "", fmt.Errorf("register transparent egress route: %w", err)
		}
	}
	return contextToken, nil
}

func (m *Manager) unregisterSourceBinding(instanceID string) {
	if m.sourceBindings == nil || strings.TrimSpace(instanceID) == "" {
		return
	}
	m.sourceBindings.UnregisterContainer(instanceID)
	if m.egressRouter != nil {
		m.egressRouter.UnregisterContainer(instanceID)
	}
}

func (m *Manager) cleanupTap(ctx context.Context, tapName string) {
	if err := m.cleanupTapChecked(ctx, tapName); err != nil {
		slog.Warn("failed to remove microVM tap", "tap", tapName, "error", err)
	}
}

// cleanupTapChecked removes an instance's tap (and its launcher state) and
// reports the outcome. It retries a bounded number of times to absorb transient
// launcher contention under load; the caller uses the returned error to decide
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
