package workspacestore

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math/bits"
)

// The guest owns every byte of a Workspace image, so this reader treats the
// ext4 metadata as untrusted input: every offset is bounded by the image, the
// geometry is restricted to the layouts the qualified formatters produce, and
// any state the guest kernel has not committed (a journal awaiting recovery,
// recorded errors, a checksum or free-count disagreement) declines the whole
// reclaim rather than punching anything.

const (
	ext4SuperblockOffset = 1024
	ext4SuperblockBytes  = 1024

	ext4FeatureIncompatFiletype   = 0x0002
	ext4FeatureIncompatRecover    = 0x0004
	ext4FeatureIncompatExtents    = 0x0040
	ext4FeatureIncompat64Bit      = 0x0080
	ext4FeatureIncompatMMP        = 0x0100
	ext4FeatureIncompatFlexBG     = 0x0200
	ext4FeatureIncompatEAInode    = 0x0400
	ext4FeatureIncompatCsumSeed   = 0x2000
	ext4FeatureIncompatLargeDir   = 0x4000
	ext4FeatureIncompatInlineData = 0x8000
	ext4FeatureIncompatEncrypt    = 0x10000
	ext4FeatureIncompatCasefold   = 0x20000

	ext4FeatureROCompatSparseSuper   = 0x0001
	ext4FeatureROCompatLargeFile     = 0x0002
	ext4FeatureROCompatBtreeDir      = 0x0004
	ext4FeatureROCompatHugeFile      = 0x0008
	ext4FeatureROCompatGDTCsum       = 0x0010
	ext4FeatureROCompatDirNlink      = 0x0020
	ext4FeatureROCompatExtraIsize    = 0x0040
	ext4FeatureROCompatQuota         = 0x0100
	ext4FeatureROCompatMetadataCsum  = 0x0400
	ext4FeatureROCompatReadonly      = 0x1000
	ext4FeatureROCompatProject       = 0x2000
	ext4FeatureROCompatVerity        = 0x8000
	ext4FeatureROCompatOrphanPresent = 0x10000

	// Only features that leave the block bitmap authoritative for block
	// allocation are accepted. META_BG relocates group descriptors, BIGALLOC
	// makes each bit a cluster, and an unknown feature may change either.
	ext4ReclaimIncompatFeatures = ext4FeatureIncompatFiletype | ext4FeatureIncompatRecover |
		ext4FeatureIncompatExtents | ext4FeatureIncompat64Bit | ext4FeatureIncompatMMP |
		ext4FeatureIncompatFlexBG | ext4FeatureIncompatEAInode | ext4FeatureIncompatCsumSeed |
		ext4FeatureIncompatLargeDir | ext4FeatureIncompatInlineData |
		ext4FeatureIncompatEncrypt | ext4FeatureIncompatCasefold
	ext4ReclaimROCompatFeatures = ext4FeatureROCompatSparseSuper | ext4FeatureROCompatLargeFile |
		ext4FeatureROCompatBtreeDir | ext4FeatureROCompatHugeFile | ext4FeatureROCompatGDTCsum |
		ext4FeatureROCompatDirNlink | ext4FeatureROCompatExtraIsize | ext4FeatureROCompatQuota |
		ext4FeatureROCompatMetadataCsum | ext4FeatureROCompatReadonly |
		ext4FeatureROCompatProject | ext4FeatureROCompatVerity | ext4FeatureROCompatOrphanPresent

	ext4StateErrors   = 0x0002
	ext4StateFCReplay = 0x0020

	ext4GroupBlockUninit = 0x0002

	ext4DescriptorBytes32 = 32
	ext4DescriptorBytes64 = 64
	ext4MaxDescriptorSize = 1024
	ext4MaxLogBlockSize   = 6 // 64 KiB
	ext4ChecksumTypeCRC32 = 1

	// Group descriptor field offsets (struct ext4_group_desc).
	ext4DescBlockBitmapLo     = 0x00
	ext4DescFreeBlocksLo      = 0x0C
	ext4DescFlags             = 0x12
	ext4DescBlockBitmapCsumLo = 0x18
	ext4DescChecksum          = 0x1E
	ext4DescBlockBitmapHi     = 0x20
	ext4DescFreeBlocksHi      = 0x2C
	ext4DescBlockBitmapCsumHi = 0x38
)

// Decline reasons are stable log values. A declined reclaim punches nothing.
const (
	ext4ReclaimSuperblockInvalid   = "ext4_superblock_invalid"
	ext4ReclaimFeaturesUnsupported = "ext4_features_unsupported"
	ext4ReclaimNeedsRecovery       = "ext4_needs_recovery"
	ext4ReclaimFilesystemErrors    = "ext4_filesystem_errors"
	ext4ReclaimChecksumMismatch    = "ext4_checksum_mismatch"
	ext4ReclaimGroupInvalid        = "ext4_group_invalid"
	ext4ReclaimFreeCountMismatch   = "ext4_free_count_mismatch"
)

var ext4CRC32CTable = crc32.MakeTable(crc32.Castagnoli)

// ext4ByteRange is a half-open byte range of the image.
type ext4ByteRange struct {
	Offset int64
	Length int64
}

type ext4Geometry struct {
	blockBytes       int64
	blocksCount      uint64
	firstDataBlock   uint64
	blocksPerGroup   uint64
	groupCount       uint64
	descriptorBytes  int64
	is64Bit          bool
	metadataChecksum bool
	groupChecksum    bool
	checksumSeed     uint32
	uuid             []byte
}

// ext4FreeRanges returns the merged byte ranges that the ext4 filesystem in
// image marks free in its initialized block groups. A non-empty reason means
// the on-disk allocation state cannot be trusted and no ranges are returned.
// Errors are reserved for failures to read the image itself.
func ext4FreeRanges(image io.ReaderAt, imageBytes int64) ([]ext4ByteRange, string, error) {
	superblock := make([]byte, ext4SuperblockBytes)
	if _, err := image.ReadAt(superblock, ext4SuperblockOffset); err != nil {
		return nil, "", fmt.Errorf("SecondBox WorkspaceStore read ext4 superblock: %w", err)
	}
	geometry, reason := parseExt4Superblock(superblock, imageBytes)
	if reason != "" {
		return nil, reason, nil
	}
	descriptors := make([]byte, geometry.groupCount*uint64(geometry.descriptorBytes))
	if _, err := image.ReadAt(
		descriptors,
		int64(geometry.firstDataBlock+1)*geometry.blockBytes,
	); err != nil {
		return nil, "", fmt.Errorf("SecondBox WorkspaceStore read ext4 group descriptors: %w", err)
	}
	bitmap := make([]byte, geometry.blockBytes)
	var ranges []ext4ByteRange
	for group := uint64(0); group < geometry.groupCount; group++ {
		descriptor := descriptors[group*uint64(geometry.descriptorBytes):][:geometry.descriptorBytes]
		if !geometry.descriptorChecksumValid(uint32(group), descriptor) {
			return nil, ext4ReclaimChecksumMismatch, nil
		}
		// An uninitialized group's bitmap block was never written; the kernel
		// synthesizes it. Such a group has never held file data, so skipping it
		// costs nothing and never trusts an unwritten block.
		if (geometry.metadataChecksum || geometry.groupChecksum) &&
			binary.LittleEndian.Uint16(descriptor[ext4DescFlags:])&ext4GroupBlockUninit != 0 {
			continue
		}
		bitmapBlock := uint64(binary.LittleEndian.Uint32(descriptor[ext4DescBlockBitmapLo:]))
		freeBlocks := uint64(binary.LittleEndian.Uint16(descriptor[ext4DescFreeBlocksLo:]))
		if geometry.is64Bit {
			bitmapBlock |= uint64(binary.LittleEndian.Uint32(descriptor[ext4DescBlockBitmapHi:])) << 32
			freeBlocks |= uint64(binary.LittleEndian.Uint16(descriptor[ext4DescFreeBlocksHi:])) << 16
		}
		if bitmapBlock <= geometry.firstDataBlock || bitmapBlock >= geometry.blocksCount {
			return nil, ext4ReclaimGroupInvalid, nil
		}
		if _, err := image.ReadAt(bitmap, int64(bitmapBlock)*geometry.blockBytes); err != nil {
			return nil, "", fmt.Errorf("SecondBox WorkspaceStore read ext4 block bitmap: %w", err)
		}
		if !geometry.bitmapChecksumValid(descriptor, bitmap) {
			return nil, ext4ReclaimChecksumMismatch, nil
		}
		groupStart := geometry.firstDataBlock + group*geometry.blocksPerGroup
		groupBlocks := min(geometry.blocksPerGroup, geometry.blocksCount-groupStart)
		// The kernel updates a group's bitmap and its free count in the same
		// journal transaction. Disagreement means the state is not committed
		// or this reader misparsed it; either way nothing is punched.
		if countExt4FreeBits(bitmap, groupBlocks) != freeBlocks {
			return nil, ext4ReclaimFreeCountMismatch, nil
		}
		ranges = appendExt4FreeRuns(ranges, bitmap, groupBlocks, groupStart, geometry.blockBytes)
	}
	return ranges, "", nil
}

func parseExt4Superblock(superblock []byte, imageBytes int64) (ext4Geometry, string) {
	le32 := func(offset int) uint32 { return binary.LittleEndian.Uint32(superblock[offset:]) }
	le16 := func(offset int) uint16 { return binary.LittleEndian.Uint16(superblock[offset:]) }
	if le16(0x38) != 0xef53 || le32(0x4C) < 1 {
		return ext4Geometry{}, ext4ReclaimSuperblockInvalid
	}
	incompat, roCompat := le32(0x60), le32(0x64)
	if incompat&^ext4ReclaimIncompatFeatures != 0 || roCompat&^ext4ReclaimROCompatFeatures != 0 {
		return ext4Geometry{}, ext4ReclaimFeaturesUnsupported
	}
	geometry := ext4Geometry{
		is64Bit:          incompat&ext4FeatureIncompat64Bit != 0,
		metadataChecksum: roCompat&ext4FeatureROCompatMetadataCsum != 0,
		groupChecksum:    roCompat&ext4FeatureROCompatGDTCsum != 0,
		uuid:             superblock[0x68:0x78],
	}
	if geometry.metadataChecksum {
		if superblock[0x175] != ext4ChecksumTypeCRC32 ||
			ext4Checksum(0xffffffff, superblock[:0x3FC]) != le32(0x3FC) {
			return ext4Geometry{}, ext4ReclaimChecksumMismatch
		}
		geometry.checksumSeed = ext4Checksum(0xffffffff, geometry.uuid)
		if incompat&ext4FeatureIncompatCsumSeed != 0 {
			geometry.checksumSeed = le32(0x270)
		}
	}
	if incompat&ext4FeatureIncompatRecover != 0 {
		return ext4Geometry{}, ext4ReclaimNeedsRecovery
	}
	if state := le16(0x3A); state&ext4StateErrors != 0 {
		return ext4Geometry{}, ext4ReclaimFilesystemErrors
	} else if state&ext4StateFCReplay != 0 {
		return ext4Geometry{}, ext4ReclaimNeedsRecovery
	}
	logBlockSize := le32(0x18)
	if logBlockSize > ext4MaxLogBlockSize || le32(0x1C) != logBlockSize {
		return ext4Geometry{}, ext4ReclaimSuperblockInvalid
	}
	geometry.blockBytes = 1024 << logBlockSize
	geometry.blocksCount = uint64(le32(0x04))
	if geometry.is64Bit {
		geometry.blocksCount |= uint64(le32(0x150)) << 32
	}
	geometry.firstDataBlock = uint64(le32(0x14))
	geometry.blocksPerGroup = uint64(le32(0x20))
	expectedFirstDataBlock := uint64(0)
	if geometry.blockBytes == 1024 {
		expectedFirstDataBlock = 1
	}
	// Requiring the full 8*blockBytes group size bounds the bitmap reads to
	// one block per 8*blockBytes blocks however the guest rewrites the image.
	if geometry.firstDataBlock != expectedFirstDataBlock ||
		geometry.blocksPerGroup != uint64(geometry.blockBytes)*8 ||
		uint64(le32(0x24)) != geometry.blocksPerGroup ||
		geometry.blocksCount <= geometry.firstDataBlock+1 ||
		geometry.blocksCount > uint64(imageBytes)/uint64(geometry.blockBytes) {
		return ext4Geometry{}, ext4ReclaimSuperblockInvalid
	}
	geometry.groupCount = (geometry.blocksCount - geometry.firstDataBlock +
		geometry.blocksPerGroup - 1) / geometry.blocksPerGroup
	geometry.descriptorBytes = ext4DescriptorBytes32
	if geometry.is64Bit {
		geometry.descriptorBytes = int64(le16(0xFE))
		if geometry.descriptorBytes < ext4DescriptorBytes64 ||
			geometry.descriptorBytes > ext4MaxDescriptorSize ||
			geometry.descriptorBytes&(geometry.descriptorBytes-1) != 0 {
			return ext4Geometry{}, ext4ReclaimSuperblockInvalid
		}
	}
	descriptorBlocks := (geometry.groupCount*uint64(geometry.descriptorBytes) +
		uint64(geometry.blockBytes) - 1) / uint64(geometry.blockBytes)
	if geometry.firstDataBlock+1+descriptorBlocks > geometry.blocksCount {
		return ext4Geometry{}, ext4ReclaimSuperblockInvalid
	}
	return geometry, ""
}

// ext4Checksum is the kernel's ext4_chksum: CRC32C without the final
// inversion that hash/crc32 applies.
func ext4Checksum(seed uint32, data []byte) uint32 {
	return ^crc32.Update(^seed, ext4CRC32CTable, data)
}

func (geometry ext4Geometry) descriptorChecksumValid(group uint32, descriptor []byte) bool {
	stored := binary.LittleEndian.Uint16(descriptor[ext4DescChecksum:])
	var groupBytes [4]byte
	binary.LittleEndian.PutUint32(groupBytes[:], group)
	tail := descriptor[ext4DescChecksum+2:]
	if geometry.metadataChecksum {
		checksum := ext4Checksum(geometry.checksumSeed, groupBytes[:])
		checksum = ext4Checksum(checksum, descriptor[:ext4DescChecksum])
		checksum = ext4Checksum(checksum, []byte{0, 0})
		checksum = ext4Checksum(checksum, tail)
		return uint16(checksum) == stored
	}
	if geometry.groupChecksum {
		checksum := ext4CRC16(0xffff, geometry.uuid)
		checksum = ext4CRC16(checksum, groupBytes[:])
		checksum = ext4CRC16(checksum, descriptor[:ext4DescChecksum])
		if geometry.is64Bit {
			checksum = ext4CRC16(checksum, tail)
		}
		return checksum == stored
	}
	return true
}

func (geometry ext4Geometry) bitmapChecksumValid(descriptor []byte, bitmap []byte) bool {
	if !geometry.metadataChecksum {
		return true
	}
	checksum := ext4Checksum(geometry.checksumSeed, bitmap[:geometry.blocksPerGroup/8])
	stored := uint32(binary.LittleEndian.Uint16(descriptor[ext4DescBlockBitmapCsumLo:]))
	if geometry.descriptorBytes >= ext4DescBlockBitmapCsumHi+2 {
		stored |= uint32(binary.LittleEndian.Uint16(descriptor[ext4DescBlockBitmapCsumHi:])) << 16
	} else {
		checksum &= 0xffff
	}
	return checksum == stored
}

// ext4CRC16 is the reflected CRC-16 (polynomial 0x8005) that ext4's
// uninit_bg group descriptor checksum uses.
func ext4CRC16(crc uint16, data []byte) uint16 {
	for _, value := range data {
		crc ^= uint16(value)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xa001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

func countExt4FreeBits(bitmap []byte, blocks uint64) uint64 {
	used := uint64(0)
	full := blocks / 8
	for _, value := range bitmap[:full] {
		used += uint64(bits.OnesCount8(value))
	}
	if remainder := blocks % 8; remainder != 0 {
		used += uint64(bits.OnesCount8(bitmap[full] & (1<<remainder - 1)))
	}
	return blocks - used
}

// appendExt4FreeRuns appends the group's free-bit runs as byte ranges, merging
// a run with the previous range when they are contiguous across groups.
func appendExt4FreeRuns(
	ranges []ext4ByteRange,
	bitmap []byte,
	blocks uint64,
	groupStart uint64,
	blockBytes int64,
) []ext4ByteRange {
	appendRun := func(start, end uint64) {
		offset := int64(groupStart+start) * blockBytes
		length := int64(end-start) * blockBytes
		if last := len(ranges) - 1; last >= 0 && ranges[last].Offset+ranges[last].Length == offset {
			ranges[last].Length += length
			return
		}
		ranges = append(ranges, ext4ByteRange{Offset: offset, Length: length})
	}
	runStart, inRun := uint64(0), false
	for block := uint64(0); block < blocks; {
		if block%8 == 0 && blocks-block >= 8 {
			switch bitmap[block/8] {
			case 0xff:
				if inRun {
					appendRun(runStart, block)
					inRun = false
				}
				block += 8
				continue
			case 0x00:
				if !inRun {
					runStart, inRun = block, true
				}
				block += 8
				continue
			}
		}
		free := bitmap[block/8]&(1<<(block%8)) == 0
		if free && !inRun {
			runStart, inRun = block, true
		} else if !free && inRun {
			appendRun(runStart, block)
			inRun = false
		}
		block++
	}
	if inRun {
		appendRun(runStart, blocks)
	}
	return ranges
}
