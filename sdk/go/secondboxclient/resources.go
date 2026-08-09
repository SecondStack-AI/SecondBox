package secondboxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
