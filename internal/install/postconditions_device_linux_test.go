//go:build linux

package install

import (
	"errors"
	"testing"
)

func TestWorkspaceFilesystemIdentityUsesStableOnDiskUUID(t *testing.T) {
	uuid := [16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}
	tests := []struct {
		name       string
		magic      uint64
		request    uintptr
		bufferSize int
		uuidOffset int
		want       string
	}{
		{"btrfs", btrfsSuperMagic, btrfsIOCFSInfo, 1024, 16, "btrfs-uuid:01234567-89ab-cdef-0123-456789abcdef"},
		{"xfs", xfsSuperMagic, xfsIOCFsGeometryV4, 112, 64, "xfs-uuid:01234567-89ab-cdef-0123-456789abcdef"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity, err := workspaceFilesystemIdentityForType(test.magic, func(request uintptr, buffer []byte) error {
				if request != test.request || len(buffer) != test.bufferSize {
					t.Fatalf("ioctl request = %#x buffer = %d, want %#x and %d", request, len(buffer), test.request, test.bufferSize)
				}
				copy(buffer[test.uuidOffset:], uuid[:])
				return nil
			})
			if err != nil || identity != test.want {
				t.Fatalf("identity = %q, %v; want %q", identity, err, test.want)
			}
		})
	}
}

func TestWorkspaceFilesystemIdentityRejectsUnsupportedOrEmptyIdentity(t *testing.T) {
	if _, err := workspaceFilesystemIdentityForType(0x01021994, func(uintptr, []byte) error { return nil }); err == nil {
		t.Fatal("tmpfs identity was accepted")
	}
	if _, err := workspaceFilesystemIdentityForType(btrfsSuperMagic, func(uintptr, []byte) error { return nil }); err == nil {
		t.Fatal("empty Btrfs UUID was accepted")
	}
	injected := errors.New("injected ioctl failure")
	if _, err := workspaceFilesystemIdentityForType(xfsSuperMagic, func(uintptr, []byte) error { return injected }); !errors.Is(err, injected) {
		t.Fatalf("ioctl error = %v", err)
	}
}
