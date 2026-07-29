package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func runLogsCommand(
	ctx context.Context,
	command string,
	args []string,
	output io.Writer,
) error {
	flags := flag.NewFlagSet("logs "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("path", "", "absolute control-plane log path")
	initialBytes := flags.Int64("bytes", 0, "maximum initial tail bytes")
	pollInterval := flags.Duration("poll-interval", 0, "replacement and truncation poll interval")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("SecondBox logs %s options: %w", command, err)
	}
	if len(flags.Args()) != 0 {
		return fmt.Errorf("SecondBox logs %s received unexpected arguments", command)
	}
	if !filepath.IsAbs(*path) {
		return fmt.Errorf("SecondBox logs %s --path must be absolute", command)
	}
	if *initialBytes < 1 || *initialBytes > 100<<20 {
		return fmt.Errorf(
			"SecondBox logs %s --bytes must be from 1 through 104857600",
			command,
		)
	}
	switch command {
	case "tail":
		if *pollInterval != 0 {
			return errors.New("SecondBox logs tail does not accept --poll-interval")
		}
		return writeLogTail(*path, *initialBytes, output)
	case "follow":
		if *pollInterval < 10*time.Millisecond || *pollInterval > time.Minute {
			return errors.New(
				"SecondBox logs follow --poll-interval must be from 10ms through 1m",
			)
		}
		return followLog(ctx, *path, *initialBytes, *pollInterval, output)
	default:
		return fmt.Errorf("SecondBox logs command is invalid: %s", command)
	}
}

func writeLogTail(path string, maximumBytes int64, output io.Writer) error {
	file, info, err := openRegularLog(path)
	if err != nil {
		return err
	}
	offset := max(info.Size()-maximumBytes, 0)
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return errors.Join(
			fmt.Errorf("SecondBox logs tail seek failed: %w", err),
			file.Close(),
		)
	}
	_, copyErr := io.CopyN(output, file, min(maximumBytes, info.Size()-offset))
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("SecondBox logs tail read failed: %w", err)
	}
	return nil
}

func followLog(
	ctx context.Context,
	path string,
	initialBytes int64,
	pollInterval time.Duration,
	output io.Writer,
) error {
	file, info, err := openRegularLog(path)
	if err != nil {
		return err
	}
	offset := max(info.Size()-initialBytes, 0)
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return errors.Join(
			fmt.Errorf("SecondBox logs follow initial seek failed: %w", err),
			file.Close(),
		)
	}
	if _, err := io.Copy(output, file); err != nil {
		return errors.Join(
			fmt.Errorf("SecondBox logs follow initial read failed: %w", err),
			file.Close(),
		)
	}
	position, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return errors.Join(
			fmt.Errorf("SecondBox logs follow position failed: %w", err),
			file.Close(),
		)
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), file.Close())
		case <-ticker.C:
		}
		currentPathInfo, err := os.Lstat(path)
		if err != nil {
			return errors.Join(
				fmt.Errorf("SecondBox logs follow replacement stat failed: %w", err),
				file.Close(),
			)
		}
		currentFileInfo, err := file.Stat()
		if err != nil {
			return errors.Join(
				fmt.Errorf("SecondBox logs follow open-file stat failed: %w", err),
				file.Close(),
			)
		}
		if currentPathInfo.Mode()&os.ModeSymlink != 0 ||
			!currentPathInfo.Mode().IsRegular() {
			return errors.Join(
				errors.New("SecondBox logs follow path became non-regular or symbolic"),
				file.Close(),
			)
		}
		if !os.SameFile(currentPathInfo, currentFileInfo) {
			replacement, replacementInfo, err := openRegularLog(path)
			if err != nil {
				return errors.Join(err, file.Close())
			}
			if err := file.Close(); err != nil {
				return errors.Join(
					fmt.Errorf("SecondBox logs follow replaced-file close failed: %w", err),
					replacement.Close(),
				)
			}
			file = replacement
			position = 0
			currentFileInfo = replacementInfo
		}
		if currentFileInfo.Size() < position {
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return errors.Join(
					fmt.Errorf("SecondBox logs follow truncation seek failed: %w", err),
					file.Close(),
				)
			}
			position = 0
		}
		if currentFileInfo.Size() == position {
			continue
		}
		written, err := io.CopyN(output, file, currentFileInfo.Size()-position)
		position += written
		if err != nil {
			return errors.Join(
				fmt.Errorf("SecondBox logs follow read failed: %w", err),
				file.Close(),
			)
		}
	}
}

func openRegularLog(path string) (*os.File, os.FileInfo, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox logs path stat failed: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, nil, errors.New(
			"SecondBox logs path must be a non-symbolic-link regular file",
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox logs path open failed: %w", err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(
			fmt.Errorf("SecondBox logs open-file stat failed: %w", err),
			file.Close(),
		)
	}
	if !fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) {
		return nil, nil, errors.Join(
			errors.New("SecondBox logs path changed during secure open"),
			file.Close(),
		)
	}
	return file, fileInfo, nil
}
