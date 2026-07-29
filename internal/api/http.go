// Package api exposes the versioned standalone SecondBox HTTP contract.
package api

import (
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
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

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
	MaximumDataPlaneBodyBytes int64
}

type handler struct {
	service                   *service.ControlPlaneService
	logger                    *slog.Logger
	platformTokenHash         [sha256.Size]byte
	maximumDataPlaneBodyBytes int64
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
	if config.MaximumDataPlaneBodyBytes <= 0 {
		return nil, errors.New("SecondBox HTTP data-plane body bound is required")
	}
	apiHandler := &handler{
		service: config.Service, logger: config.Logger,
		platformTokenHash:         sha256.Sum256([]byte(config.PlatformToken)),
		maximumDataPlaneBodyBytes: config.MaximumDataPlaneBodyBytes,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", apiHandler.health)
	mux.HandleFunc("GET /readyz", apiHandler.ready)
	mux.HandleFunc("GET /metrics", apiHandler.metrics)
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
	mux.Handle("GET /v1/sandboxes", apiHandler.authenticate(http.HandlerFunc(apiHandler.listSandboxes)))
	mux.Handle("POST /v1/sandboxes", apiHandler.authenticate(http.HandlerFunc(apiHandler.createSandbox)))
	mux.Handle("GET /v1/sandboxes/{sandboxID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getSandbox)))
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
	mux.Handle("GET /v1/sandboxes/{sandboxID}/artifacts", apiHandler.authenticate(http.HandlerFunc(apiHandler.listSandboxArtifacts)))
	mux.Handle("POST /v1/sandboxes/{sandboxID}/artifacts", apiHandler.authenticate(http.HandlerFunc(apiHandler.uploadSandboxArtifact)))
	mux.Handle("GET /v1/artifacts/{artifactID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getArtifact)))
	mux.Handle("DELETE /v1/artifacts/{artifactID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.deleteArtifact)))
	mux.Handle("GET /v1/artifacts/{artifactID}/content", apiHandler.authenticate(http.HandlerFunc(apiHandler.downloadArtifact)))
	mux.Handle("GET /v1/sandboxes/{sandboxID}/snapshots", apiHandler.authenticate(http.HandlerFunc(apiHandler.listSandboxSnapshots)))
	mux.Handle("POST /v1/sandboxes/{sandboxID}/snapshots", apiHandler.authenticate(http.HandlerFunc(apiHandler.createSandboxSnapshot)))
	mux.Handle("GET /v1/snapshots/{snapshotID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getSnapshot)))
	mux.Handle("DELETE /v1/snapshots/{snapshotID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.deleteSnapshot)))
	mux.Handle("GET /v1/leases/{leaseID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getLease)))
	mux.Handle("DELETE /v1/leases/{leaseID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.releaseLease)))
	mux.Handle("POST /v1/leases/{leaseAction}", apiHandler.authenticate(http.HandlerFunc(apiHandler.renewLease)))
	mux.Handle("GET /v1/operations/{operationID}", apiHandler.authenticate(http.HandlerFunc(apiHandler.getOperation)))
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
		apiHandler.writeError(writer, request, runnercontrol.ErrRelaySessionLimit)
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
		return errors.New("SecondBox File upload Content-Type must be application/octet-stream")
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

func (apiHandler *handler) listSandboxArtifacts(writer http.ResponseWriter, request *http.Request) {
	limit, err := queryLimit(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	page, err := apiHandler.service.ListSandboxArtifacts(
		request.Context(), requestPrincipal(request), request.PathValue("sandboxID"),
		limit, request.URL.Query().Get("cursor"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, page)
}

func (apiHandler *handler) uploadSandboxArtifact(writer http.ResponseWriter, request *http.Request) {
	generation, err := parseGeneration(request)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	upload, err := decodeArtifactUpload(writer, request, apiHandler.maximumDataPlaneBodyBytes)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	defer func() {
		if closeErr := upload.Content.Close(); closeErr != nil {
			apiHandler.logResponseAbort(request, "Artifact upload staging close", closeErr)
		}
	}()
	artifact, err := apiHandler.service.UploadSandboxArtifact(
		request.Context(), requestPrincipal(request), request.PathValue("sandboxID"),
		generation, request.Header.Get("SecondBox-Lease-ID"),
		request.Header.Get("Idempotency-Key"), upload,
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusCreated, artifact)
}

func (apiHandler *handler) getArtifact(writer http.ResponseWriter, request *http.Request) {
	artifact, err := apiHandler.service.GetArtifact(
		request.Context(), requestPrincipal(request), request.PathValue("artifactID"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	apiHandler.writeJSON(writer, request, http.StatusOK, artifact)
}

func (apiHandler *handler) downloadArtifact(writer http.ResponseWriter, request *http.Request) {
	content, artifact, err := apiHandler.service.DownloadArtifact(
		request.Context(), requestPrincipal(request), request.PathValue("artifactID"),
	)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	defer func() {
		if closeErr := content.Close(); closeErr != nil {
			apiHandler.logResponseAbort(request, "Artifact download close", closeErr)
		}
	}()
	digest, err := protocolChecksumToHTTPDigest("sha256:" + artifact.SHA256)
	if err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Digest", digest)
	writer.WriteHeader(http.StatusOK)
	if _, err := io.Copy(writer, content); err != nil {
		apiHandler.logResponseAbort(request, "Artifact download", err)
	}
}

func (apiHandler *handler) deleteArtifact(writer http.ResponseWriter, request *http.Request) {
	if err := requireEmptyBody(request); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	if err := apiHandler.service.DeleteArtifact(
		request.Context(), requestPrincipal(request), request.PathValue("artifactID"),
		request.Header.Get("Idempotency-Key"),
	); err != nil {
		apiHandler.writeError(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func decodeArtifactUpload(
	writer http.ResponseWriter,
	request *http.Request,
	maximumBodyBytes int64,
) (service.ArtifactUpload, error) {
	request.Body = http.MaxBytesReader(writer, request.Body, maximumBodyBytes)
	reader, err := request.MultipartReader()
	if err != nil {
		return service.ArtifactUpload{}, errors.New("SecondBox Artifact upload must be multipart/form-data")
	}
	fields := map[string][]byte{}
	var contentFile *os.File
	var contentSize int64
	var contentSHA256 string
	cleanupContent := true
	defer func() {
		if cleanupContent && contentFile != nil {
			_ = contentFile.Close()
			_ = os.Remove(contentFile.Name())
		}
	}()
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		var maximumBytesError *http.MaxBytesError
		if errors.As(err, &maximumBytesError) {
			return service.ArtifactUpload{}, runnercontrol.ErrRelaySessionLimit
		}
		if err != nil {
			return service.ArtifactUpload{}, fmt.Errorf("SecondBox Artifact multipart read failed: %w", err)
		}
		name := part.FormName()
		if name == "" {
			return service.ArtifactUpload{}, errors.Join(
				errors.New("SecondBox Artifact multipart field name is required"),
				part.Close(),
			)
		}
		if _, exists := fields[name]; exists {
			return service.ArtifactUpload{}, errors.Join(
				errors.New("SecondBox Artifact multipart fields must be unique"),
				part.Close(),
			)
		}
		switch name {
		case "name", "mediaType", "sha256", "metadata", "content":
		default:
			return service.ArtifactUpload{}, errors.Join(
				errors.New("SecondBox Artifact multipart field is unknown"),
				part.Close(),
			)
		}
		if name == "metadata" || name == "content" {
			mediaType, _, mediaTypeErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
			expectedMediaType := "application/json"
			if name == "content" {
				expectedMediaType = "application/octet-stream"
			}
			if mediaTypeErr != nil || mediaType != expectedMediaType {
				return service.ArtifactUpload{}, errors.Join(
					errors.New("SecondBox Artifact multipart field Content-Type is invalid: "+name),
					part.Close(),
				)
			}
		}
		if name == "content" {
			contentFile, err = os.CreateTemp("", "secondbox-artifact-upload-*")
			if err != nil {
				return service.ArtifactUpload{}, errors.Join(
					fmt.Errorf("SecondBox Artifact staging create failed: %w", err),
					part.Close(),
				)
			}
			hasher := sha256.New()
			contentSize, err = io.Copy(io.MultiWriter(contentFile, hasher), part)
			closeErr := part.Close()
			if err != nil || closeErr != nil {
				if errors.As(err, &maximumBytesError) {
					return service.ArtifactUpload{}, runnercontrol.ErrRelaySessionLimit
				}
				return service.ArtifactUpload{}, errors.Join(
					fmt.Errorf("SecondBox Artifact content staging failed: %w", err),
					closeErr,
				)
			}
			contentSHA256 = hex.EncodeToString(hasher.Sum(nil))
			fields[name] = nil
			continue
		}
		value, readErr := io.ReadAll(io.LimitReader(part, 1<<20+1))
		closeErr := part.Close()
		if readErr != nil || closeErr != nil {
			if errors.As(readErr, &maximumBytesError) {
				return service.ArtifactUpload{}, runnercontrol.ErrRelaySessionLimit
			}
			return service.ArtifactUpload{}, fmt.Errorf(
				"SecondBox Artifact multipart field read failed: read=%v close=%v",
				readErr, closeErr,
			)
		}
		fields[name] = value
	}
	for _, required := range []string{"name", "mediaType", "sha256", "metadata", "content"} {
		if _, exists := fields[required]; !exists {
			return service.ArtifactUpload{}, errors.New(
				"SecondBox Artifact multipart field is required: " + required,
			)
		}
	}
	var metadata map[string]string
	if err := json.Unmarshal(fields["metadata"], &metadata); err != nil || metadata == nil {
		return service.ArtifactUpload{}, errors.New("SecondBox Artifact metadata must be a JSON string map")
	}
	cleanupContent = false
	return service.ArtifactUpload{
		Name: string(fields["name"]), MediaType: string(fields["mediaType"]),
		SHA256: string(fields["sha256"]), Metadata: metadata, Content: &removingArtifactFile{contentFile},
		SizeBytes: contentSize, ActualSHA256: contentSHA256,
	}, nil
}

type removingArtifactFile struct {
	*os.File
}

func (file *removingArtifactFile) Close() error {
	name := file.Name()
	return errors.Join(file.File.Close(), os.Remove(name))
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
	name, action, ok := splitAction(request.PathValue("profileAction"))
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
	page, err := apiHandler.service.ListSandboxes(
		request.Context(), requestPrincipal(request), limit, request.URL.Query().Get("cursor"),
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
	sandboxID, action, ok := splitAction(request.PathValue("sandboxAction"))
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
	case "checkpoint":
		var body contracts.CheckpointSandboxRequest
		if err := decodeStrictJSON(request, &body); err != nil {
			apiHandler.writeError(writer, request, err)
			return
		}
		apiHandler.mutateSandboxLifecycle(writer, request, sandboxID, action, body.Metadata)
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
	leaseID, action, ok := splitAction(request.PathValue("leaseAction"))
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
		if subtle.ConstantTimeCompare(presentedHash[:], apiHandler.platformTokenHash[:]) != 1 {
			apiHandler.writeError(writer, request, ports.ErrAuthenticationFailed)
			return
		}
		tenantRef := request.Header.Get("X-SecondBox-Tenant-Ref")
		subjectRef := request.Header.Get("X-SecondBox-Subject-Ref")
		if !ownershipRefPattern.MatchString(tenantRef) ||
			!ownershipRefPattern.MatchString(subjectRef) {
			apiHandler.writeError(
				writer,
				request,
				errors.New("SecondBox tenant and subject references must contain 1 to 128 visible ASCII characters"),
			)
			return
		}
		principal := contracts.Principal{
			Kind: "platform", ID: subjectRef,
			TenantRef: tenantRef, SubjectRef: subjectRef,
		}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal)))
	})
}

func (apiHandler *handler) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID := request.Header.Get("X-Request-ID")
		if !correlationIDPattern.MatchString(requestID) {
			requestID = service.NewOpaqueID("req")
		}
		writer.Header().Set("X-Request-ID", requestID)
		request.Header.Set("X-Request-ID", requestID)
		request = request.WithContext(service.ContextWithRequestID(request.Context(), requestID))
		next.ServeHTTP(writer, request)
		apiHandler.logger.InfoContext(
			request.Context(),
			"SecondBox HTTP request completed",
			"request_id", requestID,
			"method", request.Method,
			"route", request.Pattern,
		)
	})
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
	apiHandler.writeJSON(writer, request, status, contracts.Problem{
		Type: "https://secondbox.dev/problems/" + code, Title: title, Status: status,
		Code: code, RequestID: writer.Header().Get("X-Request-ID"), Retryable: retryable,
	})
}

func classifyError(err error) (int, string, string, bool) {
	switch {
	case errors.Is(err, ports.ErrAuthenticationFailed):
		return http.StatusUnauthorized, "authentication_failed", "Authentication failed", false
	case errors.Is(err, ports.ErrPortTokenInvalid):
		return http.StatusUnauthorized, "authentication_failed", "Port tunnel token is invalid", false
	case errors.Is(err, ports.ErrAuthorizationDenied):
		return http.StatusForbidden, "authorization_failed", "Authorization failed", false
	case errors.Is(err, pagination.ErrInvalidListCursor):
		return http.StatusBadRequest, "invalid_request", "List page cursor is invalid", false
	case errors.Is(err, ports.ErrPortPolicyDenied):
		return http.StatusForbidden, "authorization_failed", "Exposed port is not permitted", false
	case errors.Is(err, runnercontrol.ErrFilePermission):
		return http.StatusForbidden, "file_permission_denied", "File operation permission denied", false
	case errors.Is(err, ports.ErrProfileNotFound),
		errors.Is(err, ports.ErrRunnerPoolNotFound), errors.Is(err, ports.ErrRunnerNotFound),
		errors.Is(err, ports.ErrSandboxNotFound), errors.Is(err, ports.ErrLeaseNotFound),
		errors.Is(err, ports.ErrArtifactNotFound), errors.Is(err, ports.ErrCheckpointNotFound),
		errors.Is(err, ports.ErrSnapshotNotFound), errors.Is(err, ports.ErrPortSessionNotFound),
		errors.Is(err, runnercontrol.ErrDataPlaneNotFound):
		return http.StatusNotFound, "not_found", "Resource not found", false
	case errors.Is(err, ports.ErrProfileDisabled):
		return http.StatusConflict, "profile_unavailable", "Profile is unavailable", false
	case errors.Is(err, ports.ErrSnapshotUnavailable):
		return http.StatusConflict, "state_conflict", "Snapshot requires stopped committed disk state", false
	case errors.Is(err, ports.ErrRunnerPoolUnavailable):
		return http.StatusConflict, "execution_node_unavailable", "Compatible execution node unavailable", true
	case errors.Is(err, ports.ErrRunnerPoolExists):
		return http.StatusConflict, "state_conflict", "RunnerPool already exists", false
	case errors.Is(err, ports.ErrIdempotencyConflict):
		return http.StatusConflict, "idempotency_conflict", "Idempotency key payload conflict", false
	case errors.Is(err, ports.ErrArtifactIntegrity):
		return http.StatusConflict, "state_conflict", "Artifact integrity verification failed", false
	case errors.Is(err, ports.ErrPortTokenConsumed):
		return http.StatusConflict, "state_conflict", "Port tunnel token was already consumed", false
	case errors.Is(err, ports.ErrArtifactStorage):
		return http.StatusServiceUnavailable, "dependency_unavailable", "Artifact storage unavailable", true
	case errors.Is(err, runnercontrol.ErrFileChecksum):
		return http.StatusConflict, "checksum_mismatch", "Content checksum mismatch", false
	case errors.Is(err, runnercontrol.ErrDataPlaneDeadline):
		return http.StatusConflict, "operation_deadline_exceeded", "Operation deadline exceeded", false
	case errors.Is(err, runnercontrol.ErrRelaySessionLimit), errors.Is(err, runnercontrol.ErrRelayFrameLimit):
		return http.StatusRequestEntityTooLarge, "limit_exceeded", "Configured byte limit exceeded", false
	case errors.Is(err, ports.ErrQuotaExceeded):
		return http.StatusTooManyRequests, "quota_exceeded", "Quota exceeded", false
	case errors.Is(err, ports.ErrPortBackpressure):
		return http.StatusTooManyRequests, "backpressure", "Port tunnel has no available byte credit", true
	case errors.Is(err, ports.ErrRevisionConflict):
		return http.StatusPreconditionFailed, "precondition_failed", "Resource revision changed", false
	case errors.Is(err, ports.ErrGenerationFenced):
		return http.StatusConflict, "generation_fenced", "Sandbox generation is fenced", false
	case errors.Is(err, ports.ErrLeaseInactive):
		return http.StatusConflict, "lease_fenced", "Lease is inactive or missing", false
	case errors.Is(err, runnercontrol.ErrTerminalAttached):
		return http.StatusConflict, "state_conflict", "Terminal already has an active attachment", false
	case errors.Is(err, runnercontrol.ErrTerminalDetached):
		return http.StatusConflict, "state_conflict", "Terminal attachment is inactive", false
	case errors.Is(err, ports.ErrWaitExpired):
		return http.StatusRequestTimeout, "wait_expired", "Sandbox wait deadline expired", false
	case errors.Is(err, ports.ErrLifecycleUnavailable):
		return http.StatusConflict, "execution_node_unavailable", "Execution node lifecycle unavailable", true
	default:
		if strings.HasPrefix(err.Error(), "SecondBox ") {
			return http.StatusBadRequest, "invalid_request", err.Error(), false
		}
		return http.StatusInternalServerError, "internal_error", "SecondBox request failed", true
	}
}

func parseHTTPDigest(value string) (string, error) {
	const prefix = "sha-256=:"
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, ":") {
		return "", errors.New("SecondBox Digest must contain a SHA-256 content digest")
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(value, prefix), ":")
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size || base64.StdEncoding.EncodeToString(decoded) != encoded {
		return "", errors.New("SecondBox Digest must contain canonical SHA-256 base64")
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
		return errors.New("SecondBox JSON request body is required")
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, (1<<20)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("SecondBox JSON request decoding failed: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("SecondBox JSON request must contain exactly one object")
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
	return errors.New("SecondBox request body must be empty")
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
		return 0, errors.New("SecondBox list limit must be an integer between 1 and 200")
	}
	return value, nil
}

func setRevisionETag(writer http.ResponseWriter, revision int64) {
	writer.Header().Set("ETag", fmt.Sprintf(`"revision-%d"`, revision))
}

func parseIfMatch(request *http.Request) (int64, error) {
	value := strings.Trim(request.Header.Get("If-Match"), `"`)
	value = strings.TrimPrefix(value, "revision-")
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 1 {
		return 0, errors.New("SecondBox If-Match must contain a positive revision ETag")
	}
	return revision, nil
}

func parseGeneration(request *http.Request) (int64, error) {
	generation, err := strconv.ParseInt(request.Header.Get("SecondBox-Generation"), 10, 64)
	if err != nil || generation < 1 {
		return 0, errors.New("SecondBox-Generation must contain a positive integer")
	}
	return generation, nil
}

func splitAction(value string) (string, string, bool) {
	resource, action, ok := strings.Cut(value, ":")
	return resource, action, ok && resource != "" && action != ""
}
