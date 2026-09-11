//go:build linux

package firecracker

import (
	"context"
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
	"github.com/SecondStack-AI/SecondBox/runner/internal/networkpolicy"
	"github.com/SecondStack-AI/SecondBox/runner/internal/runnerevidence"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

func newFirecrackerAttributedQualification(t *testing.T, lifetime time.Duration) (*Manager, *instance, *net.UnixListener, egressattribution.ExecutionAttribution) {
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
	expiry := time.Now().Add(lifetime)
	guard, err := runtimemanager.NewAttributedExecutionGuard("assignment-attributed", expiry)
	if err != nil {
		t.Fatal(err)
	}
	attribution := egressattribution.ExecutionAttribution{TenantRef: "tenant-a", SubjectRef: "owner-a", SandboxID: "sandbox-attributed", InstanceID: "instance-attributed", AssignmentID: "assignment-attributed", Generation: 1, AuthorizationRef: "authorization-a", ExpiresAt: expiry}
	opts := smokeGuestProtocolOpts(t, cfg, runtimemanager.StartOpts{
		Timezone: "UTC", CompartmentID: attribution.InstanceID, AssignmentID: attribution.AssignmentID,
		RequestID: "request-attributed", OperationID: "operation-attributed",
		WorkspaceAttachment: attachment, AttributedExecution: guard,
		ExecutionNetwork: &runtimemanager.AttributedExecutionNetwork{Attribution: attribution, GatewaySocket: gateway.Addr().String(), MaximumConnections: 2, CompileOptions: networkpolicy.CompileOptions{MaximumPins: 1, MaximumTTL: time.Minute}},
	})
	instanceID, err := manager.createAndStart(ctx, attribution.SandboxID, opts)
	if err != nil {
		if closeErr := attachment.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatalf("attributed VM startup: %v\n%s", err, latestSmokeLog(t, workDir))
	}
	instance := manager.lookup(instanceID)
	if instance == nil || instance.jailRoot == "" || instance.guestProtocolSession == nil {
		t.Fatal("attributed VM did not establish jailed guest session")
	}
	return manager, instance, gateway, attribution
}

func TestSmokeFirecrackerAttributedCommand(t *testing.T) {
	manager, instance, gateway, attribution := newFirecrackerAttributedQualification(t, time.Minute)
	ctx, expiry := t.Context(), attribution.ExpiresAt
	result := make(chan error, 1)
	if err := gateway.SetDeadline(expiry); err != nil {
		t.Fatal(err)
	}
	go func() {
		connection, err := gateway.AcceptUnix()
		if err != nil {
			result <- err
			return
		}
		defer connection.Close()
		actual, err := egressattribution.ReadRunnerExecutionAttribution(connection, uint32(os.Getuid()), expiry)
		if err == nil && (actual.SubjectRef != attribution.SubjectRef || actual.AuthorizationRef != attribution.AuthorizationRef || actual.InstanceID != attribution.InstanceID) {
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
			_, err = connection.Write([]byte("qualified-attributed"))
		}
		result <- err
	}()
	request := &runnerprotocol.ExecOpen{Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", `endpoint=$SECONDBOX_EXECUTION_GATEWAY; printf 'guest-context: forged\n' | nc -w 2 "${endpoint%:*}" "${endpoint##*:}"`}}}, Cwd: ".", DeadlineUnixMs: uint64(expiry.UnixMilli()), OutputLimitBytes: 1024}
	executed, err := ExecuteBufferedOverSession(ctx, instance.guestProtocolSession, attribution.AssignmentID, request)
	if err != nil || executed.Terminal.GetExitCode() != 0 || string(executed.Stdout) != "qualified-attributed" {
		t.Fatalf("attributed exec: %v stdout=%q stderr=%q terminal=%+v", err, executed.Stdout, executed.Stderr, executed.Terminal)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, err := ExecuteBufferedOverSession(ctx, instance.guestProtocolSession, attribution.AssignmentID, request); err == nil {
		t.Fatal("second attributed command admitted")
	}
	if err := manager.Remove(ctx, instance.id); err != nil {
		t.Fatal(err)
	}
	if running, err := manager.IsRunning(ctx, instance.id); err != nil || running {
		t.Fatalf("attributed VM teardown: running=%v error=%v", running, err)
	}
}

func TestSmokeFirecrackerAttributedRevocation(t *testing.T) {
	for _, trigger := range []string{"expiry", "stop", "compute-loss"} {
		t.Run(trigger, func(t *testing.T) {
			lifetime := time.Minute
			if trigger == "expiry" {
				lifetime = 8 * time.Second
			}
			manager, instance, gateway, attribution := newFirecrackerAttributedQualification(t, lifetime)
			if err := gateway.SetDeadline(attribution.ExpiresAt); err != nil {
				t.Fatal(err)
			}
			commandDone := make(chan struct{})
			go func() {
				defer close(commandDone)
				_, _ = ExecuteBufferedOverSession(t.Context(), instance.guestProtocolSession, attribution.AssignmentID, &runnerprotocol.ExecOpen{
					Command: &runnerprotocol.ExecOpen_Argv{Argv: &runnerprotocol.ArgvCommand{Argument: []string{"/bin/sh", "-c", `endpoint=$SECONDBOX_EXECUTION_GATEWAY; ( { printf ready; sleep 120; } | nc "${endpoint%:*}" "${endpoint##*:}" ) & wait`}}},
					Cwd:     ".", DeadlineUnixMs: uint64(attribution.ExpiresAt.UnixMilli()), OutputLimitBytes: 1024,
				})
			}()
			connection, err := gateway.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if _, err := egressattribution.ReadRunnerExecutionAttribution(connection, uint32(os.Getuid()), attribution.ExpiresAt); err != nil {
				t.Fatal(err)
			}
			if err := connection.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
				t.Fatal(err)
			}
			ready := make([]byte, len("ready"))
			if _, err := io.ReadFull(connection, ready); err != nil || string(ready) != "ready" {
				t.Fatalf("attributed descendant readiness: %q %v", ready, err)
			}
			switch trigger {
			case "stop":
				if err := manager.Remove(t.Context(), instance.id); err != nil {
					t.Fatal(err)
				}
			case "compute-loss":
				if err := instance.cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := connection.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("attributed connection survived revocation: bytes=%d error=%v", n, err)
			}
			select {
			case <-commandDone:
			case <-time.After(20 * time.Second):
				t.Fatal("attributed descendant exec survived teardown")
			}
			select {
			case <-instance.done:
			case <-time.After(20 * time.Second):
				t.Fatal("attributed Firecracker process survived teardown")
			}
			if running, err := manager.IsRunning(t.Context(), instance.id); err != nil || running {
				t.Fatalf("attributed compute revocation: running=%v error=%v", running, err)
			}
		})
	}
}
