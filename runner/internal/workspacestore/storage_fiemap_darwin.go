package workspacestore

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func observeExclusiveBytes(_ *os.File) (*int64, string) {
	return nil, "fiemap_unsupported"
}

// platformRangeAllocated reports whether the range holds data. APFS exposes no
// unwritten extents, so SEEK_DATA is an allocation query there.
func platformRangeAllocated(file *os.File, offset int64, length int64) (bool, error) {
	data, err := unix.Seek(int(file.Fd()), offset, unix.SEEK_DATA)
	if errors.Is(err, unix.ENXIO) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return data < offset+length, nil
}
