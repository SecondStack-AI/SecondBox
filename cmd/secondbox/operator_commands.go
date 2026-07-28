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
)

const (
	maximumOperationalLogBytes = int64(100 * 1024 * 1024)
	maximumProbeBodyBytes      = int64(1024 * 1024)
	maximumProbeTimeout        = 60 * time.Second
)

func runOperationalCommand(
	ctx context.Context,
	rawURL string,
	token string,
	args []string,
	output io.Writer,
) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "sandbox":
		if len(args) > 1 && args[1] == "shell" {
			return true, runSandboxShellCommand(
				ctx, rawURL, token, args[2:],
				sandboxShellEnvironment{
					input: os.Stdin, output: output,
					inputFD: int(os.Stdin.Fd()), outputFD: outputFileDescriptor(output),
					httpClient: http.DefaultClient,
				},
			)
		}
		return false, nil
	case "exec":
		if len(args) > 1 && args[1] == "stream" {
			return true, runExecStreamCommand(
				ctx, rawURL, token, args[2:], os.Stdin, output,
				http.DefaultClient, nil,
			)
		}
		return false, nil
	case "logs":
		return true, runLogsCommand(ctx, args[1:], output)
	case "diagnostics":
		return true, runDiagnosticsCommand(ctx, rawURL, args[1:], output)
	default:
		return false, nil
	}
}

func outputFileDescriptor(output io.Writer) int {
	file, ok := output.(*os.File)
	if !ok {
		return -1
	}
	return int(file.Fd())
}

func runLogsCommand(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("SecondBox CLI logs requires tail or follow")
	}
	switch args[0] {
	case "tail":
		options, err := parseLogOptions("logs tail", args[1:], false)
		if err != nil {
			return err
		}
		file, _, err := copyLogTail(options.path, options.maximumBytes, output)
		if err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("SecondBox CLI close log %q: %w", options.path, err)
		}
		return nil
	case "follow":
		options, err := parseLogOptions("logs follow", args[1:], true)
		if err != nil {
			return err
		}
		return followLog(ctx, options, output)
	default:
		return fmt.Errorf("SecondBox CLI logs unknown command %q; available commands: follow, tail", args[0])
	}
}

type logOptions struct {
	path         string
	maximumBytes int64
	pollInterval time.Duration
}

func parseLogOptions(command string, args []string, follow bool) (logOptions, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("path", "", "absolute control-plane JSON log path")
	byteText := flags.String("bytes", "", "maximum initial tail bytes")
	pollText := flags.String("poll-interval", "", "positive follow polling interval")
	if err := flags.Parse(args); err != nil {
		return logOptions{}, fmt.Errorf("SecondBox CLI parse %s options: %w", command, err)
	}
	if len(flags.Args()) != 0 {
		return logOptions{}, fmt.Errorf("SecondBox CLI unexpected %s arguments: %s", command, strings.Join(flags.Args(), " "))
	}
	if !filepath.IsAbs(*path) {
		return logOptions{}, fmt.Errorf("SecondBox CLI %s --path must be absolute", command)
	}
	maximumBytes, err := parseBoundedPositiveInteger(
		command+" --bytes", *byteText, maximumOperationalLogBytes,
	)
	if err != nil {
		return logOptions{}, err
	}
	options := logOptions{path: *path, maximumBytes: maximumBytes}
	if !follow {
		if *pollText != "" {
			return logOptions{}, fmt.Errorf("SecondBox CLI %s does not accept --poll-interval", command)
		}
		return options, nil
	}
	if *pollText == "" {
		return logOptions{}, fmt.Errorf("SecondBox CLI %s requires --poll-interval", command)
	}
	options.pollInterval, err = time.ParseDuration(*pollText)
	if err != nil || options.pollInterval <= 0 {
		return logOptions{}, fmt.Errorf("SecondBox CLI %s --poll-interval must be a positive duration", command)
	}
	return options, nil
}

func copyLogTail(path string, maximumBytes int64, output io.Writer) (*os.File, os.FileInfo, error) {
	file, info, err := openVerifiedRegularFile(path, "log")
	if err != nil {
		return nil, nil, err
	}
	start := info.Size() - maximumBytes
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, nil, errors.Join(
			fmt.Errorf("SecondBox CLI seek log %q: %w", path, err),
			file.Close(),
		)
	}
	if _, err := io.CopyN(output, file, info.Size()-start); err != nil {
		return nil, nil, errors.Join(
			fmt.Errorf("SecondBox CLI read log %q: %w", path, err),
			file.Close(),
		)
	}
	return file, info, nil
}

func followLog(ctx context.Context, options logOptions, output io.Writer) (resultErr error) {
	file, openedInfo, err := copyLogTail(options.path, options.maximumBytes, output)
	if err != nil {
		return err
	}
	defer func() {
		if file != nil {
			resultErr = errors.Join(resultErr, file.Close())
		}
	}()
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("SecondBox CLI determine log offset %q: %w", options.path, err)
	}
	ticker := time.NewTicker(options.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			pathInfo, err := os.Lstat(options.path)
			if err != nil {
				return fmt.Errorf("SecondBox CLI stat followed log %q: %w", options.path, err)
			}
			if pathInfo.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("SecondBox CLI followed log %q must not be a symbolic link", options.path)
			}
			if !pathInfo.Mode().IsRegular() {
				return fmt.Errorf("SecondBox CLI followed log %q must remain a regular file", options.path)
			}
			if !os.SameFile(openedInfo, pathInfo) {
				rotatedFile := file
				file = nil
				if err := rotatedFile.Close(); err != nil {
					return fmt.Errorf("SecondBox CLI close rotated log %q: %w", options.path, err)
				}
				file, openedInfo, err = openVerifiedRegularFile(options.path, "rotated log")
				if err != nil {
					return fmt.Errorf("SecondBox CLI open rotated log %q: %w", options.path, err)
				}
				offset = 0
			} else if pathInfo.Size() < offset {
				if _, err := file.Seek(0, io.SeekStart); err != nil {
					return fmt.Errorf("SecondBox CLI seek truncated log %q: %w", options.path, err)
				}
				offset = 0
			}
			available := pathInfo.Size() - offset
			if available == 0 {
				continue
			}
			if _, err := io.CopyN(output, file, available); err != nil {
				return fmt.Errorf("SecondBox CLI follow log %q: %w", options.path, err)
			}
			offset += available
		}
	}
}

func runDiagnosticsCommand(
	ctx context.Context,
	rawURL string,
	args []string,
	output io.Writer,
) error {
	if len(args) == 0 || args[0] != "bundle" {
		return errors.New("SecondBox CLI diagnostics requires bundle")
	}
	flags := flag.NewFlagSet("diagnostics bundle", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	outputPath := flags.String("output", "", "support bundle output path")
	logPath := flags.String("control-plane-log", "", "absolute control-plane JSON log path")
	byteText := flags.String("max-log-bytes", "", "maximum log tail bytes")
	timeoutText := flags.String("http-timeout", "", "probe timeout")
	if err := flags.Parse(args[1:]); err != nil {
		return fmt.Errorf("SecondBox CLI parse diagnostics bundle options: %w", err)
	}
	if len(flags.Args()) != 0 {
		return fmt.Errorf("SecondBox CLI unexpected diagnostics bundle arguments: %s", strings.Join(flags.Args(), " "))
	}
	if rawURL == "" {
		return errors.New("SecondBox CLI diagnostics bundle requires --url")
	}
	baseURL, err := parseProbeBaseURL(rawURL)
	if err != nil {
		return err
	}
	if *outputPath == "" {
		return errors.New("SecondBox CLI diagnostics bundle requires --output")
	}
	if !filepath.IsAbs(*logPath) {
		return errors.New("SecondBox CLI diagnostics bundle --control-plane-log must be absolute")
	}
	maximumLogBytes, err := parseBoundedPositiveInteger(
		"diagnostics bundle --max-log-bytes", *byteText, maximumOperationalLogBytes,
	)
	if err != nil {
		return err
	}
	timeout, err := time.ParseDuration(*timeoutText)
	if err != nil || timeout <= 0 || timeout > maximumProbeTimeout {
		return errors.New("SecondBox CLI diagnostics bundle --http-timeout must be a positive duration no greater than 60s")
	}
	if _, err := os.Lstat(*outputPath); err == nil {
		return fmt.Errorf("SecondBox CLI refuses to overwrite diagnostic bundle %q", *outputPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("SecondBox CLI inspect diagnostic bundle output %q: %w", *outputPath, err)
	}

	files := make(map[string][]byte)
	client := &http.Client{Timeout: timeout}
	for _, probe := range []string{"healthz", "readyz", "metrics"} {
		probeBody, probeStatus := collectProbe(ctx, client, baseURL, probe)
		if probeBody != nil {
			files[probe+".body"] = probeBody
		}
		files[probe+".status"] = []byte(probeStatus + "\n")
	}
	logTail, err := readLogTail(*logPath, maximumLogBytes)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		files["control-plane.log.status"] = []byte("configured log file is unavailable\n")
	} else {
		files["control-plane.log.tail"] = logTail
	}
	files["SHA256SUMS"] = buildChecksums(files)
	if err := writeTarGzipExclusive(*outputPath, files); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Created bounded support bundle: %s\n", *outputPath)
	return err
}

func parseProbeBaseURL(rawURL string) (*url.URL, error) {
	baseURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox CLI parse diagnostic base URL: %w", err)
	}
	if (baseURL.Scheme != "http" && baseURL.Scheme != "https") ||
		baseURL.Host == "" ||
		baseURL.User != nil ||
		baseURL.RawQuery != "" ||
		baseURL.Fragment != "" {
		return nil, errors.New("SecondBox CLI diagnostic --url must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/")
	return baseURL, nil
}

func collectProbe(
	ctx context.Context,
	client *http.Client,
	baseURL *url.URL,
	name string,
) ([]byte, string) {
	probeURL := *baseURL
	probeURL.Path = strings.TrimRight(baseURL.Path, "/") + "/" + name
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL.String(), nil)
	if err != nil {
		return nil, "transport_error"
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "transport_error"
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumProbeBodyBytes+1))
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil {
		return nil, "transport_error"
	}
	if int64(len(body)) > maximumProbeBodyBytes {
		return body[:maximumProbeBodyBytes], fmt.Sprintf("%d body_truncated", response.StatusCode)
	}
	return body, strconv.Itoa(response.StatusCode)
}

func readLogTail(path string, maximumBytes int64) (contents []byte, resultErr error) {
	file, info, err := openVerifiedRegularFile(path, "diagnostic log")
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	start := info.Size() - maximumBytes
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, fmt.Errorf("SecondBox CLI seek diagnostic log %q: %w", path, err)
	}
	contents = make([]byte, info.Size()-start)
	if _, err := io.ReadFull(file, contents); err != nil {
		return nil, fmt.Errorf("SecondBox CLI read diagnostic log %q: %w", path, err)
	}
	return contents, nil
}

func openVerifiedRegularFile(path, purpose string) (*os.File, os.FileInfo, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox CLI inspect %s %q: %w", purpose, path, err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("SecondBox CLI %s %q must not be a symbolic link", purpose, path)
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("SecondBox CLI %s %q must be a regular file", purpose, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox CLI open %s %q: %w", purpose, path, err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(
			fmt.Errorf("SecondBox CLI inspect opened %s %q: %w", purpose, path, err),
			file.Close(),
		)
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, errors.Join(
			fmt.Errorf("SecondBox CLI recheck %s %q: %w", purpose, path, err),
			file.Close(),
		)
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 ||
		!currentInfo.Mode().IsRegular() ||
		!openedInfo.Mode().IsRegular() ||
		!os.SameFile(currentInfo, openedInfo) {
		return nil, nil, errors.Join(
			fmt.Errorf("SecondBox CLI %s %q changed during secure open", purpose, path),
			file.Close(),
		)
	}
	return file, openedInfo, nil
}

func buildChecksums(files map[string][]byte) []byte {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var checksums strings.Builder
	for _, name := range names {
		sum := sha256.Sum256(files[name])
		_, _ = fmt.Fprintf(&checksums, "%x  %s\n", sum, name)
	}
	return []byte(checksums.String())
}

func writeTarGzipExclusive(path string, files map[string][]byte) (resultErr error) {
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("SecondBox CLI create diagnostic bundle %q: %w", path, err)
	}
	defer func() {
		if closeErr := output.Close(); resultErr == nil && closeErr != nil {
			resultErr = fmt.Errorf("SecondBox CLI close diagnostic bundle %q: %w", path, closeErr)
		}
		if resultErr != nil {
			_ = os.Remove(path)
		}
	}()
	compressed := gzip.NewWriter(output)
	archive := tar.NewWriter(compressed)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		contents := files[name]
		header := &tar.Header{
			Name: name,
			Mode: 0o600,
			Size: int64(len(contents)),
		}
		if err := archive.WriteHeader(header); err != nil {
			return fmt.Errorf("SecondBox CLI write diagnostic bundle header %q: %w", name, err)
		}
		if _, err := archive.Write(contents); err != nil {
			return fmt.Errorf("SecondBox CLI write diagnostic bundle file %q: %w", name, err)
		}
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("SecondBox CLI finalize diagnostic bundle archive: %w", err)
	}
	if err := compressed.Close(); err != nil {
		return fmt.Errorf("SecondBox CLI finalize diagnostic bundle compression: %w", err)
	}
	return nil
}

func parseBoundedPositiveInteger(name, text string, maximum int64) (int64, error) {
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 1 || value > maximum {
		return 0, fmt.Errorf("SecondBox CLI %s must be from 1 through %d", name, maximum)
	}
	return value, nil
}
