package workspacestore

import (
	"bytes"
	"math"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestWorkspaceExclusiveBytesFiemap(t *testing.T) {
	if unsafe.Offsetof(workspaceFiemap{}.Extents) != 32 || unsafe.Sizeof(workspaceFiemapExtent{}) != 56 {
		t.Fatal("FIEMAP layout differs from Linux UAPI")
	}
	for _, test := range []struct {
		name    string
		extents []workspaceFiemapExtent
		err     error
		want    int64
		reason  string
	}{
		{name: "holes only"},
		{name: "mixed shared and unwritten", extents: []workspaceFiemapExtent{
			{Logical: 0, Length: 4096, Flags: fiemapExtentShared},
			{Logical: 8192, Length: 4096},
			{Logical: 12288, Length: 8192, Flags: 0x800},
			{Logical: 20480, Length: 4096, Flags: fiemapExtentShared | 0x800 | fiemapExtentLast},
		}, want: 12288},
		{name: "all shared is zero", extents: []workspaceFiemapExtent{{Length: 4096, Flags: fiemapExtentShared | fiemapExtentLast}}},
		{name: "no shared flag", extents: []workspaceFiemapExtent{{Length: 8192, Flags: fiemapExtentLast}}, want: 8192},
		{name: "unsupported ioctl", err: unix.ENOTTY, reason: "fiemap_unsupported"},
		{name: "unsupported filesystem", err: unix.EOPNOTSUPP, reason: "fiemap_unsupported"},
		{name: "io failure", err: unix.EIO, reason: "exclusive_probe_failed"},
		{name: "incomplete is not partial", extents: []workspaceFiemapExtent{{Length: 4096}}, reason: "exclusive_extent_limit"},
		{name: "delayed allocation is not partial", extents: []workspaceFiemapExtent{{Length: 4096}, {Logical: 4096, Length: 4096, Flags: fiemapExtentDelalloc | fiemapExtentLast}}, reason: "exclusive_extents_unstable"},
		{name: "unknown", extents: []workspaceFiemapExtent{{Length: 4096, Flags: fiemapExtentUnknown | fiemapExtentLast}}, reason: "exclusive_extents_unstable"},
		{name: "overflow", extents: []workspaceFiemapExtent{{Length: math.MaxUint64, Flags: fiemapExtentLast}}, reason: "exclusive_probe_failed"},
		{name: "encoded is not physical bytes", extents: []workspaceFiemapExtent{{Length: 4096}, {Logical: 4096, Length: 65536, Flags: fiemapExtentEncoded | fiemapExtentLast}}, reason: "exclusive_extents_encoded"},
		{name: "shared encoded contributes zero", extents: []workspaceFiemapExtent{{Length: 65536, Flags: fiemapExtentShared | fiemapExtentEncoded | fiemapExtentLast}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, reason := measureWorkspaceExclusiveBytes(func(mapping *workspaceFiemap) error {
				if mapping.Flags != 0 || mapping.Start != 0 || mapping.Length != math.MaxUint64 || mapping.ExtentCount != workspaceFiemapExtentLimit {
					t.Fatal("FIEMAP must request the whole file without writeback and with bounded extent count")
				}
				mapping.MappedExtents = uint32(len(test.extents))
				copy(mapping.Extents[:], test.extents)
				return test.err
			})
			if reason != test.reason || (reason != "" && got != nil) || (reason == "" && (got == nil || *got != test.want)) {
				t.Fatalf("exclusive = %v, reason = %q; want %d, %q", got, reason, test.want, test.reason)
			}
		})
	}
}

func TestWorkspaceExclusiveBytesRealCompressedBtrfs(t *testing.T) {
	parent := os.Getenv("SECONDBOX_WORKSPACESTORE_COMPRESSED_QUALIFICATION_FILESYSTEM")
	if parent == "" {
		t.Skip("compressed btrfs qualification filesystem must be explicit")
	}
	file, err := os.CreateTemp(parent, "secondbox-compressed-observation-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Remove(file.Name()); err != nil {
			t.Error(err)
		}
	}()
	defer file.Close()
	if _, err := file.Write(bytes.Repeat([]byte("compressed Workspace observation\n"), 8192)); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	allocated := info.Sys().(*syscall.Stat_t).Blocks * 512
	exclusive, reason := observeExclusiveBytes(file)
	if exclusive != nil || reason != "exclusive_extents_encoded" {
		t.Fatalf("compressed extent result=%v reason=%q", exclusive, reason)
	}
	t.Logf("compressed file: logicalBytes=%d allocatedBytes=%d exclusiveReason=%s", info.Size(), allocated, reason)
}
