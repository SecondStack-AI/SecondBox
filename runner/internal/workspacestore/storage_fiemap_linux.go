package workspacestore

import (
	"errors"
	"math"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	workspaceFiemapExtentLimit = 4096
	fiemapExtentLast           = 0x00000001
	fiemapExtentUnknown        = 0x00000002
	fiemapExtentDelalloc       = 0x00000004
	fiemapExtentEncoded        = 0x00000008
	fiemapExtentShared         = 0x00002000
)

// These layouts are the Linux UAPI struct fiemap and struct fiemap_extent.
type workspaceFiemapExtent struct {
	Logical    uint64
	Physical   uint64
	Length     uint64
	Reserved64 [2]uint64
	Flags      uint32
	Reserved   [3]uint32
}

type workspaceFiemap struct {
	Start         uint64
	Length        uint64
	Flags         uint32
	MappedExtents uint32
	ExtentCount   uint32
	Reserved      uint32
	Extents       [workspaceFiemapExtentLimit]workspaceFiemapExtent
}

func observeExclusiveBytes(file *os.File) (*int64, string) {
	return measureWorkspaceExclusiveBytes(func(mapping *workspaceFiemap) error {
		// FS_IOC_FIEMAP = _IOWR('f', 11, struct fiemap), whose header is 32 bytes.
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), 0xc020660b, uintptr(unsafe.Pointer(mapping)))
		if errno != 0 {
			return errno
		}
		return nil
	})
}

func measureWorkspaceExclusiveBytes(probe func(*workspaceFiemap) error) (*int64, string) {
	// No FIEMAP_FLAG_SYNC: observing metadata must never flush guest writes.
	mapping := workspaceFiemap{Length: math.MaxUint64, ExtentCount: workspaceFiemapExtentLimit}
	if err := probe(&mapping); err != nil {
		if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, "fiemap_unsupported"
		}
		return nil, "exclusive_probe_failed"
	}
	if mapping.MappedExtents > workspaceFiemapExtentLimit {
		return nil, "exclusive_probe_failed"
	}
	extents := mapping.Extents[:mapping.MappedExtents]
	if len(extents) != 0 && extents[len(extents)-1].Flags&fiemapExtentLast == 0 {
		return nil, "exclusive_extent_limit"
	}
	var exclusive int64
	var end uint64
	for _, extent := range extents {
		// Delayed allocations have no physical extents yet. Do not invent a
		// reclaimable-byte count or force writeback to resolve them.
		if extent.Flags&(fiemapExtentUnknown|fiemapExtentDelalloc) != 0 {
			return nil, "exclusive_extents_unstable"
		}
		if extent.Length == 0 || extent.Logical < end || extent.Length > math.MaxUint64-extent.Logical {
			return nil, "exclusive_probe_failed"
		}
		end = extent.Logical + extent.Length
		// Unwritten extents are allocated and must count unless shared.
		if extent.Flags&fiemapExtentShared == 0 {
			// Encoded (including compressed) extent lengths are logical bytes;
			// FIEMAP does not expose their reclaimable physical allocation.
			if extent.Flags&fiemapExtentEncoded != 0 {
				return nil, "exclusive_extents_encoded"
			}
			if extent.Length > uint64(math.MaxInt64-exclusive) {
				return nil, "exclusive_probe_failed"
			}
			exclusive += int64(extent.Length)
		}
	}
	return &exclusive, ""
}
