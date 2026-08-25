//go:build linux

package gvisor

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"google.golang.org/protobuf/proto"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// The network qualification builds a second namespace holding probe-owned
// HTTP targets and a file-controlled DNS resolver, routes it from the host,
// and proves the enforced matrix through guest execs under the active
// policy: exact-domain admission via the runner DNS proxy pin, disjoint
// domain, private, and metadata denial, deny-all isolation, and pin expiry
// after a DNS change.

const (
	netTargetsNamespace   = "sbxgvt"
	netTargetsHostVeth    = "gvth0"
	netTargetsGuestVeth   = "gvtg0"
	netTargetsHostAddr    = "93.184.216.1"
	netTargetsBaseAddr    = "93.184.216.2"
	netTargetsDNSAddr     = "93.184.216.10"
	netTargetsAllowedA    = "93.184.216.11"
	netTargetsAllowedB    = "93.184.216.13"
	netTargetsDeniedAddr  = "93.184.216.12"
	netTargetsHTTPPort    = 8080
	netTargetsInvocation  = "gvisor-net-targets"
	netQualPrivateTarget  = "192.168.77.7"
	netQualMetadataTarget = "169.254.169.254"
)

// runNetworkTargets serves HTTP on every target address plus a minimal DNS
// responder whose answers follow the map file; the parent rewrites the file
// to change resolution mid-scenario. It runs inside the targets namespace
// through the TestMain re-exec dispatch.
func runNetworkTargets(readyFile, dnsMapPath string) error {
	for _, address := range []string{
		netTargetsAllowedA, netTargetsAllowedB, netTargetsDeniedAddr,
		netQualPrivateTarget, netQualMetadataTarget,
	} {
		listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", address, netTargetsHTTPPort))
		if err != nil {
			return fmt.Errorf("listen %s: %w", address, err)
		}
		body := "tgt-" + address + "\n"
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		})}
		go func() { _ = server.Serve(listener) }()
	}
	dnsConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(netTargetsDNSAddr), Port: 53})
	if err != nil {
		return fmt.Errorf("listen DNS: %w", err)
	}
	go serveQualificationDNS(dnsConn, dnsMapPath)
	if err := os.WriteFile(readyFile, []byte("ready\n"), 0o600); err != nil {
		return err
	}
	select {}
}

func serveQualificationDNS(connection *net.UDPConn, mapPath string) {
	buffer := make([]byte, 512)
	for {
		length, remote, err := connection.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		response := answerQualificationDNS(buffer[:length], mapPath)
		if response != nil {
			_, _ = connection.WriteToUDP(response, remote)
		}
	}
}

func answerQualificationDNS(query []byte, mapPath string) []byte {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil || header.Response {
		return nil
	}
	question, err := parser.Question()
	if err != nil {
		return nil
	}
	name := strings.TrimSuffix(strings.ToLower(question.Name.String()), ".")
	address := lookupQualificationDNSMap(mapPath, name)
	response := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID: header.ID, Response: true, OpCode: header.OpCode,
			RecursionDesired: header.RecursionDesired, RecursionAvailable: true,
		},
		Questions: []dnsmessage.Question{question},
	}
	if question.Type == dnsmessage.TypeA && address != nil {
		response.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name: question.Name, Type: dnsmessage.TypeA,
				Class: dnsmessage.ClassINET, TTL: 2,
			},
			Body: &dnsmessage.AResource{A: [4]byte(address)},
		}}
	} else if address == nil {
		response.Header.RCode = dnsmessage.RCodeNameError
	}
	packed, err := response.Pack()
	if err != nil {
		return nil
	}
	return packed
}

func lookupQualificationDNSMap(mapPath, name string) net.IP {
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

type networkTargets struct {
	child   *exec.Cmd
	mapPath string
}

func startNetworkTargets(t *testing.T) *networkTargets {
	t.Helper()
	teardownNetworkTargetsTopology()
	steps := [][]string{
		{"netns", "add", netTargetsNamespace},
		{"link", "add", netTargetsHostVeth, "type", "veth", "peer", "name", netTargetsGuestVeth},
		{"link", "set", netTargetsGuestVeth, "netns", netTargetsNamespace},
		{"addr", "add", netTargetsHostAddr + "/24", "dev", netTargetsHostVeth},
		{"link", "set", netTargetsHostVeth, "up"},
		{"-n", netTargetsNamespace, "link", "set", "lo", "up"},
		{"-n", netTargetsNamespace, "addr", "add", netTargetsBaseAddr + "/24", "dev", netTargetsGuestVeth},
		{"-n", netTargetsNamespace, "addr", "add", netTargetsDNSAddr + "/24", "dev", netTargetsGuestVeth},
		{"-n", netTargetsNamespace, "addr", "add", netTargetsAllowedA + "/24", "dev", netTargetsGuestVeth},
		{"-n", netTargetsNamespace, "addr", "add", netTargetsAllowedB + "/24", "dev", netTargetsGuestVeth},
		{"-n", netTargetsNamespace, "addr", "add", netTargetsDeniedAddr + "/24", "dev", netTargetsGuestVeth},
		{"-n", netTargetsNamespace, "addr", "add", netQualPrivateTarget + "/32", "dev", netTargetsGuestVeth},
		{"-n", netTargetsNamespace, "addr", "add", netQualMetadataTarget + "/32", "dev", netTargetsGuestVeth},
		{"-n", netTargetsNamespace, "link", "set", netTargetsGuestVeth, "up"},
		{"-n", netTargetsNamespace, "route", "add", "default", "via", netTargetsHostAddr},
		{"route", "add", netQualPrivateTarget + "/32", "via", netTargetsBaseAddr},
		{"route", "add", netQualMetadataTarget + "/32", "via", netTargetsBaseAddr},
	}
	for _, step := range steps {
		if err := ipCommand(t.Context(), step...); err != nil {
			teardownNetworkTargetsTopology()
			t.Fatal(err)
		}
	}
	stateDir := t.TempDir()
	mapPath := stateDir + "/dns-map"
	writeQualificationDNSMap(t, mapPath, netTargetsAllowedA)
	readyFile := stateDir + "/ready"
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("ip", "netns", "exec", netTargetsNamespace,
		self, netTargetsInvocation, readyFile, mapPath)
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	child.Stdout = os.Stderr
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		teardownNetworkTargetsTopology()
		t.Fatal(err)
	}
	targets := &networkTargets{child: child, mapPath: mapPath}
	t.Cleanup(func() { targets.stop() })
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			return targets
		}
		if time.Now().After(deadline) {
			t.Fatal("network targets never became ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func writeQualificationDNSMap(t *testing.T, path, allowedAddress string) {
	t.Helper()
	content := fmt.Sprintf("allowed.test %s\ndenied.test %s\n", allowedAddress, netTargetsDeniedAddr)
	if err := os.WriteFile(path+".next", []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".next", path); err != nil {
		t.Fatal(err)
	}
}

func (targets *networkTargets) stop() {
	if targets.child != nil && targets.child.Process != nil {
		_ = syscall.Kill(-targets.child.Process.Pid, syscall.SIGKILL)
		_ = targets.child.Wait()
	}
	teardownNetworkTargetsTopology()
}

func teardownNetworkTargetsTopology() {
	_ = exec.Command("ip", "link", "delete", netTargetsHostVeth).Run()
	_ = exec.Command("ip", "netns", "delete", netTargetsNamespace).Run()
}

func guestFetch(
	t *testing.T,
	fixture qualificationFixture,
	url string,
) (string, bool) {
	t.Helper()
	result, err := fixture.backend.ExecuteBuffered(t.Context(), fixture.fence, &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{
			Argument: []string{"/bin/sh", "-c", "wget -T 3 -q -O - " + url},
		}},
		Cwd: ".", DeadlineUnixMs: uint64(time.Now().Add(15 * time.Second).UnixMilli()),
		OutputLimitBytes: 4096,
	})
	if err != nil {
		t.Fatalf("guest fetch %s: %v", url, err)
	}
	if result.Terminal.GetKind() == runnerprotocol.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED &&
		result.Terminal.GetExitCode() == 0 {
		return strings.TrimSpace(string(result.Stdout)), true
	}
	t.Logf("guest fetch %s denied: terminal=%v stderr=%q", url, result.Terminal, result.Stderr)
	return "", false
}

// TestQualifiedGVisorNetworkPolicy proves the enforced matrix end to end
// through the real backend, DNS proxy, and nftables state.
func TestQualifiedGVisorNetworkPolicy(t *testing.T) {
	qualificationBuild(t)
	targets := startNetworkTargets(t)

	fixture := newQualificationFixtureWithDNS(t, "network", netTargetsDNSAddr+":53")
	backend, fence := fixture.backend, fixture.fence
	t.Cleanup(func() { _ = backend.Shutdown(context.Background()) })

	command := proto.Clone(fixture.command).(*runnerprotocol.AssignmentCommand)
	command.NetworkPolicy = &runnerprotocol.NetworkPolicy{
		Mode: runnerprotocol.NetworkPolicyMode_NETWORK_POLICY_MODE_ALLOW_LIST,
		Destinations: []*runnerprotocol.NetworkDestination{{
			Protocol: runnerprotocol.NetworkDestinationProtocol_NETWORK_DESTINATION_PROTOCOL_TCP,
			Port:     netTargetsHTTPPort,
			Target:   &runnerprotocol.NetworkDestination_Domain{Domain: "allowed.test"},
		}},
	}
	if _, err := backend.StartAssignment(t.Context(), command,
		func(runnerprotocol.AssignmentProgressStage) error { return nil }); err != nil {
		t.Fatalf("boot allow-list Instance: %v", err)
	}
	if err := backend.MarkAssignmentReady(fence); err != nil {
		t.Fatal(err)
	}

	allowedURL := fmt.Sprintf("http://allowed.test:%d/", netTargetsHTTPPort)
	if body, ok := guestFetch(t, fixture, allowedURL); !ok || body != "tgt-"+netTargetsAllowedA {
		diagnose, _ := fixture.backend.ExecuteBuffered(t.Context(), fixture.fence, &runnerprotocol.ExecOpen{
			Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{
				Argument: []string{"/bin/sh", "-c", "cat /etc/resolv.conf; nslookup allowed.test 2>&1 | head -6; ip addr 2>&1 | head -12"},
			}},
			Cwd: ".", DeadlineUnixMs: uint64(time.Now().Add(15 * time.Second).UnixMilli()),
			OutputLimitBytes: 8192,
		})
		t.Logf("guest diagnostics:\n%s\n%s", diagnose.Stdout, diagnose.Stderr)
		t.Fatalf("allowed domain fetch = %q ok=%t", body, ok)
	}
	if _, ok := guestFetch(t, fixture, fmt.Sprintf("http://denied.test:%d/", netTargetsHTTPPort)); ok {
		t.Fatal("disjoint domain was admitted")
	}
	if _, ok := guestFetch(t, fixture, fmt.Sprintf("http://%s:%d/", netQualPrivateTarget, netTargetsHTTPPort)); ok {
		t.Fatal("private address was admitted")
	}
	if _, ok := guestFetch(t, fixture, fmt.Sprintf("http://%s:%d/", netQualMetadataTarget, netTargetsHTTPPort)); ok {
		t.Fatal("metadata address was admitted")
	}

	// A DNS change rotates the pin: the new address is admitted after
	// resolution, and the old pinned address is revoked once its bounded TTL
	// expires.
	writeQualificationDNSMap(t, targets.mapPath, netTargetsAllowedB)
	deadline := time.Now().Add(30 * time.Second)
	for {
		body, ok := guestFetch(t, fixture, allowedURL)
		if ok && body == "tgt-"+netTargetsAllowedB {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new pin never admitted; last body=%q ok=%t", body, ok)
		}
		time.Sleep(500 * time.Millisecond)
	}
	oldPinURL := fmt.Sprintf("http://%s:%d/", netTargetsAllowedA, netTargetsHTTPPort)
	deadline = time.Now().Add(30 * time.Second)
	for {
		if _, ok := guestFetch(t, fixture, oldPinURL); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old pinned address was never revoked after TTL expiry")
		}
		time.Sleep(500 * time.Millisecond)
	}

	if _, err := backend.FenceAssignment(t.Context(), &runnerprotocol.FenceCommand{
		Fence: fence, DeadlineUnixMs: uint64(time.Now().Add(15 * time.Second).UnixMilli()),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestQualifiedGVisorDenyAllBlocksRoutedTargets proves total isolation under
// deny_all while the agent transport keeps operating.
func TestQualifiedGVisorDenyAllBlocksRoutedTargets(t *testing.T) {
	qualificationBuild(t)
	startNetworkTargets(t)
	fixture := newQualificationFixtureWithDNS(t, "denyall", netTargetsDNSAddr+":53")
	backend, fence := fixture.backend, fixture.fence
	t.Cleanup(func() { _ = backend.Shutdown(context.Background()) })
	if _, err := backend.StartAssignment(t.Context(), fixture.command,
		func(runnerprotocol.AssignmentProgressStage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := backend.MarkAssignmentReady(fence); err != nil {
		t.Fatal(err)
	}
	if _, ok := guestFetch(t, fixture, fmt.Sprintf("http://%s:%d/", netTargetsAllowedA, netTargetsHTTPPort)); ok {
		t.Fatal("deny_all admitted a routed target")
	}
	// The agent transport is independent of the policed path.
	result, err := backend.ExecuteBuffered(t.Context(), fence, &runnerprotocol.ExecOpen{
		Command:          &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", "printf agent-alive"}}},
		Cwd:              ".",
		OutputLimitBytes: 1024,
	})
	if err != nil || !bytes.Equal(result.Stdout, []byte("agent-alive")) {
		t.Fatalf("agent transport under deny_all = %#v, %v", result, err)
	}
}
