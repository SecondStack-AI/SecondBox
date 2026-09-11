package firecracker

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	runtimemanager "github.com/SecondStack-AI/SecondBox/runner/internal/runtime"
)

type failedAttributedExecStream struct {
	guestv1.GuestAgent_ConnectClient
	sends int
}

func (stream *failedAttributedExecStream) Send(*guestv1.RunnerToGuest) error {
	stream.sends++
	return errors.New("test transport disconnected")
}

func TestAttributedExecutionGuestProtocolBoundary(t *testing.T) {
	expiry := time.Now().Add(time.Minute)
	guard, err := runtimemanager.NewAttributedExecutionGuard("assignment", expiry)
	if err != nil {
		t.Fatal(err)
	}
	stream := &failedAttributedExecStream{}
	session := &GuestProtocolSession{
		Stream: stream, Binding: &guestv1.ConnectionBinding{}, attributedExecution: guard,
		executionGateway: netip.MustParseAddrPort("169.254.104.1:41000"),
		EnabledFeatures: map[guestv1.GuestFeature]bool{
			guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC:               true,
			guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM: true,
		},
	}
	ctx := context.Background()
	assertDenied := func(err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "attributed execution") {
			t.Fatalf("expected attributed execution denial, got %v", err)
		}
	}
	_, err = session.ExecutePTY(ctx, "assignment", nil, nil, nil)
	assertDenied(err)
	_, _, err = session.openPortProtocolStream(ctx)
	assertDenied(err)
	for _, operation := range []guestv1.FileOperation{
		guestv1.FileOperation_FILE_OPERATION_WRITE, guestv1.FileOperation_FILE_OPERATION_MKDIR, guestv1.FileOperation_FILE_OPERATION_REMOVE,
	} {
		_, err = session.ExecuteFileOperation(ctx, "assignment", &guestv1.FileRequest{WorkspaceRelativePath: "file", Operation: operation}, nil)
		assertDenied(err)
	}
	request := &guestv1.ExecRequest{DeadlineUnixMs: uint64(expiry.UnixMilli()), OutputLimitBytes: 1024}
	_, err = session.ExecuteStreaming(ctx, "other", request, nil, nil)
	assertDenied(err)
	if stream.sends != 0 {
		t.Fatalf("denied operations sent %d frames", stream.sends)
	}
	_, err = session.ExecuteStreaming(ctx, "assignment", request, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "test transport disconnected") || stream.sends != 1 {
		t.Fatalf("first exec: sends=%d, err=%v", stream.sends, err)
	}
	// A failed send may have reached the guest. A new connection shares the consumed allowance.
	reconnected := &GuestProtocolSession{Stream: stream, Binding: session.Binding, EnabledFeatures: session.EnabledFeatures, attributedExecution: guard, executionGateway: session.executionGateway}
	_, err = reconnected.ExecuteBuffered(ctx, "assignment", request)
	assertDenied(err)
	if stream.sends != 1 {
		t.Fatalf("reconnect sent another exec: %d", stream.sends)
	}
}

func TestAttributedExecutionLegacyControlDenied(t *testing.T) {
	guard, err := runtimemanager.NewAttributedExecutionGuard("assignment", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{instances: map[string]*instance{"instance": {attributedExecution: guard}}}
	ctx := context.Background()
	_, execErr := manager.ExecuteTool(ctx, "instance", ToolExecRequest{})
	_, _, writeErr := manager.PutWorkspaceFileStream(ctx, "instance", "file", strings.NewReader("content"))
	secretErr := manager.ApplySecrets(ctx, "instance", SecretBundle{})
	for _, err := range []error{execErr, writeErr, secretErr} {
		if err == nil || !strings.Contains(err.Error(), "attributed execution forbids legacy") {
			t.Fatalf("legacy operation: %v", err)
		}
	}
}
