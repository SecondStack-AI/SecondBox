package materialization

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const flatRootDigestVersion = "secondbox.flat-root-digest/v1"

// DigestFlatRoot returns the content identity of a pre-materialized Unix flat
// root. The identity covers names, entry kinds, ownership, modes, modification
// times, symlink targets, extended attributes, and regular-file bytes.
func DigestFlatRoot(root string) (string, error) {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("SecondBox flat root inspect: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("SecondBox flat root is not a directory")
	}
	digest := sha256.New()
	writeDigestBytes(digest, []byte(flatRootDigestVersion))
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative != "." && (relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return fmt.Errorf("entry escapes flat root")
		}
		return digestFlatRootEntry(digest, path, filepath.ToSlash(relative), entry)
	})
	if err != nil {
		return "", fmt.Errorf("SecondBox flat root digest: %w", err)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func digestFlatRootEntry(digest hash.Hash, path, relative string, entry os.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	writeDigestBytes(digest, []byte(relative))
	entryType := byte(0)
	switch {
	case info.IsDir():
		entryType = 'd'
	case info.Mode().IsRegular():
		entryType = 'f'
	case info.Mode()&os.ModeSymlink != 0:
		entryType = 'l'
	default:
		return fmt.Errorf("unsupported entry type %s", path)
	}
	digest.Write([]byte{entryType})
	writeDigestUint64(digest, flatRootMode(info.Mode()))
	uid, gid, err := flatRootOwner(info)
	if err != nil {
		return fmt.Errorf("inspect owner %s: %w", path, err)
	}
	writeDigestUint64(digest, uint64(uid))
	writeDigestUint64(digest, uint64(gid))
	writeDigestUint64(digest, uint64(info.ModTime().Unix()))
	writeDigestUint64(digest, uint64(info.ModTime().Nanosecond()))
	xattrs, err := flatRootXattrs(path)
	if err != nil {
		return fmt.Errorf("inspect xattrs %s: %w", path, err)
	}
	names := make([]string, 0, len(xattrs))
	for name := range xattrs {
		names = append(names, name)
	}
	sort.Strings(names)
	writeDigestUint64(digest, uint64(len(names)))
	for _, name := range names {
		writeDigestBytes(digest, []byte(name))
		writeDigestBytes(digest, xattrs[name])
	}
	switch entryType {
	case 'f':
		writeDigestUint64(digest, uint64(info.Size()))
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(digest, file, info.Size())
		var extra [1]byte
		extraCount, extraErr := file.Read(extra[:])
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if extraCount != 0 || extraErr != io.EOF {
			return fmt.Errorf("file changed while hashing %s", path)
		}
		if closeErr != nil {
			return closeErr
		}
	case 'l':
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		writeDigestBytes(digest, []byte(target))
	}
	return nil
}

func flatRootMode(mode os.FileMode) uint64 {
	result := uint64(mode.Perm())
	for bit, encoded := range map[os.FileMode]uint64{
		os.ModeSetuid: 1 << 16,
		os.ModeSetgid: 1 << 17,
		os.ModeSticky: 1 << 18,
	} {
		if mode&bit != 0 {
			result |= encoded
		}
	}
	return result
}

func writeDigestBytes(digest hash.Hash, value []byte) {
	writeDigestUint64(digest, uint64(len(value)))
	_, _ = digest.Write(value)
}

func writeDigestUint64(digest hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = digest.Write(encoded[:])
}
