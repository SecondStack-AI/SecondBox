package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SecondStack-AI/SecondBox/internal/objectstore"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

var artifactSHA256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type artifactReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

// ArtifactUpload is one validated multipart Artifact publication request.
type ArtifactUpload struct {
	Name         string
	MediaType    string
	SHA256       string
	Metadata     map[string]string
	Content      artifactReadSeekCloser
	SizeBytes    int64
	ActualSHA256 string
}

// UploadSandboxArtifact publishes immutable bytes only after durable admission and provider verification.
func (service *ControlPlaneService) UploadSandboxArtifact(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	generation int64,
	leaseID string,
	idempotencyKey string,
	upload ArtifactUpload,
) (contracts.Artifact, error) {
	if err := service.requireArtifacts(principal); err != nil {
		return contracts.Artifact{}, err
	}
	if generation < 1 {
		return contracts.Artifact{}, invalidRequest(errors.New("SecondBox Artifact generation must be positive"))
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Artifact{}, err
	}
	if err := validateArtifactUpload(upload); err != nil {
		return contracts.Artifact{}, err
	}
	if upload.Content == nil || upload.SizeBytes < 0 || upload.ActualSHA256 != upload.SHA256 {
		return contracts.Artifact{}, ports.ErrArtifactIntegrity
	}
	_, retentionPolicy, err := service.store.GetSandboxLifecyclePolicy(
		ctx, principal.TenantRef, principal.SubjectRef, sandboxID,
	)
	if err != nil {
		return contracts.Artifact{}, err
	}
	requestHash, err := hashCanonicalRequest(struct {
		Name      string            `json:"name"`
		MediaType string            `json:"mediaType"`
		SHA256    string            `json:"sha256"`
		Metadata  map[string]string `json:"metadata"`
		SizeBytes int64             `json:"sizeBytes"`
	}{
		Name: upload.Name, MediaType: upload.MediaType, SHA256: upload.SHA256,
		Metadata: upload.Metadata, SizeBytes: upload.SizeBytes,
	})
	if err != nil {
		return contracts.Artifact{}, err
	}
	now := service.now().UTC()
	artifact := contracts.Artifact{
		ID: service.newID("art"), TenantRef: principal.TenantRef,
		SubjectRef: principal.SubjectRef, SandboxID: sandboxID,
		SourceGeneration: generation, Name: upload.Name, MediaType: upload.MediaType,
		SizeBytes: upload.SizeBytes, SHA256: upload.SHA256,
		Metadata: cloneMetadata(upload.Metadata),
		RetainUntil: now.Add(
			time.Duration(retentionPolicy.ArtifactRetentionSeconds) * time.Second,
		),
		CreatedAt: now,
	}
	storageKey := artifactStorageKey(artifact)
	publication := ports.ArtifactPublicationInput{
		Artifact: artifact, StorageKey: storageKey, ExpectedGeneration: generation,
		LeaseID:        leaseID,
		IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		IdempotencyEnds: now.Add(idempotencyRetention),
	}
	staged, err := service.store.StageArtifact(ctx, publication)
	if err != nil {
		return contracts.Artifact{}, err
	}
	if staged.State == contracts.ObjectStatePublished {
		return staged, nil
	}
	publication.Artifact = staged
	publication.StorageKey = artifactStorageKey(staged)
	if _, err := upload.Content.Seek(0, io.SeekStart); err != nil {
		return contracts.Artifact{}, fmt.Errorf("%w: rewind Artifact staging file: %v", ports.ErrArtifactStorage, err)
	}
	if _, err := service.artifactObjectStore.PutImmutable(
		ctx, publication.StorageKey, upload.Content,
		staged.SizeBytes, staged.SHA256,
	); err != nil {
		return contracts.Artifact{}, fmt.Errorf("%w: %v", ports.ErrArtifactStorage, err)
	}
	audit := service.newAudit(
		ctx, principal, "artifact.published", "artifact", staged.ID, principal.TenantRef, now,
	)
	published, err := service.store.PublishArtifact(ctx, publication, now)
	if err != nil {
		return contracts.Artifact{}, err
	}
	if err := service.store.AppendAuditEvent(ctx, audit); err != nil {
		return contracts.Artifact{}, err
	}
	return published, nil
}

// ListSandboxArtifacts returns one retained Artifact page inside the authenticated Project.
func (service *ControlPlaneService) ListSandboxArtifacts(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	limit int,
	cursor string,
) (contracts.ArtifactPage, error) {
	if err := service.requireArtifacts(principal); err != nil {
		return contracts.ArtifactPage{}, err
	}
	if len(cursor) > 512 {
		return contracts.ArtifactPage{}, invalidRequest(errors.New("SecondBox Artifact page cursor exceeds its bound"))
	}
	return service.store.ListArtifacts(
		ctx, principal.TenantRef, principal.SubjectRef, sandboxID,
		boundedLimit(limit), cursor, service.now().UTC(),
	)
}

// GetArtifact returns retained metadata without exposing private provider identity.
func (service *ControlPlaneService) GetArtifact(
	ctx context.Context,
	principal contracts.Principal,
	artifactID string,
) (contracts.Artifact, error) {
	if err := service.requireArtifacts(principal); err != nil {
		return contracts.Artifact{}, err
	}
	object, err := service.store.GetArtifactObject(
		ctx, principal.TenantRef, principal.SubjectRef, artifactID, service.now().UTC(),
	)
	return object.Artifact, err
}

// DownloadArtifact returns verified immutable bytes and metadata.
func (service *ControlPlaneService) DownloadArtifact(
	ctx context.Context,
	principal contracts.Principal,
	artifactID string,
) (io.ReadCloser, contracts.Artifact, error) {
	if err := service.requireArtifacts(principal); err != nil {
		return nil, contracts.Artifact{}, err
	}
	object, err := service.store.GetArtifactObject(
		ctx, principal.TenantRef, principal.SubjectRef, artifactID, service.now().UTC(),
	)
	if err != nil {
		return nil, contracts.Artifact{}, err
	}
	body, evidence, err := service.artifactObjectStore.GetVerified(ctx, object.StorageKey, objectstore.Evidence{
		SHA256: object.Artifact.SHA256, SizeBytes: object.Artifact.SizeBytes,
	})
	if err != nil {
		return nil, contracts.Artifact{}, fmt.Errorf("%w: %v", ports.ErrArtifactStorage, err)
	}
	if evidence.SHA256 != object.Artifact.SHA256 ||
		evidence.SizeBytes != object.Artifact.SizeBytes {
		_ = body.Close()
		return nil, contracts.Artifact{}, ports.ErrArtifactIntegrity
	}
	return body, object.Artifact, nil
}

// DeleteArtifact ends retention idempotently and leaves provider deletion to garbage collection.
func (service *ControlPlaneService) DeleteArtifact(
	ctx context.Context,
	principal contracts.Principal,
	artifactID string,
	idempotencyKey string,
) error {
	if err := service.requireArtifacts(principal); err != nil {
		return err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	requestHash, err := hashCanonicalRequest(struct {
		ArtifactID string `json:"artifactId"`
	}{ArtifactID: artifactID})
	if err != nil {
		return err
	}
	now := service.now().UTC()
	audit := service.newAudit(
		ctx, principal, "artifact.retention_ended", "artifact",
		artifactID, principal.TenantRef, now,
	)
	if err := service.store.EndArtifactRetention(ctx, ports.ArtifactRetentionInput{
		TenantRef:  principal.TenantRef,
		SubjectRef: principal.SubjectRef, ArtifactID: artifactID,
		IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		IdempotencyEnds: now.Add(idempotencyRetention), Now: now,
	}); err != nil {
		return err
	}
	return service.store.AppendAuditEvent(ctx, audit)
}

func (service *ControlPlaneService) requireArtifacts(principal contracts.Principal) error {
	if service.artifactObjectStore == nil {
		return ports.ErrArtifactStorage
	}
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return ports.ErrAuthorizationDenied
	}
	return nil
}

func validateArtifactUpload(upload ArtifactUpload) error {
	if !utf8.ValidString(upload.Name) || strings.TrimSpace(upload.Name) == "" ||
		len(upload.Name) > 255 ||
		!utf8.ValidString(upload.MediaType) || len(upload.MediaType) < 1 ||
		len(upload.MediaType) > 255 ||
		!artifactSHA256Pattern.MatchString(upload.SHA256) {
		return invalidRequest(errors.New("SecondBox Artifact name, media type, or SHA-256 is invalid"))
	}
	mediaType, parameters, err := mime.ParseMediaType(upload.MediaType)
	if err != nil || mediaType == "" || len(parameters) > 16 {
		return invalidRequest(errors.New("SecondBox Artifact media type is invalid"))
	}
	if err := validateSandboxMetadata(upload.Metadata); err != nil {
		return err
	}
	return nil
}

func artifactStorageKey(artifact contracts.Artifact) string {
	return "artifacts/" + artifact.TenantRef + "/" + artifact.SandboxID + "/" + artifact.ID
}
