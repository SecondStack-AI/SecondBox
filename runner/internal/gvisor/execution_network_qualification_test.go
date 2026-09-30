//go:build linux && listener_qualification

package gvisor

import (
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
	"google.golang.org/protobuf/proto"
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
	assignment := &runnerprotocol.AssignmentCommand{
		Fence:                         &runnerprotocol.AssignmentFence{SandboxId: "sandbox", InstanceId: "instance", AssignmentId: "assignment", SandboxGeneration: 3},
		EgressContext:                 "tenant-a",
		AttributedExecutionPermission: &runnerprotocol.AttributedExecutionPermission{Gateway: "tools.internal", MaximumConnections: 2},
	}
	attributedGateway, err := firecracker.ResolveAttributedExecGateway(contexts, assignment)
	if err != nil {
		t.Fatal(err)
	}
	network, _, err := backend.installInstanceNetwork(t.Context(), assignment, denyAll)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.teardownInstanceNetwork(assignment.Fence.InstanceId, network); err != nil {
			t.Error(err)
		}
	})
	active := &activeAssignment{fence: assignment.Fence, network: network, attributedGateway: attributedGateway}
	openWindow := func(reference string) *firecracker.AttributedExecWindow {
		t.Helper()
		expiry := time.Now().Add(time.Minute)
		window, err := backend.openAttributedExecWindow(t.Context(), active, assignment.Fence, &runnerprotocol.ExecOpen{
			DeadlineUnixMs: uint64(expiry.UnixMilli()),
			AttributedExecution: &runnerprotocol.AttributedExecution{
				TenantRef: "tenant-a", SubjectRef: "subject-a", AuthorizationRef: reference, ExpiresAtUnixMs: uint64(expiry.UnixMilli()),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return window
	}
	policyTable := func() string {
		t.Helper()
		output, err := exec.CommandContext(t.Context(), "nft", "list", "table", "inet", firecracker.PolicyTableName(assignment.Fence.InstanceId)).CombinedOutput()
		if err != nil {
			t.Fatalf("Instance policy: %v: %s", err, output)
		}
		return string(output)
	}
	var previous netip.AddrPort
	for _, reference := range []string{"authorization-1", "authorization-2"} {
		window := openWindow(reference)
		endpoint := window.Gateway()
		if endpoint == previous {
			t.Fatalf("sequential exec windows share listener %s", endpoint)
		}
		previous = endpoint
		result := make(chan error, 1)
		go func() {
			connection, err := gateway.AcceptUnix()
			if err != nil {
				result <- err
				return
			}
			defer connection.Close()
			attribution, err := egressattribution.ReadRunnerExecutionAttribution(connection, uint32(os.Getuid()), time.Now().Add(5*time.Second))
			if err == nil && (attribution.AssignmentID != "assignment" || attribution.Generation != 3 || attribution.SubjectRef != "subject-a" ||
				attribution.TenantRef != "tenant-a" || attribution.AuthorizationRef != reference) {
				err = fmt.Errorf("wrong execution attribution: %+v", attribution)
			}
			if err == nil {
				_, err = connection.Write([]byte("ok\n"))
			}
			result <- err
		}()
		probe(network.namespaceName, endpoint, true)
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		probe("", endpoint, false)
		if !strings.Contains(policyTable(), fmt.Sprintf("tcp dport %d", endpoint.Port())) {
			t.Fatalf("window listener missing from the Instance policy:\n%s", policyTable())
		}
		if err := window.Close(); err != nil {
			t.Fatal(err)
		}
		probe(network.namespaceName, endpoint, false)
		if strings.Contains(policyTable(), fmt.Sprintf("tcp dport %d", endpoint.Port())) {
			t.Fatalf("closed window left its listener in the Instance policy:\n%s", policyTable())
		}
	}
	// A Runner crash leaves an open window's rules; startup reconciliation
	// removes them with the rest of the profile's residue.
	crashed := openWindow("authorization-crashed")
	if err := reconcileStaleNetworks(t.Context(), backend.config.NetworkProfile); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("nft", "list", "tables").CombinedOutput()
	if err != nil || strings.Contains(string(output), "sbx_exec_") {
		t.Fatalf("restart left execution rules: %v: %s", err, output)
	}
	if err := crashed.Close(); err != nil {
		t.Fatalf("closing a swept window = %v", err)
	}
	withoutRoute := proto.CloneOf(assignment)
	withoutRoute.AttributedExecutionPermission.Gateway = "absent.internal"
	if _, err := firecracker.ResolveAttributedExecGateway(contexts, withoutRoute); err == nil {
		t.Fatal("unconfigured attributed gateway resolved")
	}
}
