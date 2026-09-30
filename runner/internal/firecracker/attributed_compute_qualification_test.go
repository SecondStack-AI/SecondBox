//go:build linux

package firecracker

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
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/egressattribution"
	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/egressforwarder"
	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

// newFirecrackerAttributedQualification starts one ordinary jailed, networked
// Instance under the default deny-all policy and a Runner-side gateway socket.
func newFirecrackerAttributedQualification(t *testing.T) (*Manager, *instance, *net.UnixListener) {
	t.Helper()
	if os.Getenv("SECONDBOX_RUNNER_QUALIFY_FIRECRACKER") != "1" {
		t.Skip("set SECONDBOX_RUNNER_QUALIFY_FIRECRACKER=1 for jailed attributed compute qualification")
	}
	workDir := shortSmokeDir(t)
	cfg := &config.Config{
		FirecrackerPath:        requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_PATH"),
		JailerPath:             requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH"),
		MicroVMKernelPath:      requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"),
		MicroVMRootfsPath:      requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"),
		MicroVMSharedImagePath: requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH"),
		MicroVMPublicKeyPath:   requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"),
		MicroVMPublicKeySHA256: requiredEnv(t, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"),
		MicroVMKernelArgs:      requiredEnv(t, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS"),
		RunnerWorkspaceRoot:    filepath.Join(workDir, "workspaces"),
		MicroVMRunDir:          filepath.Join(workDir, "run"), MicroVMLogDir: filepath.Join(workDir, "logs"),
		MicroVMSnapshotTemplateCacheRoot: filepath.Join(workDir, "templates"),
		MicroVMMemoryMiB:                 512, MicroVMVCPUs: 1, MicroVMWorkspaceSizeMiB: 256,
		MicroVMJailerChrootBaseDir: filepath.Join(workDir, "jail"),
		MicroVMJailerUIDStart:      52000, MicroVMJailerUIDCount: 16, MicroVMJailerGID: 52000,
		MicroVMJailerCgroupVersion: 2, MicroVMJailerParentCgroup: "attributed-qualification",
		MicroVMBridgeName: "attrbr", MicroVMBridgeCIDR: "10.254.72.1/29", MicroVMGuestIP: "10.254.72.2", MicroVMTapPrefix: "attr",
		NetworkPolicyNFTPath: "/usr/sbin/nft", NetworkPolicyMaximumDNSPins: 1, NetworkPolicyMaximumDNSTTL: time.Minute,
		NetworkPolicyRunnerAddresses: []netip.Addr{netip.MustParseAddr("10.254.72.1")},
		NetworkPolicyManagementCIDRs: []netip.Prefix{netip.MustParsePrefix("10.254.72.0/29")},
		NetworkPolicyDNSUpstream:     netip.MustParseAddrPort("127.0.0.1:53"),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	manager, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetRunnerEvidenceSink(runnerevidence.SlogSink{}, "runner-attributed-qualification")
	t.Cleanup(func() {
		if err := manager.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
		if output, err := exec.Command("ip", "link", "delete", cfg.MicroVMBridgeName).CombinedOutput(); err != nil {
			t.Errorf("attributed bridge cleanup: %v: %s", err, output)
		}
	})
	store, err := workspacestore.New(ctx, workspacestore.Config{Root: cfg.RunnerWorkspaceRoot, TemplateCapacityBytes: 256 << 20, FormatterKind: workspacestore.FormatterMke2fs})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetWorkspaceStore(store); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, workspacestore.CreateWorkspaceRequest{Mutation: workspacestore.Mutation{OperationID: "create-attributed", WorkspaceID: "attributed", FencingToken: []byte("attributed-workspace-fence-00000")}, CapacityBytes: 256 << 20}); err != nil {
		t.Fatal(err)
	}
	attachment, err := store.Open(ctx, "attributed", 1)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(workDir, "gateway.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close() })
	opts := smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone: "UTC", CompartmentID: "instance-attributed", AssignmentID: "assignment-attributed",
		RequestID: "request-attributed", OperationID: "operation-attributed",
		WorkspaceAttachment: attachment,
	})
	instanceID, err := manager.createAndStart(ctx, "sandbox-attributed", opts)
	if err != nil {
		if closeErr := attachment.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatalf("attributed qualification VM startup: %v\n%s", err, latestSmokeLog(t, workDir))
	}
	instance := manager.lookup(instanceID)
	if instance == nil || instance.jailRoot == "" || instance.guestProtocolSession == nil || instance.tapName == "" {
		t.Fatal("qualification VM did not establish a jailed networked guest session")
	}
	return manager, instance, gateway
}

var qualificationAttributedFence = &runnerprotocol.AssignmentFence{
	AssignmentId: "assignment-attributed", SandboxId: "sandbox-attributed", InstanceId: "instance-attributed", SandboxGeneration: 1,
}

// openQualificationExecWindow opens one attributed exec window inside the
// running ordinary Instance, as the assignment backend does for an ExecOpen.
func openQualificationExecWindow(t *testing.T, manager *Manager, instance *instance, gateway *net.UnixListener, reference string, lifetime time.Duration, script string) (*AttributedExecWindow, *runnerprotocol.ExecOpen) {
	t.Helper()
	expiry := time.Now().Add(lifetime)
	open := &runnerprotocol.ExecOpen{
		Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", script}}},
		Cwd:     ".", DeadlineUnixMs: uint64(expiry.UnixMilli()), OutputLimitBytes: 1024,
		AttributedExecution: &runnerprotocol.AttributedExecution{
			TenantRef: "tenant-a", SubjectRef: "owner-a", AuthorizationRef: reference, ExpiresAtUnixMs: uint64(expiry.UnixMilli()),
		},
	}
	window, err := OpenAttributedExecWindow(t.Context(), AttributedExecWindowConfig{
		NFTPath: manager.cfg.NetworkPolicyNFTPath, Policy: manager.networkPolicy, PolicyInstanceID: instance.id,
		Gateway: AttributedExecGateway{Socket: gateway.Addr().String(), MaximumConnections: 2},
		Fence:   qualificationAttributedFence, Open: open,
		Listener: egressforwarder.ExecutionListenerPolicy{
			GuestInterface: instance.tapName, BridgeInterface: manager.cfg.MicroVMBridgeName,
			GuestAddress:    netip.MustParseAddr(instance.guestIP),
			ListenerAddress: netip.AddrPortFrom(bridgeAddress(manager.cfg.MicroVMBridgeCIDR), 0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return window, open
}

func acceptQualificationAttribution(gateway *net.UnixListener, reference string, expiry time.Time) <-chan error {
	result := make(chan error, 1)
	go func() {
		connection, err := gateway.AcceptUnix()
		if err != nil {
			result <- err
			return
		}
		defer connection.Close()
		actual, err := egressattribution.ReadRunnerExecutionAttribution(connection, uint32(os.Getuid()), expiry)
		if err == nil && (actual.SubjectRef != "owner-a" || actual.AuthorizationRef != reference ||
			actual.InstanceID != qualificationAttributedFence.InstanceId || actual.AssignmentID != qualificationAttributedFence.AssignmentId) {
			err = fmt.Errorf("incorrect Firecracker attribution: %+v", actual)
		}
		if err == nil {
			err = connection.SetDeadline(expiry)
		}
		request := make([]byte, len("guest-context: forged\n"))
		if err == nil {
			_, err = io.ReadFull(connection, request)
		}
		if err == nil && string(request) != "guest-context: forged\n" {
			err = fmt.Errorf("unexpected guest bytes: %q", request)
		}
		if err == nil {
			_, err = connection.Write([]byte(reference))
		}
		result <- err
	}()
	return result
}

func runQualificationExec(t *testing.T, instance *instance, script string) BufferedGuestExecResult {
	t.Helper()
	result, err := instance.guestProtocolSession.ExecuteBuffered(t.Context(), qualificationAttributedFence.AssignmentId, &guestv1.ExecRequest{
		Command: &guestv1.ExecRequest_Argv{Argv: &guestv1.ArgvCommand{Argument: []string{"/bin/sh", "-c", script}}},
		Cwd:     ".", DeadlineUnixMs: uint64(time.Now().Add(20 * time.Second).UnixMilli()), OutputLimitBytes: 1024,
	}, netip.AddrPort{})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Each attributed exec in one ordinary Instance gets its own listener and
// identity; a daemon survives, and the listener is gone once the exec ends.
func TestSmokeFirecrackerAttributedExecWindows(t *testing.T) {
	manager, instance, gateway := newFirecrackerAttributedQualification(t)
	if result := runQualificationExec(t, instance, `nohup sh -c 'while :; do sleep 1; done' >/dev/null 2>&1 & echo $! > daemon.pid`); result.Terminal.GetExitCode() != 0 {
		t.Fatalf("daemon start: %+v %q", result.Terminal, result.Stderr)
	}
	var previous netip.AddrPort
	for _, reference := range []string{"authorization-1", "authorization-2"} {
		script := `endpoint=$SECONDBOX_EXECUTION_GATEWAY; printf %s "$endpoint" > gateway-` + reference + `; printf 'guest-context: forged\n' | nc -w 2 "${endpoint%:*}" "${endpoint##*:}"`
		window, open := openQualificationExecWindow(t, manager, instance, gateway, reference, time.Minute, script)
		if window.Gateway() == previous {
			t.Fatalf("sequential attributed execs share listener %s", previous)
		}
		previous = window.Gateway()
		expiry := time.UnixMilli(int64(open.AttributedExecution.ExpiresAtUnixMs))
		if err := gateway.SetDeadline(expiry); err != nil {
			t.Fatal(err)
		}
		accepted := acceptQualificationAttribution(gateway, reference, expiry)
		executed, err := ExecuteBufferedOverSession(window.Context(), instance.guestProtocolSession, qualificationAttributedFence.AssignmentId, open, window.Gateway())
		if closeErr := window.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil || executed.Terminal.GetExitCode() != 0 || string(executed.Stdout) != reference {
			t.Fatalf("attributed exec: %v stdout=%q stderr=%q terminal=%+v", err, executed.Stdout, executed.Stderr, executed.Terminal)
		}
		if err := <-accepted; err != nil {
			t.Fatal(err)
		}
		probe := runQualificationExec(t, instance, `endpoint=$(cat gateway-`+reference+`); nc -z -w 2 "${endpoint%:*}" "${endpoint##*:}"`)
		if probe.Terminal.GetExitCode() == 0 {
			t.Fatalf("closed attributed listener %s stayed reachable", window.Gateway())
		}
	}
	if result := runQualificationExec(t, instance, `kill -0 "$(cat daemon.pid)"`); result.Terminal.GetExitCode() != 0 {
		t.Fatal("background daemon did not survive attributed execs")
	}
	if running, err := manager.IsRunning(t.Context(), instance.id); err != nil || !running {
		t.Fatalf("attributed execs stopped the Instance: running=%v error=%v", running, err)
	}
}

// Revoking a window closes an active relay held by a descendant process;
// only stopping the Instance ends its compute.
func TestSmokeFirecrackerAttributedWindowRevocation(t *testing.T) {
	for _, trigger := range []string{"exec-end", "expiry", "stop"} {
		t.Run(trigger, func(t *testing.T) {
			manager, instance, gateway := newFirecrackerAttributedQualification(t)
			lifetime, script := time.Minute, `endpoint=$SECONDBOX_EXECUTION_GATEWAY; ( { printf ready; sleep 120; } | nc "${endpoint%:*}" "${endpoint##*:}" ) & wait`
			switch trigger {
			case "exec-end":
				script = `endpoint=$SECONDBOX_EXECUTION_GATEWAY; ( { printf ready; sleep 120; } | nc "${endpoint%:*}" "${endpoint##*:}" ) & sleep 3`
			case "expiry":
				lifetime = 8 * time.Second
			}
			window, open := openQualificationExecWindow(t, manager, instance, gateway, "authorization-"+trigger, lifetime, script)
			expiry := time.UnixMilli(int64(open.AttributedExecution.ExpiresAtUnixMs))
			if err := gateway.SetDeadline(expiry); err != nil {
				t.Fatal(err)
			}
			commandDone := make(chan error, 1)
			go func() {
				_, err := ExecuteBufferedOverSession(window.Context(), instance.guestProtocolSession, qualificationAttributedFence.AssignmentId, open, window.Gateway())
				commandDone <- errors.Join(err, window.Close())
			}()
			connection, err := gateway.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if _, err := egressattribution.ReadRunnerExecutionAttribution(connection, uint32(os.Getuid()), expiry); err != nil {
				t.Fatal(err)
			}
			if err := connection.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
				t.Fatal(err)
			}
			ready := make([]byte, len("ready"))
			if _, err := io.ReadFull(connection, ready); err != nil || string(ready) != "ready" {
				t.Fatalf("attributed descendant readiness: %q %v", ready, err)
			}
			if trigger == "stop" {
				if err := manager.Remove(t.Context(), instance.id); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := connection.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("attributed connection survived revocation: bytes=%d error=%v", n, err)
			}
			select {
			case <-commandDone:
			case <-time.After(20 * time.Second):
				t.Fatal("attributed exec did not return after revocation")
			}
			running, err := manager.IsRunning(t.Context(), instance.id)
			if err != nil || running != (trigger != "stop") {
				t.Fatalf("Instance after %s: running=%v error=%v", trigger, running, err)
			}
		})
	}
}
