package secondboxclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
)

// ReadFileTo streams file bytes to the caller's writer without buffering the file.
func (handle *SandboxHandle) ReadFileTo(ctx context.Context, path WorkspacePath, output io.Writer, leaseID string) error {
	if path == "" || output == nil {
		return errors.New("SecondBox file path and output are required")
	}
	response, err := handle.client.Request(ctx, "readSandboxFile", CallOptions{
		PathParameters:  map[string]string{"sandboxId": handle.Snapshot().ID},
		QueryParameters: url.Values{"path": {path}}, Headers: handle.GenerationHeaders(leaseID),
	})
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, response.Body)
	if err := errors.Join(copyErr, response.Body.Close()); err != nil {
		return fmt.Errorf("SecondBox file stream: %w", err)
	}
	return nil
}

// WriteFileFrom hashes then streams a seekable file, starting at its current
// position. The caller must keep its contents stable until the request finishes.
func (handle *SandboxHandle) WriteFileFrom(ctx context.Context, path WorkspacePath, input io.ReadSeeker, idempotencyKey, leaseID string) (FileWriteResult, error) {
	if path == "" || input == nil {
		return FileWriteResult{}, errors.New("SecondBox file path and input are required")
	}
	key, err := resolveIdempotencyKey(idempotencyKey)
	if err != nil {
		return FileWriteResult{}, err
	}
	offset, err := input.Seek(0, io.SeekCurrent)
	if err != nil {
		return FileWriteResult{}, fmt.Errorf("SecondBox file seek: %w", err)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, input); err != nil {
		return FileWriteResult{}, fmt.Errorf("SecondBox file digest: %w", err)
	}
	if _, err := input.Seek(offset, io.SeekStart); err != nil {
		return FileWriteResult{}, fmt.Errorf("SecondBox file rewind: %w", err)
	}
	headers := handle.GenerationHeaders(leaseID)
	headers.Set("Idempotency-Key", key)
	headers.Set("Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest.Sum(nil))+":")
	var result FileWriteResult
	err = handle.client.RequestJSON(ctx, "writeSandboxFile", CallOptions{
		PathParameters: map[string]string{"sandboxId": handle.Snapshot().ID}, QueryParameters: url.Values{"path": {path}},
		Headers: headers, Body: input, ContentType: "application/octet-stream",
	}, &result)
	return result, err
}
