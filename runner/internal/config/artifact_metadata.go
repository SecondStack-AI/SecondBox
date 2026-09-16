package config

import (
	"fmt"
	"io"
	"os"
)

const MaximumArtifactManifestBytes int64 = 1 << 20
const MaximumArtifactSignatureBytes int64 = 16 << 10

// ReadArtifactMetadata bounds memory even if an untrusted file grows during the read.
func ReadArtifactMetadata(path string, maximumBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maximumBytes {
		return nil, fmt.Errorf("SecondBox artifact metadata must be a regular file of at most %d bytes: %s", maximumBytes, path)
	}
	content, err := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maximumBytes {
		return nil, fmt.Errorf("SecondBox artifact metadata exceeds %d bytes: %s", maximumBytes, path)
	}
	return content, nil
}
