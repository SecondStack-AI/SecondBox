package microvmguest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func templateBindRequest() AssignmentBindRequest {
	return AssignmentBindRequest{
		InstanceID:              "instance-1",
		SandboxID:               "sandbox-1",
		SandboxGeneration:       7,
		GuestBuildID:            "guest-build-1",
		ImageManifestDigest:     "sha256:image",
		ToolchainManifestDigest: "sha256:toolchain",
		HeartbeatIntervalMs:     1000,
	}
}

type fakeWorkspaceMounter struct {
	mounts   int
	writable bool
	dir      string
	err      error
}

func (f *fakeWorkspaceMounter) Mount(_ context.Context, workspaceDir string, writable bool) error {
	f.mounts++
	f.dir = workspaceDir
	f.writable = writable
	return f.err
}

func newTemplateGuestServer(t *testing.T) (Server, *fakeWorkspaceMounter) {
	t.Helper()
	mounter := &fakeWorkspaceMounter{}
	return Server{
		WorkspaceDir:      t.TempDir(),
		RuntimePrivateDir: t.TempDir(),
		Hardener:          &fakeHardener{},
		Mounter:           mounter,
		Assignment:        NewAssignmentGate(),
	}, mounter
}

func postTemplateControl(t *testing.T, server Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s body: %v", path, err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded)))
	return recorder
}

func hardenTemplateGuest(t *testing.T, server Server) {
	t.Helper()
	recorder := postTemplateControl(t, server, "/restore/harden", RestoreHardenRequest{
		HostTime:      time.Now().UTC().Format(time.RFC3339Nano),
		EntropyBase64: "AAAAAAAAAAAAAAAAAAAAAA==",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("harden template guest: status %d body %s", recorder.Code, recorder.Body.String())
	}
}

func TestTemplateGuestRefusesAssignmentBindBeforeHardening(t *testing.T) {
	server, _ := newTemplateGuestServer(t)
	recorder := postTemplateControl(t, server, "/assignment/bind", templateBindRequest())
	if recorder.Code != http.StatusConflict {
		t.Fatalf("bind before harden: status %d body %s", recorder.Code, recorder.Body.String())
	}
	if _, bound := server.Assignment.Identity(); bound {
		t.Fatal("assignment identity was installed without post-restore hardening")
	}
}

func TestTemplateGuestInstallsAssignmentIdentityExactlyOnce(t *testing.T) {
	server, _ := newTemplateGuestServer(t)
	hardenTemplateGuest(t, server)

	first := postTemplateControl(t, server, "/assignment/bind", templateBindRequest())
	if first.Code != http.StatusOK {
		t.Fatalf("first bind: status %d body %s", first.Code, first.Body.String())
	}
	var installed struct {
		Bound      bool   `json:"bound"`
		InstanceID string `json:"instanceId"`
		SandboxID  string `json:"sandboxId"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &installed); err != nil {
		t.Fatalf("decode bind response: %v", err)
	}
	if !installed.Bound || installed.InstanceID != "instance-1" || installed.SandboxID != "sandbox-1" {
		t.Fatalf("bind response = %#v", installed)
	}
	identity, bound := server.Assignment.Identity()
	if !bound {
		t.Fatal("assignment identity was not installed")
	}
	if identity.SandboxGeneration != 7 || identity.HeartbeatInterval != time.Second {
		t.Fatalf("installed identity = %#v", identity)
	}

	second := templateBindRequest()
	second.InstanceID = "instance-2"
	second.SandboxID = "sandbox-2"
	conflict := postTemplateControl(t, server, "/assignment/bind", second)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("second bind: status %d body %s", conflict.Code, conflict.Body.String())
	}
	retained, bound := server.Assignment.Identity()
	if !bound || retained.InstanceID != "instance-1" || retained.SandboxID != "sandbox-1" {
		t.Fatalf("second bind replaced the installed identity: %#v", retained)
	}
}

func TestTemplateGuestRejectsIncompleteAssignmentBind(t *testing.T) {
	for name, mutate := range map[string]func(*AssignmentBindRequest){
		"missing instance":    func(r *AssignmentBindRequest) { r.InstanceID = "" },
		"missing sandbox":     func(r *AssignmentBindRequest) { r.SandboxID = "" },
		"zero generation":     func(r *AssignmentBindRequest) { r.SandboxGeneration = 0 },
		"missing build id":    func(r *AssignmentBindRequest) { r.GuestBuildID = "" },
		"missing image":       func(r *AssignmentBindRequest) { r.ImageManifestDigest = "" },
		"missing toolchain":   func(r *AssignmentBindRequest) { r.ToolchainManifestDigest = "" },
		"zero heartbeat":      func(r *AssignmentBindRequest) { r.HeartbeatIntervalMs = 0 },
		"heartbeat too large": func(r *AssignmentBindRequest) { r.HeartbeatIntervalMs = 60001 },
	} {
		t.Run(name, func(t *testing.T) {
			server, _ := newTemplateGuestServer(t)
			hardenTemplateGuest(t, server)
			request := templateBindRequest()
			mutate(&request)
			recorder := postTemplateControl(t, server, "/assignment/bind", request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status %d body %s", recorder.Code, recorder.Body.String())
			}
			if _, bound := server.Assignment.Identity(); bound {
				t.Fatal("an incomplete bind installed an identity")
			}
		})
	}
}

func TestTemplateGuestMountsWorkspaceInsideTheBind(t *testing.T) {
	server, mounter := newTemplateGuestServer(t)
	hardenTemplateGuest(t, server)
	request := templateBindRequest()
	request.WorkspaceWritable = true
	if code := postTemplateControl(t, server, "/assignment/bind", request).Code; code != http.StatusOK {
		t.Fatalf("bind status %d", code)
	}
	if mounter.mounts != 1 || !mounter.writable || mounter.dir != server.WorkspaceDir {
		t.Fatalf("workspace mount = %#v", mounter)
	}
	// The Workspace is attached exactly once, with the assignment identity.
	if code := postTemplateControl(t, server, "/assignment/bind", request).Code; code != http.StatusConflict {
		t.Fatalf("second bind status %d", code)
	}
	if mounter.mounts != 1 {
		t.Fatalf("a refused bind remounted the Workspace: %d mounts", mounter.mounts)
	}
}

func TestTemplateGuestStaysUnboundWhenWorkspaceMountFails(t *testing.T) {
	server, mounter := newTemplateGuestServer(t)
	mounter.err = errors.New("no such device")
	hardenTemplateGuest(t, server)
	recorder := postTemplateControl(t, server, "/assignment/bind", templateBindRequest())
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body.String())
	}
	if _, bound := server.Assignment.Identity(); bound {
		t.Fatal("identity was installed despite an unavailable Workspace")
	}
}

func TestTemplateGuestRefusesWorkspaceRequestsBeforeBind(t *testing.T) {
	server, _ := newTemplateGuestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/workspace/list?path=/", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body.String())
	}
	hardenTemplateGuest(t, server)
	if code := postTemplateControl(t, server, "/assignment/bind", templateBindRequest()).Code; code != http.StatusOK {
		t.Fatalf("bind status %d", code)
	}
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/workspace/list?path=/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("post-bind workspace list: status %d body %s", recorder.Code, recorder.Body.String())
	}
}

func TestIdentityBearingGuestRefusesAssignmentBind(t *testing.T) {
	server := Server{
		WorkspaceDir:      t.TempDir(),
		RuntimePrivateDir: t.TempDir(),
		InstanceID:        "instance-1",
		SandboxID:         "sandbox-1",
		Hardener:          &fakeHardener{},
	}
	recorder := postTemplateControl(t, server, "/assignment/bind", templateBindRequest())
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body.String())
	}
}

func TestTemplateGuestHeartbeatReportsNoIdentityUntilBind(t *testing.T) {
	server, _ := newTemplateGuestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/heartbeat", nil))
	var before struct {
		InstanceID string `json:"instanceId"`
		SandboxID  string `json:"sandboxId"`
		Healthy    bool   `json:"healthy"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode heartbeat: %v", err)
	}
	if before.InstanceID != "" || before.SandboxID != "" || !before.Healthy {
		t.Fatalf("template heartbeat leaked identity: %#v", before)
	}

	hardenTemplateGuest(t, server)
	if code := postTemplateControl(t, server, "/assignment/bind", templateBindRequest()).Code; code != http.StatusOK {
		t.Fatalf("bind status %d", code)
	}

	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/heartbeat", nil))
	var after struct {
		InstanceID string `json:"instanceId"`
		SandboxID  string `json:"sandboxId"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode heartbeat: %v", err)
	}
	if after.InstanceID != "instance-1" || after.SandboxID != "sandbox-1" {
		t.Fatalf("bound heartbeat = %#v", after)
	}
}

func TestTemplateGateListenerRefusesConnectionsUntilBind(t *testing.T) {
	server, _ := newTemplateGuestServer(t)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer inner.Close()
	gated := server.Assignment.GateListener(inner)

	accepted := make(chan net.Conn, 1)
	acceptErrors := make(chan error, 1)
	go func() {
		conn, err := gated.Accept()
		if err != nil {
			acceptErrors <- err
			return
		}
		accepted <- conn
	}()

	refused, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatalf("dial template protocol listener: %v", err)
	}
	defer refused.Close()
	if err := refused.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := refused.Read(make([]byte, 1)); err == nil {
		t.Fatal("template protocol listener accepted a connection before the assignment bind")
	}
	select {
	case conn := <-accepted:
		conn.Close()
		t.Fatal("template protocol listener handed a connection to the protocol server before bind")
	case err := <-acceptErrors:
		t.Fatalf("gated accept: %v", err)
	default:
	}

	hardenTemplateGuest(t, server)
	if code := postTemplateControl(t, server, "/assignment/bind", templateBindRequest()).Code; code != http.StatusOK {
		t.Fatalf("bind status %d", code)
	}

	bound, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatalf("dial after bind: %v", err)
	}
	defer bound.Close()
	select {
	case conn := <-accepted:
		conn.Close()
	case err := <-acceptErrors:
		t.Fatalf("gated accept after bind: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("gated listener did not accept after the assignment bind")
	}
}

func TestTemplateProtocolServiceRejectsHandshakeBeforeBind(t *testing.T) {
	server, _ := newTemplateGuestServer(t)
	stream, cleanup := openTemplateProtocolTestStream(t, server)
	defer cleanup()
	if err := stream.Send(templateProtocolHello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	frame, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive handshake result: %v", err)
	}
	rejection := frame.GetRejection()
	if rejection == nil {
		t.Fatalf("unbound template guest negotiated: %#v", frame)
	}
	if rejection.Kind != guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_BINDING_MISMATCH {
		t.Fatalf("rejection = %#v", rejection)
	}
}

func TestTemplateProtocolServiceNegotiatesBoundIdentity(t *testing.T) {
	server, _ := newTemplateGuestServer(t)
	hardenTemplateGuest(t, server)
	if code := postTemplateControl(t, server, "/assignment/bind", templateBindRequest()).Code; code != http.StatusOK {
		t.Fatalf("bind status %d", code)
	}

	stream, cleanup := openTemplateProtocolTestStream(t, server)
	defer cleanup()
	if err := stream.Send(templateProtocolHello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	frame, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive welcome: %v", err)
	}
	welcome := frame.GetWelcome()
	if welcome == nil {
		t.Fatalf("bound template guest rejected the handshake: %#v", frame)
	}
	if welcome.GuestBuildId != "guest-build-1" ||
		welcome.ImageManifestDigest != "sha256:image" ||
		welcome.ToolchainManifestDigest != "sha256:toolchain" ||
		welcome.HeartbeatIntervalMs != 1000 {
		t.Fatalf("welcome does not carry the bound identity: %#v", welcome)
	}
	if welcome.Binding.GetInstanceId() != "instance-1" || welcome.Binding.GetSandboxGeneration() != 7 {
		t.Fatalf("welcome binding = %#v", welcome.Binding)
	}
}

func TestTemplateProtocolServiceRejectsMismatchedBinding(t *testing.T) {
	server, _ := newTemplateGuestServer(t)
	hardenTemplateGuest(t, server)
	if code := postTemplateControl(t, server, "/assignment/bind", templateBindRequest()).Code; code != http.StatusOK {
		t.Fatalf("bind status %d", code)
	}

	stream, cleanup := openTemplateProtocolTestStream(t, server)
	defer cleanup()
	hello := templateProtocolHello()
	hello.GetHello().Binding.SandboxGeneration = 8
	if err := stream.Send(hello); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	frame, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive handshake result: %v", err)
	}
	rejection := frame.GetRejection()
	if rejection == nil ||
		rejection.Kind != guestv1.HandshakeRejectionKind_HANDSHAKE_REJECTION_KIND_BINDING_MISMATCH {
		t.Fatalf("stale generation was not fenced: %#v", frame)
	}
}

func TestNewTemplateProtocolServiceRequiresAssignmentGate(t *testing.T) {
	if _, err := NewTemplateProtocolService(Server{
		WorkspaceDir:      t.TempDir(),
		RuntimePrivateDir: t.TempDir(),
	}); err == nil {
		t.Fatal("template protocol service accepted a server without an assignment gate")
	}
}

func templateProtocolHello() *guestv1.RunnerToGuest {
	return &guestv1.RunnerToGuest{Message: &guestv1.RunnerToGuest_Hello{Hello: &guestv1.Hello{
		Binding:                         protocolTestBinding(),
		SupportedGenerations:            &guestv1.ProtocolGenerationRange{Minimum: 1, Maximum: 1},
		RequestedFeatures:               []guestv1.GuestFeature{guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC},
		MandatoryFeatures:               []guestv1.GuestFeature{guestv1.GuestFeature_GUEST_FEATURE_STREAMING_EXEC},
		ExpectedImageManifestDigest:     "sha256:image",
		ExpectedToolchainManifestDigest: "sha256:toolchain",
	}}}
}

func openTemplateProtocolTestStream(
	t *testing.T,
	guestServer Server,
) (guestv1.GuestAgent_ConnectClient, func()) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	service, err := NewTemplateProtocolService(guestServer)
	if err != nil {
		t.Fatalf("create template guest protocol service: %v", err)
	}
	guestv1.RegisterGuestAgentServer(server, service)
	go func() {
		_ = server.Serve(listener)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	connection, err := grpc.NewClient(
		"passthrough:///template-guest-protocol",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		cancel()
		server.Stop()
		t.Fatalf("dial template guest protocol: %v", err)
	}
	stream, err := guestv1.NewGuestAgentClient(connection).Connect(ctx)
	if err != nil {
		connection.Close()
		cancel()
		server.Stop()
		t.Fatalf("connect template guest protocol: %v", err)
	}
	return stream, func() {
		connection.Close()
		cancel()
		server.Stop()
		listener.Close()
	}
}
