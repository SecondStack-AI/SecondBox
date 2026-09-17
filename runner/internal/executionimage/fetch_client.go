package executionimage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
)

// FetchExecutionImage uses only the host-private fetcher socket, never a registry.
func FetchExecutionImage(ctx context.Context, socket string, input FetchRequest, progress func(FetchResult) error) (FetchResult, error) {
	if !filepath.IsAbs(socket) {
		return FetchResult{}, errors.New("SecondBox image fetcher socket must be absolute")
	}
	body, err := json.Marshal(input)
	if err != nil {
		return FetchResult{}, err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://secondbox-image-fetcher/prepare", bytes.NewReader(body))
	if err != nil {
		return FetchResult{}, err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return FetchResult{}, fmt.Errorf("SecondBox image fetcher request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return FetchResult{}, fmt.Errorf("SecondBox image fetcher rejected preparation: HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	for {
		var result FetchResult
		if err := decoder.Decode(&result); err != nil {
			return FetchResult{}, fmt.Errorf("SecondBox image fetcher response: %w", err)
		}
		if result.Error != "" {
			return FetchResult{}, errors.New(result.Error)
		}
		if result.Digest != "" {
			if !imageDigestPattern.MatchString(result.Digest) || len(result.Manifest) == 0 || len(result.Signature) == 0 {
				return FetchResult{}, errors.New("SecondBox image fetcher returned incomplete signed metadata")
			}
			return result, nil
		}
		if err := progress(result); err != nil {
			return FetchResult{}, err
		}
	}
}
