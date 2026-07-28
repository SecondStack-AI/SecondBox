package firecracker

import (
	"context"
	"errors"
	"testing"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"google.golang.org/grpc"
)

func TestGuestProtocolCancellationSendFailureIsReturned(t *testing.T) {
	cancelSendFailure := errors.New("cancel transport failed")
	for _, test := range []struct {
		name string
		run  func(context.Context, *GuestProtocolSession) error
	}{
		{
			name: "exec",
			run: func(ctx context.Context, session *GuestProtocolSession) error {
				_, err := session.ExecuteBuffered(ctx, "assignment-1", &guestv1.ExecRequest{
					Command:          &guestv1.ExecRequest_Shell{Shell: "sleep 60"},
					OutputLimitBytes: 1024,
				})
				return err
			},
		},
		{
			name: "file",
			run: func(ctx context.Context, session *GuestProtocolSession) error {
				_, err := session.ExecuteFileOperation(ctx, "assignment-1", &guestv1.FileRequest{
					Operation:             guestv1.FileOperation_FILE_OPERATION_READ,
					WorkspaceRelativePath: "data.bin",
					ExpectedSize:          1024,
				}, nil)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			stream := &cancelFailingGuestStream{
				ctx:               ctx,
				initialSend:       make(chan struct{}),
				cancelSendFailure: cancelSendFailure,
			}
			session := &GuestProtocolSession{
				Stream: stream,
				Binding: &guestv1.ConnectionBinding{
					InstanceId: "instance-1", SandboxId: "sandbox-1", SandboxGeneration: 1,
					ConnectionNonce: []byte("01234567890123456789012345678901"),
				},
				EnabledFeatures: map[guestv1.GuestFeature]bool{
					guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC:               true,
					guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM: true,
				},
			}
			done := make(chan error, 1)
			go func() {
				done <- test.run(ctx, session)
			}()
			<-stream.initialSend
			cancel()
			err := <-done
			if !errors.Is(err, cancelSendFailure) {
				t.Fatalf("cancellation error = %v, want %v", err, cancelSendFailure)
			}
		})
	}
}

type cancelFailingGuestStream struct {
	grpc.ClientStream
	ctx               context.Context
	initialSend       chan struct{}
	cancelSendFailure error
	sendCount         int
}

func (stream *cancelFailingGuestStream) Send(*guestv1.RunnerToGuest) error {
	stream.sendCount++
	if stream.sendCount == 1 {
		close(stream.initialSend)
		return nil
	}
	return stream.cancelSendFailure
}

func (stream *cancelFailingGuestStream) Recv() (*guestv1.GuestToRunner, error) {
	<-stream.ctx.Done()
	return nil, stream.ctx.Err()
}
