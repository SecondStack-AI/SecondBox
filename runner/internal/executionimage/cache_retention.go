package executionimage

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Retain bytes between preparation and assignment without a second job store.
// Callers hold the digest lock for both retention changes and eviction.
func retainPreparedDirectory(directory string, deadline time.Time) error {
	current, err := preparedDirectoryDeadline(directory)
	if err != nil {
		return err
	}
	if current.After(deadline) {
		return nil
	}
	return os.WriteFile(filepath.Join(directory, ".prepare-until"), []byte(strconv.FormatInt(deadline.UnixMilli(), 10)), 0o600)
}

func preparedDirectoryDeadline(directory string) (time.Time, error) {
	file, err := os.Open(filepath.Join(directory, ".prepare-until"))
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, 32))
	if err != nil {
		return time.Time{}, err
	}
	milliseconds, err := strconv.ParseInt(string(content), 10, 64)
	if err != nil {
		return time.Time{}, errors.New("SecondBox prepared image retention is invalid")
	}
	return time.UnixMilli(milliseconds), nil
}
