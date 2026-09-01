package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
)

const maximumDiagnosticProbeBytes = int64(10 << 20)

func runDiagnosticsBundleCommand(
	ctx context.Context,
	rawURL string,
	token string,
	args []string,
	output io.Writer,
	httpClient *http.Client,
) error {
	flags := flag.NewFlagSet("diagnostics bundle", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	outputPath := flags.String("output", "", "absolute output archive path")
	controlPlaneLog := flags.String(
		"control-plane-log", "", "absolute control-plane log path",
	)
	maximumLogBytes := flags.Int64("max-log-bytes", 0, "maximum log-tail bytes")
	maximumProbeBytes := flags.Int64("max-probe-bytes", 0, "maximum bytes per HTTP response")
	httpTimeout := flags.Duration("http-timeout", 0, "probe HTTP timeout")
	timingWindow := flags.Duration("timing-window", 0, "timing aggregation window")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("SecondBox diagnostics bundle options: %w", err)
	}
	if len(flags.Args()) != 0 {
		return errors.New("SecondBox diagnostics bundle received unexpected arguments")
	}
	if rawURL == "" {
		return errors.New("SecondBox diagnostics bundle requires --url" + sessionSourceHint)
	}
	if token == "" {
		return errors.New("SecondBox diagnostics bundle requires --token" + sessionSourceHint)
	}
	if !filepath.IsAbs(*outputPath) {
		return errors.New("SecondBox diagnostics bundle --output must be absolute")
	}
	if !filepath.IsAbs(*controlPlaneLog) {
		return errors.New(
			"SecondBox diagnostics bundle --control-plane-log must be absolute",
		)
	}
	if *maximumLogBytes < 1 || *maximumLogBytes > 100<<20 {
		return errors.New(
			"SecondBox diagnostics bundle --max-log-bytes must be from 1 through 104857600",
		)
	}
	if *maximumProbeBytes < 1 || *maximumProbeBytes > maximumDiagnosticProbeBytes {
		return errors.New(
			"SecondBox diagnostics bundle --max-probe-bytes must be from 1 through 10485760",
		)
	}
	if *httpTimeout < time.Second || *httpTimeout > time.Minute {
		return errors.New(
			"SecondBox diagnostics bundle --http-timeout must be from 1s through 1m",
		)
	}
	if *timingWindow < time.Minute || *timingWindow > time.Hour ||
		*timingWindow%time.Second != 0 {
		return errors.New(
			"SecondBox diagnostics bundle --timing-window must be whole seconds from 1m through 1h",
		)
	}
	baseURL, err := diagnosticBaseURL(rawURL)
	if err != nil {
		return err
	}
	client := *httpClient
	client.Timeout = *httpTimeout
	files := make(map[string][]byte)
	for _, probe := range []string{"healthz", "readyz", "metrics"} {
		body, status := diagnosticProbe(
			ctx, &client, baseURL, probe, "", 0, *maximumProbeBytes,
		)
		files[probe+".body"] = body
		files[probe+".status"] = []byte(status + "\n")
	}
	timingBody, timingStatus := diagnosticProbe(
		ctx, &client, baseURL, "v1/timings", token,
		int64(*timingWindow/time.Second), *maximumProbeBytes,
	)
	files["timing-summary.json"] = timingBody
	files["timing-summary.status"] = []byte(timingStatus + "\n")
	egressBody, egressStatus := diagnosticProbe(
		ctx, &client, baseURL, "v1/diagnostics/egress-contexts", token, 0, *maximumProbeBytes,
	)
	files["egress-context-preflight.json"] = egressBody
	files["egress-context-preflight.status"] = []byte(egressStatus + "\n")
	logTail, err := readBoundedRegularFileTail(*controlPlaneLog, *maximumLogBytes)
	if err != nil {
		return err
	}
	files["control-plane.log.tail"] = logTail
	files["SHA256SUMS"] = diagnosticChecksums(files)
	if err := writeDiagnosticArchive(*outputPath, files); err != nil {
		return err
	}
	view := presentationFromContext(ctx, output)
	if view.renderer.HumanOutput() {
		return view.renderer.WriteSummary(cliui.Summary{Title: "Support bundle created", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Archive", Value: *outputPath}, {Key: "Files", Value: strconv.Itoa(len(files))}}, Next: "Keep the archive private; it contains bounded operational evidence."})
	}
	if _, err := fmt.Fprintf(output, "Created bounded support bundle: %s\n", *outputPath); err != nil {
		return fmt.Errorf("SecondBox diagnostics bundle result write failed: %w", err)
	}
	return nil
}

func runEgressContextDiagnosticsCommand(ctx context.Context, rawURL, token string, args []string, output io.Writer, httpClient *http.Client) error {
	if len(args) != 0 {
		return errors.New("SecondBox diagnostics egress-contexts accepts no arguments")
	}
	if rawURL == "" || token == "" {
		return errors.New("SecondBox diagnostics egress-contexts requires a platform session")
	}
	baseURL, err := diagnosticBaseURL(rawURL)
	if err != nil {
		return err
	}
	body, status := diagnosticProbe(ctx, httpClient, baseURL, "v1/diagnostics/egress-contexts", token, 0, maximumDiagnosticProbeBytes)
	if status != "200" {
		return fmt.Errorf("SecondBox diagnostics egress-contexts failed with status %s: %s", status, strings.TrimSpace(string(body)))
	}
	if _, err := output.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("SecondBox diagnostics egress-contexts output failed: %w", err)
	}
	return nil
}

func diagnosticBaseURL(rawURL string) (*url.URL, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" ||
		baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New(
			"SecondBox diagnostics URL must be absolute without query or fragment",
		)
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, errors.New("SecondBox diagnostics URL scheme must be http or https")
	}
	return baseURL, nil
}

func diagnosticProbe(
	ctx context.Context,
	client *http.Client,
	baseURL *url.URL,
	path string,
	token string,
	windowSeconds int64,
	maximumBytes int64,
) ([]byte, string) {
	endpoint := baseURL.ResolveReference(&url.URL{Path: "/" + path})
	if windowSeconds > 0 {
		query := make(url.Values)
		query.Set("windowSeconds", strconv.FormatInt(windowSeconds, 10))
		endpoint.RawQuery = query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, "request_error"
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-SecondBox-Tenant-Ref", "secondbox")
		request.Header.Set("X-SecondBox-Subject-Ref", "secondbox-admin")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "transport_error"
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return nil, "response_error"
	}
	if int64(len(body)) > maximumBytes {
		return body[:maximumBytes], "truncated"
	}
	return body, strconv.Itoa(response.StatusCode)
}

func readBoundedRegularFileTail(path string, maximumBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("SecondBox diagnostics control-plane log stat failed: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New(
			"SecondBox diagnostics control-plane log must be a non-symbolic-link regular file",
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("SecondBox diagnostics control-plane log open failed: %w", err)
	}
	offset := max(info.Size()-maximumBytes, 0)
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, errors.Join(
			fmt.Errorf("SecondBox diagnostics control-plane log seek failed: %w", err),
			file.Close(),
		)
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maximumBytes))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("SecondBox diagnostics control-plane log read failed: %w", err)
	}
	return content, nil
}

func diagnosticChecksums(files map[string][]byte) []byte {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var checksums strings.Builder
	for _, name := range names {
		digest := sha256.Sum256(files[name])
		_, _ = fmt.Fprintf(&checksums, "%x  %s\n", digest, name)
	}
	return []byte(checksums.String())
}

func writeDiagnosticArchive(path string, files map[string][]byte) (returnErr error) {
	archive, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("SecondBox diagnostics bundle create failed: %w", err)
	}
	compressor := gzip.NewWriter(archive)
	tarWriter := tar.NewWriter(compressor)
	defer func() {
		closeErr := errors.Join(tarWriter.Close(), compressor.Close(), archive.Close())
		if returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("SecondBox diagnostics bundle close failed: %w", closeErr)
		}
		if returnErr != nil {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				returnErr = errors.Join(
					returnErr,
					fmt.Errorf("SecondBox diagnostics partial bundle removal failed: %w", removeErr),
				)
			}
		}
	}()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := files[name]
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(content)),
		}); err != nil {
			return fmt.Errorf("SecondBox diagnostics bundle header failed: %w", err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			return fmt.Errorf("SecondBox diagnostics bundle content failed: %w", err)
		}
	}
	return nil
}
