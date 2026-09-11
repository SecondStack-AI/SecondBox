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
	for _, name := range []string{"ownedtap", "othertap"} {
		policy := ExecutionListenerPolicy{InstanceID: name, GuestInterface: name, BridgeInterface: "execbr",
			GuestAddress: netip.MustParseAddr("10.0.0.2"), ListenerAddress: netip.MustParseAddrPort("10.0.0.1:41000")}
		rules, err := RenderExecutionListenerPolicy(policy)
		if err != nil {
			t.Fatal(err)
		}
		if err := applyExecutionListenerRules(t.Context(), "/usr/sbin/nft", rules); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := RemoveExecutionListenerRules(context.Background(), "/usr/sbin/nft", []string{name}); err != nil {
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
	if err != nil || strings.Contains(string(output), executionListenerTable("ownedtap")) || strings.Count(string(output), executionListenerTable("othertap")) != 2 {
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
			policy := ExecutionListenerPolicy{InstanceID: t.Name(), GuestInterface: "attrha", GuestAddress: netip.MustParseAddr("10.73.1.2")}
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
				command("nft", "delete", "table", "inet", executionListenerTable(policy.GuestInterface))
				if bridged {
					command("nft", "delete", "table", "bridge", executionListenerTable(policy.GuestInterface))
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
	config := ExecutionForwarderConfig{NFTPath: "/usr/sbin/nft", GatewaySocket: gateway.Addr().String(), Policy: policy, Attribution: attribution, MaximumConnections: 2}
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
	if output, err := exec.Command("nft", "list", "table", "inet", executionListenerTable(policy.GuestInterface)).CombinedOutput(); err == nil || !strings.Contains(string(output), "No such file") {
		t.Fatalf("failed startup leaked firewall rules: %v: %s", err, output)
	}
	forwarder, err := StartExecutionForwarder(t.Context(), config)
	if err != nil {
		t.Fatal(err)
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
	if output, err := exec.Command("nft", "list", "table", "inet", executionListenerTable(policy.GuestInterface)).CombinedOutput(); err == nil || !strings.Contains(string(output), "No such file") {
		t.Fatalf("listener firewall remained after close: %v: %s", err, output)
	}
}
