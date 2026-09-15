package workspacestore

import (
	"math"
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
