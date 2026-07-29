package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/gorilla/websocket"
)

const execStreamSubprotocol = "secondbox.exec.v1"

func (apiHandler *handler) createSandboxExecStream(writer http.ResponseWriter, request *http.Request) {
	var body contracts.StreamingExecRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	session, replayed, err := apiHandler.service.CreateSandboxExecStream(
		request.Context(), requestPrincipal(request), request.Header.Get("X-Request-ID"),
		request.PathValue("sandboxID"), generation, request.Header.Get("SecondBox-Lease-ID"),
		request.Header.Get("Idempotency-Key"), body,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	response, err := apiHandler.publicExecStreamSession(session)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", fmt.Sprintf("%t", replayed))
	apiHandler.writeJSON(writer, request, http.StatusCreated, response)
}

func (apiHandler *handler) cancelSandboxExecStream(writer http.ResponseWriter, request *http.Request) {
	sessionID, action, ok := splitAction(request.PathValue("execSessionAction"))
	if !ok || action != "cancel" {
		apiHandler.writeError(writer, request, runnercontrol.ErrDataPlaneNotFound)
		return
	}
	if err := requireEmptyBody(request); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	session, replayed, err := apiHandler.service.CancelSandboxExecStreamAtGeneration(
		request.Context(), requestPrincipal(request), request.PathValue("sandboxID"),
		sessionID, generation, request.Header.Get("Idempotency-Key"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	response, err := apiHandler.publicExecStreamSession(session)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", fmt.Sprintf("%t", replayed))
	apiHandler.writeJSON(writer, request, http.StatusAccepted, response)
}

func (apiHandler *handler) publicExecStreamSession(
	session runnercontrol.DataPlaneSession,
) (contracts.ExecStreamSession, error) {
	endpoint, err := apiHandler.service.SandboxExecStreamEndpoint(session.SandboxID, session.ID)
	if err != nil {
		return contracts.ExecStreamSession{}, err
	}
	state := "open"
	switch session.State {
	case "cancelling":
		state = "closing"
	case "completed", "failed", "cancelled", "expired":
		state = "closed"
	}
	return contracts.ExecStreamSession{
		ID: session.ID, SandboxID: session.SandboxID, Generation: session.Generation,
		State: state, WebsocketURL: endpoint, Subprotocol: execStreamSubprotocol,
		ExpiresAt: session.DeadlineAt,
	}, nil
}

func (apiHandler *handler) connectSandboxExecStream(writer http.ResponseWriter, request *http.Request) {
	if !containsString(websocket.Subprotocols(request), execStreamSubprotocol) {
		apiHandler.writeError(writer, request, errors.New("SecondBox Exec WebSocket subprotocol is required"))
		return
	}
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	session, err := apiHandler.service.GetSandboxExecStream(
		request.Context(), requestPrincipal(request), request.PathValue("sandboxID"),
		request.PathValue("execSessionID"), generation,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	upgrader := websocket.Upgrader{
		Subprotocols: []string{execStreamSubprotocol},
		CheckOrigin:  sameWebSocketOrigin,
	}
	connection, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		apiHandler.logger.ErrorContext(request.Context(), "SecondBox Exec WebSocket upgrade failed", "error", err)
		return
	}
	defer connection.Close()
	connection.SetReadLimit(apiHandler.maximumDataPlaneBodyBytes)
	if err := apiHandler.serveSandboxExecStream(request, connection, session); err != nil {
		apiHandler.logger.InfoContext(request.Context(), "SecondBox Exec WebSocket closed", "error", err, "session_id", session.ID)
	}
}

func (apiHandler *handler) serveSandboxExecStream(
	request *http.Request,
	connection *websocket.Conn,
	session runnercontrol.DataPlaneSession,
) error {
	principal := requestPrincipal(request)
	streamContext, stopStream := context.WithCancel(request.Context())
	defer stopStream()
	readErrors := make(chan error, 1)
	go func() {
		readErrors <- apiHandler.readSandboxExecFrames(streamContext, principal, connection, session.ID)
	}()
	ticker := time.NewTicker(apiHandler.service.DataPlanePollInterval())
	defer ticker.Stop()
	afterSequence := int64(-1)
	terminal := false
	defer func() {
		if !terminal {
			cancelContext, stopCancel := context.WithTimeout(
				context.WithoutCancel(request.Context()),
				5*time.Second,
			)
			defer stopCancel()
			if _, err := apiHandler.service.CancelSandboxExecStream(
				cancelContext, principal, session.ID, "public streaming client disconnected",
			); err != nil {
				apiHandler.logger.ErrorContext(cancelContext, "SecondBox Exec disconnect cancellation failed", "error", err, "session_id", session.ID)
			}
		}
	}()
	for {
		frames, err := apiHandler.service.ListSandboxExecStreamFrames(
			request.Context(), principal, session.ID, afterSequence,
		)
		if err != nil {
			return err
		}
		for _, frame := range frames {
			if err := connection.SetWriteDeadline(session.DeadlineAt); err != nil {
				return fmt.Errorf("SecondBox Exec WebSocket write deadline: %w", err)
			}
			switch {
			case frame.Output != nil:
				channel, err := publicExecOutputChannel(frame.Output.Channel)
				if err != nil {
					return err
				}
				if err := connection.WriteJSON(contracts.StreamOutputFrame{
					Type: "output", Sequence: frame.Sequence, Stream: channel,
					DataBase64: base64.StdEncoding.EncodeToString(frame.Output.Data),
				}); err != nil {
					return fmt.Errorf("SecondBox Exec WebSocket output write: %w", err)
				}
			case frame.Terminal != nil:
				outcome, err := apiHandler.service.SandboxExecStreamOutcome(
					request.Context(), principal, session.ID,
				)
				if err != nil {
					return err
				}
				if err := connection.WriteJSON(contracts.StreamOutcomeFrame{
					Type: "outcome", Sequence: frame.Sequence, Outcome: outcome,
				}); err != nil {
					return fmt.Errorf("SecondBox Exec WebSocket outcome write: %w", err)
				}
				terminal = true
				return connection.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "terminal outcome delivered"),
					time.Now().Add(time.Second),
				)
			default:
				return errors.New("SecondBox Exec retained WebSocket frame is unsupported")
			}
			afterSequence = frame.Sequence
		}
		select {
		case err := <-readErrors:
			return err
		case <-ticker.C:
		case <-request.Context().Done():
			return request.Context().Err()
		}
	}
}

func (apiHandler *handler) readSandboxExecFrames(
	ctx context.Context,
	principal contracts.Principal,
	connection *websocket.Conn,
	sessionID string,
) error {
	for {
		messageType, payload, err := connection.ReadMessage()
		if err != nil {
			return fmt.Errorf("SecondBox Exec WebSocket read: %w", err)
		}
		if messageType != websocket.TextMessage {
			return errors.New("SecondBox Exec WebSocket accepts only JSON text frames")
		}
		frame, err := decodePublicExecClientFrame(payload)
		if err != nil {
			return err
		}
		if _, err := apiHandler.service.AppendSandboxExecStreamFrame(
			ctx, principal, sessionID, frame,
		); err != nil {
			return err
		}
	}
}

func decodePublicExecClientFrame(payload []byte) (runnercontrol.ExecClientFrame, error) {
	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &discriminator); err != nil {
		return runnercontrol.ExecClientFrame{}, errors.New("SecondBox Exec WebSocket frame is invalid JSON")
	}
	switch discriminator.Type {
	case "stdin":
		var frame contracts.StreamInputFrame
		if err := decodeStrictFrame(payload, &frame); err != nil ||
			frame.Sequence < 0 || frame.EndOfInput == nil {
			return runnercontrol.ExecClientFrame{}, errors.New("SecondBox Exec stdin frame is invalid")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(frame.DataBase64)
		if err != nil {
			return runnercontrol.ExecClientFrame{}, errors.New("SecondBox Exec stdin is not canonical base64")
		}
		if len(data) == 0 && !*frame.EndOfInput {
			return runnercontrol.ExecClientFrame{}, errors.New("SecondBox Exec stdin frame is empty")
		}
		return runnercontrol.ExecClientFrame{
			Sequence: frame.Sequence, Input: data, EndInput: *frame.EndOfInput,
		}, nil
	case "credit":
		var frame contracts.StreamCreditFrame
		if err := decodeStrictFrame(payload, &frame); err != nil || frame.Sequence < 0 || frame.Bytes < 1 {
			return runnercontrol.ExecClientFrame{}, errors.New("SecondBox Exec credit frame is invalid")
		}
		return runnercontrol.ExecClientFrame{Sequence: frame.Sequence, Credit: frame.Bytes}, nil
	case "cancel":
		var frame contracts.StreamCancelFrame
		if err := decodeStrictFrame(payload, &frame); err != nil || frame.Sequence < 0 {
			return runnercontrol.ExecClientFrame{}, errors.New("SecondBox Exec cancel frame is invalid")
		}
		return runnercontrol.ExecClientFrame{Sequence: frame.Sequence, Cancel: true}, nil
	case "signal":
		return runnercontrol.ExecClientFrame{}, errors.New("SecondBox Exec signals are not implemented")
	default:
		return runnercontrol.ExecClientFrame{}, errors.New("SecondBox Exec WebSocket frame type is invalid")
	}
}

func decodeStrictFrame(payload []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("SecondBox Exec WebSocket frame contains trailing JSON")
}

func sameWebSocketOrigin(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, request.Host) &&
		(parsed.Scheme == "http" || parsed.Scheme == "https")
}

func publicExecOutputChannel(channel runnerv1.ExecOutputChannel) (string, error) {
	switch channel {
	case runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT:
		return "stdout", nil
	case runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR:
		return "stderr", nil
	default:
		return "", errors.New("SecondBox Exec output channel is invalid")
	}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
