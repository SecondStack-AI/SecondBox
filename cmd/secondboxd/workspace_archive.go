package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/internal/workspacearchive"
)

func configureWorkspaceArchives(ctx context.Context, path string, storage *store.PostgresControlPlaneStore) (workspacearchive.ObjectStoreConfig, error) {
	if path == "" {
		return workspacearchive.ObjectStoreConfig{}, nil
	}
	if !filepath.IsAbs(path) {
		return workspacearchive.ObjectStoreConfig{}, errors.New("SecondBox workspace archive configuration path must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return workspacearchive.ObjectStoreConfig{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return workspacearchive.ObjectStoreConfig{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return workspacearchive.ObjectStoreConfig{}, errors.New("SecondBox workspace archive configuration must be a regular file no larger than 64 KiB")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	var settings workspacearchive.ObjectStoreConfig
	if err := decoder.Decode(&settings); err != nil {
		return settings, fmt.Errorf("SecondBox workspace archive configuration: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return settings, errors.New("SecondBox workspace archive configuration must contain one JSON document")
	}
	provider, err := workspacearchive.NewS3TransferProvider(ctx, settings)
	if err != nil {
		return settings, err
	}
	storage.ConfigureWorkspaceArchives(provider)
	return settings, nil
}

func runWorkspaceArchiveWorker(ctx context.Context, storage *store.PostgresControlPlaneStore, settings workspacearchive.ObjectStoreConfig, logger *slog.Logger) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	delay := time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if err := storage.ReconcileWorkspaceArchives(ctx, time.Now().UTC(), settings.ProfileIdleSeconds); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Durable mutations stay pending; this worker retries the same authority.
			logger.ErrorContext(ctx, "SecondBox workspace archive reconciliation retry", "error", err, "retryAfter", delay)
			delay = min(30*time.Second, delay*2)
		} else {
			delay = time.Second
		}
		timer.Reset(delay)
	}
}
