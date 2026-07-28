package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/gorilla/websocket"
)

const terminalSubprotocol = "secondbox.terminal.v1"

func (apiHandler *handler) createSandboxTerminal(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CreateTerminalRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	session, replayed, err := apiHandler.service.CreateSandboxTerminal(
		request.Context(), requestPrincipal(request), request.Header.Get("X-Request-ID"),
		request.PathValue("sandboxID"), generation, request.Header.Get("SecondBox-Lease-ID"),
		request.Header.Get("Idempotency-Key"), body,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	response, err := apiHandler.publicTerminalSession(session)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", fmt.Sprintf("%t", replayed))
	writeJSON(writer, http.StatusCreated, response)
}

func (apiHandler *handler) getOrConnectSandboxTerminal(writer http.ResponseWriter, request *http.Request) {
	if websocket.IsWebSocketUpgrade(request) {
		apiHandler.connectSandboxTerminal(writer, request)
		return
	}
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	session, err := apiHandler.service.GetSandboxTerminal(
		request.Context(), requestPrincipal(request), request.PathValue("sandboxID"),
		request.PathValue("terminalSessionID"), generation,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	response, err := apiHandler.publicTerminalSession(session)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (apiHandler *handler) cancelSandboxTerminal(writer http.ResponseWriter, request *http.Request) {
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	session, replayed, err := apiHandler.service.CancelSandboxTerminal(
		request.Context(), requestPrincipal(request), request.PathValue("sandboxID"),
		request.PathValue("terminalSessionID"), generation, request.Header.Get("Idempotency-Key"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	response, err := apiHandler.publicTerminalSession(session)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", fmt.Sprintf("%t", replayed))
	writeJSON(writer, http.StatusAccepted, response)
}

func (apiHandler *handler) publicTerminalSession(
	session runnercontrol.DataPlaneSession,
) (contracts.TerminalSession, error) {
	endpoint, err := apiHandler.service.SandboxTerminalEndpoint(session.SandboxID, session.ID)
	if err != nil {
		return contracts.TerminalSession{}, err
	}
	state := "open"
	switch {
	case session.State == "cancelling":
		state = "closing"
	case session.State == "completed" || session.State == "failed" ||
		session.State == "cancelled" || session.State == "expired":
		state = "closed"
	case session.DetachedAt != nil:
		state = "detached"
	}
	return contracts.TerminalSession{
		ID: session.ID, SandboxID: session.SandboxID, Generation: session.Generation,
		State: state, WebsocketURL: endpoint, Subprotocol: terminalSubprotocol,
		NextClientSequence: session.NextClientSequence, ExpiresAt: session.DeadlineAt,
	}, nil
}

func (apiHandler *handler) connectSandboxTerminal(writer http.ResponseWriter, request *http.Request) {
	if !containsString(websocket.Subprotocols(request), terminalSubprotocol) {
		apiHandler.writeError(writer, request, errors.New("SecondBox Terminal WebSocket subprotocol is required"))
		return
	}
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	session, attachmentID, err := apiHandler.service.AcquireSandboxTerminalAttachment(
		request.Context(), requestPrincipal(request), request.PathValue("sandboxID"),
		request.PathValue("terminalSessionID"), generation,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	defer func() {
		detachContext, stopDetach := context.WithTimeout(
			context.WithoutCancel(request.Context()), 5*time.Second,
		)
		defer stopDetach()
		if _, err := apiHandler.service.DetachSandboxTerminalAttachment(
			detachContext, requestPrincipal(request), session.ID, attachmentID,
		); err != nil && !errors.Is(err, runnercontrol.ErrTerminalDetached) {
			apiHandler.logger.ErrorContext(
				detachContext, "SecondBox Terminal detach failed",
				"error", err, "session_id", session.ID,
			)
		}
	}()
	upgrader := websocket.Upgrader{
		Subprotocols: []string{terminalSubprotocol},
		CheckOrigin:  sameWebSocketOrigin,
	}
	connection, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		apiHandler.logger.ErrorContext(request.Context(), "SecondBox Terminal WebSocket upgrade failed", "error", err)
		return
	}
	defer connection.Close()
	connection.SetReadLimit(apiHandler.maximumDataPlaneBodyBytes)
	_, err = apiHandler.serveSandboxTerminal(request, connection, session, attachmentID)
	if err != nil {
		apiHandler.logger.InfoContext(
			request.Context(), "SecondBox Terminal WebSocket closed",
			"error", err, "session_id", session.ID,
		)
	}
}

func (apiHandler *handler) serveSandboxTerminal(
	request *http.Request,
	connection *websocket.Conn,
	session runnercontrol.DataPlaneSession,
	attachmentID string,
) (bool, error) {
	principal := requestPrincipal(request)
	streamContext, stopStream := context.WithCancel(request.Context())
	defer stopStream()
	readErrors := make(chan error, 1)
	go func() {
		readErrors <- apiHandler.readSandboxTerminalFrames(
			streamContext, principal, connection, session.ID, attachmentID,
		)
	}()
	ticker := time.NewTicker(apiHandler.service.DataPlanePollInterval())
	defer ticker.Stop()
	afterSequence := int64(-1)
	for {
		frames, err := apiHandler.service.ListSandboxTerminalFrames(
			request.Context(), principal, session.ID, afterSequence,
		)
		if err != nil {
			return false, err
		}
		for _, frame := range frames {
			if err := connection.SetWriteDeadline(session.DeadlineAt); err != nil {
				return false, fmt.Errorf("SecondBox Terminal WebSocket write deadline: %w", err)
			}
			switch {
			case frame.Output != nil:
				if err := connection.WriteJSON(contracts.TerminalOutputFrame{
					Type: "terminal_output", Sequence: frame.Sequence,
					DataBase64: base64.StdEncoding.EncodeToString(frame.Output),
				}); err != nil {
					return false, fmt.Errorf("SecondBox Terminal WebSocket output write: %w", err)
				}
			case frame.Terminal != nil:
				outcome, err := apiHandler.service.SandboxTerminalOutcome(
					request.Context(), principal, session.ID,
				)
				if err != nil {
					return false, err
				}
				if err := connection.WriteJSON(contracts.StreamOutcomeFrame{
					Type: "outcome", Sequence: frame.Sequence, Outcome: outcome,
				}); err != nil {
					return false, fmt.Errorf("SecondBox Terminal WebSocket outcome write: %w", err)
				}
				return true, connection.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "terminal outcome delivered"),
					time.Now().Add(time.Second),
				)
			default:
				return false, errors.New("SecondBox Terminal retained WebSocket frame is unsupported")
			}
			afterSequence = frame.Sequence
		}
		select {
		case err := <-readErrors:
			return false, err
		case <-ticker.C:
		case <-request.Context().Done():
			return false, request.Context().Err()
		}
	}
}

func (apiHandler *handler) readSandboxTerminalFrames(
	ctx context.Context,
	principal contracts.Principal,
	connection *websocket.Conn,
	sessionID string,
	attachmentID string,
) error {
	for {
		messageType, payload, err := connection.ReadMessage()
		if err != nil {
			return fmt.Errorf("SecondBox Terminal WebSocket read: %w", err)
		}
		if messageType != websocket.TextMessage {
			return errors.New("SecondBox Terminal WebSocket accepts only JSON text frames")
		}
		frame, err := decodePublicTerminalClientFrame(payload)
		if err != nil {
			return err
		}
		if _, err := apiHandler.service.AppendSandboxTerminalFrame(
			ctx, principal, sessionID, attachmentID, frame,
		); err != nil {
			return err
		}
	}
}

func decodePublicTerminalClientFrame(payload []byte) (runnercontrol.TerminalClientFrame, error) {
	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &discriminator); err != nil {
		return runnercontrol.TerminalClientFrame{}, errors.New("SecondBox Terminal WebSocket frame is invalid JSON")
	}
	switch discriminator.Type {
	case "terminal_input":
		var frame contracts.TerminalInputFrame
		if err := decodeStrictFrame(payload, &frame); err != nil || frame.Sequence < 0 {
			return runnercontrol.TerminalClientFrame{}, errors.New("SecondBox Terminal input frame is invalid")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(frame.DataBase64)
		if err != nil || len(data) == 0 {
			return runnercontrol.TerminalClientFrame{}, errors.New("SecondBox Terminal input is not nonempty canonical base64")
		}
		return runnercontrol.TerminalClientFrame{Sequence: frame.Sequence, Input: data}, nil
	case "resize":
		var frame contracts.TerminalResizeFrame
		if err := decodeStrictFrame(payload, &frame); err != nil ||
			frame.Sequence < 0 || frame.Rows < 1 || frame.Rows > 1000 ||
			frame.Columns < 1 || frame.Columns > 1000 {
			return runnercontrol.TerminalClientFrame{}, errors.New("SecondBox Terminal resize frame is invalid")
		}
		return runnercontrol.TerminalClientFrame{
			Sequence: frame.Sequence, ResizeRows: uint32(frame.Rows), ResizeColumns: uint32(frame.Columns),
		}, nil
	case "credit":
		var frame contracts.StreamCreditFrame
		if err := decodeStrictFrame(payload, &frame); err != nil || frame.Sequence < 0 || frame.Bytes < 1 {
			return runnercontrol.TerminalClientFrame{}, errors.New("SecondBox Terminal credit frame is invalid")
		}
		return runnercontrol.TerminalClientFrame{Sequence: frame.Sequence, Credit: frame.Bytes}, nil
	case "cancel":
		var frame contracts.StreamCancelFrame
		if err := decodeStrictFrame(payload, &frame); err != nil || frame.Sequence < 0 {
			return runnercontrol.TerminalClientFrame{}, errors.New("SecondBox Terminal cancel frame is invalid")
		}
		return runnercontrol.TerminalClientFrame{Sequence: frame.Sequence, Cancel: true}, nil
	default:
		return runnercontrol.TerminalClientFrame{}, errors.New("SecondBox Terminal WebSocket frame type is invalid")
	}
}
