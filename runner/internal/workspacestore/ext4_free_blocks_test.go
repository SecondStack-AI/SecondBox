//go:build linux

package workspacestore

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const ext4TestImageBytes = 512 << 20

type ext4TestFile struct {
	name string
	data []byte
}

func randomExt4TestFile(t *testing.T, name string, size int) ext4TestFile {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return ext4TestFile{name: name, data: data}
}

// makeExt4TestImage formats image with the production mke2fs options plus
// extra, populated with files, without mounting it.
func makeExt4TestImage(t *testing.T, image string, imageBytes int64, extra []string, files []ext4TestFile) {
	t.Helper()
	source := t.TempDir()
	for _, file := range files {
		path := filepath.Join(source, file.name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, file.data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(image, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(image, imageBytes); err != nil {
		t.Fatal(err)
	}
	arguments := append([]string{
		"-t", "ext4", "-F", "-q", "-L", workspaceFilesystemLabel,
		"-E", "lazy_itable_init=0,lazy_journal_init=0,nodiscard",
	}, extra...)
	arguments = append(arguments, "-d", source, image)
	runExt4TestTool(t, "mke2fs", arguments...)
}

func runExt4TestTool(t *testing.T, tool string, arguments ...string) string {
	t.Helper()
	output, err := exec.Command(tool, arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", tool, strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func debugfsWrite(t *testing.T, image string, commands ...string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "debugfs.cmd")
	if err := os.WriteFile(script, []byte(strings.Join(commands, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := runExt4TestTool(t, "debugfs", "-w", "-f", script, image)
	if strings.Contains(output, "debugfs: ") && regexp.MustCompile(`(?m)^\S+: .*(not found|error|Error)`).MatchString(output) {
		t.Fatalf("debugfs reported a failure: %s", output)
	}
}

func debugfsDump(t *testing.T, image string, name string) []byte {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "dump")
	runExt4TestTool(t, "debugfs", "-R", "dump "+name+" "+destination, image)
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("dump %s from %s: %v", name, image, err)
	}
	return data
}

func requireCleanExt4(t *testing.T, image string) {
	t.Helper()
	runExt4TestTool(t, "e2fsck", "-f", "-n", image)
}

func ext4TestAllocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Blocks * 512
}

var (
	dumpe2fsGroupPattern = regexp.MustCompile(`^Group (\d+): \(Blocks (\d+)-(\d+)\)(.*)$`)
	dumpe2fsFreePattern  = regexp.MustCompile(`^\s+Free blocks: ?(.*)$`)
)

// dumpe2fsFreeRanges is the independent oracle: libext2fs's view of the free
// blocks in each initialized group, merged the same way as ext4FreeRanges.
func dumpe2fsFreeRanges(t *testing.T, image string, honorUninit bool) []ext4ByteRange {
	t.Helper()
	output := runExt4TestTool(t, "dumpe2fs", image)
	blockBytes := int64(0)
	var ranges []ext4ByteRange
	skip := false
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(line, "Block size:"); ok {
			parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			blockBytes = parsed
		}
		if match := dumpe2fsGroupPattern.FindStringSubmatch(line); match != nil {
			skip = honorUninit && strings.Contains(match[4], "BLOCK_UNINIT")
			continue
		}
		match := dumpe2fsFreePattern.FindStringSubmatch(line)
		if match == nil || skip || strings.TrimSpace(match[1]) == "" {
			continue
		}
		for _, item := range strings.Split(match[1], ", ") {
			first, last, found := strings.Cut(item, "-")
			if !found {
				last = first
			}
			start, err := strconv.ParseInt(first, 10, 64)
			if err != nil {
				t.Fatalf("parse dumpe2fs range %q: %v", item, err)
			}
			end, err := strconv.ParseInt(last, 10, 64)
			if err != nil {
				t.Fatalf("parse dumpe2fs range %q: %v", item, err)
			}
			offset, length := start*blockBytes, (end-start+1)*blockBytes
			if previous := len(ranges) - 1; previous >= 0 &&
				ranges[previous].Offset+ranges[previous].Length == offset {
				ranges[previous].Length += length
				continue
			}
			ranges = append(ranges, ext4ByteRange{Offset: offset, Length: length})
		}
	}
	if blockBytes == 0 {
		t.Fatalf("dumpe2fs printed no block size: %s", output)
	}
	return ranges
}

func ext4FreeRangesOf(t *testing.T, image string) ([]ext4ByteRange, string) {
	t.Helper()
	file, err := os.Open(image)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	ranges, reason, err := ext4FreeRanges(file, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	return ranges, reason
}

func TestExt4FreeRangesMatchLibext2fsAcrossLayouts(t *testing.T) {
	for _, layout := range []struct {
		name        string
		options     []string
		honorUninit bool
	}{
		{name: "default metadata_csum 64bit", honorUninit: true},
		{name: "checksum seed", options: []string{"-O", "metadata_csum_seed"}, honorUninit: true},
		{name: "uninit_bg crc16 64bit", options: []string{"-O", "^metadata_csum,uninit_bg"}, honorUninit: true},
		{name: "uninit_bg crc16 32bit", options: []string{"-O", "^64bit,^metadata_csum,uninit_bg"}, honorUninit: true},
		{name: "no group checksums", options: []string{"-O", "^metadata_csum,^uninit_bg"}},
		{name: "1 KiB blocks", options: []string{"-b", "1024"}, honorUninit: true},
	} {
		t.Run(layout.name, func(t *testing.T) {
			image := filepath.Join(t.TempDir(), "workspace.ext4")
			files := []ext4TestFile{
				randomExt4TestFile(t, "retained.bin", 6<<20),
				randomExt4TestFile(t, "deleted.bin", 24<<20),
				randomExt4TestFile(t, "nested/deleted-small", 70<<10),
				randomExt4TestFile(t, "nested/retained-small", 9<<10),
			}
			for index := range 40 {
				files = append(files, randomExt4TestFile(t, fmt.Sprintf("many/%02d", index), 12<<10))
			}
			makeExt4TestImage(t, image, ext4TestImageBytes, layout.options, files)
			commands := []string{"rm /deleted.bin", "rm /nested/deleted-small"}
			for index := 0; index < 40; index += 3 {
				commands = append(commands, fmt.Sprintf("rm /many/%02d", index))
			}
			debugfsWrite(t, image, commands...)
			requireCleanExt4(t, image)

			actual, reason := ext4FreeRangesOf(t, image)
			if reason != "" {
				t.Fatalf("free ranges declined: %s", reason)
			}
			expected := dumpe2fsFreeRanges(t, image, layout.honorUninit)
			if fmt.Sprint(actual) != fmt.Sprint(expected) {
				t.Fatalf("free ranges disagree with libext2fs:\nactual   %v\nexpected %v", actual, expected)
			}
			var freeBytes int64
			for _, free := range actual {
				freeBytes += free.Length
			}
			if freeBytes < 24<<20 {
				t.Fatalf("free ranges cover %d bytes, want at least the deleted 24 MiB", freeBytes)
			}
		})
	}
}

func TestReclaimFreeImageBlocksReleasesOnlyFreedBlocks(t *testing.T) {
	image := filepath.Join(t.TempDir(), "workspace.ext4")
	retained := randomExt4TestFile(t, "retained.bin", 8<<20)
	deleted := randomExt4TestFile(t, "deleted.bin", 32<<20)
	makeExt4TestImage(t, image, ext4TestImageBytes, nil, []ext4TestFile{retained, deleted})
	written := ext4TestAllocatedBytes(t, image)
	debugfsWrite(t, image, "rm /deleted.bin")
	if afterDelete := ext4TestAllocatedBytes(t, image); afterDelete < written {
		t.Fatalf("deleting a file inside the image released host blocks by itself: %d -> %d", written, afterDelete)
	}

	file, err := os.OpenFile(image, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	result, err := reclaimFreeImageBlocks(file, ext4TestImageBytes, platformPunchHole)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != "" || result.Ranges == 0 {
		t.Fatalf("reclaim = %+v", result)
	}
	released := result.AllocatedBefore - result.AllocatedAfter
	if released < 30<<20 || released > 34<<20 {
		t.Fatalf("reclaim released %d bytes for a 32 MiB deletion: %+v", released, result)
	}
	t.Logf("reclaim: allocated %d -> %d bytes over %d ranges", result.AllocatedBefore, result.AllocatedAfter, result.Ranges)
	again, err := reclaimFreeImageBlocks(file, ext4TestImageBytes, platformPunchHole)
	if err != nil {
		t.Fatal(err)
	}
	if again.Reason != "" || again.AllocatedBefore != result.AllocatedAfter || again.AllocatedAfter != result.AllocatedAfter {
		t.Fatalf("second reclaim is not idempotent: %+v after %+v", again, result)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	requireCleanExt4(t, image)
	if !bytes.Equal(debugfsDump(t, image, "/retained.bin"), retained.data) {
		t.Fatal("retained file changed")
	}
}

func TestAlignWorkspaceReclaimRangesNeverPunchesPartialUnits(t *testing.T) {
	const unit = workspaceReclaimAlignmentBytes
	actual := alignWorkspaceReclaimRanges([]ext4ByteRange{
		{Offset: 4096, Length: workspaceReclaimMinimumBytes + unit},
		{Offset: 8 * unit, Length: workspaceReclaimMinimumBytes - 4096},
		{Offset: 64 * unit, Length: workspaceReclaimMinimumBytes},
		{Offset: 128*unit + 4096, Length: workspaceReclaimMinimumBytes},
	})
	expected := []ext4ByteRange{
		{Offset: unit, Length: workspaceReclaimMinimumBytes},
		{Offset: 64 * unit, Length: workspaceReclaimMinimumBytes},
	}
	if fmt.Sprint(actual) != fmt.Sprint(expected) {
		t.Fatalf("aligned ranges = %v, want %v", actual, expected)
	}
}

// ext4TestBlockBitmapOffset returns the byte offset of group 0's block bitmap.
func ext4TestBlockBitmapOffset(t *testing.T, image string) int64 {
	t.Helper()
	output := runExt4TestTool(t, "dumpe2fs", image)
	match := regexp.MustCompile(`(?m)^\s+Block bitmap at (\d+)`).FindStringSubmatch(output)
	size := regexp.MustCompile(`(?m)^Block size:\s+(\d+)`).FindStringSubmatch(output)
	if match == nil || size == nil {
		t.Fatalf("dumpe2fs printed no group 0 bitmap: %s", output)
	}
	block, _ := strconv.ParseInt(match[1], 10, 64)
	blockBytes, _ := strconv.ParseInt(size[1], 10, 64)
	return block * blockBytes
}

// markLastBitmapBlockUsed flips the final block of group 0 to "used" without
// updating the descriptor, as a misparse or a torn write would look.
func markLastBitmapBlockUsed(t *testing.T, image string) {
	t.Helper()
	file, err := os.OpenFile(image, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	offset := ext4TestBlockBitmapOffset(t, image) + 4096 - 1
	value := []byte{0}
	if _, err := file.ReadAt(value, offset); err != nil {
		t.Fatal(err)
	}
	if value[0]&0x80 != 0 {
		t.Fatal("group 0's final block is already used")
	}
	value[0] |= 0x80
	if _, err := file.WriteAt(value, offset); err != nil {
		t.Fatal(err)
	}
}

func TestExt4FreeRangesDeclineUntrustedAllocationState(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		options []string
		corrupt func(*testing.T, string)
		reason  string
	}{
		{
			name:    "journal awaits recovery",
			corrupt: func(t *testing.T, image string) { debugfsWrite(t, image, "feature needs_recovery") },
			reason:  ext4ReclaimNeedsRecovery,
		},
		{
			name:    "filesystem recorded errors",
			corrupt: func(t *testing.T, image string) { debugfsWrite(t, image, "ssv state 3") },
			reason:  ext4ReclaimFilesystemErrors,
		},
		{
			name:    "bitmap checksum mismatch",
			corrupt: markLastBitmapBlockUsed,
			reason:  ext4ReclaimChecksumMismatch,
		},
		{
			name:    "free count mismatch without checksums",
			options: []string{"-O", "^metadata_csum,^uninit_bg"},
			corrupt: markLastBitmapBlockUsed,
			reason:  ext4ReclaimFreeCountMismatch,
		},
		{
			name:    "bigalloc",
			options: []string{"-O", "bigalloc", "-C", "16384"},
			corrupt: func(*testing.T, string) {},
			reason:  ext4ReclaimFeaturesUnsupported,
		},
		{
			name: "superblock overwritten",
			corrupt: func(t *testing.T, image string) {
				file, err := os.OpenFile(image, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if _, err := file.WriteAt(make([]byte, 64), ext4SuperblockOffset); err != nil {
					t.Fatal(err)
				}
			},
			reason: ext4ReclaimSuperblockInvalid,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			image := filepath.Join(t.TempDir(), "workspace.ext4")
			makeExt4TestImage(t, image, ext4TestImageBytes, testCase.options, []ext4TestFile{
				randomExt4TestFile(t, "deleted.bin", 8<<20),
			})
			debugfsWrite(t, image, "rm /deleted.bin")
			testCase.corrupt(t, image)
			file, err := os.OpenFile(image, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			result, err := reclaimFreeImageBlocks(file, ext4TestImageBytes, func(*os.File, int64, int64) error {
				t.Fatal("declined reclaim punched the image")
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Reason != testCase.reason || result.AllocatedAfter != result.AllocatedBefore {
				t.Fatalf("reclaim = %+v, want reason %s and no change", result, testCase.reason)
			}
		})
	}
}

// FuzzExt4FreeRanges rewrites the superblock and group descriptors of a real
// image, recomputing the superblock checksum so mutations reach the geometry
// checks. Whatever the guest writes, the reader must not panic and must only
// return ordered ranges inside the image.
func FuzzExt4FreeRanges(f *testing.F) {
	const imageBytes = 64 << 20
	image := filepath.Join(f.TempDir(), "workspace.ext4")
	if err := os.WriteFile(image, nil, 0o600); err != nil {
		f.Fatal(err)
	}
	if err := os.Truncate(image, imageBytes); err != nil {
		f.Fatal(err)
	}
	if output, err := exec.Command("mke2fs", "-t", "ext4", "-F", "-q", "-b", "1024", image).CombinedOutput(); err != nil {
		f.Fatalf("mke2fs: %v: %s", err, output)
	}
	pristine, err := os.ReadFile(image)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(uint16(0), []byte{})
	f.Add(uint16(0x04), []byte{0xff, 0xff, 0xff, 0x7f})
	f.Add(uint16(0x20), []byte{0x01})
	f.Add(uint16(0xFE), []byte{0x00, 0x04})
	f.Add(uint16(1024+0x00), []byte{0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, offset uint16, patch []byte) {
		mutated := bytes.Clone(pristine)
		position := ext4SuperblockOffset + int(offset)%4096
		copy(mutated[position:min(position+len(patch), len(mutated))], patch)
		superblock := mutated[ext4SuperblockOffset : ext4SuperblockOffset+ext4SuperblockBytes]
		binary.LittleEndian.PutUint32(superblock[0x3FC:], ext4Checksum(0xffffffff, superblock[:0x3FC]))
		ranges, _, err := ext4FreeRanges(bytes.NewReader(mutated), imageBytes)
		if err != nil {
			return
		}
		end := int64(0)
		for _, free := range ranges {
			if free.Offset < end || free.Length <= 0 || free.Offset+free.Length > imageBytes {
				t.Fatalf("range %+v escapes or overlaps after %d", free, end)
			}
			end = free.Offset + free.Length
		}
	})
}
