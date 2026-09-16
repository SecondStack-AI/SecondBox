package executionimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
)

type FetchRequest struct {
	OperationID string    `json:"operationId"`
	TenantRef   string    `json:"tenantRef"`
	Reference   string    `json:"reference"`
	Deadline    time.Time `json:"deadline"`
	ReclaimOnly bool      `json:"reclaimOnly,omitempty"`
}

type FetchResult struct {
	Stage          string `json:"stage,omitempty"`
	Digest         string `json:"digest,omitempty"`
	Manifest       []byte `json:"manifest,omitempty"`
	Signature      []byte `json:"signature,omitempty"`
	Error          string `json:"error,omitempty"`
	CacheReclaimed bool   `json:"cacheReclaimed,omitempty"`
}

// FetchService streams one bounded request through a host-private Unix socket.
// Durable work and retries remain owned by the control plane's Operation.
type FetchService struct {
	manager          *Manager
	tenantConfigPath string
}

func NewFetchService(cfg *config.Config, tenantConfigPath string) (*FetchService, error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("SecondBox image fetcher must run as an unprivileged user")
	}
	if !filepath.IsAbs(tenantConfigPath) {
		return nil, errors.New("SecondBox image fetcher requires an absolute Tenant configuration path")
	}
	manager, err := NewManager(cfg)
	if err != nil {
		return nil, err
	}
	manager.skopeoPath, err = exec.LookPath("skopeo")
	if err != nil {
		return nil, fmt.Errorf("SecondBox image fetcher requires skopeo: %w", err)
	}
	return &FetchService{manager: manager, tenantConfigPath: tenantConfigPath}, nil
}

func (service *FetchService) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/prepare" {
		http.NotFound(writer, request)
		return
	}
	var input FetchRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.OperationID == "" || len(input.OperationID) > 256 || input.TenantRef == "" || len(input.TenantRef) > 256 || !input.Deadline.After(time.Now()) || input.Deadline.After(time.Now().Add(time.Hour)) {
		http.Error(writer, "SecondBox image preparation request is invalid", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		http.Error(writer, "SecondBox image preparation request contains trailing data", http.StatusBadRequest)
		return
	}
	authDocument, err := service.tenantAuthentication(input.TenantRef, input.Reference)
	if err != nil {
		http.Error(writer, "SecondBox Tenant image access denied", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithDeadline(request.Context(), input.Deadline)
	defer cancel()
	keyBytes := sha256.Sum256([]byte(input.TenantRef + "\x00" + input.OperationID))
	key := hex.EncodeToString(keyBytes[:])
	writer.Header().Set("Content-Type", "application/x-ndjson")
	encoder := json.NewEncoder(writer)
	if input.ReclaimOnly {
		if err := service.manager.reclaimUnusedImages(input.Reference); err != nil {
			writeFetchResult(ctx, encoder, FetchResult{Error: "SecondBox image cache reclamation failed"})
			return
		}
		writeFetchResult(ctx, encoder, FetchResult{CacheReclaimed: true})
		return
	}
	prepared, err := service.manager.Fetch(ctx, key, &runnerprotocol.ExecutionImage{Reference: input.Reference}, authDocument,
		func(stage runnerprotocol.AssignmentProgressStage) error {
			if err := encoder.Encode(FetchResult{Stage: stage.String()}); err != nil {
				return err
			}
			return http.NewResponseController(writer).Flush()
		},
		func(_ context.Context, bytes uint64) (func() error, error) {
			available, err := service.manager.availableCacheBytes()
			if err != nil {
				return nil, err
			}
			if available < bytes {
				return nil, ErrCapacityAdmissionDenied
			}
			return func() error { return nil }, nil
		})
	if err != nil {
		slog.WarnContext(ctx, "SecondBox image fetch failed", "operationId", input.OperationID, "error", err)
		writeFetchResult(ctx, encoder, FetchResult{Error: "SecondBox image retrieval or verification failed"})
		return
	}
	defer prepared.Release()
	manifest, manifestErr := config.ReadArtifactMetadata(filepath.Join(prepared.Directory, "manifest.json"), config.MaximumArtifactManifestBytes)
	signature, signatureErr := config.ReadArtifactMetadata(filepath.Join(prepared.Directory, "manifest.sig"), config.MaximumArtifactSignatureBytes)
	if errors.Join(manifestErr, signatureErr) != nil {
		writeFetchResult(ctx, encoder, FetchResult{Error: "SecondBox image manifest read failed"})
		return
	}
	writeFetchResult(ctx, encoder, FetchResult{Digest: prepared.ResolvedDigest, Manifest: manifest, Signature: signature})
}

func writeFetchResult(ctx context.Context, encoder *json.Encoder, result FetchResult) {
	if err := encoder.Encode(result); err != nil {
		// The response transport has failed; no further response can be delivered.
		slog.WarnContext(ctx, "SecondBox image fetch response transport failed", "error", err)
	}
}

func (service *FetchService) tenantAuthentication(tenant, reference string) ([]byte, error) {
	content, err := readRegistrySecret(service.tenantConfigPath)
	if err != nil {
		return nil, err
	}
	var tenants map[string]TenantRegistry
	if err := json.Unmarshal(content, &tenants); err != nil {
		return nil, errors.New("SecondBox Tenant registry configuration is invalid")
	}
	registry, ok := tenants[tenant]
	if !ok {
		return nil, errors.New("SecondBox Tenant has no registry configuration")
	}
	return registry.AuthenticationDocument(reference)
}
