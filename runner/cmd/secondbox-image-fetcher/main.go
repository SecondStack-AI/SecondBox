package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/SecondStack-AI/SecondBox/runner/internal/config"
	"github.com/SecondStack-AI/SecondBox/runner/internal/executionimage"
)

func main() {
	if err := runImageFetcher(); err != nil {
		slog.Error("SecondBox image fetcher stopped", "error", err)
		os.Exit(1)
	}
}

func runImageFetcher() error {
	values := make(map[string]string)
	for _, key := range []string{"SOCKET", "TENANTS", "CACHE_ROOT", "REGISTRIES", "CERTIFICATES", "PUBLIC_KEY", "PUBLIC_KEY_SHA256", "MAX_DOWNLOAD_BYTES", "MAX_EXPANDED_BYTES", "MAX_CACHE_BYTES"} {
		value := os.Getenv("SECONDBOX_IMAGE_FETCHER_" + key)
		if value == "" {
			return fmt.Errorf("SecondBox image fetcher required configuration is missing: %s", key)
		}
		values[key] = value
	}
	limits := make(map[string]int64)
	for _, key := range []string{"MAX_DOWNLOAD_BYTES", "MAX_EXPANDED_BYTES", "MAX_CACHE_BYTES"} {
		value, err := strconv.ParseInt(values[key], 10, 64)
		if err != nil || value <= 0 {
			return fmt.Errorf("SecondBox image fetcher requires a positive byte limit: %s", key)
		}
		limits[key] = value
	}
	service, err := executionimage.NewFetchService(&config.Config{
		ExecutionImageCacheRoot: values["CACHE_ROOT"], ExecutionImageRegistryAllowlist: strings.Split(values["REGISTRIES"], ","),
		ExecutionImageRegistryCertificates: values["CERTIFICATES"], ExecutionImagePublicKeyPath: values["PUBLIC_KEY"], ExecutionImagePublicKeySHA256: values["PUBLIC_KEY_SHA256"],
		ExecutionImageMaximumDownloadBytes: limits["MAX_DOWNLOAD_BYTES"], ExecutionImageMaximumExpandedBytes: limits["MAX_EXPANDED_BYTES"], ExecutionImageMaximumCacheBytes: limits["MAX_CACHE_BYTES"],
	}, values["TENANTS"])
	if err != nil {
		return err
	}
	if !filepath.IsAbs(values["SOCKET"]) {
		return errors.New("SecondBox image fetcher socket must be absolute")
	}
	lock, err := os.OpenFile(values["SOCKET"]+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("SecondBox image fetcher socket is owned by another process: %w", err)
	}
	if info, err := os.Lstat(values["SOCKET"]); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("SecondBox image fetcher socket path contains a non-socket file")
		}
		if err := os.Remove(values["SOCKET"]); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", values["SOCKET"])
	if err != nil {
		return fmt.Errorf("SecondBox image fetcher listen: %w", err)
	}
	defer listener.Close()
	if err := os.Chmod(values["SOCKET"], 0o600); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	server := &http.Server{Handler: service, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: time.Hour, MaxHeaderBytes: 4096, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { <-ctx.Done(); _ = server.Close() }()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
