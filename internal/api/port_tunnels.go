package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/gorilla/websocket"
)

const (
	portTunnelSubprotocol      = "secondbox.port.v1"
	portTunnelTokenSubprotocol = "secondbox.port.token."
)

func (apiHandler *handler) createSandboxPortSession(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CreatePortSessionRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	session, replayed, err := apiHandler.service.CreateSandboxPortSession(
		request.Context(), requestPrincipal(request), request.Header.Get("X-Request-ID"),
		request.PathValue("sandboxID"), generation, request.Header.Get("SecondBox-Lease-ID"),
		request.Header.Get("Idempotency-Key"), body,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	writeJSON(writer, http.StatusCreated, session)
}

func (apiHandler *handler) getSandboxPortSession(writer http.ResponseWriter, request *http.Request) {
	session, err := apiHandler.service.GetSandboxPortSession(
		request.Context(), requestPrincipal(request), request.PathValue("sandboxID"),
		request.PathValue("portSessionID"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, session)
}

func (apiHandler *handler) closeSandboxPortSession(writer http.ResponseWriter, request *http.Request) {
	if err := requireEmptyBody(request); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	if err := apiHandler.service.CloseSandboxPortSession(
		request.Context(), requestPrincipal(request), request.PathValue("sandboxID"),
		request.PathValue("portSessionID"), request.Header.Get("Idempotency-Key"),
	); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (apiHandler *handler) connectPortTunnel(writer http.ResponseWriter, request *http.Request) {
	token, err := portTunnelTokenFromSubprotocols(websocket.Subprotocols(request))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	if !sameWebSocketOrigin(request) {
		apiHandler.writeError(writer, request, ports.ErrAuthorizationDenied)
		return
	}
	tunnel, err := apiHandler.service.ConsumePortTunnelToken(
		request.Context(), request.PathValue("portSessionID"), token,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	defer func() {
		closeContext, stopClose := context.WithTimeout(context.WithoutCancel(request.Context()), 5*time.Second)
		defer stopClose()
		if err := apiHandler.service.ClosePortTunnel(
			closeContext, tunnel, "public port tunnel disconnected",
		); err != nil {
			apiHandler.logger.ErrorContext(
				closeContext, "SecondBox Port tunnel disconnect cleanup failed",
				"error", err, "session_id", tunnel.Session.ID,
			)
		}
	}()
	upgrader := websocket.Upgrader{
		Subprotocols: []string{portTunnelSubprotocol},
		CheckOrigin:  sameWebSocketOrigin,
	}
	connection, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		apiHandler.logger.ErrorContext(request.Context(), "SecondBox Port WebSocket upgrade failed", "error", err)
		return
	}
	defer connection.Close()
	readLimit := min(apiHandler.maximumDataPlaneBodyBytes, tunnel.StreamWindowBytes)
	connection.SetReadLimit(readLimit)
	if err := connection.SetReadDeadline(tunnel.Session.ExpiresAt); err != nil {
		apiHandler.logger.InfoContext(request.Context(), "SecondBox Port WebSocket deadline failed", "error", err)
		return
	}
	if err := apiHandler.servePortTunnel(request, connection, tunnel); err != nil {
		apiHandler.logger.InfoContext(
			request.Context(), "SecondBox Port WebSocket closed",
			"error", err, "session_id", tunnel.Session.ID,
		)
	}
}

func (apiHandler *handler) servePortTunnel(
	request *http.Request,
	connection *websocket.Conn,
	tunnel runnercontrol.PortTunnel,
) error {
	tunnelContext, stopTunnel := context.WithCancel(request.Context())
	defer stopTunnel()
	readErrors := make(chan error, 1)
	go func() {
		readErrors <- apiHandler.readPortTunnelMessages(tunnelContext, connection, tunnel)
	}()
	ticker := time.NewTicker(apiHandler.service.DataPlanePollInterval())
	defer ticker.Stop()
	afterSequence := int64(-1)
	for {
		event, found, err := apiHandler.service.NextPortTunnelEvent(
			tunnelContext, tunnel, afterSequence,
		)
		if err != nil {
			return err
		}
		if found {
			if err := connection.SetWriteDeadline(tunnel.Session.ExpiresAt); err != nil {
				return fmt.Errorf("SecondBox Port WebSocket write deadline: %w", err)
			}
			switch {
			case event.Bytes != nil:
				if err := connection.WriteMessage(websocket.BinaryMessage, event.Bytes); err != nil {
					return fmt.Errorf("SecondBox Port WebSocket binary write: %w", err)
				}
			case event.TerminalKind != "":
				detail := event.TerminalDetail
				if detail == "" {
					detail = event.TerminalKind
				}
				if err := connection.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, detail),
					time.Now().Add(time.Second),
				); err != nil {
					return fmt.Errorf("SecondBox Port WebSocket terminal write: %w", err)
				}
			default:
				return errors.New("SecondBox Port tunnel event is invalid")
			}
			if err := apiHandler.service.AcknowledgePortTunnelEvent(
				tunnelContext, tunnel, event.Sequence,
			); err != nil {
				return err
			}
			afterSequence = event.Sequence
			if event.TerminalKind != "" {
				return nil
			}
			continue
		}
		select {
		case err := <-readErrors:
			return err
		case <-ticker.C:
		case <-tunnelContext.Done():
			return tunnelContext.Err()
		}
	}
}

func (apiHandler *handler) readPortTunnelMessages(
	ctx context.Context,
	connection *websocket.Conn,
	tunnel runnercontrol.PortTunnel,
) error {
	for {
		messageType, payload, err := connection.ReadMessage()
		if err != nil {
			return fmt.Errorf("SecondBox Port WebSocket read: %w", err)
		}
		if messageType != websocket.BinaryMessage {
			return errors.New("SecondBox Port WebSocket accepts only binary messages")
		}
		for {
			err = apiHandler.service.QueuePortTunnelBytes(ctx, tunnel, payload)
			if !errors.Is(err, ports.ErrPortBackpressure) {
				break
			}
			timer := time.NewTimer(apiHandler.service.DataPlanePollInterval())
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
		if err != nil {
			return err
		}
	}
}

func portTunnelTokenFromSubprotocols(protocols []string) (string, error) {
	hasProtocol := false
	token := ""
	for _, protocol := range protocols {
		switch {
		case protocol == portTunnelSubprotocol:
			hasProtocol = true
		case strings.HasPrefix(protocol, portTunnelTokenSubprotocol):
			if token != "" {
				return "", ports.ErrPortTokenInvalid
			}
			token = strings.TrimPrefix(protocol, portTunnelTokenSubprotocol)
		}
	}
	if !hasProtocol || token == "" || len(token) > 2048 {
		return "", ports.ErrPortTokenInvalid
	}
	return token, nil
}
