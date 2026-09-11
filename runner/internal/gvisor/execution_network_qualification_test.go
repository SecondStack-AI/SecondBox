//go:build linux && listener_qualification

package gvisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
	"github.com/SecondStack-AI/SecondBox/runner/internal/firecracker"
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

// Run only in an isolated network namespace with NET_ADMIN and SYS_ADMIN.
func TestAttributedExecutionNetworkQualification(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "gvisor-attribution-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	socket := filepath.Join(directory, "gateway.sock")
	gateway, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close() })
	configPath := filepath.Join(directory, "contexts.json")
	content := fmt.Sprintf(`{"schemaVersion":"secondbox.runner-egress-contexts/v1","contexts":[{"name":"tenant-a","gateways":[{"logicalName":"tools.internal","attributedSocket":%q,"address":"10.0.0.2"}]}]}`, socket)
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	contexts, err := networkpolicy.LoadEgressContextConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	backend := &AssignmentBackend{nftPath: "/usr/sbin/nft"}
	backend.config.NetworkProfile = 0
	backend.config.NetworkPolicy = networkpolicy.RunnerConfig{
		CompileOptions: networkpolicy.CompileOptions{MaximumPins: 1, MaximumTTL: time.Second}, EgressContexts: contexts,
	}
	backend.enforcer = firecracker.NewNetworkPolicyEnforcer(backend.nftPath, netip.Addr{}, netip.AddrPort{}, renderInetPolicy, deleteInetPolicyTables)
	t.Cleanup(func() {
		if err := backend.enforcer.Close(); err != nil {
			t.Error(err)
		}
	})
	options, err := backend.config.NetworkPolicy.CompileOptionsForAssignment("tenant-a", true)
	if err != nil {
		t.Fatal(err)
	}
	denyAll, err := networkpolicy.Compile(networkpolicy.Policy{Mode: networkpolicy.ModeDenyAll}, options)
	if err != nil {
		t.Fatal(err)
	}
	probe := func(namespace string, endpoint netip.AddrPort, allowed bool) {
		t.Helper()
		args := []string{"2", "bash", "-c", fmt.Sprintf("exec 3<>/dev/tcp/%s/%d; read -r line <&3; test \"$line\" = ok", endpoint.Addr(), endpoint.Port())}
		command := exec.CommandContext(t.Context(), "timeout", args...)
		if namespace != "" {
			command = exec.CommandContext(t.Context(), "ip", append([]string{"netns", "exec", namespace, "timeout"}, args...)...)
		}
		output, err := command.CombinedOutput()
		if (err == nil) != allowed {
			t.Fatalf("probe namespace=%q allowed=%t: %v: %s", namespace, allowed, err, output)
		}
	}
	for generation := int64(1); generation <= 2; generation++ {
		assignment := &runnerprotocol.AssignmentCommand{
			Fence:               &runnerprotocol.AssignmentFence{SandboxId: "sandbox", InstanceId: fmt.Sprintf("instance-%d", generation), AssignmentId: fmt.Sprintf("assignment-%d", generation), SandboxGeneration: uint64(generation)},
			EgressContext:       "tenant-a",
			AttributedExecution: &runnerprotocol.AttributedExecution{TenantRef: "tenant-a", SubjectRef: "subject-a", AuthorizationRef: "authorization", Gateway: "tools.internal", MaximumConnections: 2, ExpiresAtUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli())},
		}
		network, _, err := backend.installInstanceNetwork(t.Context(), assignment, denyAll, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := backend.teardownInstanceNetwork(assignment.Fence.InstanceId, network); err != nil {
				t.Error(err)
			}
		})
		if network.index != 0 {
			t.Fatalf("cleaned network slot was not reused: %d", network.index)
		}
		result := make(chan error, 1)
		go func() {
			connection, err := gateway.AcceptUnix()
			if err != nil {
				result <- err
				return
			}
			defer connection.Close()
			attribution, err := egressattribution.ReadRunnerExecutionAttribution(connection, uint32(os.Getuid()), time.Now().Add(5*time.Second))
			if err == nil && (attribution.AssignmentID != assignment.Fence.AssignmentId || attribution.Generation != generation || attribution.SubjectRef != "subject-a" || attribution.TenantRef != "tenant-a") {
				err = fmt.Errorf("wrong execution attribution: %+v", attribution)
			}
			if err == nil {
				_, err = connection.Write([]byte("ok\n"))
			}
			result <- err
		}()
		endpoint := network.executionForwarder.ListenerAddress()
		probe(network.namespaceName, endpoint, true)
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		probe("", endpoint, false)
		output, err := exec.CommandContext(t.Context(), "nft", "list", "table", "inet", firecracker.PolicyTableName(assignment.Fence.InstanceId)).CombinedOutput()
		if err != nil || strings.Contains(string(output), "dport 53 ") || strings.Contains(string(output), "daddr 10.0.0.2 tcp") {
			t.Fatalf("private forwarding policy: %v: %s", err, output)
		}
		network.executionForwarder.Revoke()
		probe(network.namespaceName, endpoint, false)
		if generation == 2 {
			if err := network.executionForwarder.Wait(); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if err := reconcileStaleNetworks(t.Context(), backend.config.NetworkProfile); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command("nft", "list", "tables").CombinedOutput()
			if err != nil || strings.Contains(string(output), "sbx_exec_") {
				t.Fatalf("restart left execution rules: %v: %s", err, output)
			}
		}
		if err := backend.teardownInstanceNetwork(assignment.Fence.InstanceId, network); err != nil {
			t.Fatal(err)
		}
		if len(backend.networkSlots) != 0 {
			t.Fatal("teardown retained network slot")
		}
		assignment.AttributedExecution.Gateway = "absent.internal"
		if _, _, err := backend.installInstanceNetwork(t.Context(), assignment, denyAll, options); err == nil {
			t.Fatal("unconfigured attributed gateway accepted")
		}
		if len(backend.networkSlots) != 0 {
			t.Fatal("failed attributed startup retained network slot")
		}
	}
}
