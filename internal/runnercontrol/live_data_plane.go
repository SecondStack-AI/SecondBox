package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

var (
	ErrLiveDataPlaneUnavailable     = errors.New("SecondBox live data-plane transport is unavailable")
	ErrLiveDataPlaneRouteNotFound   = errors.New("SecondBox live data-plane route was not found")
	ErrLiveDataPlaneCreditViolation = errors.New("SecondBox live data-plane response credit was exceeded")
	ErrLiveDataPlaneBufferInvariant = errors.New("SecondBox live data-plane buffer invariant was exceeded")
)

const liveDataPlaneControlFrameReserve int64 = 4

type liveDataPlaneDelivery struct {
	message *runnerv1.RunnerToControlPlane
	err     error
}

type liveRunnerConnection struct {
	connectionID string
	sender       controlPlaneFrameSender
	session      *Session
	mu           sync.Mutex
}

type liveDataPlaneRoute struct {
	runnerID          string
	key               string
	streamWindowBytes int64
	mu                sync.Mutex
	delivery          []liveDataPlaneDelivery
	ready             chan struct{}
	closed            chan struct{}
	closeErr          error
	responseCredit    int64
	requestCredit     int64
	replayedThrough   uint64
}

// LiveDataPlaneBroker routes Exec, PTY, File, and Port frames through the process that owns
// the authenticated Runner connection. It retains only stream-window-bounded queue entries
// and never writes a payload to PostgreSQL.
type LiveDataPlaneBroker struct {
	mu                         sync.Mutex
	connections                map[string]*liveRunnerConnection
	routes                     map[string]*liveDataPlaneRoute
	droppedRouteNotFoundFrames atomic.Uint64
}

// LiveDataPlaneMetricsSnapshot contains fixed-cardinality process-local broker counters.
type LiveDataPlaneMetricsSnapshot struct {
	DroppedRouteNotFoundFrames uint64
}

func NewLiveDataPlaneBroker() *LiveDataPlaneBroker {
	return &LiveDataPlaneBroker{
		connections: make(map[string]*liveRunnerConnection),
		routes:      make(map[string]*liveDataPlaneRoute),
	}
}

// AttachConnection binds the authenticated process-local Runner stream used by
// proxied Exec, File, and Port sessions.
func (broker *LiveDataPlaneBroker) AttachConnection(
	runnerID string,
	connectionID string,
	sender LiveDataPlaneSender,
	session *Session,
) (func(), error) {
	if runnerID == "" || connectionID == "" || sender == nil || session == nil {
		return nil, errors.New("SecondBox live data-plane connection is incomplete")
	}
	broker.bind(runnerID, connectionID, sender, session)
	return func() { broker.unbind(runnerID, connectionID) }, nil
}

func (broker *LiveDataPlaneBroker) bind(
	runnerID string,
	connectionID string,
	sender controlPlaneFrameSender,
	session *Session,
) {
	broker.mu.Lock()
	selected := broker.removeRunnerRoutesLocked(runnerID)
	broker.connections[runnerID] = &liveRunnerConnection{
		connectionID: connectionID, sender: sender, session: session,
	}
	broker.mu.Unlock()
	closeLiveDataPlaneRoutes(selected, ErrLiveDataPlaneUnavailable)
}

func (broker *LiveDataPlaneBroker) unbind(runnerID string, connectionID string) {
	broker.mu.Lock()
	connection := broker.connections[runnerID]
	if connection == nil || connection.connectionID != connectionID {
		broker.mu.Unlock()
		return
	}
	delete(broker.connections, runnerID)
	selected := broker.removeRunnerRoutesLocked(runnerID)
	broker.mu.Unlock()
	closeLiveDataPlaneRoutes(selected, ErrLiveDataPlaneUnavailable)
}

func (broker *LiveDataPlaneBroker) removeRunnerRoutesLocked(runnerID string) []*liveDataPlaneRoute {
	selected := make([]*liveDataPlaneRoute, 0)
	for key, route := range broker.routes {
		if route.runnerID == runnerID {
			selected = append(selected, route)
			delete(broker.routes, key)
		}
	}
	return selected
}

func closeLiveDataPlaneRoutes(routes []*liveDataPlaneRoute, err error) {
	for _, route := range routes {
		route.close(err)
	}
}

func (route *liveDataPlaneRoute) close(err error) {
	route.mu.Lock()
	defer route.mu.Unlock()
	route.closeLocked(err)
}

func (route *liveDataPlaneRoute) closeLocked(err error) {
	if route.closeErr != nil {
		return
	}
	route.closeErr = err
	clear(route.delivery)
	route.delivery = nil
	close(route.closed)
}

// LiveDataPlaneStream owns one in-memory proxied Exec, PTY, File, or Port route.
type LiveDataPlaneStream struct {
	broker *LiveDataPlaneBroker
	route  *liveDataPlaneRoute
	once   sync.Once
}

func (broker *LiveDataPlaneBroker) Open(
	runnerID string,
	kind string,
	operationID string,
	streamID string,
	streamWindowBytes int64,
	responseCreditBytes int64,
	replayedThrough uint64,
) (*LiveDataPlaneStream, error) {
	key, err := liveDataPlaneKey(kind, operationID, streamID)
	if err != nil {
		return nil, err
	}
	if streamWindowBytes < 1 || responseCreditBytes < 0 || responseCreditBytes > streamWindowBytes {
		return nil, errors.New("SecondBox live data-plane stream window is invalid")
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.connections[runnerID] == nil {
		return nil, ErrLiveDataPlaneUnavailable
	}
	if broker.routes[key] != nil {
		return nil, errors.New("SecondBox live data-plane session is already attached")
	}
	route := &liveDataPlaneRoute{
		runnerID: runnerID, key: key, streamWindowBytes: streamWindowBytes,
		responseCredit: responseCreditBytes, replayedThrough: replayedThrough,
		ready: make(chan struct{}, 1), closed: make(chan struct{}),
	}
	broker.routes[key] = route
	return &LiveDataPlaneStream{broker: broker, route: route}, nil
}

func (stream *LiveDataPlaneStream) Send(message *runnerv1.ControlPlaneToRunner) error {
	if stream == nil || stream.broker == nil || stream.route == nil || message == nil {
		return ErrLiveDataPlaneUnavailable
	}
	stream.broker.mu.Lock()
	if stream.broker.routes[stream.route.key] != stream.route {
		stream.broker.mu.Unlock()
		return ErrLiveDataPlaneUnavailable
	}
	connection := stream.broker.connections[stream.route.runnerID]
	stream.broker.mu.Unlock()
	if connection == nil {
		return ErrLiveDataPlaneUnavailable
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if err := connection.session.ValidateOutboundDataPlaneFrame(message); err != nil {
		return err
	}
	if err := stream.route.recordOutbound(message); err != nil {
		return err
	}
	if err := connection.sender.Send(message); err != nil {
		return errors.Join(
			ErrLiveDataPlaneUnavailable,
			fmt.Errorf("SecondBox live data-plane send: %w", err),
		)
	}
	return nil
}

func (stream *LiveDataPlaneStream) Receive(ctx context.Context) (*runnerv1.RunnerToControlPlane, error) {
	if stream == nil || stream.route == nil {
		return nil, ErrLiveDataPlaneUnavailable
	}
	for {
		stream.route.mu.Lock()
		if len(stream.route.delivery) > 0 {
			delivery := stream.route.delivery[0]
			stream.route.delivery[0] = liveDataPlaneDelivery{}
			stream.route.delivery = stream.route.delivery[1:]
			stream.route.mu.Unlock()
			return delivery.message, delivery.err
		}
		closeErr := stream.route.closeErr
		stream.route.mu.Unlock()
		if closeErr != nil {
			return nil, closeErr
		}
		select {
		case <-stream.route.ready:
		case <-stream.route.closed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (stream *LiveDataPlaneStream) Close() {
	if stream == nil {
		return
	}
	stream.once.Do(func() {
		stream.broker.mu.Lock()
		if stream.broker.routes[stream.route.key] == stream.route {
			delete(stream.broker.routes, stream.route.key)
		}
		stream.broker.mu.Unlock()
		stream.route.close(ErrLiveDataPlaneUnavailable)
	})
}

// Deliver routes one already validated Runner Exec or File event in memory.
func (broker *LiveDataPlaneBroker) Deliver(
	ctx context.Context,
	event Event,
) error {
	kind, operationID, streamID, err := liveDataPlaneMessageIdentity(event.Message)
	if err != nil {
		return err
	}
	key, err := liveDataPlaneKey(kind, operationID, streamID)
	if err != nil {
		return err
	}
	broker.mu.Lock()
	route := broker.routes[key]
	broker.mu.Unlock()
	if route == nil || route.runnerID != event.RunnerID {
		broker.dropRouteNotFoundFrame()
		return nil
	}
	if err := route.enqueue(event.Message); err != nil {
		switch {
		case errors.Is(err, ErrLiveDataPlaneRouteNotFound):
			broker.dropRouteNotFoundFrame()
			return nil
		case errors.Is(err, ErrLiveDataPlaneCreditViolation),
			errors.Is(err, ErrLiveDataPlaneBufferInvariant):
			// A route-local protocol failure is observed by its owning stream. The
			// authenticated connection remains valid for every other session.
			return nil
		default:
			return err
		}
	}
	return nil
}

func (broker *LiveDataPlaneBroker) dropRouteNotFoundFrame() {
	// A locally closed route sends or durably queues Detach or cancellation as
	// part of cleanup, so prior credit bounds output while that signal arrives.
	broker.droppedRouteNotFoundFrames.Add(1)
}

// MetricsSnapshot returns the broker's process-lifetime fixed-cardinality counters.
func (broker *LiveDataPlaneBroker) MetricsSnapshot() LiveDataPlaneMetricsSnapshot {
	if broker == nil {
		return LiveDataPlaneMetricsSnapshot{}
	}
	return LiveDataPlaneMetricsSnapshot{
		DroppedRouteNotFoundFrames: broker.droppedRouteNotFoundFrames.Load(),
	}
}

func (route *liveDataPlaneRoute) recordOutbound(
	message *runnerv1.ControlPlaneToRunner,
) error {
	responseCredit, requestBytes, err := liveDataPlaneOutboundFlow(message)
	if err != nil {
		return err
	}
	route.mu.Lock()
	defer route.mu.Unlock()
	if route.closeErr != nil {
		if liveDataPlaneStopFrame(message) {
			// A route-local failure still permits the owning service to stop the
			// Runner session before it removes the failed route.
			return nil
		}
		return ErrLiveDataPlaneUnavailable
	}
	if responseCredit > route.streamWindowBytes-route.responseCredit {
		return errors.Join(
			ErrLiveDataPlaneCreditViolation,
			ErrDataPlaneFrameLimit,
			errors.New("SecondBox live data-plane outbound credit exceeds the stream window"),
		)
	}
	if requestBytes > route.requestCredit {
		return errors.Join(
			ErrLiveDataPlaneCreditViolation,
			ErrDataPlaneFrameLimit,
			errors.New("SecondBox live data-plane outbound payload exceeds Runner credit"),
		)
	}
	route.responseCredit += responseCredit
	route.requestCredit -= requestBytes
	return nil
}

func liveDataPlaneStopFrame(message *runnerv1.ControlPlaneToRunner) bool {
	return message.GetExec() != nil && message.GetExec().GetCancel() != nil ||
		message.GetFile() != nil && message.GetFile().GetCancel() != nil ||
		message.GetPty() != nil && message.GetPty().GetDetach() != nil ||
		message.GetPort() != nil && message.GetPort().GetCancel() != nil
}

func (route *liveDataPlaneRoute) enqueue(message *runnerv1.RunnerToControlPlane) error {
	responseBytes, requestCredit, err := liveDataPlaneInboundFlow(message)
	if err != nil {
		return err
	}
	route.mu.Lock()
	defer route.mu.Unlock()
	if route.closeErr != nil {
		return ErrLiveDataPlaneRouteNotFound
	}
	if frame := message.GetPty(); frame != nil && frame.GetOutput() != nil &&
		frame.Sequence <= route.replayedThrough {
		responseBytes = 0
	}
	if responseBytes > route.responseCredit || requestCredit > route.streamWindowBytes-route.requestCredit {
		err := errors.Join(
			ErrLiveDataPlaneCreditViolation,
			ErrDataPlaneFrameLimit,
			errors.New("SecondBox Runner exceeded live data-plane stream credit"),
		)
		route.closeLocked(err)
		return err
	}
	if int64(len(route.delivery)) >= route.maximumQueuedFrames() {
		err := errors.Join(
			ErrLiveDataPlaneBufferInvariant,
			ErrDataPlaneSessionLimit,
			errors.New("SecondBox live data-plane queue exceeded its stream-window bound"),
		)
		route.closeLocked(err)
		return err
	}
	route.responseCredit -= responseBytes
	route.requestCredit += requestCredit
	route.delivery = append(route.delivery, liveDataPlaneDelivery{message: message})
	select {
	case route.ready <- struct{}{}:
	default:
	}
	return nil
}

func (route *liveDataPlaneRoute) maximumQueuedFrames() int64 {
	if route.streamWindowBytes > (math.MaxInt64-liveDataPlaneControlFrameReserve)/2 {
		return math.MaxInt64
	}
	// Port traffic can hold one window of response bytes and one window of
	// request-credit frames. The reserve covers opening metadata and terminal proof.
	return route.streamWindowBytes*2 + liveDataPlaneControlFrameReserve
}

func liveDataPlaneOutboundFlow(
	message *runnerv1.ControlPlaneToRunner,
) (int64, int64, error) {
	var credit uint64
	var requestBytes int
	switch {
	case message.GetExec() != nil && message.GetExec().GetCredit() != nil:
		credit = message.GetExec().GetCredit().ByteCount
	case message.GetFile() != nil && message.GetFile().GetCredit() != nil:
		credit = message.GetFile().GetCredit().ByteCount
	case message.GetPty() != nil && message.GetPty().GetCredit() != nil:
		credit = message.GetPty().GetCredit().ByteCount
	case message.GetPort() != nil && message.GetPort().GetCredit() != nil:
		credit = message.GetPort().GetCredit().ByteCount
	case message.GetPort() != nil && message.GetPort().GetBytes() != nil:
		requestBytes = len(message.GetPort().GetBytes().Data)
	}
	if credit > math.MaxInt64 {
		return 0, 0, errors.Join(
			ErrLiveDataPlaneCreditViolation,
			ErrDataPlaneFrameLimit,
			errors.New("SecondBox live data-plane credit exceeds the signed byte bound"),
		)
	}
	return int64(credit), int64(requestBytes), nil
}

func liveDataPlaneInboundFlow(
	message *runnerv1.RunnerToControlPlane,
) (int64, int64, error) {
	var responseBytes int
	var requestCredit uint64
	switch {
	case message.GetExec() != nil && message.GetExec().GetOutput() != nil:
		responseBytes = len(message.GetExec().GetOutput().Data)
	case message.GetFile() != nil && message.GetFile().GetChunk() != nil:
		responseBytes = len(message.GetFile().GetChunk().Data)
	case message.GetPty() != nil && message.GetPty().GetOutput() != nil:
		responseBytes = len(message.GetPty().GetOutput().Data)
	case message.GetPort() != nil && message.GetPort().GetBytes() != nil:
		responseBytes = len(message.GetPort().GetBytes().Data)
	case message.GetPort() != nil && message.GetPort().GetCredit() != nil:
		requestCredit = message.GetPort().GetCredit().ByteCount
	}
	if requestCredit > math.MaxInt64 {
		return 0, 0, errors.Join(
			ErrLiveDataPlaneCreditViolation,
			ErrDataPlaneFrameLimit,
			errors.New("SecondBox Runner live data-plane credit exceeds the signed byte bound"),
		)
	}
	return int64(responseBytes), int64(requestCredit), nil
}

func liveDataPlaneKey(kind string, operationID string, streamID string) (string, error) {
	if (kind != "exec" && kind != "terminal" && kind != "file" && kind != "port") || operationID == "" || streamID == "" {
		return "", errors.New("SecondBox live data-plane identity is incomplete")
	}
	return kind + "\x00" + operationID + "\x00" + streamID, nil
}

func liveDataPlaneMessageIdentity(
	message *runnerv1.RunnerToControlPlane,
) (string, string, string, error) {
	if frame := message.GetExec(); frame != nil {
		return "exec", frame.OperationId, frame.StreamId, nil
	}
	if frame := message.GetFile(); frame != nil {
		return "file", frame.OperationId, frame.StreamId, nil
	}
	if frame := message.GetPty(); frame != nil {
		return "terminal", frame.OperationId, frame.StreamId, nil
	}
	if frame := message.GetPort(); frame != nil {
		return "port", frame.OperationId, frame.StreamId, nil
	}
	return "", "", "", errors.New("SecondBox live data-plane message is not Exec, PTY, File, or Port")
}
