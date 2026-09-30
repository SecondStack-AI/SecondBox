//go:build linux && listener_qualification

package egressforwarder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && (os.Args[1] == "--listener-probe" || os.Args[1] == "--listener-hold") {
		connection, err := net.DialTimeout("tcp", os.Args[2], 300*time.Millisecond)
		if err != nil {
			os.Exit(1)
		}
		defer connection.Close()
		if err := connection.SetDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			os.Exit(1)
		}
		var reply [2]byte
		if _, err := io.ReadFull(connection, reply[:]); err != nil || string(reply[:]) != "ok" {
			os.Exit(1)
		}
		if os.Args[1] == "--listener-hold" {
			if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				os.Exit(1)
			}
			fmt.Println("ready")
			if _, err := io.Copy(io.Discard, connection); err != nil {
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExecutionListenerRecoveryKeepsOtherInterfaces(t *testing.T) {
	type listener struct{ guestInterface, listenerID string }
	listeners := []listener{{"ownedtap", "0000000000000001"}, {"ownedtap", "0000000000000002"}, {"othertap", "0000000000000003"}}
	for _, owned := range listeners {
		policy := ExecutionListenerPolicy{InstanceID: owned.guestInterface, GuestInterface: owned.guestInterface, BridgeInterface: "execbr",
			GuestAddress: netip.MustParseAddr("10.0.0.2"), ListenerAddress: netip.MustParseAddrPort("10.0.0.1:41000"), ListenerID: owned.listenerID}
		rules, err := RenderExecutionListenerPolicy(policy)
		if err != nil {
			t.Fatal(err)
		}
		if err := applyExecutionListenerRules(t.Context(), "/usr/sbin/nft", rules); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := RemoveExecutionListenerRules(context.Background(), "/usr/sbin/nft", []string{owned.guestInterface}); err != nil {
				t.Error(err)
			}
		})
	}
	for range 2 {
		if err := RemoveExecutionListenerRules(t.Context(), "/usr/sbin/nft", []string{"ownedtap"}); err != nil {
			t.Fatal(err)
		}
	}
	output, err := exec.Command("nft", "list", "tables").CombinedOutput()
	if err != nil || strings.Contains(string(output), executionListenerTablePrefix("ownedtap")) ||
		strings.Count(string(output), executionListenerTable("othertap", "0000000000000003")) != 2 {
		t.Fatalf("interface-scoped listener recovery: %v: %s", err, output)
	}
}

// Run only inside a disposable network namespace with NET_ADMIN and SYS_ADMIN.
func TestExecutionListenerFirewallQualification(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("listener qualification requires an isolated root network namespace")
	}
	for _, bridged := range []bool{false, true} {
		t.Run(map[bool]string{false: "routed_veth", true: "bridge_port"}[bridged], func(t *testing.T) {
			command := func(name string, args ...string) {
				t.Helper()
				if output, err := exec.Command(name, args...).CombinedOutput(); err != nil {
					t.Fatalf("%s %v: %v: %s", name, args, err, output)
				}
			}
			command("ip", "link", "set", "lo", "up")
			for _, suffix := range []string{"a", "b"} {
				namespace := "attr-" + suffix
				command("ip", "netns", "add", namespace)
				t.Cleanup(func() {
					if output, err := exec.Command("ip", "netns", "delete", namespace).CombinedOutput(); err != nil {
						t.Errorf("remove namespace: %v: %s", err, output)
					}
				})
				command("ip", "link", "add", "attrh"+suffix, "type", "veth", "peer", "name", "attrg"+suffix)
				t.Cleanup(func() { command("ip", "link", "delete", "attrh"+suffix) })
				command("ip", "link", "set", "attrg"+suffix, "netns", namespace)
				command("ip", "link", "set", "attrh"+suffix, "up")
				command("ip", "-n", namespace, "link", "set", "attrg"+suffix, "up")
				command("ip", "-n", namespace, "link", "set", "lo", "up")
			}
			policy := ExecutionListenerPolicy{InstanceID: t.Name(), GuestInterface: "attrha", GuestAddress: netip.MustParseAddr("10.73.1.2"), ListenerID: "00000000000000aa"}
			if bridged {
				policy.BridgeInterface = "attrbr"
				command("ip", "link", "add", "attrbr", "type", "bridge")
				t.Cleanup(func() { command("ip", "link", "delete", "attrbr") })
				command("ip", "addr", "add", "10.73.1.1/24", "dev", "attrbr")
				command("ip", "link", "set", "attrbr", "up")
				for _, suffix := range []string{"a", "b"} {
					command("ip", "link", "set", "attrh"+suffix, "master", "attrbr")
				}
				command("ip", "-n", "attr-b", "addr", "add", "10.73.1.3/24", "dev", "attrgb")
			} else {
				command("ip", "addr", "add", "10.73.1.1/24", "dev", "attrha")
				command("ip", "addr", "add", "10.73.2.1/24", "dev", "attrhb")
				command("ip", "-n", "attr-b", "addr", "add", "10.73.2.2/24", "dev", "attrgb")
				command("ip", "-n", "attr-b", "route", "add", "10.73.1.1/32", "via", "10.73.2.1")
			}
			command("ip", "-n", "attr-a", "addr", "add", "10.73.1.2/24", "dev", "attrga")
			listen := func() netip.AddrPort {
				listener, err := net.Listen("tcp4", "10.73.1.1:0")
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					for {
						connection, err := listener.Accept()
						if err != nil {
							return
						}
						_, writeErr := connection.Write([]byte("ok"))
						closeErr := connection.Close()
						if writeErr != nil || closeErr != nil {
							t.Errorf("probe reply: write=%v close=%v", writeErr, closeErr)
						}
					}
				}()
				t.Cleanup(func() { listener.Close(); <-done })
				return listener.Addr().(*net.TCPAddr).AddrPort()
			}
			policy.ListenerAddress = listen()
			ordinary := listen()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			probe := func(namespace string, endpoint netip.AddrPort, allowed bool) {
				t.Helper()
				args := []string{binary, "--listener-probe", endpoint.String()}
				if namespace != "" {
					args = append([]string{"ip", "netns", "exec", namespace}, args...)
				}
				output, err := exec.Command(args[0], args[1:]...).CombinedOutput()
				if (err == nil) != allowed {
					t.Fatalf("probe namespace=%q endpoint=%s allowed=%v: %v: %s", namespace, endpoint, allowed, err, output)
				}
			}
			for _, source := range []string{"attr-a", "attr-b", ""} {
				probe(source, policy.ListenerAddress, true)
			}
			rules, err := RenderExecutionListenerPolicy(policy)
			if err != nil {
				t.Fatal(err)
			}
			nft := exec.Command("nft", "-f", "-")
			nft.Stdin = strings.NewReader(rules)
			if output, err := nft.CombinedOutput(); err != nil {
				t.Fatalf("install listener rules: %v: %s\n%s", err, output, rules)
			}
			installed := true
			removePolicy := func() {
				command("nft", "delete", "table", "inet", executionListenerTable(policy.GuestInterface, policy.ListenerID))
				if bridged {
					command("nft", "delete", "table", "bridge", executionListenerTable(policy.GuestInterface, policy.ListenerID))
				}
				installed = false
			}
			t.Cleanup(func() {
				if installed {
					removePolicy()
				}
			})
			probe("attr-a", policy.ListenerAddress, true)
			probe("attr-b", policy.ListenerAddress, false)
			probe("", policy.ListenerAddress, false)
			command("ip", "-n", "attr-a", "addr", "delete", "10.73.1.2/24", "dev", "attrga")
			command("ip", "-n", "attr-a", "addr", "add", "10.73.1.9/24", "dev", "attrga")
			probe("attr-a", policy.ListenerAddress, false)
			command("ip", "-n", "attr-a", "addr", "delete", "10.73.1.9/24", "dev", "attrga")
			command("ip", "-n", "attr-a", "addr", "add", "10.73.1.2/24", "dev", "attrga")
			if bridged {
				command("ip", "-n", "attr-b", "addr", "delete", "10.73.1.3/24", "dev", "attrgb")
				command("ip", "-n", "attr-b", "addr", "add", "10.73.1.2/24", "dev", "attrgb")
			} else {
				command("ip", "-n", "attr-b", "addr", "add", "10.73.1.2/32", "dev", "attrgb")
				command("ip", "-n", "attr-b", "route", "replace", "10.73.1.1/32", "via", "10.73.2.1", "src", "10.73.1.2")
			}
			probe("attr-b", policy.ListenerAddress, false)
			if bridged {
				command("ip", "-n", "attr-b", "addr", "delete", "10.73.1.2/24", "dev", "attrgb")
				command("ip", "-n", "attr-b", "addr", "add", "10.73.1.3/24", "dev", "attrgb")
				command("ip", "neigh", "flush", "dev", "attrbr")
			} else {
				command("ip", "-n", "attr-b", "route", "replace", "10.73.1.1/32", "via", "10.73.2.1", "src", "10.73.2.2")
				command("ip", "-n", "attr-b", "addr", "delete", "10.73.1.2/32", "dev", "attrgb")
			}
			probe("attr-a", policy.ListenerAddress, true)
			probe("attr-b", ordinary, true)
			probe("", ordinary, true)
			removePolicy()
			probe("attr-b", policy.ListenerAddress, true)
			qualifyOwnedExecutionForwarder(t, policy, probe)
		})
	}
}

func qualifyOwnedExecutionForwarder(t *testing.T, policy ExecutionListenerPolicy, probe func(string, netip.AddrPort, bool)) {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "attr-gateway-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	gateway, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(directory, "gateway.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	attribution := forwarderTestAttribution()
	attribution.InstanceID = policy.InstanceID
	results := make(chan error, 4)
	gatewayDone := make(chan struct{})
	go func() {
		defer close(gatewayDone)
		for {
			connection, err := gateway.AcceptUnix()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("gateway accept: %v", err)
				}
				return
			}
			got, readErr := egressattribution.ReadRunnerExecutionAttribution(connection, uint32(os.Getuid()), time.Now().Add(time.Second))
			if readErr == nil && got != attribution {
				readErr = fmt.Errorf("gateway received different attribution: %+v", got)
			}
			if readErr == nil {
				readErr = connection.SetDeadline(attribution.ExpiresAt)
			}
			if readErr == nil {
				_, readErr = connection.Write([]byte("ok"))
			}
			if readErr == nil {
				_, readErr = io.Copy(io.Discard, connection)
			}
			results <- errors.Join(readErr, connection.Close())
		}
	}()
	t.Cleanup(func() { gateway.Close(); <-gatewayDone })
	policy.ListenerAddress = netip.AddrPortFrom(policy.ListenerAddress.Addr(), 0)
	config := ExecutionForwarderConfig{NFTPath: "/usr/sbin/nft", GatewaySocket: gateway.Addr().String(), Policy: policy, Attribution: attribution, MaximumConnections: 2, Admission: func() error { return nil }}
	marker := filepath.Join(directory, "installed")
	failingNFT := filepath.Join(directory, "nft-fail-after-install")
	script := "#!/bin/sh\nif [ \"$1\" = \"-f\" ] && [ ! -e \"" + marker + "\" ]; then\n/usr/sbin/nft \"$@\" || exit $?\n: > \"" + marker + "\"\nexit 42\nfi\nexec /usr/sbin/nft \"$@\"\n"
	if err := os.WriteFile(failingNFT, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	failedConfig := config
	failedConfig.NFTPath = failingNFT
	if forwarder, err := StartExecutionForwarder(t.Context(), failedConfig); err == nil || forwarder != nil {
		t.Fatal("startup succeeded after firewall command failure")
	}
	if output, err := exec.Command("nft", "list", "tables").CombinedOutput(); err != nil || strings.Contains(string(output), executionListenerTablePrefix(policy.GuestInterface)) {
		t.Fatalf("failed startup leaked firewall rules: %v: %s", err, output)
	}
	// A startup paused before the sweep lock while teardown fences the
	// Instance and sweeps its interface installs nothing once it resumes.
	var fenced atomic.Bool
	fencedConfig := config
	fencedConfig.Attribution.ExpiresAt = time.Now().UTC().Add(10 * time.Second).Truncate(time.Millisecond)
	fencedConfig.Admission = func() error {
		if fenced.Load() {
			return errors.New("instance fenced")
		}
		return nil
	}
	paused, resume := make(chan struct{}), make(chan struct{})
	executionListenerStartupHook = func() {
		close(paused)
		<-resume
	}
	fencedStartup := make(chan error, 1)
	go func() {
		forwarder, err := StartExecutionForwarder(t.Context(), fencedConfig)
		if forwarder != nil {
			err = errors.Join(errors.New("fenced startup returned a forwarder"), forwarder.Close(context.Background()))
		}
		fencedStartup <- err
	}()
	select {
	case <-paused:
	case err := <-fencedStartup:
		t.Fatalf("startup ended before the sweep lock: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("startup never reached the sweep lock")
	}
	executionListenerStartupHook = nil
	fenced.Store(true)
	if err := RemoveExecutionListenerRules(t.Context(), config.NFTPath, []string{policy.GuestInterface}); err != nil {
		t.Fatal(err)
	}
	close(resume)
	if err := <-fencedStartup; err == nil || !strings.Contains(err.Error(), "instance fenced") {
		t.Fatalf("startup after the teardown fence = %v", err)
	}
	if output, err := exec.Command("nft", "list", "tables").CombinedOutput(); err != nil || strings.Contains(string(output), executionListenerTablePrefix(policy.GuestInterface)) {
		t.Fatalf("fenced startup installed firewall rules: %v: %s", err, output)
	}
	// Admission runs while the startup holds the sweep lock, so a teardown
	// that fences during admission waits for the startup and then revokes it.
	admitted, release := make(chan bool, 1), make(chan struct{})
	overlapConfig := config
	overlapConfig.Attribution.ExpiresAt = time.Now().UTC().Add(10 * time.Second).Truncate(time.Millisecond)
	overlapConfig.Admission = func() error {
		locked := !executionListenerTablesMu.TryLock()
		if !locked {
			executionListenerTablesMu.Unlock()
		}
		admitted <- locked
		<-release
		return nil
	}
	type startup struct {
		forwarder *ExecutionForwarder
		err       error
	}
	overlapStartup := make(chan startup, 1)
	go func() {
		forwarder, err := StartExecutionForwarder(t.Context(), overlapConfig)
		overlapStartup <- startup{forwarder, err}
	}()
	select {
	case locked := <-admitted:
		if !locked {
			close(release)
			t.Fatal("forwarder admission ran outside the sweep lock")
		}
	case failed := <-overlapStartup:
		t.Fatalf("startup ended before admission: %v", failed.err)
	case <-time.After(10 * time.Second):
		t.Fatal("startup never reached admission")
	}
	overlapSweep := make(chan error, 1)
	go func() {
		overlapSweep <- RemoveExecutionListenerRules(t.Context(), config.NFTPath, []string{policy.GuestInterface})
	}()
	close(release)
	overlapped := <-overlapStartup
	if overlapped.err != nil {
		t.Fatal(overlapped.err)
	}
	if err := <-overlapSweep; err != nil {
		t.Fatal(err)
	}
	select {
	case <-overlapped.forwarder.Done():
	default:
		t.Fatal("teardown sweep left an admitted forwarder running")
	}
	if output, err := exec.Command("nft", "list", "tables").CombinedOutput(); err != nil || strings.Contains(string(output), executionListenerTablePrefix(policy.GuestInterface)) {
		t.Fatalf("teardown sweep left an admitted forwarder's rules: %v: %s", err, output)
	}
	if err := overlapped.forwarder.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A sibling exec window on the same interface keeps its own listener
	// table when this window closes.
	sibling, err := StartExecutionForwarder(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := sibling.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	forwarder, err := StartExecutionForwarder(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	if forwarder.policy.ListenerID == sibling.policy.ListenerID || forwarder.ListenerAddress() == sibling.ListenerAddress() {
		t.Fatalf("exec windows share listener identity: %s %s", forwarder.policy.ListenerID, forwarder.ListenerAddress())
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := forwarder.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	probe("attr-a", forwarder.ListenerAddress(), true)
	probe("attr-b", forwarder.ListenerAddress(), false)
	probe("", forwarder.ListenerAddress(), false)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client := exec.CommandContext(ctx, "ip", "netns", "exec", "attr-a", binary, "--listener-hold", forwarder.ListenerAddress().String())
	stdout, err := client.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	var ready [6]byte
	if _, err := io.ReadFull(stdout, ready[:]); err != nil || string(ready[:]) != "ready\n" {
		cancel()
		client.Wait()
		t.Fatalf("held client readiness: %v", err)
	}
	if err := forwarder.Close(ctx); err != nil {
		cancel()
		client.Wait()
		t.Fatal(err)
	}
	if err := client.Wait(); err != nil {
		t.Fatalf("active client did not receive EOF on close: %v", err)
	}
	if err := forwarder.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("forwarding outcome: %v", err)
	}
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("gateway relay did not close")
		}
	}
	if output, err := exec.Command("nft", "list", "table", "inet", executionListenerTable(policy.GuestInterface, forwarder.policy.ListenerID)).CombinedOutput(); err == nil || !strings.Contains(string(output), "No such file") {
		t.Fatalf("listener firewall remained after close: %v: %s", err, output)
	}
	if output, err := exec.Command("nft", "list", "table", "inet", executionListenerTable(policy.GuestInterface, sibling.policy.ListenerID)).CombinedOutput(); err != nil {
		t.Fatalf("closing one exec window removed its sibling's listener firewall: %v: %s", err, output)
	}
	probe("attr-a", sibling.ListenerAddress(), true)
	probe("attr-b", sibling.ListenerAddress(), false)
	// Instance teardown sweeps the interface: the live listener closes before
	// the table restricting it is deleted.
	if err := RemoveExecutionListenerRules(ctx, "/usr/sbin/nft", []string{policy.GuestInterface}); err != nil {
		t.Fatal(err)
	}
	if err := sibling.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("swept listener outcome: %v", err)
	}
	probe("attr-a", sibling.ListenerAddress(), false)
	probe("", sibling.ListenerAddress(), false)
	if output, err := exec.Command("nft", "list", "tables").CombinedOutput(); err != nil || strings.Contains(string(output), executionListenerTablePrefix(policy.GuestInterface)) {
		t.Fatalf("interface sweep left listener tables: %v: %s", err, output)
	}
}
