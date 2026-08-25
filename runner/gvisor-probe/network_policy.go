package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/firecracker"
	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

// proofNetworkPolicy proves the fail-closed egress design Task 6H must build:
// a per-Instance network namespace with a routed veth and NAT egress, runsc's
// netstack attached to that device, and an inet-family nftables rendering of
// deny-all and an exact domain/port allow-list with DNS pinning. Every probe
// request runs through the guest agent under the active policy, which also
// proves the agent transport is independent of the policed network path.
func proofNetworkPolicy(env *probeEnv) error {
	if env.rootless {
		emit(env.stdout, "network-policy", "skipped", "reason=rootless_development_mode")
		return nil
	}
	if env.agentPath == "" {
		return fmt.Errorf("network proof requires -agent")
	}
	base := filepath.Join(env.workDir, "network-policy")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	topology := newNetworkTopology()
	if err := topology.create(base, env); err != nil {
		topology.destroy()
		return err
	}
	defer topology.destroy()
	return runNetworkScenario(env, base, topology)
}

const (
	sandboxNamespace = "sbxp-net"
	targetsNamespace = "sbxp-tgt"

	sandboxHostVeth  = "vh-sbx0"
	sandboxGuestVeth = "vs-sbx0"
	targetsHostVeth  = "vh-tgt0"
	targetsGuestVeth = "vs-tgt0"

	sandboxHostAddress  = "10.200.7.1"
	sandboxGuestAddress = "10.200.7.2"
	targetsHostAddress  = "10.201.0.1"
	targetsBaseAddress  = "10.201.0.2"

	dnsAddress          = "10.201.0.10"
	allowedFirstTarget  = "10.201.0.11"
	deniedTarget        = "10.201.0.12"
	allowedSecondTarget = "10.201.0.13"
	privateTarget       = "192.168.77.7"
	metadataTarget      = "169.254.169.254"
	targetPort          = 8080

	nftTable    = "sbxprobe"
	nftNATTable = "sbxprobenat"
)

type networkTopology struct {
	created  bool
	targets  *exec.Cmd
	dnsMap   string
	readyDir string
}

func newNetworkTopology() *networkTopology {
	return &networkTopology{}
}

func ipCommand(arguments ...string) error {
	command := exec.Command("ip", arguments...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("ip %s: %v: %s", strings.Join(arguments, " "), err, bytes.TrimSpace(output))
	}
	return nil
}

func nftScript(script string) error {
	command := exec.Command("nft", "-f", "-")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, bytes.TrimSpace(output))
	}
	return nil
}

func (topology *networkTopology) create(base string, env *probeEnv) error {
	topology.created = true
	topology.readyDir = filepath.Join(base, "targets")
	topology.dnsMap = filepath.Join(topology.readyDir, "dns-map")
	if err := os.MkdirAll(topology.readyDir, 0o700); err != nil {
		return err
	}
	if err := writeDNSMap(topology.dnsMap, allowedFirstTarget); err != nil {
		return err
	}

	steps := [][]string{
		{"netns", "add", sandboxNamespace},
		{"netns", "add", targetsNamespace},
		{"link", "add", sandboxHostVeth, "type", "veth", "peer", "name", sandboxGuestVeth},
		{"link", "set", sandboxGuestVeth, "netns", sandboxNamespace},
		{"addr", "add", sandboxHostAddress + "/30", "dev", sandboxHostVeth},
		{"link", "set", sandboxHostVeth, "up"},
		{"-n", sandboxNamespace, "addr", "add", sandboxGuestAddress + "/30", "dev", sandboxGuestVeth},
		{"-n", sandboxNamespace, "link", "set", sandboxGuestVeth, "up"},
		{"-n", sandboxNamespace, "link", "set", "lo", "up"},
		{"-n", sandboxNamespace, "route", "add", "default", "via", sandboxHostAddress},
		{"link", "add", targetsHostVeth, "type", "veth", "peer", "name", targetsGuestVeth},
		{"link", "set", targetsGuestVeth, "netns", targetsNamespace},
		{"addr", "add", targetsHostAddress + "/24", "dev", targetsHostVeth},
		{"link", "set", targetsHostVeth, "up"},
		{"-n", targetsNamespace, "link", "set", "lo", "up"},
		{"-n", targetsNamespace, "addr", "add", targetsBaseAddress + "/24", "dev", targetsGuestVeth},
		{"-n", targetsNamespace, "addr", "add", dnsAddress + "/24", "dev", targetsGuestVeth},
		{"-n", targetsNamespace, "addr", "add", allowedFirstTarget + "/24", "dev", targetsGuestVeth},
		{"-n", targetsNamespace, "addr", "add", deniedTarget + "/24", "dev", targetsGuestVeth},
		{"-n", targetsNamespace, "addr", "add", allowedSecondTarget + "/24", "dev", targetsGuestVeth},
		{"-n", targetsNamespace, "addr", "add", privateTarget + "/32", "dev", targetsGuestVeth},
		{"-n", targetsNamespace, "addr", "add", metadataTarget + "/32", "dev", targetsGuestVeth},
		{"-n", targetsNamespace, "link", "set", targetsGuestVeth, "up"},
		{"-n", targetsNamespace, "route", "add", "default", "via", targetsHostAddress},
		{"route", "add", privateTarget + "/32", "via", targetsBaseAddress},
		{"route", "add", metadataTarget + "/32", "via", targetsBaseAddress},
	}
	for _, step := range steps {
		if err := ipCommand(step...); err != nil {
			return err
		}
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable IP forwarding: %w", err)
	}

	// Base enforcement: everything from the sandbox veth traverses the egress
	// chain and is dropped unless a rule accepts it; return traffic rides
	// conntrack. NAT masquerades sandbox egress toward the targets link.
	if err := nftScript(fmt.Sprintf(`
add table inet %[1]s
add chain inet %[1]s forward { type filter hook forward priority 0 ; policy accept ; }
add chain inet %[1]s egress
add rule inet %[1]s forward oifname %[2]q ct state established,related accept
add rule inet %[1]s forward iifname %[2]q jump egress
add rule inet %[1]s forward iifname %[2]q counter drop
add table ip %[3]s
add chain ip %[3]s post { type nat hook postrouting priority 100 ; }
add rule ip %[3]s post oifname %[4]q masquerade
`, nftTable, sandboxHostVeth, nftNATTable, targetsHostVeth)); err != nil {
		return err
	}

	// The DNS and HTTP targets run inside the targets namespace through a
	// re-executed probe child.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	readyFile := filepath.Join(topology.readyDir, "ready")
	topology.targets = exec.Command("ip", "netns", "exec", targetsNamespace,
		self,
		"-internal-net-targets", readyFile,
		"-internal-net-dns-map", topology.dnsMap,
	)
	topology.targets.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	topology.targets.Stdout = os.Stderr
	topology.targets.Stderr = os.Stderr
	if err := topology.targets.Start(); err != nil {
		return fmt.Errorf("start targets child: %w", err)
	}
	if err := waitForFile(readyFile, 30*time.Second); err != nil {
		return fmt.Errorf("targets child never became ready: %w", err)
	}
	return nil
}

func (topology *networkTopology) destroy() {
	if !topology.created {
		return
	}
	if topology.targets != nil && topology.targets.Process != nil {
		_ = syscall.Kill(-topology.targets.Process.Pid, syscall.SIGKILL)
		_ = topology.targets.Wait()
	}
	_ = exec.Command("nft", "delete", "table", "inet", nftTable).Run()
	_ = exec.Command("nft", "delete", "table", "ip", nftNATTable).Run()
	_ = exec.Command("ip", "netns", "del", sandboxNamespace).Run()
	_ = exec.Command("ip", "netns", "del", targetsNamespace).Run()
}

func writeDNSMap(path, allowedAddress string) error {
	content := fmt.Sprintf("allowed.probe %s\ndenied.probe %s\n", allowedAddress, deniedTarget)
	staging := path + ".next"
	if err := os.WriteFile(staging, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(staging, path)
}

func applyEgressState(pinnedAddress string) error {
	if pinnedAddress == "" {
		// deny_all: an empty egress chain falls through to the final drop.
		return nftScript(fmt.Sprintf("flush chain inet %s egress\n", nftTable))
	}
	return nftScript(fmt.Sprintf(`
flush chain inet %[1]s egress
add rule inet %[1]s egress ip daddr %[2]s counter drop
add rule inet %[1]s egress ip daddr %[3]s counter drop
add rule inet %[1]s egress ip daddr %[4]s udp dport 53 counter accept
add rule inet %[1]s egress ip daddr %[5]s tcp dport %[6]d counter accept
`, nftTable, metadataTarget, privateTarget, dnsAddress, pinnedAddress, targetPort))
}

func runNetworkScenario(env *probeEnv, base string, topology *networkTopology) error {
	area, err := newProofArea(env, base, "scenario")
	if err != nil {
		return err
	}
	socketDir := filepath.Join(base, "scenario", "sockets")
	workspaceDir := filepath.Join(base, "scenario", "workspace")
	runtimePrivateDir := filepath.Join(base, "scenario", "runtime-private")
	for _, directory := range []string{socketDir, workspaceDir, runtimePrivateDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}
	identity := agentIdentity{
		instanceID:      "probe-net-instance-1",
		sandboxID:       "probe-net-sandbox-1",
		generation:      1,
		buildID:         "secondbox-gvisor-probe-agent",
		imageDigest:     "sha256:" + repeatHex("c"),
		toolchainDigest: "sha256:" + repeatHex("d"),
	}
	if err := writeBundle(area.bundleDir, env.guestPath, bundleConfig{
		Entrypoint: []string{
			"/agent",
			"-control-socket", "/probe-sockets/control.sock",
			"-protocol-socket", "/probe-sockets/protocol.sock",
			"-instance-id", identity.instanceID,
			"-sandbox-id", identity.sandboxID,
			"-sandbox-generation", "1",
			"-guest-build-id", identity.buildID,
			"-image-manifest-digest", identity.imageDigest,
			"-toolchain-manifest-digest", identity.toolchainDigest,
			"-heartbeat-interval", "5s",
		},
		ExtraBinaries: map[string]string{"agent": env.agentPath},
		RootfsFiles: map[string][]byte{
			"etc/resolv.conf": []byte("nameserver " + dnsAddress + "\noptions timeout:1 attempts:1\n"),
		},
		NetworkNamespacePath: "/var/run/netns/" + sandboxNamespace,
		Binds: []bindMount{
			{Source: socketDir, Destination: "/probe-sockets", ReadOnly: false},
			{Source: workspaceDir, Destination: "/workspace", ReadOnly: false},
			{Source: runtimePrivateDir, Destination: "/runtime-private", ReadOnly: false},
		},
	}); err != nil {
		return err
	}

	command := env.runscRun(area, "run", "--host-uds=all", "--network=sandbox")
	if err := command.Start(); err != nil {
		return fmt.Errorf("start network sandbox: %w", err)
	}
	defer reapArea(env, area, command)

	protocolSocket := filepath.Join(socketDir, "protocol.sock")
	if err := waitForFile(protocolSocket, bootDeadline); err != nil {
		return fmt.Errorf("agent protocol socket never appeared: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	session, err := negotiateOverUnixSocket(ctx, protocolSocket, identity)
	if err != nil {
		return fmt.Errorf("negotiate: %w", err)
	}
	defer session.Close()

	allowedURL := fmt.Sprintf("http://allowed.probe:%d/", targetPort)
	deniedURL := fmt.Sprintf("http://denied.probe:%d/", targetPort)
	privateURL := fmt.Sprintf("http://%s:%d/", privateTarget, targetPort)
	metadataURL := fmt.Sprintf("http://%s:%d/", metadataTarget, targetPort)
	firstPinURL := fmt.Sprintf("http://%s:%d/", allowedFirstTarget, targetPort)

	// State 1: deny_all. The agent transport keeps working while every
	// network path is dropped.
	if err := applyEgressState(""); err != nil {
		return err
	}
	denyAll, err := runNetcheck(ctx, session, map[string]string{
		"allowed": "get|" + allowedURL,
		"direct":  "get|" + firstPinURL,
	})
	if err != nil {
		return fmt.Errorf("deny-all netcheck: %w", err)
	}
	if err := expectOutcomes(denyAll, map[string]string{
		"allowed": "blocked",
		"direct":  "blocked",
	}); err != nil {
		return fmt.Errorf("deny-all: %w", err)
	}
	emit(env.stdout, "network-deny-all", "passed",
		"agent_transport=alive", "targets_blocked=2")

	// State 2: exact allow-list with a DNS pin for allowed.probe.
	if err := applyEgressState(allowedFirstTarget); err != nil {
		return err
	}
	allowList, err := runNetcheck(ctx, session, map[string]string{
		"allowed":  "get|" + allowedURL,
		"denied":   "get|" + deniedURL,
		"private":  "get|" + privateURL,
		"metadata": "get|" + metadataURL,
	})
	if err != nil {
		return fmt.Errorf("allow-list netcheck: %w", err)
	}
	if err := expectOutcomes(allowList, map[string]string{
		"allowed":  "ok:tgt-" + allowedFirstTarget,
		"denied":   "blocked",
		"private":  "blocked",
		"metadata": "blocked",
	}); err != nil {
		return fmt.Errorf("allow-list: %w", err)
	}
	emit(env.stdout, "network-allow-list", "passed",
		"allowed=tgt-"+allowedFirstTarget,
		"denied_domain=true", "denied_private=true", "denied_metadata=true")

	// State 3: the DNS answer changes; the pin rotates, so the former address
	// is revoked and the new one is admitted.
	if err := writeDNSMap(topology.dnsMap, allowedSecondTarget); err != nil {
		return err
	}
	if err := applyEgressState(allowedSecondTarget); err != nil {
		return err
	}
	dnsChange, err := runNetcheck(ctx, session, map[string]string{
		"allowed":  "get|" + allowedURL,
		"old_pin":  "get|" + firstPinURL,
		"metadata": "get|" + metadataURL,
	})
	if err != nil {
		return fmt.Errorf("dns-change netcheck: %w", err)
	}
	if err := expectOutcomes(dnsChange, map[string]string{
		"allowed":  "ok:tgt-" + allowedSecondTarget,
		"old_pin":  "blocked",
		"metadata": "blocked",
	}); err != nil {
		return fmt.Errorf("dns-change: %w", err)
	}
	emit(env.stdout, "network-dns-change", "passed",
		"new_pin=tgt-"+allowedSecondTarget, "old_pin_revoked=true")
	return nil
}

// runNetcheck executes the guest netcheck through the agent under the active
// policy and parses its stdout outcome lines.
func runNetcheck(
	ctx context.Context,
	session *firecracker.GuestProtocolSession,
	checks map[string]string,
) (map[string]string, error) {
	arguments := []string{"/guest", "netcheck"}
	for label, check := range checks {
		arguments = append(arguments, label+"|"+check)
	}
	result, err := session.ExecuteBuffered(ctx, "probe-net-assignment-1", &guestv1.ExecRequest{
		Command:          &guestv1.ExecRequest_Argv{Argv: &guestv1.ArgvCommand{Argument: arguments}},
		Cwd:              ".",
		DeadlineUnixMs:   uint64(time.Now().Add(120 * time.Second).UnixMilli()),
		OutputLimitBytes: 1 << 20,
	})
	if err != nil {
		return nil, err
	}
	if result.Terminal.GetKind() != guestv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED ||
		result.Terminal.GetExitCode() != 0 {
		return nil, fmt.Errorf("netcheck terminal %v (stderr %q)", result.Terminal, result.Stderr)
	}
	outcomes := map[string]string{}
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		label, outcome, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("netcheck line %q is malformed", line)
		}
		outcomes[label] = outcome
	}
	return outcomes, nil
}

func expectOutcomes(got, want map[string]string) error {
	for label, expected := range want {
		if got[label] != expected {
			return fmt.Errorf("%s = %q, want %q (all: %v)", label, got[label], expected, got)
		}
	}
	return nil
}

// runNetTargets is the targets-namespace child: HTTP listeners on every
// target address plus a minimal DNS responder whose answers follow the map
// file, so the parent can change the resolution mid-scenario.
func runNetTargets(readyFile, dnsMapPath string) error {
	addresses := []string{
		allowedFirstTarget, deniedTarget, allowedSecondTarget, privateTarget, metadataTarget,
	}
	for _, address := range addresses {
		listener, err := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(targetPort)))
		if err != nil {
			return fmt.Errorf("listen %s: %w", address, err)
		}
		body := "tgt-" + address + "\n"
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		})}
		go func() { _ = server.Serve(listener) }()
	}
	dnsConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(dnsAddress), Port: 53})
	if err != nil {
		return fmt.Errorf("listen DNS: %w", err)
	}
	go serveDNS(dnsConn, dnsMapPath)
	if err := os.WriteFile(readyFile, []byte("ready\n"), 0o600); err != nil {
		return err
	}
	select {}
}

// serveDNS answers A queries from the map file with a five-second TTL and
// NXDOMAIN for unknown names. It implements only what the probe needs.
func serveDNS(connection *net.UDPConn, mapPath string) {
	buffer := make([]byte, 512)
	for {
		length, remote, err := connection.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		response := answerDNS(buffer[:length], mapPath)
		if response != nil {
			_, _ = connection.WriteToUDP(response, remote)
		}
	}
}

func answerDNS(query []byte, mapPath string) []byte {
	if len(query) < 17 || binary.BigEndian.Uint16(query[4:6]) != 1 {
		return nil
	}
	name, questionEnd := decodeDNSName(query, 12)
	if questionEnd == 0 || questionEnd+4 > len(query) {
		return nil
	}
	queryType := binary.BigEndian.Uint16(query[questionEnd : questionEnd+2])
	address := lookupDNSMap(mapPath, strings.ToLower(name))
	header := make([]byte, 12)
	copy(header[0:2], query[0:2])
	binary.BigEndian.PutUint16(header[4:6], 1)
	question := query[12 : questionEnd+4]
	if queryType != 1 || address == nil {
		binary.BigEndian.PutUint16(header[2:4], 0x8183) // NXDOMAIN
		return append(header, question...)
	}
	binary.BigEndian.PutUint16(header[2:4], 0x8180)
	binary.BigEndian.PutUint16(header[6:8], 1)
	answer := []byte{0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 5, 0, 4}
	answer = append(answer, address...)
	return append(append(header, question...), answer...)
}

func decodeDNSName(message []byte, offset int) (string, int) {
	var labels []string
	for offset < len(message) {
		length := int(message[offset])
		offset++
		if length == 0 {
			return strings.Join(labels, "."), offset
		}
		if offset+length > len(message) {
			return "", 0
		}
		labels = append(labels, string(message[offset:offset+length]))
		offset += length
	}
	return "", 0
}

func lookupDNSMap(mapPath, name string) net.IP {
	content, err := os.ReadFile(mapPath)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.EqualFold(fields[0], name) {
			if address := net.ParseIP(fields[1]); address != nil {
				return address.To4()
			}
		}
	}
	return nil
}
