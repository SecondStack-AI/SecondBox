// Package api exposes the versioned standalone SecondBox HTTP contract.
package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/observability"
	"github.com/SecondStack-AI/SecondBox/internal/pagination"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type principalContextKey struct{}

var correlationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
var ownershipRefPattern = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)

// HandlerConfig contains the control plane and mandatory logger.
type HandlerConfig struct {
	Service                   *service.ControlPlaneService
	Logger                    *slog.Logger
	PlatformToken             string
	PersistedAuthorities      PersistedAuthorityAuthenticator
	MaximumDataPlaneBodyBytes int64
}

type handler struct {
	service                   *service.ControlPlaneService
	logger                    *slog.Logger
	platformTokenHash         [sha256.Size]byte
	persistedAuthorities      PersistedAuthorityAuthenticator
	maximumDataPlaneBodyBytes int64
	timings                   *observability.TimingRecorder
}

// NewHandler constructs the public and administrative SecondBox HTTP surface.
func NewHandler(config HandlerConfig) (http.Handler, error) {
	if config.Service == nil {
		return nil, errors.New("SecondBox HTTP ControlPlaneService is required")
	}
	if config.Logger == nil {
		return nil, errors.New("SecondBox HTTP logger is required")
	}
	if len(config.PlatformToken) < 24 {
		return nil, errors.New("SecondBox HTTP platform token must contain at least 24 bytes")
	}
	if isTenantControllerBearerToken(config.PlatformToken) || isApplicationBearerToken(config.PlatformToken) {
		return nil, errors.New("SecondBox HTTP platform token uses a reserved persisted-authority prefix")
	}
	if config.MaximumDataPlaneBodyBytes <= 0 {
		return nil, errors.New("SecondBox HTTP data-plane body bound is required")
	}
	platformTokenHash := sha256.Sum256([]byte(config.PlatformToken))
	apiHandler := &handler{
		service: config.Service, logger: config.Logger,
		platformTokenHash:         platformTokenHash,
		persistedAuthorities:      config.PersistedAuthorities,
		maximumDataPlaneBodyBytes: config.MaximumDataPlaneBodyBytes,
		timings:                   observability.NewTimingRecorder(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", apiHandler.health)
	mux.HandleFunc("GET /readyz", apiHandler.ready)
	mux.HandleFunc("GET /metrics", apiHandler.metrics)
	mux.Handle("GET /v1/tenants", apiHandler.authenticatePlatformManagement(http.HandlerFunc(apiHandler.listTenants)))
	mux.Handle("POST /v1/tenants", apiHandler.authenticatePlatformManagement(http.HandlerFunc(apiHandler.createTenant)))
	mux.Handle("GET /v1/tenants/{tenantRef}", apiHandler.authenticatePlatformManagement(http.HandlerFunc(apiHandler.getTenant)))
	mux.Handle("PUT /v1/tenants/{tenantRef}/egress-context", apiHandler.authenticatePlatformManagement(http.HandlerFunc(apiHandler.updateTenantEgressContext)))
	mux.Handle("POST /v1/tenants/{tenantAction}", apiHandler.authenticatePlatformManagement(http.HandlerFunc(apiHandler.tenantManagementAction)))
	mux.Handle("GET /v1/tenants/{tenantRef}/controller-authorities", apiHandler.authenticatePlatformManagement(http.HandlerFunc(apiHandler.listTenantControllerAuthorities)))
	mux.Handle("POST /v1/tenants/{tenantRef}/controller-authorities", apiHandler.authenticatePlatformManagement(http.HandlerFunc(apiHandler.createTenantControllerAuthority)))
	mux.Handle("GET /v1/tenants/{tenantRef}/controller-authorities/{authorityID}", apiHandler.authenticatePlatformManagement(http.HandlerFunc(apiHandler.getTenantControllerAuthority)))
	mux.Handle("POST /v1/tenants/{tenantRef}/controller-authorities/{authorityAction}", apiHandler.authenticatePlatformManagement(http.HandlerFunc(apiHandler.tenantControllerAuthorityManagementAction)))
	mux.Handle("GET /v1/subjects", apiHandler.authenticateTenantControllerManagement(http.HandlerFunc(apiHandler.listSubjects)))
	mux.Handle("POST /v1/subjects", apiHandler.authenticateTenantControllerManagement(http.HandlerFunc(apiHandler.createSubject)))
	mux.Handle("GET /v1/subjects/{subjectRef}", apiHandler.authenticateTenantControllerManagement(http.HandlerFunc(apiHandler.getSubject)))
	mux.Handle("PUT /v1/subjects/{subjectRef}/quota", apiHandler.authenticateTenantControllerManagement(http.HandlerFunc(apiHandler.updateSubjectQuota)))
	mux.Handle("POST /v1/subjects/{subjectAction}", apiHandler.authenticateTenantControllerManagement(http.HandlerFunc(apiHandler.subjectManagementAction)))
	mux.Handle("GET /v1/application-authorities", apiHandler.authenticateTenantControllerManagement(http.HandlerFunc(apiHandler.listApplicationAuthorities)))
	mux.Handle("POST /v1/application-authorities", apiHandler.authenticateTenantControllerManagement(http.HandlerFunc(apiHandler.createApplicationAuthority)))
	mux.Handle("GET /v1/application-authorities/{authorityID}", apiHandler.authenticateTenantControllerManagement(http.HandlerFunc(apiHandler.getApplicationAuthority)))
	mux.Handle("POST /v1/application-authorities/{authorityAction}", apiHandler.authenticateTenantControllerManagement(http.HandlerFunc(apiHandler.applicationAuthorityManagementAction)))
	mux.Handle("GET /v1/usage", apiHandler.authenticateTenantControllerManagement(http.HandlerFunc(apiHandler.getTenantUsage)))
	mux.Handle("GET /v1/deployment-usage", apiHandler.authenticatePlatformManagement(http.HandlerFunc(apiHandler.getDeploymentUsage)))
	mux.Handle("GET /v1/profiles", apiHandler.authenticate(http.HandlerFunc(apiHandler.listProfiles)))
	mux.Handle("POST /v1/profiles", apiHandler.authenticate(http.HandlerFunc(apiHandler.createProfile)))
	mux.Handle("GET /v1/profiles/{profileName}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getProfile)))
	mux.Handle("POST /v1/profiles/{profileAction}", apiHandler.authenticate(http.HandlerFunc(apiHandler.mutateProfile)))
	mux.Handle("GET /v1/runner-pools", apiHandler.authenticate(http.HandlerFunc(apiHandler.listRunnerPools)))
	mux.Handle("POST /v1/runner-pools", apiHandler.authenticate(http.HandlerFunc(apiHandler.createRunnerPool)))
	mux.Handle("GET /v1/runner-pools/{runnerPoolName}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getRunnerPool)))
	mux.Handle("PATCH /v1/runner-pools/{runnerPoolName}", apiHandler.authenticate(http.HandlerFunc(apiHandler.updateRunnerPool)))
	mux.Handle("GET /v1/runners", apiHandler.authenticate(http.HandlerFunc(apiHandler.listRunners)))
	mux.Handle("GET /v1/runners/{runnerID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getRunner)))
	mux.Handle("GET /v1/timings", apiHandler.authenticate(http.HandlerFunc(apiHandler.getDeploymentTiming)))
	mux.Handle("GET /v1/sandboxes", apiHandler.authenticate(http.HandlerFunc(apiHandler.listSandboxes)))
	mux.Handle("POST /v1/sandboxes", apiHandler.authenticate(http.HandlerFunc(apiHandler.createSandbox)))
	mux.Handle("GET /v1/sandboxes/{sandboxID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getSandbox)))
	mux.Handle("PUT /v1/sandboxes/{sandboxID}/metadata", apiHandler.authenticate(http.HandlerFunc(apiHandler.updateSandboxMetadata)))
	mux.Handle("GET /v1/sandboxes/{sandboxID}/timings", apiHandler.authenticate(http.HandlerFunc(apiHandler.getSandboxTiming)))
	mux.Handle("DELETE /v1/sandboxes/{sandboxID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.deleteSandbox)))
	mux.Handle("POST /v1/sandboxes/{sandboxAction}", apiHandler.authenticate(http.HandlerFunc(apiHandler.mutateSandbox)))
	mux.Handle("POST /v1/sandboxes/{sandboxID}/leases", apiHandler.authenticate(http.HandlerFunc(apiHandler.acquireLease)))
	mux.Handle("POST /v1/sandboxes/{sandboxID}/exec", apiHandler.authenticate(http.HandlerFunc(apiHandler.executeSandboxCommand)))
	mux.Handle("POST /v1/sandboxes/{sandboxID}/exec-streams", apiHandler.authenticate(http.HandlerFunc(apiHandler.createSandboxExecStream)))
	mux.Handle("POST /v1/sandboxes/{sandboxID}/exec-streams/{execSessionAction}", apiHandler.authenticate(http.HandlerFunc(apiHandler.cancelSandboxExecStream)))
	mux.Handle("GET /v1/sandboxes/{sandboxID}/exec-streams/{execSessionID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.connectSandboxExecStream)))
	mux.Handle("POST /v1/sandboxes/{sandboxID}/terminals", apiHandler.authenticate(http.HandlerFunc(apiHandler.createSandboxTerminal)))
	mux.Handle("GET /v1/sandboxes/{sandboxID}/terminals/{terminalSessionID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getOrConnectSandboxTerminal)))
	mux.Handle("DELETE /v1/sandboxes/{sandboxID}/terminals/{terminalSessionID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.cancelSandboxTerminal)))
	mux.Handle("POST /v1/sandboxes/{sandboxID}/port-sessions", apiHandler.authenticate(http.HandlerFunc(apiHandler.createSandboxPortSession)))
	mux.Handle("GET /v1/sandboxes/{sandboxID}/port-sessions/{portSessionID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getSandboxPortSession)))
	mux.Handle("DELETE /v1/sandboxes/{sandboxID}/port-sessions/{portSessionID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.closeSandboxPortSession)))
	mux.HandleFunc("GET /v1/port-tunnels/{portSessionID}", apiHandler.connectPortTunnel)
	mux.Handle("GET /v1/sandboxes/{sandboxID}/files", apiHandler.authenticate(http.HandlerFunc(apiHandler.readSandboxFile)))
	mux.Handle("PUT /v1/sandboxes/{sandboxID}/files", apiHandler.authenticate(http.HandlerFunc(apiHandler.writeSandboxFile)))
	mux.Handle("GET /v1/sandboxes/{sandboxID}/files:stat", apiHandler.authenticate(http.HandlerFunc(apiHandler.statSandboxFile)))
	mux.Handle("GET /v1/sandboxes/{sandboxID}/files:exists", apiHandler.authenticate(http.HandlerFunc(apiHandler.sandboxFileExists)))
	mux.Handle("GET /v1/sandboxes/{sandboxID}/directories", apiHandler.authenticate(http.HandlerFunc(apiHandler.listSandboxDirectory)))
	mux.Handle("POST /v1/sandboxes/{sandboxID}/directories", apiHandler.authenticate(http.HandlerFunc(apiHandler.createSandboxDirectory)))
	mux.Handle("DELETE /v1/sandboxes/{sandboxID}/directories", apiHandler.authenticate(http.HandlerFunc(apiHandler.removeSandboxPath)))
	mux.Handle("GET /v1/sandboxes/{sandboxID}/snapshots", apiHandler.authenticate(http.HandlerFunc(apiHandler.listSandboxSnapshots)))
	mux.Handle("POST /v1/sandboxes/{sandboxID}/snapshots", apiHandler.authenticate(http.HandlerFunc(apiHandler.createSandboxSnapshot)))
	mux.Handle("GET /v1/snapshots/{snapshotID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getSnapshot)))
	mux.Handle("DELETE /v1/snapshots/{snapshotID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.deleteSnapshot)))
	mux.Handle("GET /v1/leases/{leaseID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getLease)))
	mux.Handle("DELETE /v1/leases/{leaseID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.releaseLease)))
	mux.Handle("POST /v1/leases/{leaseAction}", apiHandler.authenticate(http.HandlerFunc(apiHandler.renewLease)))
	mux.Handle("GET /v1/operations/{operationID}", apiHandler.authenticateOperationInspection(http.HandlerFunc(apiHandler.getOperation)))
	mux.Handle("GET /v1/operations/{operationID}/timings", apiHandler.authenticate(http.HandlerFunc(apiHandler.getOperationTiming)))
	return apiHandler.withRequestID(mux), nil
}

func (apiHandler *handler) executeSandboxCommand(writer http.ResponseWriter, request *http.Request) {
	var body contracts.BufferedExecRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	outcome, replayed, err := apiHandler.service.ExecuteSandboxCommand(
		request.Context(), requestPrincipal(request), request.Header.Get("X-Request-ID"),
		request.PathValue("sandboxID"),
		generation, request.Header.Get("SecondBox-Lease-ID"),
		request.Header.Get("Idempotency-Key"), body,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusOK, outcome)
}

func (apiHandler *handler) readSandboxFile(writer http.ResponseWriter, request *http.Request) {
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	content, checksum, err := apiHandler.service.ReadSandboxFile(
		request.Context(), requestPrincipal(request), request.Header.Get("X-Request-ID"),
		request.PathValue("sandboxID"),
		generation, request.Header.Get("SecondBox-Lease-ID"), request.URL.Query().Get("path"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	digest, err := protocolChecksumToHTTPDigest(checksum)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.Itoa(len(content)))
	writer.Header().Set("Digest", digest)
	writer.WriteHeader(http.StatusOK)
	apiHandler.writeResponseBytes(writer, request, "binary File download", content)
}

func (apiHandler *handler) writeSandboxFile(writer http.ResponseWriter, request *http.Request) {
	if err := requireBinaryContentType(request); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	checksum, err := parseHTTPDigest(request.Header.Get("Digest"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	content, err := io.ReadAll(io.LimitReader(request.Body, apiHandler.maximumDataPlaneBodyBytes+1))
	if err != nil {
		apiHandler.writeError(writer, request, fmt.Errorf("SecondBox File request read failed: %w", err))
		return
	}
	if int64(len(content)) > apiHandler.maximumDataPlaneBodyBytes {
		apiHandler.writeError(writer, request, runnercontrol.ErrDataPlaneSessionLimit)
		return
	}
	actual := sha256.Sum256(content)
	if hex.EncodeToString(actual[:]) != checksum {
		apiHandler.writeError(writer, request, runnercontrol.ErrFileChecksum)
		return
	}
	result, replayed, err := apiHandler.service.WriteSandboxFile(
		request.Context(), requestPrincipal(request), request.Header.Get("X-Request-ID"),
		request.PathValue("sandboxID"),
		generation, request.Header.Get("SecondBox-Lease-ID"),
		request.Header.Get("Idempotency-Key"), request.URL.Query().Get("path"), content, checksum,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusOK, result)
}

func requireBinaryContentType(request *http.Request) error {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/octet-stream" {
		return requestValidationError(errors.New("SecondBox File upload Content-Type must be application/octet-stream"))
	}
	return nil
}

func (apiHandler *handler) statSandboxFile(writer http.ResponseWriter, request *http.Request) {
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	result, err := apiHandler.service.StatSandboxFile(
		request.Context(), requestPrincipal(request), request.Header.Get("X-Request-ID"),
		request.PathValue("sandboxID"),
		generation, request.Header.Get("SecondBox-Lease-ID"), request.URL.Query().Get("path"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, result)
}

func (apiHandler *handler) sandboxFileExists(writer http.ResponseWriter, request *http.Request) {
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	result, err := apiHandler.service.SandboxFileExists(
		request.Context(), requestPrincipal(request), request.Header.Get("X-Request-ID"),
		request.PathValue("sandboxID"),
		generation, request.Header.Get("SecondBox-Lease-ID"), request.URL.Query().Get("path"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, result)
}

func (apiHandler *handler) listSandboxDirectory(writer http.ResponseWriter, request *http.Request) {
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	result, err := apiHandler.service.ListSandboxDirectory(
		request.Context(), requestPrincipal(request), request.Header.Get("X-Request-ID"),
		request.PathValue("sandboxID"),
		generation, request.Header.Get("SecondBox-Lease-ID"), request.URL.Query().Get("path"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, result)
}

func (apiHandler *handler) createSandboxDirectory(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CreateDirectoryRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	replayed, err := apiHandler.service.CreateSandboxDirectory(
		request.Context(), requestPrincipal(request), request.Header.Get("X-Request-ID"),
		request.PathValue("sandboxID"),
		generation, request.Header.Get("SecondBox-Lease-ID"),
		request.Header.Get("Idempotency-Key"), body,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	writer.WriteHeader(http.StatusNoContent)
}

func (apiHandler *handler) removeSandboxPath(writer http.ResponseWriter, request *http.Request) {
	var body contracts.RemovePathRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	replayed, err := apiHandler.service.RemoveSandboxPath(
		request.Context(), requestPrincipal(request), request.Header.Get("X-Request-ID"),
		request.PathValue("sandboxID"),
		generation, request.Header.Get("SecondBox-Lease-ID"),
		request.Header.Get("Idempotency-Key"), body,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	writer.WriteHeader(http.StatusNoContent)
}

func (apiHandler *handler) health(writer http.ResponseWriter, request *http.Request) {
	apiHandler.writeJSON(writer, request, http.StatusOK, map[string]string{"status": "healthy"})
}

func (apiHandler *handler) ready(writer http.ResponseWriter, request *http.Request) {
	if err := apiHandler.service.Ready(request.Context()); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, map[string]string{"status": "ready"})
}

func (apiHandler *handler) metrics(writer http.ResponseWriter, request *http.Request) {
	snapshot, err := apiHandler.service.Metrics(request.Context())
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4")
	writer.WriteHeader(http.StatusOK)
	for _, family := range []struct {
		name   string
		values map[string]int64
	}{
		{name: "secondbox_sandboxes", values: snapshot.SandboxStates},
		{name: "secondbox_operations", values: snapshot.OperationStates},
	} {
		if err := writeMetricFamily(writer, family.name, family.values); err != nil {
			apiHandler.logResponseAbort(request, "metrics response", err)
			return
		}
	}
	if _, err := fmt.Fprintf(
		writer,
		"# TYPE secondbox_live_data_plane_dropped_route_not_found_frames_total counter\n"+
			"secondbox_live_data_plane_dropped_route_not_found_frames_total %d\n",
		snapshot.LiveDataPlaneDroppedRouteNotFoundFrames,
	); err != nil {
		apiHandler.logResponseAbort(request, "live data-plane metrics response", err)
		return
	}
	if err := writeHTTPDurationMetrics(writer, apiHandler.timings.HTTPSnapshot()); err != nil {
		apiHandler.logResponseAbort(request, "HTTP duration metrics response", err)
		return
	}
	if err := writeOperationDurationMetrics(writer, snapshot.OperationDurations); err != nil {
		apiHandler.logResponseAbort(request, "Operation duration metrics response", err)
		return
	}
	if err := writeDatabaseTimingMetrics(writer, snapshot); err != nil {
		apiHandler.logResponseAbort(request, "database timing metrics response", err)
		return
	}
}

func (apiHandler *handler) createProfile(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CreateProfileRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	profile, replayed, err := apiHandler.service.CreateProfileIdempotent(
		request.Context(), requestPrincipal(request), request.Header.Get("Idempotency-Key"), body,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	setRevisionETag(writer, profile.Revision)
	apiHandler.writeJSON(writer, request, http.StatusCreated, profile)
}

func (apiHandler *handler) listProfiles(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryLimit(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	page, err := apiHandler.service.ListProfiles(
		request.Context(), requestPrincipal(request), limit, request.URL.Query().Get("cursor"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, page)
}

func (apiHandler *handler) getProfile(writer http.ResponseWriter, request *http.Request) {
	profile, err := apiHandler.service.GetProfile(request.Context(), requestPrincipal(request), request.PathValue("profileName"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, profile.Revision)
	apiHandler.writeJSON(writer, request, http.StatusOK, profile)
}

func (apiHandler *handler) mutateProfile(writer http.ResponseWriter, request *http.Request) {
	name, action, ok := splitAction(request.PathValue("profileAction"), "revise", "disable")
	if !ok {
		apiHandler.writeError(writer, request, ports.ErrProfileNotFound)
		return
	}
	switch action {
	case "revise":
		var body contracts.ReviseProfileRequest
		if err := decodeStrictJSON(request, &body); err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		expectedRevision, err := parseIfMatch(request)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		profile, replayed, err := apiHandler.service.ReviseProfileAtRevisionIdempotent(
			request.Context(), requestPrincipal(request), name,
			request.Header.Get("Idempotency-Key"), body, expectedRevision,
		)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
		setRevisionETag(writer, profile.Revision)
		apiHandler.writeJSON(writer, request, http.StatusOK, profile)
	case "disable":
		expectedRevision, err := parseIfMatch(request)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		profile, replayed, err := apiHandler.service.DisableProfileAtRevisionIdempotent(
			request.Context(), requestPrincipal(request), name,
			request.Header.Get("Idempotency-Key"), expectedRevision,
		)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
		setRevisionETag(writer, profile.Revision)
		apiHandler.writeJSON(writer, request, http.StatusOK, profile)
	default:
		apiHandler.writeError(writer, request, ports.ErrProfileNotFound)
	}
}

func (apiHandler *handler) createSandbox(writer http.ResponseWriter, request *http.Request) {
	var body contracts.CreateSandboxRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	if err := authorizeApplicationProfile(request, body.Profile); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	operation, replayed, err := apiHandler.service.CreateSandboxOperation(
		request.Context(), requestPrincipal(request), request.Header.Get("Idempotency-Key"), body,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(!replayed))
	apiHandler.writeJSON(writer, request, http.StatusAccepted, operation)
}

func (apiHandler *handler) listSandboxes(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryLimit(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	metadata, err := queryMetadataFilter(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	page, err := apiHandler.service.ListSandboxes(
		request.Context(), requestPrincipal(request), limit,
		request.URL.Query().Get("cursor"), metadata,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, page)
}

func (apiHandler *handler) getSandbox(writer http.ResponseWriter, request *http.Request) {
	sandbox, err := apiHandler.service.GetSandbox(request.Context(), requestPrincipal(request), request.PathValue("sandboxID"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	setRevisionETag(writer, sandbox.Revision)
	apiHandler.writeJSON(writer, request, http.StatusOK, sandbox)
}

func (apiHandler *handler) mutateSandbox(writer http.ResponseWriter, request *http.Request) {
	sandboxID, action, ok := splitAction(
		request.PathValue("sandboxAction"),
		"start", "drain", "stop", "relocate", "restore", "wait", "inspect", "ping", "touch",
	)
	if !ok {
		apiHandler.writeError(writer, request, ports.ErrSandboxNotFound)
		return
	}
	switch action {
	case "start", "drain", "stop":
		if err := requireEmptyBody(request); err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		apiHandler.mutateSandboxLifecycle(writer, request, sandboxID, action, nil)
	case "relocate":
		var body contracts.RelocateSandboxRequest
		if err := decodeStrictJSON(request, &body); err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		expectedRevision, err := parseIfMatch(request)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		operation, replayed, err := apiHandler.service.RelocateSandbox(
			request.Context(), requestPrincipal(request), sandboxID,
			request.Header.Get("Idempotency-Key"), expectedRevision, body,
		)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
		apiHandler.writeJSON(writer, request, http.StatusAccepted, operation)
	case "restore":
		var body contracts.RestoreSnapshotRequest
		if err := decodeStrictJSON(request, &body); err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		expectedRevision, err := parseIfMatch(request)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		operation, replayed, err := apiHandler.service.RestoreSandboxSnapshot(
			request.Context(), requestPrincipal(request), sandboxID,
			request.Header.Get("Idempotency-Key"), expectedRevision, body,
		)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
		apiHandler.writeJSON(writer, request, http.StatusAccepted, operation)
	case "wait":
		var body contracts.WaitSandboxRequest
		if err := decodeStrictJSON(request, &body); err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		sandbox, err := apiHandler.service.WaitSandbox(
			request.Context(), requestPrincipal(request), sandboxID, body,
		)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		setRevisionETag(writer, sandbox.Revision)
		apiHandler.writeJSON(writer, request, http.StatusOK, sandbox)
	case "inspect":
		if err := requireEmptyBody(request); err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		generation, err := parseGeneration(request)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		inspection, err := apiHandler.service.InspectSandbox(
			request.Context(), requestPrincipal(request), sandboxID, generation,
		)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		apiHandler.writeJSON(writer, request, http.StatusOK, inspection)
	case "ping":
		if err := requireEmptyBody(request); err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		generation, err := parseGeneration(request)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		result, err := apiHandler.service.PingSandbox(
			request.Context(), requestPrincipal(request), sandboxID, generation,
		)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		apiHandler.writeJSON(writer, request, http.StatusOK, result)
	case "touch":
		if err := requireEmptyBody(request); err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		generation, err := parseGeneration(request)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		result, err := apiHandler.service.TouchSandbox(
			request.Context(), requestPrincipal(request), sandboxID, generation,
			request.Header.Get("SecondBox-Lease-ID"), request.Header.Get("Idempotency-Key"),
		)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		apiHandler.writeJSON(writer, request, http.StatusOK, result)
	default:
		apiHandler.writeError(writer, request, ports.ErrSandboxNotFound)
	}
}

func (apiHandler *handler) mutateSandboxLifecycle(
	writer http.ResponseWriter,
	request *http.Request,
	sandboxID string,
	action string,
	metadata map[string]string,
) {
	expectedRevision, err := parseIfMatch(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	operation, replayed, err := apiHandler.service.MutateSandbox(
		request.Context(), requestPrincipal(request), sandboxID, action,
		request.Header.Get("Idempotency-Key"), expectedRevision, metadata,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	apiHandler.writeJSON(writer, request, http.StatusAccepted, operation)
}

func (apiHandler *handler) deleteSandbox(writer http.ResponseWriter, request *http.Request) {
	if err := requireEmptyBody(request); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.mutateSandboxLifecycle(
		writer, request, request.PathValue("sandboxID"), "delete", nil,
	)
}

func (apiHandler *handler) acquireLease(writer http.ResponseWriter, request *http.Request) {
	var body contracts.AcquireLeaseRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	lease, err := apiHandler.service.AcquireSandboxLease(
		request.Context(), requestPrincipal(request), request.PathValue("sandboxID"),
		generation, request.Header.Get("Idempotency-Key"), body.DurationSeconds,
		body.ReplaceActive,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusCreated, lease)
}

func (apiHandler *handler) getLease(writer http.ResponseWriter, request *http.Request) {
	lease, err := apiHandler.service.GetSandboxLease(
		request.Context(), requestPrincipal(request), request.PathValue("leaseID"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, lease)
}

func (apiHandler *handler) renewLease(writer http.ResponseWriter, request *http.Request) {
	leaseID, action, ok := splitAction(request.PathValue("leaseAction"), "renew")
	if !ok || action != "renew" {
		apiHandler.writeError(writer, request, ports.ErrLeaseNotFound)
		return
	}
	var body contracts.RenewLeaseRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	lease, err := apiHandler.service.RenewSandboxLease(
		request.Context(), requestPrincipal(request), leaseID,
		request.Header.Get("Idempotency-Key"), body.DurationSeconds,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, lease)
}

func (apiHandler *handler) releaseLease(writer http.ResponseWriter, request *http.Request) {
	if err := requireEmptyBody(request); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	lease, err := apiHandler.service.ReleaseSandboxLease(
		request.Context(), requestPrincipal(request), request.PathValue("leaseID"),
		request.Header.Get("Idempotency-Key"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, lease)
}

func (apiHandler *handler) getOperation(writer http.ResponseWriter, request *http.Request) {
	principal := requestPrincipal(request)
	operation, err := apiHandler.service.GetOperation(request.Context(), principal, request.PathValue("operationID"))
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, operation)
}

func (apiHandler *handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization := request.Header.Get("Authorization")
		credential, ok := strings.CutPrefix(authorization, "Bearer ")
		if !ok || credential == "" {
			apiHandler.writeError(writer, request, ports.ErrAuthenticationFailed)
			return
		}
		presentedHash := sha256.Sum256([]byte(credential))
		if subtle.ConstantTimeCompare(presentedHash[:], apiHandler.platformTokenHash[:]) == 1 {
			tenantRef := request.Header.Get("X-SecondBox-Tenant-Ref")
			subjectRef := request.Header.Get("X-SecondBox-Subject-Ref")
			if !ownershipRefPattern.MatchString(tenantRef) ||
				!ownershipRefPattern.MatchString(subjectRef) {
				apiHandler.writeError(
					writer,
					request,
					requestValidationError(errors.New("SecondBox tenant and subject references must contain 1 to 128 visible ASCII characters")),
				)
				return
			}
			principal := contracts.Principal{
				Kind: "platform", ID: subjectRef,
				TenantRef: tenantRef, SubjectRef: subjectRef,
			}
			next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal)))
			return
		}
		if isTenantControllerBearerToken(credential) {
			apiHandler.writeError(writer, request, ports.ErrAuthenticationFailed)
			return
		}
		if apiHandler.persistedAuthorities == nil || !isApplicationBearerToken(credential) {
			apiHandler.writeError(writer, request, ports.ErrAuthenticationFailed)
			return
		}
		persisted, err := apiHandler.persistedAuthorities.AuthenticateApplicationAuthority(
			request.Context(), credential, time.Now().UTC(),
		)
		if err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		authority := resolvedPersistedApplicationAuthority(persisted)
		if err := authorizeApplicationRequest(authority, request); err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		requestContext := context.WithValue(
			request.Context(),
			applicationAuthorityContextKey{},
			authority,
		)
		requestContext = context.WithValue(
			requestContext,
			principalContextKey{},
			applicationPrincipal(authority),
		)
		next.ServeHTTP(writer, request.WithContext(requestContext))
	})
}

func (apiHandler *handler) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		startedAt := time.Now()
		requestID := request.Header.Get("X-Request-ID")
		if !correlationIDPattern.MatchString(requestID) {
			requestID = service.NewOpaqueID("req")
		}
		statusWriter := &statusResponseWriter{ResponseWriter: writer}
		statusWriter.Header().Set("X-Request-ID", requestID)
		request.Header.Set("X-Request-ID", requestID)
		request = request.WithContext(service.ContextWithRequestID(request.Context(), requestID))
		next.ServeHTTP(statusWriter, request)
		status := statusWriter.status
		if status == 0 {
			status = http.StatusOK
		}
		completedAt := time.Now()
		duration := completedAt.Sub(startedAt)
		apiHandler.timings.ObserveHTTPAt(
			request.Pattern, httpStatusClass(status), duration, completedAt,
		)
		apiHandler.logger.InfoContext(
			request.Context(),
			"SecondBox HTTP request completed",
			"request_id", requestID,
			"method", request.Method,
			"route", request.Pattern,
			"status", status,
			"duration_ms", duration.Milliseconds(),
		)
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (writer *statusResponseWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *statusResponseWriter) Write(content []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	return writer.ResponseWriter.Write(content)
}

func (writer *statusResponseWriter) Flush() {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (writer *statusResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := writer.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("SecondBox HTTP response does not support connection hijacking")
	}
	connection, buffer, err := hijacker.Hijack()
	if err == nil && writer.status == 0 {
		writer.status = http.StatusSwitchingProtocols
	}
	return connection, buffer, err
}

func (writer *statusResponseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := writer.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func (writer *statusResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func httpStatusClass(status int) string {
	switch status / 100 {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	case 5:
		return "5xx"
	default:
		return "other"
	}
}

func (apiHandler *handler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	status, code, title, retryable := classifyError(err)
	if status >= 500 {
		apiHandler.logger.ErrorContext(
			request.Context(),
			"SecondBox HTTP request failed",
			"request_id", writer.Header().Get("X-Request-ID"),
			"code", code,
			"error", err,
		)
	}
	writer.Header().Set("Content-Type", "application/problem+json")
	problem := contracts.Problem{
		Type: "https://secondbox.dev/problems/" + code, Title: title, Status: status,
		Code: code, RequestID: writer.Header().Get("X-Request-ID"), Retryable: retryable,
	}
	if errors.Is(err, ports.ErrHomeRunnerUnavailable) {
		retryAfterMilliseconds := int64(time.Second / time.Millisecond)
		problem.RetryAfterMilliseconds = &retryAfterMilliseconds
		writer.Header().Set("Retry-After", "1")
	}
	apiHandler.writeJSON(writer, request, status, problem)
}

func classifyError(err error) (int, string, string, bool) {
	switch {
	case errors.Is(err, ports.ErrAuthenticationFailed):
		return http.StatusUnauthorized, "authentication_failed", "Authentication failed", false
	case errors.Is(err, ports.ErrPortTokenInvalid):
		return http.StatusUnauthorized, "authentication_failed", "Port tunnel token is invalid", false
	case errors.Is(err, ports.ErrAuthorizationDenied):
		return http.StatusForbidden, "authorization_failed", "Authorization failed", false
	case errors.Is(err, ports.ErrManagementUnavailable):
		return http.StatusServiceUnavailable, "management_unavailable", "Management API is unavailable", false
	case errors.Is(err, ports.ErrInvalidRequest):
		return http.StatusBadRequest, "invalid_request", "Request is invalid", false
	case errors.Is(err, ports.ErrManagementNotFound):
		return http.StatusNotFound, "not_found", "Resource not found", false
	case errors.Is(err, ports.ErrManagementConflict):
		return http.StatusConflict, "state_conflict", "Management resource conflicts with current state", false
	case errors.Is(err, ports.ErrInvalidLifecycleTransition):
		return http.StatusConflict, "invalid_lifecycle_transition", "Management lifecycle transition is invalid", false
	case errors.Is(err, ports.ErrResourceExpired):
		return http.StatusConflict, "resource_expired", "Management resource is expired", false
	case errors.Is(err, ports.ErrTenantSuspended):
		return http.StatusConflict, "tenant_suspended", "Tenant is suspended", false
	case errors.Is(err, ports.ErrTenantEgressContextRequired):
		return http.StatusConflict, "tenant_egress_context_required", "Profile requires a Tenant egress context", false
	case errors.Is(err, ports.ErrGrantEscalationDenied):
		return http.StatusForbidden, "grant_escalation_denied", "Requested grant exceeds the Tenant ceiling", false
	case errors.Is(err, pagination.ErrInvalidListCursor):
		return http.StatusBadRequest, "invalid_request", "List page cursor is invalid", false
	case errors.Is(err, ports.ErrPortPolicyDenied):
		return http.StatusForbidden, "authorization_failed", "Exposed port is not permitted", false
	case errors.Is(err, runnercontrol.ErrFilePermission):
		return http.StatusForbidden, "file_permission_denied", "File operation permission denied", false
	case errors.Is(err, ports.ErrProfileNotFound),
		errors.Is(err, ports.ErrRunnerPoolNotFound), errors.Is(err, ports.ErrRunnerNotFound),
		errors.Is(err, ports.ErrSandboxNotFound), errors.Is(err, ports.ErrLeaseNotFound),
		errors.Is(err, ports.ErrSnapshotNotFound), errors.Is(err, ports.ErrPortSessionNotFound),
		errors.Is(err, runnercontrol.ErrDataPlaneNotFound):
		return http.StatusNotFound, "not_found", "Resource not found", false
	case errors.Is(err, ports.ErrProfileDisabled):
		return http.StatusConflict, "profile_unavailable", "Profile is unavailable", false
	case errors.Is(err, ports.ErrSnapshotUnavailable):
		return http.StatusConflict, "state_conflict", "Snapshot requires stopped committed disk state", false
	case errors.Is(err, ports.ErrWorkspaceMutation):
		return http.StatusConflict, "workspace_mutation_conflict", "Workspace has a conflicting mutation", false
	case errors.Is(err, ports.ErrSandboxNotStopped):
		return http.StatusConflict, "sandbox_not_stopped", "Workspace relocation requires a stopped Sandbox", false
	case errors.Is(err, ports.ErrRelocationSnapshotsPresent):
		return http.StatusConflict, "workspace_relocation_snapshots_present", "Workspace relocation requires all Snapshots to be deleted", false
	case errors.Is(err, ports.ErrRelocationTargetUnavailable):
		return http.StatusConflict, "workspace_relocation_target_unavailable", "Workspace relocation target is unavailable or incompatible", true
	case errors.Is(err, ports.ErrHomeRunnerUnavailable):
		return http.StatusServiceUnavailable, "home_runner_unavailable", "Sandbox home runner is unavailable", true
	case errors.Is(err, ports.ErrStartupModeUnsupported):
		return http.StatusConflict, "startup_mode_unsupported", "Profile startup mode is not supported by its RunnerPool", false
	case errors.Is(err, ports.ErrRunnerPoolUnavailable):
		return http.StatusConflict, "execution_node_unavailable", "Compatible execution node unavailable", true
	case errors.Is(err, ports.ErrRunnerPoolExists):
		return http.StatusConflict, "state_conflict", "RunnerPool already exists", false
	case errors.Is(err, ports.ErrSandboxNameConflict):
		return http.StatusConflict, "state_conflict", "Sandbox name is already in use", false
	case errors.Is(err, ports.ErrIdempotencyConflict):
		return http.StatusConflict, "idempotency_conflict", "Idempotency key payload conflict", false
	case errors.Is(err, ports.ErrCredentialResponseUnavailable):
		return http.StatusConflict, "credential_response_unavailable", "Credential response is unavailable", false
	case errors.Is(err, ports.ErrPortTokenConsumed):
		return http.StatusConflict, "state_conflict", "Port tunnel token was already consumed", false
	case errors.Is(err, runnercontrol.ErrFileChecksum):
		return http.StatusConflict, "checksum_mismatch", "Content checksum mismatch", false
	case errors.Is(err, runnercontrol.ErrDataPlaneDeadline):
		return http.StatusConflict, "operation_deadline_exceeded", "Operation deadline exceeded", false
	case errors.Is(err, runnercontrol.ErrDataPlaneSessionLimit), errors.Is(err, runnercontrol.ErrDataPlaneFrameLimit):
		return http.StatusRequestEntityTooLarge, "limit_exceeded", "Configured byte limit exceeded", false
	case errors.Is(err, ports.ErrQuotaExceeded):
		return http.StatusTooManyRequests, "quota_exceeded", "Quota exceeded", false
	case errors.Is(err, ports.ErrPortBackpressure):
		return http.StatusTooManyRequests, "backpressure", "Port tunnel has no available byte credit", true
	case errors.Is(err, ports.ErrRevisionConflict):
		return http.StatusPreconditionFailed, "precondition_failed", "Resource revision changed", false
	case errors.Is(err, ports.ErrGenerationFenced):
		return http.StatusConflict, "generation_fenced", "Sandbox generation is fenced", false
	case errors.Is(err, ports.ErrLeaseAlreadyActive):
		return http.StatusConflict, "state_conflict", "Sandbox already has an active Lease", false
	case errors.Is(err, ports.ErrLeaseInactive):
		return http.StatusConflict, "lease_fenced", "Lease is inactive or missing", false
	case errors.Is(err, runnercontrol.ErrTerminalAttached):
		return http.StatusConflict, "state_conflict", "Terminal already has an active attachment", false
	case errors.Is(err, runnercontrol.ErrTerminalDetached):
		return http.StatusConflict, "state_conflict", "Terminal attachment is inactive", false
	case errors.Is(err, runnercontrol.ErrTerminalReplayEvicted):
		return http.StatusConflict, "terminal_replay_evicted", "Terminal replay sequence is no longer available", false
	case errors.Is(err, ports.ErrWaitExpired):
		return http.StatusRequestTimeout, "wait_expired", "Sandbox wait deadline expired", false
	case errors.Is(err, ports.ErrLifecycleUnavailable),
		errors.Is(err, runnercontrol.ErrLiveDataPlaneUnavailable):
		return http.StatusConflict, "execution_node_unavailable", "Execution node lifecycle unavailable", true
	default:
		return http.StatusInternalServerError, "internal_error", "SecondBox request failed", true
	}
}

func requestValidationError(err error) error {
	return errors.Join(ports.ErrInvalidRequest, err)
}

func parseHTTPDigest(value string) (string, error) {
	const prefix = "sha-256=:"
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, ":") {
		return "", requestValidationError(errors.New("SecondBox Digest must contain a SHA-256 content digest"))
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(value, prefix), ":")
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size || base64.StdEncoding.EncodeToString(decoded) != encoded {
		return "", requestValidationError(errors.New("SecondBox Digest must contain canonical SHA-256 base64"))
	}
	return hex.EncodeToString(decoded), nil
}

func protocolChecksumToHTTPDigest(checksum string) (string, error) {
	hexadecimal, ok := strings.CutPrefix(checksum, "sha256:")
	if !ok {
		return "", errors.New("SecondBox File checksum evidence is invalid")
	}
	decoded, err := hex.DecodeString(hexadecimal)
	if err != nil || len(decoded) != sha256.Size {
		return "", errors.New("SecondBox File checksum evidence is invalid")
	}
	return "sha-256=:" + base64.StdEncoding.EncodeToString(decoded) + ":", nil
}

func decodeStrictJSON(request *http.Request, destination any) error {
	if request.Body == nil {
		return requestValidationError(errors.New("SecondBox JSON request body is required"))
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, (1<<20)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return requestValidationError(fmt.Errorf("SecondBox JSON request decoding failed: %w", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return requestValidationError(errors.New("SecondBox JSON request must contain exactly one object"))
	}
	return nil
}

func requireEmptyBody(request *http.Request) error {
	if request.Body == nil || request.Body == http.NoBody {
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, 2))
	var value any
	if errors.Is(decoder.Decode(&value), io.EOF) {
		return nil
	}
	return requestValidationError(errors.New("SecondBox request body must be empty"))
}

func (apiHandler *handler) writeJSON(
	writer http.ResponseWriter,
	request *http.Request,
	status int,
	value any,
) {
	if writer.Header().Get("Content-Type") == "" {
		writer.Header().Set("Content-Type", "application/json")
	}
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		apiHandler.logResponseAbort(request, "JSON response", err)
	}
}

func writeMetricFamily(writer io.Writer, name string, values map[string]int64) error {
	states := make([]string, 0, len(values))
	for state := range values {
		states = append(states, state)
	}
	sort.Strings(states)
	for _, state := range states {
		if _, err := fmt.Fprintf(writer, "%s{state=%q} %d\n", name, state, values[state]); err != nil {
			return fmt.Errorf("SecondBox metrics response write failed: %w", err)
		}
	}
	return nil
}

func writeHTTPDurationMetrics(
	writer io.Writer,
	series []observability.HTTPDuration,
) error {
	if _, err := fmt.Fprintln(writer, "# TYPE secondbox_http_request_duration_seconds histogram"); err != nil {
		return fmt.Errorf("SecondBox HTTP duration metric type write failed: %w", err)
	}
	for _, metric := range series {
		labels := fmt.Sprintf(
			"route=%q,status_class=%q",
			metric.Route, metric.StatusClass,
		)
		if err := writeDurationHistogram(
			writer, "secondbox_http_request_duration_seconds", labels, metric.Histogram,
		); err != nil {
			return err
		}
	}
	return nil
}

func writeOperationDurationMetrics(
	writer io.Writer,
	series []contracts.OperationDurationMetric,
) error {
	if _, err := fmt.Fprintln(writer, "# TYPE secondbox_operation_duration_seconds histogram"); err != nil {
		return fmt.Errorf("SecondBox Operation duration metric type write failed: %w", err)
	}
	for _, metric := range series {
		labels := fmt.Sprintf(
			"kind=%q,terminal_state=%q",
			metric.Kind, metric.TerminalState,
		)
		histogram := observability.DurationHistogram{
			Count: metric.Histogram.Count, SumSeconds: metric.Histogram.SumSeconds,
			BucketCounts: metric.Histogram.BucketCounts,
		}
		if err := writeDurationHistogram(
			writer, "secondbox_operation_duration_seconds", labels, histogram,
		); err != nil {
			return err
		}
	}
	return nil
}

func writeDatabaseTimingMetrics(
	writer io.Writer,
	snapshot contracts.MetricsSnapshot,
) error {
	if _, err := fmt.Fprintln(
		writer, "# TYPE secondbox_sandbox_start_duration_seconds histogram",
	); err != nil {
		return fmt.Errorf("SecondBox Sandbox start duration metric type write failed: %w", err)
	}
	if err := writeDurationHistogram(
		writer,
		"secondbox_sandbox_start_duration_seconds",
		"",
		publicMetricHistogram(snapshot.BootDuration),
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(
		writer, "# TYPE secondbox_sandbox_start_stage_duration_seconds histogram",
	); err != nil {
		return fmt.Errorf("SecondBox Sandbox start stage duration metric type write failed: %w", err)
	}
	for _, metric := range snapshot.BootStageDurations {
		if err := writeDurationHistogram(
			writer,
			"secondbox_sandbox_start_stage_duration_seconds",
			fmt.Sprintf("stage=%q", metric.Stage),
			publicMetricHistogram(metric.Histogram),
		); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(
		writer, "# TYPE secondbox_exec_duration_seconds histogram",
	); err != nil {
		return fmt.Errorf("SecondBox Exec duration metric type write failed: %w", err)
	}
	for _, metric := range snapshot.ExecDurations {
		if err := writeDurationHistogram(
			writer,
			"secondbox_exec_duration_seconds",
			fmt.Sprintf("mode=%q,outcome=%q", metric.Mode, metric.Outcome),
			publicMetricHistogram(metric.Histogram),
		); err != nil {
			return err
		}
	}
	return nil
}

func publicMetricHistogram(
	histogram contracts.MetricDurationHistogram,
) observability.DurationHistogram {
	return observability.DurationHistogram{
		Count: histogram.Count, SumSeconds: histogram.SumSeconds,
		BucketCounts: histogram.BucketCounts,
	}
}

func writeDurationHistogram(
	writer io.Writer,
	name string,
	labels string,
	histogram observability.DurationHistogram,
) error {
	if len(histogram.BucketCounts) != len(observability.DurationBucketsSeconds) {
		return errors.New("SecondBox metrics duration histogram has invalid bucket count")
	}
	for index, upperBound := range observability.DurationBucketsSeconds {
		bucketLabels := fmt.Sprintf("le=%q", strconv.FormatFloat(upperBound, 'g', -1, 64))
		if labels != "" {
			bucketLabels = labels + "," + bucketLabels
		}
		if _, err := fmt.Fprintf(
			writer, "%s_bucket{%s} %d\n",
			name, bucketLabels,
			histogram.BucketCounts[index],
		); err != nil {
			return fmt.Errorf("SecondBox metrics duration bucket write failed: %w", err)
		}
	}
	infiniteLabels := `le="+Inf"`
	if labels != "" {
		infiniteLabels = labels + "," + infiniteLabels
	}
	if _, err := fmt.Fprintf(
		writer, "%s_bucket{%s} %d\n", name, infiniteLabels, histogram.Count,
	); err != nil {
		return fmt.Errorf("SecondBox metrics duration infinite bucket write failed: %w", err)
	}
	metricLabels := ""
	if labels != "" {
		metricLabels = "{" + labels + "}"
	}
	if _, err := fmt.Fprintf(
		writer, "%s_sum%s %s\n", name, metricLabels,
		strconv.FormatFloat(histogram.SumSeconds, 'g', -1, 64),
	); err != nil {
		return fmt.Errorf("SecondBox metrics duration sum write failed: %w", err)
	}
	if _, err := fmt.Fprintf(
		writer, "%s_count%s %d\n", name, metricLabels, histogram.Count,
	); err != nil {
		return fmt.Errorf("SecondBox metrics duration count write failed: %w", err)
	}
	return nil
}

func (apiHandler *handler) logResponseAbort(
	request *http.Request,
	responseKind string,
	err error,
) {
	apiHandler.logger.ErrorContext(
		request.Context(),
		"SecondBox HTTP response write aborted",
		"response_kind", responseKind,
		"method", request.Method,
		"path", request.URL.Path,
		"request_id", request.Header.Get("X-Request-ID"),
		"error", err,
	)
}

func (apiHandler *handler) writeResponseBytes(
	writer io.Writer,
	request *http.Request,
	responseKind string,
	content []byte,
) {
	if _, err := writer.Write(content); err != nil {
		apiHandler.logResponseAbort(request, responseKind, err)
	}
}

func requestPrincipal(request *http.Request) contracts.Principal {
	principal, ok := request.Context().Value(principalContextKey{}).(contracts.Principal)
	if !ok {
		panic("SecondBox authenticated request has no Principal")
	}
	return principal
}

func queryLimit(request *http.Request) (int, error) {
	raw := request.URL.Query().Get("limit")
	if raw == "" {
		return 100, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 200 {
		return 0, requestValidationError(errors.New("SecondBox list limit must be an integer between 1 and 200"))
	}
	return value, nil
}

// maximumMetadataFilterEntries bounds one containment filter.
const maximumMetadataFilterEntries = 8

// queryMetadataFilter parses the repeatable metadata=name=value containment
// filter. A value may itself contain '=', so only the first one separates.
func queryMetadataFilter(request *http.Request) (map[string]string, error) {
	values := request.URL.Query()["metadata"]
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) > maximumMetadataFilterEntries {
		return nil, requestValidationError(fmt.Errorf(
			"SecondBox list metadata filter must not exceed %d entries",
			maximumMetadataFilterEntries,
		))
	}
	filter := make(map[string]string, len(values))
	for _, entry := range values {
		name, value, found := strings.Cut(entry, "=")
		if !found || strings.TrimSpace(name) == "" || len(name) > 128 || len(value) > 1024 {
			return nil, requestValidationError(errors.New(
				"SecondBox list metadata filter must be name=value within the Metadata bounds",
			))
		}
		if _, duplicate := filter[name]; duplicate {
			return nil, requestValidationError(errors.New("SecondBox list metadata filter must not repeat a name"))
		}
		filter[name] = value
	}
	return filter, nil
}

func setRevisionETag(writer http.ResponseWriter, revision int64) {
	writer.Header().Set("ETag", fmt.Sprintf(`"revision-%d"`, revision))
}

func parseIfMatch(request *http.Request) (int64, error) {
	value := strings.Trim(request.Header.Get("If-Match"), `"`)
	value = strings.TrimPrefix(value, "revision-")
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 1 {
		return 0, requestValidationError(errors.New("SecondBox If-Match must contain a positive revision ETag"))
	}
	return revision, nil
}

func parseGeneration(request *http.Request) (int64, error) {
	generation, err := strconv.ParseInt(request.Header.Get("SecondBox-Generation"), 10, 64)
	if err != nil || generation < 1 {
		return 0, requestValidationError(errors.New("SecondBox-Generation must contain a positive integer"))
	}
	return generation, nil
}

func splitAction(value string, actions ...string) (string, string, bool) {
	for _, action := range actions {
		suffix := ":" + action
		if resource, ok := strings.CutSuffix(value, suffix); ok && resource != "" {
			return resource, action, true
		}
	}
	return "", "", false
}
