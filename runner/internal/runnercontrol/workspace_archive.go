package runnercontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

// ExecuteWorkspaceArchive sends image bytes directly between trusted storage
// and the WorkspaceStore. Transfer grants never enter guest environments.
func ExecuteWorkspaceArchive(
	ctx context.Context,
	command *runnerprotocol.LocalWorkspaceCommand,
	store workspacestore.WorkspaceStore,
) (_ LocalWorkspaceEvidence, resultErr error) {
	archiveStore, ok := store.(workspacestore.WorkspaceArchiveTransferStore)
	if !ok || command == nil || command.ArchiveTransfer == nil ||
		command.ExpectedGeneration == 0 || command.LogicalCapacityBytes == 0 ||
		command.LogicalCapacityBytes > math.MaxInt64 || len(command.FencingToken) == 0 {
		return LocalWorkspaceEvidence{}, errors.New("SecondBox runner Workspace archive authority is incomplete")
	}
	transfer := command.ArchiveTransfer
	expiry := time.UnixMilli(int64(transfer.ExpiresAtUnixMs))
	if transfer.ExpiresAtUnixMs > math.MaxInt64 || !expiry.After(time.Now()) {
		return LocalWorkspaceEvidence{}, errors.New("SecondBox runner Workspace archive grant expired")
	}
	if len(transfer.PartUrls) == 0 || command.Kind == runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT {
		if err := validateWorkspaceArchiveEndpoint(transfer.Url); err != nil {
			return LocalWorkspaceEvidence{}, err
		}
	}
	ctx, cancel := context.WithDeadline(ctx, expiry)
	defer cancel()
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: nil, DialContext: (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	mutation := workspacestore.Mutation{
		OperationID: command.OperationId, WorkspaceID: command.WorkspaceId,
		FencingToken: append([]byte(nil), command.FencingToken...),
	}
	var receipt workspacestore.Receipt
	var descriptor *runnerprotocol.WorkspaceArchiveDescriptor
	switch command.Kind {
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_EXPORT:
		upload, err := archiveStore.OpenWorkspaceArchiveUpload(ctx, workspacestore.RelocationExportRequest{
			Mutation: mutation, ExpectedGeneration: command.ExpectedGeneration,
		})
		if err != nil {
			return LocalWorkspaceEvidence{}, err
		}
		defer func() { resultErr = errors.Join(resultErr, upload.Close()) }()
		stored := upload.Descriptor()
		if upload.Receipt().CapacityBytes != int64(command.LogicalCapacityBytes) {
			return LocalWorkspaceEvidence{}, errors.New("SecondBox runner Workspace archive capacity conflicts with sealed disk")
		}
		if err := uploadWorkspaceArchiveHTTP(ctx, client, transfer, upload); err != nil {
			return LocalWorkspaceEvidence{}, err
		}
		receipt = upload.Receipt()
		descriptor = &runnerprotocol.WorkspaceArchiveDescriptor{
			ArchiveBytes: uint64(stored.ArchiveBytes), ArchiveChecksum: stored.ArchiveChecksum,
			ImageChecksum: stored.ImageChecksum,
		}
	case runnerprotocol.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT:
		descriptor = transfer.GetArchiveDescriptor()
		if descriptor == nil || descriptor.ArchiveBytes > math.MaxInt64 {
			return LocalWorkspaceEvidence{}, errors.New("SecondBox runner Workspace archive restore descriptor is invalid")
		}
		request, err := newWorkspaceArchiveHTTPRequest(ctx, http.MethodGet, transfer.Url, transfer.Headers, nil)
		if err != nil {
			return LocalWorkspaceEvidence{}, err
		}
		response, err := client.Do(request)
		if err != nil {
			return LocalWorkspaceEvidence{}, workspaceArchiveTransportError(err)
		}
		defer func() { resultErr = errors.Join(resultErr, response.Body.Close()) }()
		if response.StatusCode != http.StatusOK ||
			(response.ContentLength >= 0 && response.ContentLength != int64(descriptor.ArchiveBytes)) {
			return LocalWorkspaceEvidence{}, fmt.Errorf("SecondBox runner Workspace archive download refused or size changed (HTTP %d)", response.StatusCode)
		}
		receipt, err = archiveStore.ImportWorkspaceArchive(ctx, workspacestore.WorkspaceArchiveImportRequest{
			RelocationImportRequest: workspacestore.RelocationImportRequest{
				Mutation: mutation, Generation: command.ExpectedGeneration,
				CapacityBytes: int64(command.LogicalCapacityBytes),
			},
			Archive: workspacestore.WorkspaceArchiveDescriptor{
				ArchiveBytes: int64(descriptor.ArchiveBytes), ArchiveChecksum: descriptor.ArchiveChecksum,
				ImageChecksum: descriptor.ImageChecksum,
			},
		}, response.Body)
		if err != nil {
			return LocalWorkspaceEvidence{}, err
		}
	default:
		return LocalWorkspaceEvidence{}, errors.New("SecondBox runner Workspace archive command kind is unsupported")
	}
	return LocalWorkspaceEvidence{
		Generation: receipt.Generation, LogicalCapacity: uint64(receipt.CapacityBytes),
		ReceiptRecordedAt: receipt.RecordedAt, Checksum: receipt.Checksum, ArchiveDescriptor: descriptor,
	}, nil
}

func newWorkspaceArchiveHTTPRequest(
	ctx context.Context, method, endpoint string, headers map[string]string, body io.Reader,
) (*http.Request, error) {
	if err := validateWorkspaceArchiveEndpoint(endpoint); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, workspaceArchiveTransportError(err)
	}
	for name, value := range headers {
		if strings.EqualFold(name, "Host") {
			if !strings.EqualFold(value, request.URL.Host) {
				return nil, errors.New("SecondBox runner Workspace archive grant changes the destination host")
			}
			continue
		}
		if strings.EqualFold(name, "Content-Length") || strings.EqualFold(name, "Transfer-Encoding") {
			return nil, errors.New("SecondBox runner Workspace archive grant overrides transport framing")
		}
		request.Header.Set(name, value)
	}
	return request, nil
}

func validateWorkspaceArchiveEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil || parsed.Fragment != "" || parsed.Host == "" ||
		(parsed.Scheme != "https" && !(parsed.Scheme == "http" && archiveLoopbackHost(parsed.Hostname()))) {
		return errors.New("SecondBox runner Workspace archive endpoint requires HTTPS")
	}
	return nil
}

func archiveLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func workspaceArchiveTransportError(err error) error {
	var requestError *url.Error
	if errors.As(err, &requestError) {
		err = requestError.Err
	}
	return fmt.Errorf("SecondBox runner Workspace archive transport: %w", err)
}
