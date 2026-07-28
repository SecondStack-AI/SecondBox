package firecracker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"secondstack/sandbox-host/internal/config"
	"secondstack/sandbox-host/internal/runtime"
)

const sandboxHostContractVersion = "sandbox-host.secondstack.ai/v1"
const maxSandboxHostHTTPBodyBytes = 1 << 20
const maxSandboxHostArtifactBytes = 64 << 20

type sandboxHostEnvironment struct {
	ID                string            `json:"id"`
	TenantRef         string            `json:"tenantRef"`
	SubjectRef        string            `json:"subjectRef"`
	EnvironmentKey    string            `json:"environmentKey"`
	WorkspaceID       string            `json:"workspaceId"`
	CurrentGeneration int64             `json:"currentGeneration"`
	Metadata          map[string]string `json:"metadata"`
}

type sandboxHostWorkspace struct {
	ID         string `json:"id"`
	StorageRef string `json:"storageRef"`
}

type sandboxHostSnapshot struct {
	OpaqueRef   string `json:"opaqueRef"`
	ContentHash string `json:"contentHash"`
	SizeBytes   int64  `json:"sizeBytes"`
}

type sandboxHostResourceClass struct {
	CPUMillis    int64 `json:"cpuMillis"`
	MemoryBytes  int64 `json:"memoryBytes"`
	DiskBytes    int64 `json:"diskBytes"`
	ProcessLimit int64 `json:"processLimit"`
}

type sandboxHostInstance struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
}

type sandboxHostComputeRequest struct {
	Environment   sandboxHostEnvironment   `json:"environment"`
	Workspace     sandboxHostWorkspace     `json:"workspace"`
	ResourceClass sandboxHostResourceClass `json:"resourceClass"`
	Instance      sandboxHostInstance      `json:"instance"`
}

type sandboxHostIdentity struct {
	EnvironmentID string `json:"environmentId"`
	InstanceID    string `json:"instanceId"`
	Generation    int64  `json:"generation"`
	BackendRef    string `json:"backendRef"`
}

type sandboxHostArtifactInput struct {
	Identity  sandboxHostIdentity `json:"identity"`
	SourceRef string              `json:"sourceRef"`
	Name      string              `json:"name"`
	MimeType  string              `json:"mimeType"`
	Metadata  map[string]string   `json:"metadata"`
}

type sandboxHostExecuteOperation struct {
	Operation            string            `json:"operation"`
	Command              string            `json:"command,omitempty"`
	Args                 []string          `json:"args,omitempty"`
	Cwd                  string            `json:"cwd,omitempty"`
	Environment          map[string]string `json:"environment,omitempty"`
	TimeoutMillis        int64             `json:"timeoutMillis,omitempty"`
	Path                 string            `json:"path,omitempty"`
	Content              string            `json:"content,omitempty"`
	ContentBase64        string            `json:"contentBase64,omitempty"`
	Encoding             string            `json:"encoding,omitempty"`
	Recursive            bool              `json:"recursive,omitempty"`
	Force                bool              `json:"force,omitempty"`
	AllowedConnectionIDs []string          `json:"allowedConnectionIds"`
}

type sandboxHostExecuteInput struct {
	Identity  sandboxHostIdentity         `json:"identity"`
	Operation sandboxHostExecuteOperation `json:"operation"`
}

type sandboxHostEnvelope struct {
	ContractVersion string                     `json:"contractVersion"`
	Compute         *sandboxHostComputeRequest `json:"compute,omitempty"`
	Identity        *sandboxHostIdentity       `json:"identity,omitempty"`
	OperationRef    string                     `json:"operationRef,omitempty"`
	Artifact        *sandboxHostArtifactInput  `json:"artifact,omitempty"`
	Execute         *sandboxHostExecuteInput   `json:"execute,omitempty"`
	Environment     *sandboxHostEnvironment    `json:"environment,omitempty"`
	Workspace       *sandboxHostWorkspace      `json:"workspace,omitempty"`
	Snapshot        *sandboxHostSnapshot       `json:"snapshot,omitempty"`
}

type sandboxHostRuntime interface {
	Ready(context.Context) error
	Prepare(context.Context, sandboxHostComputeRequest) (string, error)
	Start(context.Context, string, sandboxHostComputeRequest) (string, error)
	Inspect(context.Context, sandboxHostIdentity) (string, error)
	Stop(context.Context, sandboxHostIdentity) error
	Destroy(context.Context, sandboxHostIdentity) error
	Purge(context.Context, sandboxHostEnvironment, sandboxHostWorkspace) error
	Checkpoint(context.Context, sandboxHostIdentity) (string, string, int64, error)
	CheckpointWorkspace(context.Context, sandboxHostEnvironment, sandboxHostWorkspace) (string, string, int64, error)
	MaterializeWorkspace(context.Context, sandboxHostEnvironment, sandboxHostWorkspace, sandboxHostSnapshot) error
	ExchangeArtifact(context.Context, sandboxHostArtifactInput) (string, int64, string, error)
	Execute(context.Context, sandboxHostExecuteInput) (ToolExecResponse, error)
	OpenWorkspaceFile(context.Context, sandboxHostIdentity, string) (io.ReadCloser, int64, error)
	PutWorkspaceFile(context.Context, sandboxHostIdentity, string, io.Reader) (int64, string, error)
}

// NewSandboxHostHTTPHandler exposes the stable provider-neutral host contract.
func NewSandboxHostHTTPHandler(runtime sandboxHostRuntime, token string, maxFileTransferBytes int64) (http.Handler, error) {
	if runtime == nil {
		return nil, errors.New("Sandbox Host runtime is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("Sandbox Host bearer token is required")
	}
	if maxFileTransferBytes <= 0 {
		return nil, errors.New("Sandbox Host file transfer limit must be positive")
	}
	handler := &sandboxHostHTTPHandler{runtime: runtime, token: token, maxFileTransferBytes: maxFileTransferBytes}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ready", handler.ready)
	mux.HandleFunc("POST /v1/compute:prepare", handler.prepare)
	mux.HandleFunc("POST /v1/compute:start", handler.start)
	mux.HandleFunc("POST /v1/compute:inspect", handler.inspect)
	mux.HandleFunc("POST /v1/compute:stop", handler.stop)
	mux.HandleFunc("POST /v1/compute:destroy", handler.destroy)
	mux.HandleFunc("POST /v1/compute:checkpoint", handler.checkpoint)
	mux.HandleFunc("POST /v1/workspace:checkpoint", handler.checkpointWorkspace)
	mux.HandleFunc("POST /v1/workspace:materialize", handler.materializeWorkspace)
	mux.HandleFunc("POST /v1/compute:execute", handler.execute)
	mux.HandleFunc("POST /v1/artifacts:exchange", handler.exchangeArtifact)
	mux.HandleFunc("POST /v1/environment:purge", handler.purge)
	mux.HandleFunc("GET /v1/workspace:file", handler.getWorkspaceFile)
	mux.HandleFunc("PUT /v1/workspace:file", handler.putWorkspaceFile)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if subtle.ConstantTimeCompare(
			[]byte(request.Header.Get("Authorization")),
			[]byte("Bearer "+token),
		) != 1 {
			writeSandboxHostError(writer, http.StatusUnauthorized, "unauthorized")
			return
		}
		mux.ServeHTTP(writer, request)
	}), nil
}

type sandboxHostHTTPHandler struct {
	runtime              sandboxHostRuntime
	token                string
	maxFileTransferBytes int64
}

func (h *sandboxHostHTTPHandler) ready(writer http.ResponseWriter, request *http.Request) {
	if err := h.runtime.Ready(request.Context()); err != nil {
		writeSandboxHostError(writer, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeSandboxHostJSON(writer, http.StatusOK, map[string]bool{"ready": true})
}

func (h *sandboxHostHTTPHandler) prepare(writer http.ResponseWriter, request *http.Request) {
	envelope, ok := decodeSandboxHostEnvelope(writer, request)
	if !ok || envelope.Compute == nil {
		if ok {
			writeSandboxHostError(writer, http.StatusBadRequest, "compute request is required")
		}
		return
	}
	operationRef, err := h.runtime.Prepare(request.Context(), *envelope.Compute)
	if err != nil {
		writeSandboxHostError(writer, http.StatusBadRequest, err.Error())
		return
	}
	writeSandboxHostJSON(writer, http.StatusOK, map[string]string{"operationRef": operationRef})
}

func (h *sandboxHostHTTPHandler) start(writer http.ResponseWriter, request *http.Request) {
	envelope, ok := decodeSandboxHostEnvelope(writer, request)
	if !ok || envelope.Compute == nil {
		if ok {
			writeSandboxHostError(writer, http.StatusBadRequest, "compute request is required")
		}
		return
	}
	backendRef, err := h.runtime.Start(request.Context(), envelope.OperationRef, *envelope.Compute)
	if err != nil {
		writeSandboxHostError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeSandboxHostJSON(writer, http.StatusOK, map[string]string{"backendRef": backendRef, "state": "ready"})
}

func (h *sandboxHostHTTPHandler) inspect(writer http.ResponseWriter, request *http.Request) {
	envelope, ok := decodeSandboxHostEnvelope(writer, request)
	if !ok || envelope.Identity == nil {
		if ok {
			writeSandboxHostError(writer, http.StatusBadRequest, "compute identity is required")
		}
		return
	}
	state, err := h.runtime.Inspect(request.Context(), *envelope.Identity)
	if err != nil {
		writeSandboxHostError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeSandboxHostJSON(writer, http.StatusOK, map[string]string{"state": state})
}

func (h *sandboxHostHTTPHandler) stop(writer http.ResponseWriter, request *http.Request) {
	h.identityOperation(writer, request, h.runtime.Stop)
}

func (h *sandboxHostHTTPHandler) destroy(writer http.ResponseWriter, request *http.Request) {
	h.identityOperation(writer, request, h.runtime.Destroy)
}

func (h *sandboxHostHTTPHandler) identityOperation(
	writer http.ResponseWriter,
	request *http.Request,
	operation func(context.Context, sandboxHostIdentity) error,
) {
	envelope, ok := decodeSandboxHostEnvelope(writer, request)
	if !ok || envelope.Identity == nil {
		if ok {
			writeSandboxHostError(writer, http.StatusBadRequest, "compute identity is required")
		}
		return
	}
	if err := operation(request.Context(), *envelope.Identity); err != nil {
		writeSandboxHostError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeSandboxHostJSON(writer, http.StatusOK, struct{}{})
}

func (h *sandboxHostHTTPHandler) purge(writer http.ResponseWriter, request *http.Request) {
	envelope, ok := decodeSandboxHostEnvelope(writer, request)
	if !ok || envelope.Environment == nil || envelope.Workspace == nil {
		if ok {
			writeSandboxHostError(writer, http.StatusBadRequest, "Environment and Workspace are required")
		}
		return
	}
	if err := h.runtime.Purge(request.Context(), *envelope.Environment, *envelope.Workspace); err != nil {
		writeSandboxHostError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeSandboxHostJSON(writer, http.StatusOK, struct{}{})
}

func (h *sandboxHostHTTPHandler) checkpoint(writer http.ResponseWriter, request *http.Request) {
	envelope, ok := decodeSandboxHostEnvelope(writer, request)
	if !ok || envelope.Identity == nil {
		if ok {
			writeSandboxHostError(writer, http.StatusBadRequest, "compute identity is required")
		}
		return
	}
	ref, hash, size, err := h.runtime.Checkpoint(request.Context(), *envelope.Identity)
	if err != nil {
		writeSandboxHostError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeSandboxHostJSON(writer, http.StatusOK, map[string]any{"opaqueRef": ref, "contentHash": hash, "sizeBytes": size})
}

func (h *sandboxHostHTTPHandler) materializeWorkspace(writer http.ResponseWriter, request *http.Request) {
	envelope, ok := decodeSandboxHostEnvelope(writer, request)
	if !ok || envelope.Environment == nil || envelope.Workspace == nil || envelope.Snapshot == nil {
		if ok {
			writeSandboxHostError(writer, http.StatusBadRequest, "Environment, Workspace, and Snapshot are required")
		}
		return
	}
	if err := h.runtime.MaterializeWorkspace(request.Context(), *envelope.Environment, *envelope.Workspace, *envelope.Snapshot); err != nil {
		writeSandboxHostError(writer, http.StatusConflict, err.Error())
		return
	}
	writeSandboxHostJSON(writer, http.StatusOK, struct{}{})
}

func (h *sandboxHostHTTPHandler) checkpointWorkspace(writer http.ResponseWriter, request *http.Request) {
	envelope, ok := decodeSandboxHostEnvelope(writer, request)
	if !ok || envelope.Environment == nil || envelope.Workspace == nil {
		if ok {
			writeSandboxHostError(writer, http.StatusBadRequest, "Environment and Workspace are required")
		}
		return
	}
	ref, hash, size, err := h.runtime.CheckpointWorkspace(request.Context(), *envelope.Environment, *envelope.Workspace)
	if err != nil {
		writeSandboxHostError(writer, http.StatusConflict, err.Error())
		return
	}
	writeSandboxHostJSON(writer, http.StatusOK, map[string]any{"opaqueRef": ref, "contentHash": hash, "sizeBytes": size})
}

func (h *sandboxHostHTTPHandler) exchangeArtifact(writer http.ResponseWriter, request *http.Request) {
	envelope, ok := decodeSandboxHostEnvelope(writer, request)
	if !ok || envelope.Artifact == nil {
		if ok {
			writeSandboxHostError(writer, http.StatusBadRequest, "artifact request is required")
		}
		return
	}
	ref, size, hash, err := h.runtime.ExchangeArtifact(request.Context(), *envelope.Artifact)
	if err != nil {
		writeSandboxHostError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeSandboxHostJSON(writer, http.StatusOK, map[string]any{"opaqueRef": ref, "sizeBytes": size, "sha256": hash})
}

func (h *sandboxHostHTTPHandler) execute(writer http.ResponseWriter, request *http.Request) {
	envelope, ok := decodeSandboxHostEnvelope(writer, request)
	if !ok || envelope.Execute == nil {
		if ok {
			writeSandboxHostError(writer, http.StatusBadRequest, "execute request is required")
		}
		return
	}
	response, err := h.runtime.Execute(request.Context(), *envelope.Execute)
	if err != nil {
		writeSandboxHostError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeSandboxHostJSON(writer, http.StatusOK, response)
}

func (h *sandboxHostHTTPHandler) getWorkspaceFile(writer http.ResponseWriter, request *http.Request) {
	identity, path, err := sandboxHostWorkspaceFileQuery(request)
	if err != nil {
		writeSandboxHostError(writer, http.StatusBadRequest, err.Error())
		return
	}
	reader, size, err := h.runtime.OpenWorkspaceFile(request.Context(), identity, path)
	if err != nil {
		writeSandboxHostError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	defer reader.Close()
	if size < 0 || size > h.maxFileTransferBytes {
		writeSandboxHostError(writer, http.StatusRequestEntityTooLarge, "Sandbox Host workspace file exceeds transfer limit")
		return
	}
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	writer.WriteHeader(http.StatusOK)
	if _, err := io.CopyN(writer, reader, size); err != nil {
		panic(err)
	}
}

func (h *sandboxHostHTTPHandler) putWorkspaceFile(writer http.ResponseWriter, request *http.Request) {
	identity, path, err := sandboxHostWorkspaceFileQuery(request)
	if err != nil {
		writeSandboxHostError(writer, http.StatusBadRequest, err.Error())
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxFileTransferBytes)
	size, hash, err := h.runtime.PutWorkspaceFile(request.Context(), identity, path, request.Body)
	if err != nil {
		writeSandboxHostError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	if size < 0 || size > h.maxFileTransferBytes {
		writeSandboxHostError(writer, http.StatusRequestEntityTooLarge, "Sandbox Host workspace file exceeds transfer limit")
		return
	}
	writeSandboxHostJSON(writer, http.StatusOK, map[string]any{"sizeBytes": size, "sha256": hash})
}

func sandboxHostWorkspaceFileQuery(request *http.Request) (sandboxHostIdentity, string, error) {
	generation, err := strconv.ParseInt(request.URL.Query().Get("generation"), 10, 64)
	if err != nil {
		return sandboxHostIdentity{}, "", errors.New("Sandbox Host workspace file generation is invalid")
	}
	identity := sandboxHostIdentity{
		EnvironmentID: request.URL.Query().Get("environmentId"),
		InstanceID:    request.URL.Query().Get("instanceId"),
		Generation:    generation,
		BackendRef:    request.URL.Query().Get("backendRef"),
	}
	if err := validateSandboxHostIdentity(identity); err != nil {
		return sandboxHostIdentity{}, "", err
	}
	path := strings.TrimSpace(request.URL.Query().Get("path"))
	if path == "" || strings.Contains(path, "..") || strings.HasPrefix(path, "/") {
		return sandboxHostIdentity{}, "", errors.New("Sandbox Host workspace file path is invalid")
	}
	return identity, path, nil
}

func decodeSandboxHostEnvelope(writer http.ResponseWriter, request *http.Request) (sandboxHostEnvelope, bool) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxSandboxHostHTTPBodyBytes)
	var envelope sandboxHostEnvelope
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		writeSandboxHostError(writer, http.StatusBadRequest, err.Error())
		return sandboxHostEnvelope{}, false
	}
	if envelope.ContractVersion != sandboxHostContractVersion {
		writeSandboxHostError(writer, http.StatusBadRequest, "unsupported Sandbox Host contract version")
		return sandboxHostEnvelope{}, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeSandboxHostError(writer, http.StatusBadRequest, "request must contain one JSON value")
		return sandboxHostEnvelope{}, false
	}
	return envelope, true
}

func writeSandboxHostError(writer http.ResponseWriter, status int, message string) {
	writeSandboxHostJSON(writer, status, map[string]string{"error": message})
}

func writeSandboxHostJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		panic(err)
	}
}

type firecrackerSandboxHostRuntime struct {
	manager           *Manager
	stateRoot         string
	cloneEvidenceFile func(destination, source *os.File) error
}

// NewFirecrackerSandboxHostRuntime binds stable domain requests to the real microVM manager.
func NewFirecrackerSandboxHostRuntime(manager *Manager, stateRoot string) (sandboxHostRuntime, error) {
	if manager == nil {
		return nil, errors.New("Sandbox Host microVM manager is required")
	}
	if !filepath.IsAbs(stateRoot) {
		return nil, errors.New("Sandbox Host state root must be absolute")
	}
	return &firecrackerSandboxHostRuntime{
		manager: manager, stateRoot: stateRoot, cloneEvidenceFile: reflinkFile,
	}, nil
}

func (r *firecrackerSandboxHostRuntime) Ready(context.Context) error {
	return r.manager.VerifyArtifactHealth()
}

func (r *firecrackerSandboxHostRuntime) Prepare(_ context.Context, request sandboxHostComputeRequest) (string, error) {
	if err := validateSandboxHostCompute(request); err != nil {
		return "", err
	}
	return sandboxHostOperationRef(request), nil
}

func (r *firecrackerSandboxHostRuntime) Start(ctx context.Context, operationRef string, request sandboxHostComputeRequest) (string, error) {
	if err := validateSandboxHostCompute(request); err != nil {
		return "", err
	}
	if operationRef != sandboxHostOperationRef(request) {
		return "", errors.New("Sandbox Host preparation reference does not match compute intent")
	}
	agentID, compartmentID := sandboxHostRuntimeIdentity(request.Environment, request.Instance.Generation)
	lease, err := r.manager.acquireWarmToolVM(ctx, agentID, compartmentID, sandboxHostStartOpts(request))
	if err != nil {
		return "", err
	}
	r.manager.releaseWarmToolVM(lease.instanceID)
	return lease.instanceID, nil
}

func (r *firecrackerSandboxHostRuntime) Inspect(ctx context.Context, identity sandboxHostIdentity) (string, error) {
	if err := validateSandboxHostIdentity(identity); err != nil {
		return "", err
	}
	running, err := r.manager.IsRunning(ctx, identity.BackendRef)
	if err != nil {
		return "", err
	}
	if !running {
		return "lost", nil
	}
	return "ready", nil
}

func (r *firecrackerSandboxHostRuntime) Stop(ctx context.Context, identity sandboxHostIdentity) error {
	if err := validateSandboxHostIdentity(identity); err != nil {
		return err
	}
	freezeWorkspace := r.manager.FreezeWorkspace
	if r.manager.freezeWorkspace != nil {
		freezeWorkspace = r.manager.freezeWorkspace
	}
	if _, err := freezeWorkspace(ctx, identity.BackendRef); err != nil {
		return err
	}
	return r.manager.Stop(ctx, identity.BackendRef)
}

func (r *firecrackerSandboxHostRuntime) Destroy(ctx context.Context, identity sandboxHostIdentity) error {
	if err := validateSandboxHostIdentity(identity); err != nil {
		return err
	}
	return r.manager.Remove(ctx, identity.BackendRef)
}

func (r *firecrackerSandboxHostRuntime) Purge(_ context.Context, environment sandboxHostEnvironment, workspace sandboxHostWorkspace) error {
	agentID, compartmentID := sandboxHostRuntimeIdentity(environment, environment.CurrentGeneration)
	workspacePath := filepath.Join(r.manager.cfg.MicroVMWorkspaceDir, agentID, compartmentID+"."+workspaceName)
	root := filepath.Clean(r.manager.cfg.MicroVMWorkspaceDir) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(workspacePath), root) {
		return errors.New("Sandbox Host workspace path escaped the configured root")
	}
	if err := os.Remove(workspacePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("purge Sandbox workspace: %w", err)
	}
	return nil
}

func (r *firecrackerSandboxHostRuntime) Checkpoint(_ context.Context, identity sandboxHostIdentity) (string, string, int64, error) {
	if err := validateSandboxHostIdentity(identity); err != nil {
		return "", "", 0, err
	}
	instance := r.manager.lookup(identity.BackendRef)
	if instance == nil {
		return "", "", 0, errors.New("Sandbox Host Instance is not active")
	}
	return r.copyEvidenceFile(instance.workspacePath, "checkpoints")
}

func (r *firecrackerSandboxHostRuntime) CheckpointWorkspace(_ context.Context, environment sandboxHostEnvironment, workspace sandboxHostWorkspace) (string, string, int64, error) {
	if strings.TrimSpace(workspace.ID) == "" || strings.TrimSpace(environment.WorkspaceID) != workspace.ID {
		return "", "", 0, errors.New("Sandbox Host Workspace binding is invalid")
	}
	agentID, compartmentID := sandboxHostRuntimeIdentity(environment, environment.CurrentGeneration)
	workspacePath := filepath.Join(r.manager.cfg.MicroVMWorkspaceDir, agentID, compartmentID+"."+workspaceName)
	if _, err := os.Stat(workspacePath); err != nil {
		return "", "", 0, err
	}
	return r.copyEvidenceFile(workspacePath, "checkpoints")
}

func (r *firecrackerSandboxHostRuntime) MaterializeWorkspace(_ context.Context, environment sandboxHostEnvironment, workspace sandboxHostWorkspace, snapshot sandboxHostSnapshot) error {
	if strings.TrimSpace(workspace.ID) == "" || strings.TrimSpace(environment.WorkspaceID) != workspace.ID {
		return errors.New("Sandbox Host target Workspace binding is invalid")
	}
	const prefix = "sandbox-host:checkpoints:"
	hashText := strings.TrimPrefix(strings.TrimSpace(snapshot.OpaqueRef), prefix)
	if hashText == snapshot.OpaqueRef || hashText != snapshot.ContentHash || len(hashText) != 64 || snapshot.SizeBytes < 1 {
		return errors.New("Sandbox Host Snapshot evidence is invalid")
	}
	sourcePath := filepath.Join(r.stateRoot, "checkpoints", hashText)
	agentID, compartmentID := sandboxHostRuntimeIdentity(environment, environment.CurrentGeneration)
	targetPath := filepath.Join(r.manager.cfg.MicroVMWorkspaceDir, agentID, compartmentID+"."+workspaceName)
	if _, err := os.Lstat(targetPath); err == nil {
		return errors.New("Sandbox Host target Workspace already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return materializeSandboxWorkspaceImage(sourcePath, targetPath, snapshot.ContentHash, snapshot.SizeBytes)
}

func (r *firecrackerSandboxHostRuntime) ExchangeArtifact(ctx context.Context, input sandboxHostArtifactInput) (string, int64, string, error) {
	if err := validateSandboxHostIdentity(input.Identity); err != nil {
		return "", 0, "", err
	}
	source := strings.TrimPrefix(strings.TrimSpace(input.SourceRef), "workspace:")
	source = strings.TrimPrefix(source, "/")
	if source == "" || strings.Contains(source, "..") {
		return "", 0, "", errors.New("Sandbox Host artifact source must be a workspace-relative path")
	}
	content, err := r.manager.ReadWorkspaceFile(ctx, input.Identity.BackendRef, source, maxSandboxHostArtifactBytes)
	if err != nil {
		return "", 0, "", err
	}
	hash := sha256.Sum256(content)
	hashText := hex.EncodeToString(hash[:])
	dir := filepath.Join(r.stateRoot, "artifacts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, "", err
	}
	path := filepath.Join(dir, hashText)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return "", 0, "", err
	}
	return "sandbox-host:artifact:" + hashText, int64(len(content)), hashText, nil
}

func (r *firecrackerSandboxHostRuntime) Execute(ctx context.Context, input sandboxHostExecuteInput) (response ToolExecResponse, err error) {
	if err := validateSandboxHostIdentity(input.Identity); err != nil {
		return ToolExecResponse{}, err
	}
	allowedConnectionIDs, err := validateAllowedConnectionIDs(input.Operation.AllowedConnectionIDs)
	if err != nil {
		return ToolExecResponse{}, err
	}
	instance := r.manager.lookup(input.Identity.BackendRef)
	if instance == nil || instance.environmentID != input.Identity.EnvironmentID ||
		instance.sandboxInstanceID != input.Identity.InstanceID || instance.generation != input.Identity.Generation {
		return ToolExecResponse{}, errors.New("Sandbox Host active Instance identity does not match execution")
	}
	operationEnvironment := make(map[string]string, len(input.Operation.Environment)+1)
	for name, value := range input.Operation.Environment {
		operationEnvironment[name] = value
	}
	delete(operationEnvironment, "SANDBOX_EGRESS_SOURCE_TOKEN")
	if len(allowedConnectionIDs) == 0 {
		if err := r.manager.unregisterSourceBinding(ctx, instance); err != nil {
			return ToolExecResponse{}, err
		}
	} else {
		sourceToken, err := r.manager.registerSourceBinding(
			ctx,
			input.Identity.BackendRef,
			input.Identity.EnvironmentID,
			input.Identity.InstanceID,
			input.Identity.Generation,
			allowedConnectionIDs,
		)
		if err != nil {
			return ToolExecResponse{}, err
		}
		operationEnvironment["SANDBOX_EGRESS_SOURCE_TOKEN"] = sourceToken
		defer func() {
			err = errors.Join(err, r.manager.unregisterSourceBinding(context.WithoutCancel(ctx), instance))
		}()
	}
	request := ToolExecRequest{
		Operation: ToolExecutorOperation(input.Operation.Operation),
		Command:   input.Operation.Command, Args: append([]string(nil), input.Operation.Args...),
		Cwd: input.Operation.Cwd, Env: operationEnvironment,
		TimeoutMillis: input.Operation.TimeoutMillis, Path: input.Operation.Path,
		Content: input.Operation.Content, ContentBase64: input.Operation.ContentBase64,
		Encoding: input.Operation.Encoding, Recursive: input.Operation.Recursive, Force: input.Operation.Force,
	}
	return r.manager.ExecuteTool(ctx, input.Identity.BackendRef, request)
}

func validateAllowedConnectionIDs(values []string) ([]string, error) {
	if len(values) > 100 {
		return nil, errors.New("Sandbox Host allowedConnectionIds exceeds 100 items")
	}
	seen := map[string]struct{}{}
	connectionIDs := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != value || value == "" || len(value) > 500 {
			return nil, errors.New("Sandbox Host allowedConnectionIds contains an invalid connection ID")
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("Sandbox Host allowedConnectionIds contains duplicate %q", value)
		}
		seen[value] = struct{}{}
		connectionIDs = append(connectionIDs, value)
	}
	return connectionIDs, nil
}

func (r *firecrackerSandboxHostRuntime) OpenWorkspaceFile(ctx context.Context, identity sandboxHostIdentity, path string) (io.ReadCloser, int64, error) {
	if err := validateSandboxHostIdentity(identity); err != nil {
		return nil, 0, err
	}
	return r.manager.OpenWorkspaceFileStream(ctx, identity.BackendRef, path)
}

func (r *firecrackerSandboxHostRuntime) PutWorkspaceFile(ctx context.Context, identity sandboxHostIdentity, path string, reader io.Reader) (int64, string, error) {
	if err := validateSandboxHostIdentity(identity); err != nil {
		return 0, "", err
	}
	return r.manager.PutWorkspaceFileStream(ctx, identity.BackendRef, path, reader)
}

func (r *firecrackerSandboxHostRuntime) copyEvidenceFile(source, kind string) (string, string, int64, error) {
	file, err := os.Open(source)
	if err != nil {
		return "", "", 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", "", 0, err
	}
	hash := sha256.New()
	dir := filepath.Join(r.stateRoot, kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", 0, err
	}
	temp, err := os.CreateTemp(dir, ".evidence-*")
	if err != nil {
		return "", "", 0, err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if r.cloneEvidenceFile == nil {
		temp.Close()
		return "", "", 0, errors.New("Sandbox Host workspace evidence copy-on-write clone is unavailable")
	}
	if err := r.cloneEvidenceFile(temp, file); err != nil {
		temp.Close()
		return "", "", 0, fmt.Errorf("Sandbox Host workspace evidence copy-on-write clone failed: %w", err)
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		temp.Close()
		return "", "", 0, err
	}
	if _, err := io.Copy(hash, temp); err != nil {
		temp.Close()
		return "", "", 0, err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return "", "", 0, err
	}
	if err := temp.Close(); err != nil {
		return "", "", 0, err
	}
	hashText := hex.EncodeToString(hash.Sum(nil))
	finalPath := filepath.Join(dir, hashText)
	if err := os.Rename(tempName, finalPath); err != nil {
		return "", "", 0, err
	}
	return "sandbox-host:" + kind + ":" + hashText, hashText, info.Size(), nil
}

func materializeSandboxWorkspaceImage(sourcePath, targetPath, expectedHash string, expectedSize int64) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if expectedSize < 1 || info.Size() != expectedSize {
		return errors.New("Sandbox Host Snapshot size evidence does not match retained content")
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(targetPath), ".sandbox-workspace-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(temp, hash), source, expectedSize); err != nil {
		temp.Close()
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedHash {
		temp.Close()
		return errors.New("Sandbox Host Snapshot hash evidence does not match retained content")
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(targetPath))
}

func validateSandboxHostCompute(request sandboxHostComputeRequest) error {
	for name, value := range map[string]string{
		"Environment ID":    request.Environment.ID,
		"subject reference": request.Environment.SubjectRef,
		"Environment key":   request.Environment.EnvironmentKey,
		"Workspace ID":      request.Workspace.ID,
		"Instance ID":       request.Instance.ID,
	} {
		if strings.TrimSpace(value) == "" || strings.Contains(value, "..") || strings.ContainsAny(value, `/\`) {
			return fmt.Errorf("Sandbox Host %s is invalid", name)
		}
	}
	if request.Instance.Generation < 1 || request.Environment.CurrentGeneration != request.Instance.Generation {
		return errors.New("Sandbox Host compute generation is invalid")
	}
	if request.ResourceClass.CPUMillis < 1000 || request.ResourceClass.CPUMillis%1000 != 0 ||
		request.ResourceClass.MemoryBytes < 1<<20 || request.ResourceClass.MemoryBytes%(1<<20) != 0 ||
		request.ResourceClass.DiskBytes < 1<<20 || request.ResourceClass.DiskBytes%(1<<20) != 0 ||
		request.ResourceClass.ProcessLimit < 1 {
		return errors.New("Sandbox Host Resource Class is invalid")
	}
	return nil
}

func validateSandboxHostIdentity(identity sandboxHostIdentity) error {
	if identity.Generation < 1 || strings.TrimSpace(identity.EnvironmentID) == "" ||
		strings.TrimSpace(identity.InstanceID) == "" || strings.TrimSpace(identity.BackendRef) == "" {
		return errors.New("Sandbox Host compute identity is invalid")
	}
	return nil
}

func sandboxHostOperationRef(request sandboxHostComputeRequest) string {
	input := fmt.Sprintf("%s\x00%s\x00%d\x00%s", request.Environment.ID, request.Instance.ID, request.Instance.Generation, request.Workspace.ID)
	sum := sha256.Sum256([]byte(input))
	return "prepare:" + hex.EncodeToString(sum[:])
}

func sandboxHostRuntimeIdentity(environment sandboxHostEnvironment, _ int64) (string, string) {
	agentID := environment.SubjectRef
	if environment.TenantRef == "chat" {
		sum := sha256.Sum256([]byte(environment.SubjectRef))
		agentID = "sandbox-chat-" + hex.EncodeToString(sum[:12])
	}
	return agentID, environment.EnvironmentKey
}

func sandboxHostStartOpts(request sandboxHostComputeRequest) runtimemanager.StartOpts {
	const mib = int64(1 << 20)
	return runtimemanager.StartOpts{
		EnvironmentID:    request.Environment.ID,
		InstanceID:       request.Instance.ID,
		Generation:       request.Instance.Generation,
		CompartmentID:    request.Environment.EnvironmentKey,
		ShapeFingerprint: sandboxHostOperationRef(request),
		RuntimeClass:     runtimemanager.RuntimeClassToolExecutor,
		SandboxPolicy: &runtimemanager.SandboxRuntimePolicy{
			VCPUs:             int(request.ResourceClass.CPUMillis / 1000),
			MemoryMiB:         int(request.ResourceClass.MemoryBytes / mib),
			WorkspaceSizeMiB:  int(request.ResourceClass.DiskBytes / mib),
			ProcessLimit:      int(request.ResourceClass.ProcessLimit),
			WorkspaceWritable: true,
			SharedReadOnly:    true,
		},
	}
}

// SandboxHostManagerConfig builds the real in-host microVM manager configuration.
func SandboxHostManagerConfig(launcher PrivilegedLauncherConfig) (*config.Config, error) {
	rootfs := strings.TrimSpace(os.Getenv("SANDBOX_HOST_MICROVM_ROOTFS_PATH"))
	publicKey := strings.TrimSpace(os.Getenv("SANDBOX_HOST_MICROVM_PUBLIC_KEY"))
	publicKeySHA := strings.TrimSpace(os.Getenv("SANDBOX_HOST_MICROVM_PUBLIC_KEY_SHA256"))
	workspaceBackend := strings.TrimSpace(os.Getenv("SANDBOX_HOST_MICROVM_WORKSPACE_BACKEND"))
	guestIP := strings.TrimSpace(os.Getenv("SANDBOX_HOST_MICROVM_GUEST_IP"))
	for name, value := range map[string]string{
		"SANDBOX_HOST_MICROVM_ROOTFS_PATH":       rootfs,
		"SANDBOX_HOST_MICROVM_PUBLIC_KEY":        publicKey,
		"SANDBOX_HOST_MICROVM_PUBLIC_KEY_SHA256": publicKeySHA,
		"SANDBOX_HOST_MICROVM_WORKSPACE_BACKEND": workspaceBackend,
		"SANDBOX_HOST_MICROVM_GUEST_IP":          guestIP,
	} {
		if value == "" {
			return nil, fmt.Errorf("%s is required by Sandbox Host", name)
		}
	}
	maxPerSubject, err := sandboxHostPositiveInt("SANDBOX_HOST_MAX_CONCURRENT_PER_SUBJECT")
	if err != nil {
		return nil, err
	}
	maxGlobal, err := sandboxHostPositiveInt("SANDBOX_HOST_MAX_CONCURRENT_GLOBAL")
	if err != nil {
		return nil, err
	}
	memoryBudgetMiB, err := sandboxHostPositiveInt("SANDBOX_HOST_MICROVM_MEMORY_BUDGET_MIB")
	if err != nil {
		return nil, err
	}
	sandboxHostFileTransferMaxBytes, err := SandboxHostFileTransferMaxBytes()
	if err != nil {
		return nil, err
	}
	return &config.Config{
		DataDir:         launcher.StateRoot,
		FirecrackerPath: launcher.FirecrackerPath, JailerPath: launcher.JailerPath,
		MicroVMJailerChrootBaseDir: launcher.JailRoot,
		MicroVMJailerUID:           launcher.JailerUID, MicroVMJailerGID: launcher.JailerGID,
		MicroVMJailerCgroupVersion: launcher.JailerCgroupVersion,
		MicroVMJailerParentCgroup:  launcher.JailerParentCgroup,
		MicroVMKernelPath:          launcher.KernelPath, MicroVMRootfsPath: rootfs,
		MicroVMSharedImagePath: strings.TrimSpace(os.Getenv("SANDBOX_HOST_MICROVM_SHARED_IMAGE_PATH")),
		MicroVMPublicKeyPath:   publicKey, MicroVMPublicKeySHA256: publicKeySHA,
		MicroVMWorkspaceDir: launcher.WorkspaceRoot, MicroVMRunDir: launcher.RunRoot,
		MicroVMLogDir: launcher.LogRoot, MicroVMKernelArgs: launcher.KernelArgs,
		MicroVMMemoryMiB: launcher.MemoryMiB, MicroVMVCPUs: launcher.VCPUs,
		MicroVMCPUTemplate: launcher.CPUTemplate, MicroVMWorkspaceSizeMiB: launcher.WorkspaceSizeMiB,
		MicroVMWorkspaceBackend: workspaceBackend, MicroVMGuestIP: guestIP,
		MicroVMMaxConcurrentPerAgent: maxPerSubject, MicroVMMaxConcurrentGlobal: maxGlobal,
		MicroVMMemoryBudgetMiB: memoryBudgetMiB,
		FileTransferMaxBytes:   int64(sandboxHostFileTransferMaxBytes),
		MicroVMBridgeName:      launcher.BridgeName, MicroVMBridgeCIDR: launcher.BridgeCIDR,
		MicroVMTapPrefix: launcher.TapPrefix,
	}, nil
}

// SandboxHostFileTransferMaxBytes loads the required host-side streaming bound.
func SandboxHostFileTransferMaxBytes() (int, error) {
	return sandboxHostPositiveInt("SANDBOX_HOST_FILE_TRANSFER_MAX_BYTES")
}

func sandboxHostPositiveInt(name string) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}
