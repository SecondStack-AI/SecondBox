package firecracker

import (
	"context"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
)

// welcomingGuestStream answers an operation stream's Hello with the matching
// Welcome, then hands every operation frame to the wrapped test stream.
type welcomingGuestStream struct {
	guestv1.GuestAgent_ConnectClient
	session *GuestProtocolSession
	hello   *guestv1.Hello
}

func (stream *welcomingGuestStream) Send(message *guestv1.RunnerToGuest) error {
	if hello := message.GetHello(); hello != nil {
		stream.hello = hello
		return nil
	}
	return stream.GuestAgent_ConnectClient.Send(message)
}

func (stream *welcomingGuestStream) Recv() (*guestv1.GuestToRunner, error) {
	if hello := stream.hello; hello != nil {
		stream.hello = nil
		return &guestv1.GuestToRunner{Message: &guestv1.GuestToRunner_Welcome{Welcome: &guestv1.Welcome{
			Binding:                 hello.Binding,
			SelectedGeneration:      currentGuestProtocolGeneration,
			EnabledFeatures:         hello.RequestedFeatures,
			GuestBuildId:            stream.session.GuestBuildID,
			ImageManifestDigest:     stream.session.ImageManifestDigest,
			ToolchainManifestDigest: stream.session.ToolchainManifestDigest,
		}}}, nil
	}
	return stream.GuestAgent_ConnectClient.Recv()
}

// connectEveryOperationTo makes each operation stream of session negotiate
// and then use stream.
func connectEveryOperationTo(session *GuestProtocolSession, stream guestv1.GuestAgent_ConnectClient) {
	session.connect = func(context.Context) (guestv1.GuestAgent_ConnectClient, error) {
		return &welcomingGuestStream{GuestAgent_ConnectClient: stream, session: session}, nil
	}
}
