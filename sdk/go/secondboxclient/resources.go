package secondboxclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
)

// PageOptions bounds one SDK list request and carries its opaque continuation.
type PageOptions struct {
	Limit  int
	Cursor string
}

// SandboxListOptions adds exact Metadata-containment filters to pagination.
type SandboxListOptions struct {
	PageOptions
	Metadata Metadata
}

func (client *Client) ListProfiles(ctx context.Context, options PageOptions) (ProfilePage, error) {
	query, err := pageQuery(options)
	if err != nil {
		return ProfilePage{}, err
	}
	var page ProfilePage
	err = client.RequestJSON(ctx, "listProfiles", CallOptions{QueryParameters: query}, &page)
	return page, err
}

func (client *Client) GetProfile(ctx context.Context, name ProfileName) (Profile, error) {
	if name == "" {
		return Profile{}, errors.New("SecondBox Profile name is required")
	}
	var profile Profile
	err := client.RequestJSON(ctx, "getProfile", CallOptions{PathParameters: map[string]string{"profileName": name}}, &profile)
	return profile, err
}

// ValidateProfile proves that a named Profile currently accepts new Sandboxes.
func (client *Client) ValidateProfile(ctx context.Context, name ProfileName) (Profile, error) {
	profile, err := client.GetProfile(ctx, name)
	if err != nil {
		return Profile{}, err
	}
	if profile.State != ProfileStateEnabled {
		return Profile{}, fmt.Errorf("SecondBox Profile %q is %s", name, profile.State)
	}
	return profile, nil
}

func (client *Client) CreateProfile(ctx context.Context, request CreateProfileRequest, idempotencyKey string) (Profile, error) {
	if request.Name == "" {
		return Profile{}, errors.New("SecondBox Profile name is required")
	}
	var profile Profile
	err := client.mutateJSON(ctx, "createProfile", nil, 0, idempotencyKey, request, &profile)
	return profile, err
}

func (client *Client) ReviseProfile(ctx context.Context, name ProfileName, expectedRevision int64, request ReviseProfileRequest, idempotencyKey string) (Profile, error) {
	if name == "" {
		return Profile{}, errors.New("SecondBox Profile name is required")
	}
	var profile Profile
	err := client.mutateJSON(ctx, "reviseProfile", map[string]string{"profileName": name}, expectedRevision, idempotencyKey, request, &profile)
	return profile, err
}

func (client *Client) DisableProfile(ctx context.Context, name ProfileName, expectedRevision int64, idempotencyKey string) (Profile, error) {
	if name == "" {
		return Profile{}, errors.New("SecondBox Profile name is required")
	}
	var profile Profile
	err := client.mutateJSON(ctx, "disableProfile", map[string]string{"profileName": name}, expectedRevision, idempotencyKey, nil, &profile)
	return profile, err
}

func (client *Client) ListRunnerPools(ctx context.Context, options PageOptions) (RunnerPoolPage, error) {
	query, err := pageQuery(options)
	if err != nil {
		return RunnerPoolPage{}, err
	}
	var page RunnerPoolPage
	err = client.RequestJSON(ctx, "listRunnerPools", CallOptions{QueryParameters: query}, &page)
	return page, err
}

func (client *Client) GetRunnerPool(ctx context.Context, name ProfileName) (RunnerPool, error) {
	if name == "" {
		return RunnerPool{}, errors.New("SecondBox RunnerPool name is required")
	}
	var pool RunnerPool
	err := client.RequestJSON(ctx, "getRunnerPool", CallOptions{PathParameters: map[string]string{"runnerPoolName": name}}, &pool)
	return pool, err
}

func (client *Client) CreateRunnerPool(ctx context.Context, request CreateRunnerPoolRequest) (RunnerPool, error) {
	if request.Name == "" {
		return RunnerPool{}, errors.New("SecondBox RunnerPool name is required")
	}
	var pool RunnerPool
	err := client.mutateJSON(ctx, "createRunnerPool", nil, 0, "", request, &pool)
	return pool, err
}

func (client *Client) UpdateRunnerPool(ctx context.Context, name ProfileName, expectedRevision int64, request UpdateRunnerPoolRequest) (RunnerPool, error) {
	if name == "" {
		return RunnerPool{}, errors.New("SecondBox RunnerPool name is required")
	}
	var pool RunnerPool
	err := client.mutateJSON(ctx, "updateRunnerPool", map[string]string{"runnerPoolName": name}, expectedRevision, "", request, &pool)
	return pool, err
}

func (client *Client) ListSandboxes(ctx context.Context, options SandboxListOptions) (SandboxPage, error) {
	query, err := pageQuery(options.PageOptions)
	if err != nil {
		return SandboxPage{}, err
	}
	if len(options.Metadata) > 8 {
		return SandboxPage{}, errors.New("SecondBox Sandbox metadata filter must not exceed 8 entries")
	}
	keys := make([]string, 0, len(options.Metadata))
	for key := range options.Metadata {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		query.Add("metadata", key+"="+options.Metadata[key])
	}
	var page SandboxPage
	err = client.RequestJSON(ctx, "listSandboxes", CallOptions{QueryParameters: query}, &page)
	return page, err
}

// AdoptSandbox attaches a caller-owned handle to an existing durable Sandbox.
func (client *Client) AdoptSandbox(ctx context.Context, sandboxID OpaqueID) (*SandboxHandle, error) {
	if sandboxID == "" {
		return nil, errors.New("SecondBox Sandbox ID is required")
	}
	var sandbox Sandbox
	if err := client.RequestJSON(ctx, "getSandbox", CallOptions{PathParameters: map[string]string{"sandboxId": sandboxID}}, &sandbox); err != nil {
		return nil, err
	}
	return NewSandboxHandle(client, sandbox), nil
}

// UpdateMetadata replaces Metadata using the handle's observed revision. It
// never refreshes or replays after a failed optimistic-concurrency check.
func (handle *SandboxHandle) UpdateMetadata(ctx context.Context, metadata Metadata) (Sandbox, error) {
	current := handle.Snapshot()
	var sandbox Sandbox
	err := handle.client.mutateJSON(ctx, "updateSandboxMetadata", map[string]string{"sandboxId": current.ID}, current.Revision, "", UpdateSandboxMetadataRequest{Metadata: metadata}, &sandbox)
	if err == nil {
		handle.store(sandbox)
	}
	return sandbox, err
}

func (handle *SandboxHandle) ListSnapshots(ctx context.Context, options PageOptions) (SnapshotPage, error) {
	query, err := pageQuery(options)
	if err != nil {
		return SnapshotPage{}, err
	}
	var page SnapshotPage
	err = handle.client.RequestJSON(ctx, "listSandboxSnapshots", CallOptions{
		PathParameters: map[string]string{"sandboxId": handle.Snapshot().ID}, QueryParameters: query,
	}, &page)
	return page, err
}

func (client *Client) GetSnapshot(ctx context.Context, snapshotID OpaqueID) (Snapshot, error) {
	if snapshotID == "" {
		return Snapshot{}, errors.New("SecondBox Snapshot ID is required")
	}
	var snapshot Snapshot
	err := client.RequestJSON(ctx, "getSnapshot", CallOptions{PathParameters: map[string]string{"snapshotId": snapshotID}}, &snapshot)
	return snapshot, err
}

func (handle *SandboxHandle) ListArtifacts(ctx context.Context, options PageOptions) (ArtifactPage, error) {
	query, err := pageQuery(options)
	if err != nil {
		return ArtifactPage{}, err
	}
	var page ArtifactPage
	err = handle.client.RequestJSON(ctx, "listSandboxArtifacts", CallOptions{
		PathParameters: map[string]string{"sandboxId": handle.Snapshot().ID}, QueryParameters: query,
	}, &page)
	return page, err
}

func (client *Client) GetArtifact(ctx context.Context, artifactID OpaqueID) (Artifact, error) {
	if artifactID == "" {
		return Artifact{}, errors.New("SecondBox Artifact ID is required")
	}
	var artifact Artifact
	err := client.RequestJSON(ctx, "getArtifact", CallOptions{PathParameters: map[string]string{"artifactId": artifactID}}, &artifact)
	return artifact, err
}

// DownloadArtifact reads one immutable Artifact under an explicit output bound
// and verifies the HTTP Digest header before returning bytes.
func (client *Client) DownloadArtifact(ctx context.Context, artifactID OpaqueID, maximumBytes int64) ([]byte, error) {
	if artifactID == "" || maximumBytes < 1 {
		return nil, errors.New("SecondBox Artifact ID and positive download bound are required")
	}
	response, err := client.Request(ctx, "downloadArtifactContent", CallOptions{PathParameters: map[string]string{"artifactId": artifactID}})
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return nil, fmt.Errorf("SecondBox Artifact download read and close: %w", errors.Join(readErr, closeErr))
	}
	if int64(len(content)) > maximumBytes {
		return nil, fmt.Errorf("SecondBox Artifact download exceeds %d bytes", maximumBytes)
	}
	sum := sha256.Sum256(content)
	want := "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
	if response.Header.Get("Digest") != want {
		return nil, errors.New("SecondBox Artifact download Digest header does not match content")
	}
	return content, nil
}

func (client *Client) DeleteArtifact(ctx context.Context, artifactID OpaqueID, idempotencyKey string) error {
	if artifactID == "" {
		return errors.New("SecondBox Artifact ID is required")
	}
	idempotencyKey, err := resolveIdempotencyKey(idempotencyKey)
	if err != nil {
		return err
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", idempotencyKey)
	response, err := client.Request(ctx, "deleteArtifact", CallOptions{PathParameters: map[string]string{"artifactId": artifactID}, Headers: headers})
	if err != nil {
		return err
	}
	return response.Body.Close()
}

// UploadArtifact creates one immutable Artifact from bounded caller-owned bytes.
func (handle *SandboxHandle) UploadArtifact(ctx context.Context, name, mediaType string, metadata Metadata, content []byte, idempotencyKey, leaseID string) (Artifact, error) {
	if name == "" || mediaType == "" {
		return Artifact{}, errors.New("SecondBox Artifact name and media type are required")
	}
	idempotencyKey, err := resolveIdempotencyKey(idempotencyKey)
	if err != nil {
		return Artifact{}, err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writeField := func(name, value string) error { return writer.WriteField(name, value) }
	sum := sha256.Sum256(content)
	metadataJSON, err := json.Marshal(metadata)
	if err == nil {
		err = writeField("name", name)
	}
	if err == nil {
		err = writeField("mediaType", mediaType)
	}
	if err == nil {
		err = writeField("sha256", hex.EncodeToString(sum[:]))
	}
	if err == nil {
		headers := make(textproto.MIMEHeader)
		headers.Set("Content-Disposition", `form-data; name="metadata"`)
		headers.Set("Content-Type", "application/json")
		var part io.Writer
		part, err = writer.CreatePart(headers)
		if err == nil {
			_, err = part.Write(metadataJSON)
		}
	}
	if err == nil {
		headers := make(textproto.MIMEHeader)
		headers.Set("Content-Disposition", `form-data; name="content"; filename="content"`)
		headers.Set("Content-Type", "application/octet-stream")
		var part io.Writer
		part, err = writer.CreatePart(headers)
		if err == nil {
			_, err = part.Write(content)
		}
	}
	err = errors.Join(err, writer.Close())
	if err != nil {
		return Artifact{}, fmt.Errorf("SecondBox Artifact encode multipart request: %w", err)
	}
	headers := handle.GenerationHeaders(leaseID)
	headers.Set("Idempotency-Key", idempotencyKey)
	var artifact Artifact
	err = handle.client.RequestJSON(ctx, "uploadSandboxArtifact", CallOptions{
		PathParameters: map[string]string{"sandboxId": handle.Snapshot().ID}, Headers: headers,
		Body: &body, ContentType: writer.FormDataContentType(),
	}, &artifact)
	return artifact, err
}

func (client *Client) mutateJSON(ctx context.Context, operationID string, path map[string]string, expectedRevision int64, idempotencyKey string, request any, target any) error {
	headers := make(http.Header)
	if expectedRevision != 0 {
		if expectedRevision < 1 {
			return fmt.Errorf("SecondBox %s expected revision must be positive", operationID)
		}
		headers.Set("If-Match", RevisionETag(expectedRevision))
	}
	if operationID == "createProfile" || operationID == "reviseProfile" || operationID == "disableProfile" {
		resolved, err := resolveIdempotencyKey(idempotencyKey)
		if err != nil {
			return err
		}
		headers.Set("Idempotency-Key", resolved)
	}
	options := CallOptions{PathParameters: path, Headers: headers}
	if request != nil {
		body, err := json.Marshal(request)
		if err != nil {
			return fmt.Errorf("SecondBox %s encode request: %w", operationID, err)
		}
		options.Body = bytes.NewReader(body)
		options.ContentType = "application/json"
	}
	return client.RequestJSON(ctx, operationID, options, target)
}

func pageQuery(options PageOptions) (url.Values, error) {
	if options.Limit < 0 || options.Limit > 200 {
		return nil, errors.New("SecondBox page limit must be from 1 through 200 when supplied")
	}
	query := make(url.Values)
	if options.Limit != 0 {
		query.Set("limit", strconv.Itoa(options.Limit))
	}
	if options.Cursor != "" {
		query.Set("cursor", options.Cursor)
	}
	return query, nil
}
