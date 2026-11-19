package runnercontrol

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/workspacestore"
)

type workspaceArchiveMultipartCompletion struct {
	XMLName xml.Name                       `xml:"CompleteMultipartUpload"`
	Parts   []workspaceArchiveUploadedPart `xml:"Part"`
}

type workspaceArchiveUploadedPart struct {
	Number int    `xml:"PartNumber"`
	ETag   string `xml:"ETag"`
}

func uploadWorkspaceArchiveHTTP(
	ctx context.Context,
	client *http.Client,
	transfer *runnerprotocol.WorkspaceArchiveTransfer,
	upload *workspacestore.WorkspaceArchiveUpload,
) (resultErr error) {
	size := upload.Descriptor().ArchiveBytes
	if len(transfer.PartUrls) == 0 {
		request, err := newWorkspaceArchiveHTTPRequest(ctx, http.MethodPut, transfer.Url, transfer.Headers, io.NopCloser(upload))
		if err != nil {
			return err
		}
		request.ContentLength = size
		response, err := client.Do(request)
		if err != nil {
			return workspaceArchiveTransportError(err)
		}
		closeErr := response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return errors.Join(fmt.Errorf("SecondBox runner Workspace archive upload refused (HTTP %d)", response.StatusCode), closeErr)
		}
		return closeErr
	}
	if transfer.PartSizeBytes == 0 || transfer.PartSizeBytes > 5<<30 || len(transfer.PartUrls) > 512 {
		return errors.New("SecondBox runner Workspace archive multipart grant exceeds bounds")
	}
	for _, endpoint := range append(append([]string{}, transfer.PartUrls...), transfer.CompleteUrl, transfer.AbortUrl) {
		if err := validateWorkspaceArchiveEndpoint(endpoint); err != nil {
			return err
		}
	}
	parts := (uint64(size) + transfer.PartSizeBytes - 1) / transfer.PartSizeBytes
	if parts == 0 || parts > uint64(len(transfer.PartUrls)) {
		return errors.New("SecondBox runner Workspace archive exceeds multipart grant capacity")
	}
	defer func() {
		if resultErr != nil {
			abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			request, err := newWorkspaceArchiveHTTPRequest(abortCtx, http.MethodDelete, transfer.AbortUrl, nil, nil)
			if err != nil {
				resultErr = errors.Join(resultErr, err)
				return
			}
			response, err := client.Do(request)
			if err != nil {
				resultErr = errors.Join(resultErr, workspaceArchiveTransportError(err))
				return
			}
			closeErr := response.Body.Close()
			if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
				resultErr = errors.Join(resultErr, fmt.Errorf("SecondBox runner Workspace archive multipart abort refused (HTTP %d)", response.StatusCode))
			}
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()
	completion := workspaceArchiveMultipartCompletion{}
	remaining := size
	for index := uint64(0); index < parts; index++ {
		partSize := min(remaining, int64(transfer.PartSizeBytes))
		request, err := newWorkspaceArchiveHTTPRequest(ctx, http.MethodPut, transfer.PartUrls[index], transfer.Headers, io.LimitReader(upload, partSize))
		if err != nil {
			return err
		}
		request.ContentLength = partSize
		response, err := client.Do(request)
		if err != nil {
			return workspaceArchiveTransportError(err)
		}
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return errors.Join(fmt.Errorf("SecondBox runner Workspace archive part upload refused (HTTP %d)", response.StatusCode), closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		etag := response.Header.Get("ETag")
		if etag == "" || len(etag) > 256 || strings.ContainsAny(etag, "\r\n") {
			return errors.New("SecondBox runner Workspace archive part lacks a bounded ETag")
		}
		completion.Parts = append(completion.Parts, workspaceArchiveUploadedPart{Number: int(index) + 1, ETag: etag})
		remaining -= partSize
	}
	encoded, err := xml.Marshal(completion)
	if err != nil {
		return fmt.Errorf("SecondBox runner Workspace archive multipart completion encoding: %w", err)
	}
	request, err := newWorkspaceArchiveHTTPRequest(ctx, http.MethodPost, transfer.CompleteUrl, nil, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/xml")
	response, err := client.Do(request)
	if err != nil {
		return workspaceArchiveTransportError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, response.Body.Close()) }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("SecondBox runner Workspace archive multipart completion refused (HTTP %d)", response.StatusCode)
	}
	// S3 may return HTTP 200 and then an XML Error while completing an upload.
	var result struct {
		XMLName xml.Name
		ETag    string `xml:"ETag"`
	}
	if err := xml.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil ||
		result.XMLName.Local != "CompleteMultipartUploadResult" || result.ETag == "" {
		return errors.New("SecondBox runner Workspace archive multipart completion did not confirm the object")
	}
	return nil
}
