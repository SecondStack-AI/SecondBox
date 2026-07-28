// Package api exposes the versioned Sandbox Service HTTP contract.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"secondstack/sandbox-service/internal/ports"
	"secondstack/sandbox-service/internal/service"
	"secondstack/sandbox-service/pkg/contracts"
)

const maxRequestBytes = 1 << 20

// HandlerConfig supplies the HTTP transport's explicit dependencies.
type HandlerConfig struct {
	Service              *service.SandboxService
	InternalToken        string
	Logger               *slog.Logger
	MaxFileTransferBytes int64
}

// NewHandler constructs an authenticated versioned HTTP API.
func NewHandler(config HandlerConfig) (http.Handler, error) {
	if config.Service == nil {
		return nil, errors.New("Sandbox Service API coordinator is required")
	}
	if strings.TrimSpace(config.InternalToken) == "" {
		return nil, errors.New("Sandbox Service internal bearer token is required")
	}
	if config.Logger == nil {
		return nil, errors.New("Sandbox Service API logger is required")
	}
	if config.MaxFileTransferBytes <= 0 {
		return nil, errors.New("Sandbox Service API file transfer limit must be positive")
	}
	handler := &handler{
		service:              config.Service,
		token:                config.InternalToken,
		logger:               config.Logger,
		maxFileTransferBytes: config.MaxFileTransferBytes,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handler.health)
	mux.HandleFunc("GET /readyz", handler.ready)
	mux.Handle("POST /v1/environments:resolve", handler.auth(http.HandlerFunc(handler.resolveEnvironment)))
	mux.Handle("GET /v1/environments/{environmentID}", handler.auth(http.HandlerFunc(handler.getEnvironment)))
	mux.Handle("GET /v1/environments/{environmentID}/files", handler.auth(http.HandlerFunc(handler.getWorkspaceFile)))
	mux.Handle("PUT /v1/environments/{environmentID}/files", handler.auth(http.HandlerFunc(handler.putWorkspaceFile)))
	mux.Handle("GET /v1/environments/{environmentID}/versions:current", handler.auth(http.HandlerFunc(handler.getCurrentWorkspaceVersion)))
	mux.Handle("GET /v1/environments/{environmentID}/versions/{logicalVersion}", handler.auth(http.HandlerFunc(handler.getWorkspaceVersion)))
	mux.Handle("POST /v1/environments/{environmentOperation}", handler.auth(http.HandlerFunc(handler.environmentOperation)))
	mux.Handle("POST /v1/environments/{environmentID}/{nestedOperation}", handler.auth(http.HandlerFunc(handler.nestedEnvironmentOperation)))
	mux.Handle("POST /v1/leases/{leaseOperation}", handler.auth(http.HandlerFunc(handler.leaseOperation)))
	mux.Handle("GET /v1/resource-classes", handler.auth(http.HandlerFunc(handler.listResourceClasses)))
	mux.Handle("GET /v1/lifecycle-policies", handler.auth(http.HandlerFunc(handler.listLifecyclePolicies)))
	mux.Handle("GET /v1/workspace-usage", handler.auth(http.HandlerFunc(handler.getWorkspaceUsage)))
	mux.Handle("GET /metrics", handler.auth(http.HandlerFunc(handler.metrics)))
	return recoverMiddleware(config.Logger, mux), nil
}

type handler struct {
	service              *service.SandboxService
	token                string
	logger               *slog.Logger
	maxFileTransferBytes int64
}

func (h *handler) environmentOperation(writer http.ResponseWriter, request *http.Request) {
	operation := request.PathValue("environmentOperation")
	for suffix, endpoint := range map[string]http.HandlerFunc{
		":start": h.startEnvironment, ":inspect": h.inspectEnvironment,
		":stop": h.stopEnvironment, ":checkpoint": h.checkpointEnvironment,
		":execute": h.executeEnvironment, ":purge": h.purgeEnvironment,
		":materialize": h.materializeWorkspaceVersion,
	} {
		if environmentID, ok := strings.CutSuffix(operation, suffix); ok && environmentID != "" {
			request.SetPathValue("environmentID", environmentID)
			endpoint(writer, request)
			return
		}
	}
	writeFailure(writer, http.StatusNotFound, "not_found", "Sandbox Service route was not found", false)
}

func (h *handler) nestedEnvironmentOperation(writer http.ResponseWriter, request *http.Request) {
	switch request.PathValue("nestedOperation") {
	case "artifacts:exchange":
		h.exchangeArtifact(writer, request)
	case "leases:acquire":
		h.acquireLease(writer, request)
	case "versions:commit":
		h.commitWorkspaceVersion(writer, request)
	default:
		writeFailure(writer, http.StatusNotFound, "not_found", "Sandbox Service route was not found", false)
	}
}

func (h *handler) leaseOperation(writer http.ResponseWriter, request *http.Request) {
	operation := request.PathValue("leaseOperation")
	for suffix, endpoint := range map[string]http.HandlerFunc{
		":renew": h.renewLease, ":release": h.releaseLease,
	} {
		if leaseID, ok := strings.CutSuffix(operation, suffix); ok && leaseID != "" {
			request.SetPathValue("leaseID", leaseID)
			endpoint(writer, request)
			return
		}
	}
	writeFailure(writer, http.StatusNotFound, "not_found", "Sandbox Service route was not found", false)
}

func (h *handler) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) ready(writer http.ResponseWriter, request *http.Request) {
	if err := h.service.Ready(request.Context()); err != nil {
		h.logger.ErrorContext(request.Context(), "Sandbox Service readiness failed", "error", err)
		writeFailure(writer, http.StatusServiceUnavailable, "not_ready", "Sandbox Service is not ready", true)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *handler) metrics(writer http.ResponseWriter, request *http.Request) {
	output, err := h.service.PrometheusMetrics(request.Context())
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	if _, err := writer.Write([]byte(output)); err != nil {
		panic(fmt.Sprintf("write Sandbox Service metrics response: %v", err))
	}
}

func (h *handler) resolveEnvironment(writer http.ResponseWriter, request *http.Request) {
	var body contracts.ResolveEnvironmentRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	response, err := h.service.ResolveEnvironment(request.Context(), body)
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	status := http.StatusOK
	if response.Created {
		status = http.StatusCreated
	}
	writeJSON(writer, status, response)
}

func (h *handler) getEnvironment(writer http.ResponseWriter, request *http.Request) {
	response, err := h.service.GetEnvironment(request.Context(), request.PathValue("environmentID"))
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) startEnvironment(writer http.ResponseWriter, request *http.Request) {
	var body contracts.EnvironmentGenerationRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	if body.ContractVersion != contracts.ContractVersionV1 {
		h.writeError(writer, request, errors.New("unsupported Sandbox Service contract version"))
		return
	}
	response, err := h.service.StartEnvironment(
		request.Context(), request.PathValue("environmentID"), body.ExpectedGeneration,
	)
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) inspectEnvironment(writer http.ResponseWriter, request *http.Request) {
	response, err := h.service.InspectEnvironment(request.Context(), request.PathValue("environmentID"))
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) stopEnvironment(writer http.ResponseWriter, request *http.Request) {
	var body contracts.EnvironmentGenerationRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	if body.ContractVersion != contracts.ContractVersionV1 {
		h.writeError(writer, request, errors.New("unsupported Sandbox Service contract version"))
		return
	}
	response, err := h.service.StopEnvironment(
		request.Context(), request.PathValue("environmentID"), body.ExpectedGeneration,
	)
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) checkpointEnvironment(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CheckpointRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	response, err := h.service.CheckpointEnvironment(
		request.Context(), request.PathValue("environmentID"), body,
	)
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, response)
}

func (h *handler) exchangeArtifact(writer http.ResponseWriter, request *http.Request) {
	var body contracts.ExchangeArtifactRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	response, err := h.service.ExchangeArtifact(
		request.Context(), request.PathValue("environmentID"), body,
	)
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, response)
}

func (h *handler) executeEnvironment(writer http.ResponseWriter, request *http.Request) {
	var body contracts.ExecuteRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	response, err := h.service.ExecuteEnvironment(
		request.Context(), request.PathValue("environmentID"), body,
	)
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) getWorkspaceFile(writer http.ResponseWriter, request *http.Request) {
	expectedGeneration, err := parsePositiveInt64(request.URL.Query().Get("expectedGeneration"))
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	reader, size, err := h.service.OpenWorkspaceFile(
		request.Context(), request.PathValue("environmentID"), request.URL.Query().Get("path"),
		expectedGeneration, request.URL.Query().Get("leaseId"),
	)
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	defer reader.Close()
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	writer.WriteHeader(http.StatusOK)
	if _, err := io.CopyN(writer, reader, size); err != nil {
		h.logger.ErrorContext(request.Context(), "stream Sandbox workspace file failed", "error", err)
	}
}

func (h *handler) putWorkspaceFile(writer http.ResponseWriter, request *http.Request) {
	expectedGeneration, err := parsePositiveInt64(request.URL.Query().Get("expectedGeneration"))
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxFileTransferBytes)
	result, err := h.service.PutWorkspaceFile(
		request.Context(), request.PathValue("environmentID"), request.URL.Query().Get("path"),
		expectedGeneration, request.URL.Query().Get("leaseId"), request.Body,
	)
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"sizeBytes": result.SizeBytes, "sha256": result.SHA256})
}

func (h *handler) commitWorkspaceVersion(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CommitWorkspaceVersionRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	response, err := h.service.CommitWorkspaceVersion(request.Context(), request.PathValue("environmentID"), body)
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, response)
}

func (h *handler) getCurrentWorkspaceVersion(writer http.ResponseWriter, request *http.Request) {
	response, err := h.service.GetCurrentWorkspaceVersion(request.Context(), request.PathValue("environmentID"))
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	if response == nil {
		writeFailure(writer, http.StatusNotFound, "not_found", "workspace version not found", false)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) getWorkspaceVersion(writer http.ResponseWriter, request *http.Request) {
	logicalVersion, err := parsePositiveInt(request.PathValue("logicalVersion"))
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	response, err := h.service.GetWorkspaceVersion(request.Context(), request.PathValue("environmentID"), int64(logicalVersion))
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) materializeWorkspaceVersion(writer http.ResponseWriter, request *http.Request) {
	var body contracts.MaterializeWorkspaceVersionRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	if err := h.service.MaterializeWorkspaceVersion(request.Context(), request.PathValue("environmentID"), body); err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, struct{}{})
}

func (h *handler) purgeEnvironment(writer http.ResponseWriter, request *http.Request) {
	var body contracts.PurgeEnvironmentRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	if err := h.service.PurgeEnvironment(request.Context(), request.PathValue("environmentID"), body); err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, struct{}{})
}

func (h *handler) acquireLease(writer http.ResponseWriter, request *http.Request) {
	var body contracts.AcquireLeaseRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	response, err := h.service.AcquireLease(
		request.Context(), request.PathValue("environmentID"), body,
	)
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, response)
}

func (h *handler) renewLease(writer http.ResponseWriter, request *http.Request) {
	var body contracts.RenewLeaseRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	response, err := h.service.RenewLease(request.Context(), request.PathValue("leaseID"), body)
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) releaseLease(writer http.ResponseWriter, request *http.Request) {
	if request.ContentLength > 0 {
		var body struct{}
		if !decodeBody(writer, request, &body) {
			return
		}
	}
	response, err := h.service.ReleaseLease(request.Context(), request.PathValue("leaseID"))
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) listResourceClasses(writer http.ResponseWriter, request *http.Request) {
	response, err := h.service.ListResourceClasses(request.Context())
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) listLifecyclePolicies(writer http.ResponseWriter, request *http.Request) {
	response, err := h.service.ListLifecyclePolicies(request.Context())
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) getWorkspaceUsage(writer http.ResponseWriter, request *http.Request) {
	response, err := h.service.GetWorkspaceUsage(
		request.Context(),
		request.URL.Query().Get("tenantRef"),
		request.URL.Query().Get("subjectRef"),
	)
	if err != nil {
		h.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		provided := []byte(request.Header.Get("Authorization"))
		expected := []byte("Bearer " + h.token)
		if subtle.ConstantTimeCompare(provided, expected) != 1 {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			writeFailure(writer, http.StatusUnauthorized, "unauthorized", "Bearer authentication is required", false)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (h *handler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	status, code, message, retryable := classifyError(err)
	if status >= http.StatusInternalServerError {
		h.logger.ErrorContext(request.Context(), "Sandbox Service request failed",
			"method", request.Method, "path", request.URL.Path, "error", err)
	}
	writeFailure(writer, status, code, message, retryable)
}

func classifyError(err error) (int, string, string, bool) {
	switch {
	case errors.Is(err, ports.ErrEnvironmentNotFound), errors.Is(err, ports.ErrLeaseNotFound):
		return http.StatusNotFound, "not_found", err.Error(), false
	case errors.Is(err, ports.ErrGenerationFenced):
		return http.StatusConflict, "generation_fenced", err.Error(), false
	case errors.Is(err, ports.ErrLeaseExpired), errors.Is(err, ports.ErrLeaseReleased),
		errors.Is(err, ports.ErrEnvironmentBusy):
		return http.StatusConflict, "lifecycle_conflict", err.Error(), false
	default:
		if isValidationError(err) {
			return http.StatusBadRequest, "invalid_request", err.Error(), false
		}
		return http.StatusInternalServerError, "internal_error", "Sandbox Service request failed", true
	}
}

func isValidationError(err error) bool {
	text := err.Error()
	for _, fragment := range []string{
		"required", "must", "unsupported Sandbox Service contract", "outside the allowed",
		"exceeds", "between 1 and", "current Sandbox Service contract",
	} {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}

func decodeBody(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeFailure(writer, http.StatusBadRequest, "invalid_json", "Request body is invalid: "+err.Error(), false)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeFailure(writer, http.StatusBadRequest, "invalid_json", "Request body must contain one JSON value", false)
		return false
	}
	return true
}

func writeFailure(writer http.ResponseWriter, status int, code, message string, retryable bool) {
	writeJSON(writer, status, contracts.ErrorResponse{Code: code, Message: message, Retryable: retryable})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		panic(fmt.Sprintf("encode Sandbox Service HTTP response: %v", err))
	}
}

func recoverMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.ErrorContext(request.Context(), "Sandbox Service HTTP panic",
					"panic", recovered, "method", request.Method, "path", request.URL.Path)
				writeFailure(writer, http.StatusInternalServerError, "internal_error", "Sandbox Service request failed", true)
			}
		}()
		next.ServeHTTP(writer, request)
	})
}

func parsePositiveInt(value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, errors.New("value must be a positive integer")
	}
	return parsed, nil
}

func parsePositiveInt64(value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 {
		return 0, errors.New("value must be a positive integer")
	}
	return parsed, nil
}
