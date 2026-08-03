package runnercontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/portdirect"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

const directPortTestCredential = "direct-port-single-use-credential-000000"

func TestRunnerDataPlaneListenerRequiresPinnedTLSAndRejectsUnwiredKinds(t *testing.T) {
	service, stream, _ := newDirectPortTestService(t)
	stopListener, err := service.startDataPlaneListener(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stopListener(); err != nil {
			t.Errorf("stop data-plane listener: %v", err)
		}
	})

	plaintext, err := net.Dial("tcp", service.dataPlane.address())
	if err != nil {
		t.Fatal(err)
	}
	if err := plaintext.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := portdirect.WriteCredential(
		plaintext,
		portdirect.SessionKindPort,
		directPortTestCredential,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := portdirect.ReadVerdict(plaintext); err == nil {
		t.Fatal("plaintext data-plane connection received a protocol verdict")
	}
	if err := plaintext.Close(); err != nil {
		t.Fatal(err)
	}
	wrongPin := strings.Repeat("0", sha256.Size*2)
	if wrongPin == service.dataPlaneSPKIPin {
		wrongPin = strings.Repeat("1", sha256.Size*2)
	}
	wrongTLSConfig, err := portdirect.TLSConfigForSPKIPin(wrongPin)
	if err != nil {
		t.Fatal(err)
	}
	if connection, err := tls.Dial("tcp", service.dataPlane.address(), wrongTLSConfig); err == nil {
		connection.Close()
		t.Fatal("data-plane TLS accepted the wrong certificate SPKI pin")
	}

	tlsConfig, err := portdirect.TLSConfigForSPKIPin(service.dataPlaneSPKIPin)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		kind    portdirect.SessionKind
		verdict portdirect.Verdict
		detail  string
	}{
		{portdirect.SessionKindExec, portdirect.VerdictDenied, "credential rejected"},
		{portdirect.SessionKindPTY, portdirect.VerdictSessionKindUnsupported, "pty session kind is not implemented"},
		{portdirect.SessionKindFile, portdirect.VerdictDenied, "credential rejected"},
	} {
		connection, err := tls.Dial("tcp", service.dataPlane.address(), tlsConfig)
		if err != nil {
			t.Fatal(err)
		}
		if connection.ConnectionState().Version != tls.VersionTLS13 {
			t.Fatalf("data-plane TLS version = %x, want TLS 1.3", connection.ConnectionState().Version)
		}
		if err := portdirect.WriteCredential(
			connection,
			testCase.kind,
			directPortTestCredential,
		); err != nil {
			t.Fatal(err)
		}
		verdict, detail, err := portdirect.ReadVerdict(connection)
		if err != nil {
			t.Fatal(err)
		}
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
		if verdict != testCase.verdict || detail != testCase.detail {
			t.Fatalf("unwired kind %s verdict = %d/%q", testCase.kind, verdict, detail)
		}
	}
	for _, message := range stream.messages() {
		if message.GetPortDirectConsume() != nil {
			t.Fatal("unwired session kind forced control-plane consumption")
		}
	}
}

func TestRunnerDataPlaneListenerGatesReadinessAndAdvertisesOnlyItsAddress(t *testing.T) {
	service, _, _ := newDirectPortTestService(t)

	unbound, err := service.readiness(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if unbound.Capabilities.DataPlaneReady ||
		!slices.Contains(
			unbound.ReadinessFailures,
			runnerprotocol.RunnerReadinessFailure_RUNNER_READINESS_FAILURE_DATA_PLANE,
		) {
		t.Fatalf("unbound data-plane readiness = %+v", unbound)
	}

	stopListener, err := service.startDataPlaneListener(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stopListener(); err != nil {
			t.Errorf("stop data-plane listener: %v", err)
		}
	})
	bound, err := service.readiness(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !bound.Capabilities.DataPlaneReady ||
		slices.Contains(
			bound.ReadinessFailures,
			runnerprotocol.RunnerReadinessFailure_RUNNER_READINESS_FAILURE_DATA_PLANE,
		) {
		t.Fatalf("bound data-plane readiness = %+v", bound)
	}

	registration := &threadSafeRunnerStream{}
	if err := service.sendRegistration(registration, "connection-1", 1, bound); err != nil {
		t.Fatal(err)
	}
	advertised := registration.messages()[0].GetRegistration().GetDataPlaneAdvertisedAddress()
	if advertised != service.config.DataPlaneAdvertisedAddress {
		t.Fatalf("advertised data-plane address = %q", advertised)
	}
	if got := registration.messages()[0].GetRegistration().GetDataPlaneCertificateSpkiSha256(); got != service.dataPlaneSPKIPin {
		t.Fatalf("advertised data-plane certificate SPKI SHA-256 = %q", got)
	}
	// The advertised value is administrative capacity evidence: a dialable
	// host:port and nothing that identifies a Sandbox, Instance, or Assignment.
	if _, _, err := net.SplitHostPort(advertised); err != nil {
		t.Fatalf("advertised data-plane address is not host:port: %v", err)
	}
	fence := relayRunnerFence()
	for _, identity := range []string{
		fence.SandboxId, fence.InstanceId, fence.AssignmentId, string(fence.FencingToken),
	} {
		if strings.Contains(advertised, identity) {
			t.Fatalf("advertised data-plane address %q carries Sandbox identity %q", advertised, identity)
		}
	}

	heartbeat := &threadSafeRunnerStream{}
	if err := service.sendHeartbeat(heartbeat, "connection-1", bound); err != nil {
		t.Fatal(err)
	}
	if got := heartbeat.messages()[0].GetHeartbeat().GetDataPlaneAdvertisedAddress(); got != advertised {
		t.Fatalf("heartbeat advertised data-plane address = %q, want %q", got, advertised)
	}
}

func TestRunnerDataPlaneAcceptFailureReportsUnreadyAndFencesTheSession(t *testing.T) {
	service, _, _ := newDirectPortTestService(t)
	if err := service.dataPlane.bind(service.config.DataPlaneListenAddress, service.config.DataPlaneCertificate); err != nil {
		t.Fatal(err)
	}
	failures := make(chan error, 1)
	// Closing the listener out from under the accept loop is how the runtime
	// observes an unavailable caller-facing transport.
	service.dataPlane.mu.Lock()
	listener := service.dataPlane.listener
	service.dataPlane.mu.Unlock()
	if err := listener.(*net.TCPListener).SetDeadline(time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	service.acceptDataPlaneConnections(t.Context(), failures)
	select {
	case err := <-failures:
		if err == nil {
			t.Fatal("data-plane accept failure was not reported")
		}
	default:
		t.Fatal("data-plane accept failure was not reported")
	}
	readiness, err := service.readiness(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if readiness.Capabilities.DataPlaneReady {
		t.Fatal("runner stayed ready after its data-plane listener failed")
	}
	if err := service.dataPlane.close(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectPortAdmissionRejectsLocallyWithoutControlPlaneWork(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, *RunnerProtocolService, *directPortSession){
		"unknown_credential": func(_ *testing.T, service *RunnerProtocolService, session *directPortSession) {
			service.directPorts.remove(session)
		},
		"superseded_generation": func(_ *testing.T, service *RunnerProtocolService, session *directPortSession) {
			superseded := cloneRunnerFence(session.fence)
			superseded.SandboxGeneration++
			service.stateMu.Lock()
			service.active = map[string]*runnerprotocol.ActiveAssignmentSummary{}
			service.stateMu.Unlock()
			service.recordActiveAssignment(superseded, "fc-instance-1")
		},
		"post_deadline": func(_ *testing.T, _ *RunnerProtocolService, session *directPortSession) {
			session.deadline = time.Now().UTC().Add(-time.Second)
		},
		"drain": func(_ *testing.T, service *RunnerProtocolService, _ *directPortSession) {
			service.stateMu.Lock()
			service.drain = runnerprotocol.DrainPhase_DRAIN_PHASE_DRAINING
			service.stateMu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, stream, _ := newDirectPortTestService(t)
			session := admitDirectPortSession(t, service, directPortTestCredential, time.Minute)
			mutate(t, service, session)

			verdict, detail := directPortHandshake(t, service, directPortTestCredential)
			if verdict != portdirect.VerdictDenied {
				t.Fatalf("verdict = %d detail = %q, want denied", verdict, detail)
			}
			for _, message := range stream.messages() {
				if message.GetPortDirectConsume() != nil {
					t.Fatal("locally rejected connection forced control-plane consumption")
				}
			}
		})
	}
}

func TestDirectPortAdmissionSpendsTheCredentialExactlyOnceAndBridgesBytes(t *testing.T) {
	service, stream, guest := newDirectPortTestService(t)
	evidence := &recordingEvidenceSink{}
	service.SetEvidenceSink(evidence)
	admitDirectPortSession(t, service, directPortTestCredential, time.Minute)
	stopAdmitting := answerDirectPortAdmissions(
		service,
		stream,
		runnerprotocol.PortDirectAdmissionKind_PORT_DIRECT_ADMISSION_KIND_ADMITTED,
	)
	defer stopAdmitting()

	caller, served := dialDirectPort(t, service)
	if err := portdirect.WriteCredential(caller, portdirect.SessionKindPort, directPortTestCredential); err != nil {
		t.Fatal(err)
	}
	verdict, detail, err := portdirect.ReadVerdict(caller)
	if err != nil || verdict != portdirect.VerdictAdmitted {
		t.Fatalf("verdict = %d detail = %q error = %v", verdict, detail, err)
	}

	if _, err := caller.Write([]byte("ssh-2.0-secondbox")); err != nil {
		t.Fatal(err)
	}
	waitGuestWrite(t, guest, []byte("ssh-2.0-secondbox"))
	guest.queueRead([]byte("ssh-2.0-openssh"), nil)
	response := make([]byte, len("ssh-2.0-openssh"))
	if _, err := io.ReadFull(caller, response); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response, []byte("ssh-2.0-openssh")) {
		t.Fatalf("bridged guest response = %q", response)
	}

	// A second connection presenting the same credential is refused locally: the
	// session was claimed by the first, and PostgreSQL would refuse it anyway.
	replayVerdict, _ := directPortHandshake(t, service, directPortTestCredential)
	if replayVerdict != portdirect.VerdictDenied {
		t.Fatal("replayed credential was admitted")
	}

	guest.queueRead(nil, io.EOF)
	if err := <-served; err != nil {
		t.Fatalf("bridged connection error = %v", err)
	}
	if !guest.isClosed() {
		t.Fatal("guest Port stream stayed open after the caller connection ended")
	}
	assertDirectPortEvidence(t, evidence, "PORT_TERMINAL_KIND_CLOSED")
	assertDirectPortTerminalFrame(
		t, stream, runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_CLOSED,
	)
}

func TestDirectPortConnectionDoesNotSurviveAFence(t *testing.T) {
	service, stream, guest := newDirectPortTestService(t)
	session := admitDirectPortSession(t, service, directPortTestCredential, time.Minute)
	stopAdmitting := answerDirectPortAdmissions(
		service,
		stream,
		runnerprotocol.PortDirectAdmissionKind_PORT_DIRECT_ADMISSION_KIND_ADMITTED,
	)
	defer stopAdmitting()

	caller, served := dialDirectPort(t, service)
	if err := portdirect.WriteCredential(caller, portdirect.SessionKindPort, directPortTestCredential); err != nil {
		t.Fatal(err)
	}
	if verdict, _, err := portdirect.ReadVerdict(caller); err != nil ||
		verdict != portdirect.VerdictAdmitted {
		t.Fatalf("verdict = %d error = %v", verdict, err)
	}

	service.directPorts.closeAssignment(session.fence.AssignmentId, "assignment fenced")
	<-served
	if _, err := io.ReadAll(caller); err == nil {
		remaining := make([]byte, 1)
		if _, err := caller.Read(remaining); err == nil {
			t.Fatal("caller socket survived the fence")
		}
	}
	if !guest.isClosed() {
		t.Fatal("guest Port stream survived the fence")
	}
	assertDirectPortTerminalFrame(
		t, stream, runnerprotocol.PortTerminalKind_PORT_TERMINAL_KIND_FENCED,
	)
}

func TestDirectPortHandshakeIsBoundedInTimeAndSize(t *testing.T) {
	service, stream, _ := newDirectPortTestService(t)
	admitDirectPortSession(t, service, directPortTestCredential, time.Minute)

	caller, runner := net.Pipe()
	defer caller.Close()
	recording := &deadlineRecordingConn{Conn: runner}
	served := make(chan struct{})
	go func() {
		defer close(served)
		service.serveDirectPortConnection(t.Context(), recording)
	}()
	// Exactly one malformed header and nothing more: the Runner must reject it
	// without consuming a payload byte.
	written := make(chan error, 1)
	go func() {
		_, err := caller.Write([]byte("NOTDP1\x00\x00\x04"))
		written <- err
	}()
	verdict, _, err := portdirect.ReadVerdict(caller)
	if writeErr := <-written; writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || verdict != portdirect.VerdictDenied {
		t.Fatalf("malformed handshake verdict = %d error = %v", verdict, err)
	}
	<-served
	deadline := recording.firstDeadline()
	if deadline.IsZero() {
		t.Fatal("direct Port handshake set no read deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > directPortHandshakeTimeout {
		t.Fatalf("direct Port handshake deadline is %v away", remaining)
	}
	for _, message := range stream.messages() {
		if message.GetPortDirectConsume() != nil {
			t.Fatal("malformed handshake forced control-plane consumption")
		}
	}

	oversized := strings.Repeat("c", portdirect.MaximumCredentialBytes+1)
	if err := portdirect.WriteCredential(io.Discard, portdirect.SessionKindPort, oversized); err == nil {
		t.Fatal("oversized direct Port credential was framed")
	}
	if _, err := portdirect.ReadCredential(bytes.NewReader(
		append([]byte(portdirect.Magic), byte(portdirect.SessionKindPort), 0xff, 0xff),
	)); !errors.Is(err, portdirect.ErrHandshakeMalformed) {
		t.Fatal("unbounded direct Port credential length was accepted")
	}
}

// TestDirectPortAdmissionWaitsForItsAdmittingFrame proves a caller that connects
// before the admitting frame reaches the Runner is admitted rather than denied.
func TestDirectPortAdmissionWaitsForItsAdmittingFrame(t *testing.T) {
	service, stream, _ := newDirectPortTestService(t)
	if err := service.dataPlane.bind(service.config.DataPlaneListenAddress, service.config.DataPlaneCertificate); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.dataPlane.close(); err != nil {
			t.Errorf("close data-plane listener: %v", err)
		}
	})
	stopAdmitting := answerDirectPortAdmissions(
		service,
		stream,
		runnerprotocol.PortDirectAdmissionKind_PORT_DIRECT_ADMISSION_KIND_ADMITTED,
	)
	defer stopAdmitting()

	caller, served := dialDirectPort(t, service)
	if err := portdirect.WriteCredential(caller, portdirect.SessionKindPort, directPortTestCredential); err != nil {
		t.Fatal(err)
	}
	// The admitting frame arrives only after the caller has already presented
	// its credential.
	time.Sleep(50 * time.Millisecond)
	admitDirectPortSession(t, service, directPortTestCredential, time.Minute)
	verdict, detail, err := portdirect.ReadVerdict(caller)
	if err != nil || verdict != portdirect.VerdictAdmitted {
		t.Fatalf("late-admitted verdict = %d detail = %q error = %v", verdict, detail, err)
	}
	_ = caller.Close()
	<-served
}

func TestDirectPortAdmissionDeniedByPostgresIsNotBridged(t *testing.T) {
	service, stream, guest := newDirectPortTestService(t)
	admitDirectPortSession(t, service, directPortTestCredential, time.Minute)
	stopAdmitting := answerDirectPortAdmissions(
		service,
		stream,
		runnerprotocol.PortDirectAdmissionKind_PORT_DIRECT_ADMISSION_KIND_DENIED,
	)
	defer stopAdmitting()

	verdict, _ := directPortHandshake(t, service, directPortTestCredential)
	if verdict != portdirect.VerdictDenied {
		t.Fatal("connection denied by PostgreSQL was admitted")
	}
	if guest.opened() {
		t.Fatal("guest Port stream opened before the credential was spent")
	}
}

func newDirectPortTestService(t *testing.T) (
	*RunnerProtocolService,
	*threadSafeRunnerStream,
	*testPortConnection,
) {
	t.Helper()
	guest := newTestPortConnection()
	backend := &portRelayAssignmentBackend{connection: guest}
	stream := &threadSafeRunnerStream{}
	service, err := NewRunnerProtocolService(
		testRunnerConfig(), backend, staticProtocolConnector{stream: stream},
	)
	if err != nil {
		t.Fatal(err)
	}
	service.directPorts.bindStream(stream)
	service.recordActiveAssignment(relayRunnerFence(), "fc-instance-1")
	return service, stream, guest
}

// admitDirectPortSession registers one direct PortSession exactly as the control
// plane does, then returns the Runner-held state.
func admitDirectPortSession(
	t *testing.T,
	service *RunnerProtocolService,
	credential string,
	duration time.Duration,
) *directPortSession {
	t.Helper()
	// The Runner refuses to hold a direct session it could never serve, so the
	// caller-facing listener must be bound before admission.
	if !service.dataPlane.ready() {
		if err := service.dataPlane.bind(service.config.DataPlaneListenAddress, service.config.DataPlaneCertificate); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := service.dataPlane.close(); err != nil {
				t.Errorf("close data-plane listener: %v", err)
			}
		})
	}
	fence := relayRunnerFence()
	digest := sha256.Sum256([]byte(credential))
	frame := &runnerprotocol.PortFrame{
		Fence: cloneRunnerFence(fence), OperationId: "port-1", StreamId: "port-stream-1",
		Sequence:    1,
		Correlation: relayOperationCorrelation(fence, "port-1", "request-port-1", "lease-port-1"),
		Payload: &runnerprotocol.PortFrame_DirectOpen{DirectOpen: &runnerprotocol.PortDirectOpen{
			GuestPort: 8080, Protocol: "tcp", PortName: "ssh",
			DeadlineUnixMs:   uint64(time.Now().UTC().Add(duration).UnixMilli()),
			CredentialDigest: digest[:], LeaseId: "lease-port-1",
		}},
	}
	if err := service.handlePortFrame(
		t.Context(),
		service.directPorts.currentStream(),
		frame,
		map[runnerprotocol.RunnerFeature]bool{
			runnerprotocol.RunnerFeature_RUNNER_FEATURE_PORT_PROXY: true,
		},
		make(chan error, 1),
	); err != nil {
		t.Fatal(err)
	}
	session := service.directPorts.lookup(digest)
	if session == nil {
		t.Fatal("direct PortSession was not registered")
	}
	return session
}

// answerDirectPortAdmissions stands in for the control plane's consumption
// verdict so the Runner's admission path can be exercised end to end.
func answerDirectPortAdmissions(
	service *RunnerProtocolService,
	stream *threadSafeRunnerStream,
	kind runnerprotocol.PortDirectAdmissionKind,
) func() {
	stop := make(chan struct{})
	answered := map[string]bool{}
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, message := range stream.messages() {
				consume := message.GetPortDirectConsume()
				if consume == nil || answered[consume.MessageId] {
					continue
				}
				answered[consume.MessageId] = true
				_ = service.directPorts.deliverAdmission(&runnerprotocol.PortDirectAdmission{
					MessageId: consume.MessageId, Fence: consume.Fence,
					OperationId: consume.OperationId, StreamId: consume.StreamId,
					Kind: kind, SafeDetail: "test verdict",
				})
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return func() { close(stop) }
}

func dialDirectPort(t *testing.T, service *RunnerProtocolService) (net.Conn, <-chan error) {
	t.Helper()
	caller, runner := net.Pipe()
	t.Cleanup(func() { _ = caller.Close() })
	served := make(chan error, 1)
	go func() {
		service.serveDirectPortConnection(context.WithoutCancel(t.Context()), runner)
		served <- nil
	}()
	return caller, served
}

// directPortHandshake performs one complete handshake and returns its verdict.
func directPortHandshake(
	t *testing.T,
	service *RunnerProtocolService,
	credential string,
) (portdirect.Verdict, string) {
	t.Helper()
	caller, served := dialDirectPort(t, service)
	if err := portdirect.WriteCredential(caller, portdirect.SessionKindPort, credential); err != nil {
		t.Fatal(err)
	}
	verdict, detail, err := portdirect.ReadVerdict(caller)
	if err != nil {
		t.Fatal(err)
	}
	<-served
	return verdict, detail
}

func waitGuestWrite(t *testing.T, guest *testPortConnection, want []byte) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Equal(guest.writtenBytes(), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("guest Port write = %q, want %q", guest.writtenBytes(), want)
}

func assertDirectPortEvidence(
	t *testing.T,
	sink *recordingEvidenceSink,
	wantTerminalKind string,
) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var records []struct {
		event    string
		terminal string
	}
	for time.Now().Before(deadline) {
		records = records[:0]
		for _, record := range sink.snapshot() {
			records = append(records, struct {
				event    string
				terminal string
			}{string(record.Event), record.TerminalKind})
		}
		if len(records) == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(records) != 2 ||
		records[0].event != "port_open" || records[0].terminal != "PORT_DIRECT_ADMITTED" ||
		records[1].event != "port_terminal" || records[1].terminal != wantTerminalKind {
		t.Fatalf("direct Port evidence = %+v", records)
	}
	fence := relayRunnerFence()
	for _, record := range sink.snapshot() {
		if record.RequestID != "request-port-1" || record.OperationID != "port-1" ||
			record.LeaseID != "lease-port-1" || record.SandboxID != fence.SandboxId ||
			record.InstanceID != fence.InstanceId ||
			record.SandboxGeneration != fence.SandboxGeneration ||
			record.AssignmentID != fence.AssignmentId || record.RunnerID != "runner-1" {
			t.Fatalf("direct Port evidence correlation = %+v", record)
		}
	}
}

func assertDirectPortTerminalFrame(
	t *testing.T,
	stream *threadSafeRunnerStream,
	want runnerprotocol.PortTerminalKind,
) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, message := range stream.messages() {
			if terminal := message.GetPort().GetTerminal(); terminal != nil && terminal.Kind == want {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("direct Port terminal frame %s was not returned", want)
}

// deadlineRecordingConn proves the handshake is time bounded before the Runner
// reads an unauthenticated byte.
type deadlineRecordingConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
}

func (connection *deadlineRecordingConn) SetDeadline(deadline time.Time) error {
	connection.mu.Lock()
	if connection.deadline.IsZero() && !deadline.IsZero() {
		connection.deadline = deadline
	}
	connection.mu.Unlock()
	return connection.Conn.SetDeadline(deadline)
}

func (connection *deadlineRecordingConn) firstDeadline() time.Time {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.deadline
}
