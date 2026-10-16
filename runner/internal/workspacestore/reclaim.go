package workspacestore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// Punches are aligned inward to this boundary, a multiple of every
	// supported host filesystem block size, so a punch never zeroes part of a
	// host block. Zeroing would be a write, and on a reflinked image a write
	// unshares a Snapshot's extent instead of releasing the Workspace's
	// reference to it.
	workspaceReclaimAlignmentBytes = 64 << 10
	// Shorter free runs stay allocated. The floor bounds the punches, and so
	// the host extent splits, that one fragmented or hostile guest filesystem
	// can cause per reclaim.
	workspaceReclaimMinimumBytes = 1 << 20
	// The guest controls how many free ranges exist, and every punch is a host
	// filesystem transaction on the runner's command path, so one pass punches
	// at most this many. Ranges with no host extent, written or unwritten, do
	// not count, so the next stop continues where this one ended.
	workspaceReclaimMaximumPunches = 4096

	workspaceReclaimRelocationSealed     = "relocation_sealed"
	workspaceReclaimPunchLimitReached    = "punch_limit_reached"
	workspaceReclaimHostStorageExhausted = "host_storage_exhausted"
)

// freeBlockReclaim reports one pass. Reason is empty when the pass punched
// every eligible free range; otherwise it names why the pass punched nothing
// or stopped early, leaving the remaining ranges to the next stop.
type freeBlockReclaim struct {
	Reason          string
	Ranges          int
	PunchedBytes    int64
	AllocatedBefore int64
	AllocatedAfter  int64
}

// reclaimFreeBlocks releases the host allocation behind ext4 blocks that the
// guest has freed, which the guest cannot do itself: the pinned VMM offers no
// discard and the image is never mounted with it. The caller must hold the
// Workspace writer lock. The gVisor and Microsandbox helpers inherit that
// lock's open file description, and the Firecracker attachment releases it
// only after the jailer supervisor has reaped the VMM, so holding it proves no
// guest or host helper can still write the image and its on-disk ext4
// allocation state is final.
//
// Punching only drops this image's reference to an extent. A Snapshot or
// template that shares the extent keeps its bytes, and the extent becomes
// exclusive to that holder rather than being copied or counted twice.
func (store *Store) reclaimFreeBlocks(ctx context.Context, workspaceID string, manifest currentManifest) error {
	if manifest.RelocationSealed {
		logFreeBlockReclaim(workspaceID, freeBlockReclaim{Reason: workspaceReclaimRelocationSealed}, 0)
		return nil
	}
	path, err := store.validatedVersionPath(workspaceID, manifest.Image)
	if err != nil {
		return err
	}
	image, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore open image for freed-block reclaim: %w", err)
	}
	startedAt := time.Now()
	result, err := reclaimFreeImageBlocks(ctx, image, manifest.CapacityBytes, store.driver.PunchHole)
	if closeErr := image.Close(); err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	logFreeBlockReclaim(workspaceID, result, time.Since(startedAt))
	return nil
}

func reclaimFreeImageBlocks(
	ctx context.Context,
	image *os.File,
	capacityBytes int64,
	punch func(*os.File, int64, int64) error,
) (freeBlockReclaim, error) {
	before, err := allocatedImageBytes(image, capacityBytes)
	if err != nil {
		return freeBlockReclaim{}, err
	}
	result := freeBlockReclaim{AllocatedBefore: before, AllocatedAfter: before}
	ranges, reason, err := ext4FreeRanges(image, capacityBytes, alignWorkspaceReclaimRange)
	if err != nil {
		return freeBlockReclaim{}, err
	}
	if reason != "" {
		result.Reason = reason
		return result, nil
	}
	if err := punchWorkspaceReclaimRanges(
		ctx, image, ranges, platformRangeAllocated, punch, workspaceReclaimMaximumPunches, &result,
	); err != nil {
		return freeBlockReclaim{}, err
	}
	if result.AllocatedAfter, err = allocatedImageBytes(image, capacityBytes); err != nil {
		return freeBlockReclaim{}, err
	}
	return result, nil
}

// punchWorkspaceReclaimRanges punches, in offset order, up to limit ranges that
// still hold host allocation.
func punchWorkspaceReclaimRanges(
	ctx context.Context,
	image *os.File,
	ranges []ext4ByteRange,
	allocated func(*os.File, int64, int64) (bool, error),
	punch func(*os.File, int64, int64) error,
	limit int,
	result *freeBlockReclaim,
) error {
	for _, free := range ranges {
		if err := ctx.Err(); err != nil {
			return err
		}
		held, err := allocated(image, free.Offset, free.Length)
		if err != nil {
			return fmt.Errorf("SecondBox WorkspaceStore locate allocated Workspace blocks: %w", err)
		}
		if !held {
			continue
		}
		if result.Ranges == limit {
			result.Reason = workspaceReclaimPunchLimitReached
			return nil
		}
		if err := punch(image, free.Offset, free.Length); err != nil {
			// A full host filesystem can refuse the metadata a punch needs.
			// The blocks stay allocated and the next stop retries.
			if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
				result.Reason = workspaceReclaimHostStorageExhausted
				return nil
			}
			return fmt.Errorf("SecondBox WorkspaceStore punch freed Workspace blocks: %w", err)
		}
		result.Ranges++
		result.PunchedBytes += free.Length
	}
	return nil
}

// alignWorkspaceReclaimRange shrinks a free range to whole alignment units and
// drops it when the result is below the minimum punch length.
func alignWorkspaceReclaimRange(free ext4ByteRange) (ext4ByteRange, bool) {
	start := (free.Offset + workspaceReclaimAlignmentBytes - 1) /
		workspaceReclaimAlignmentBytes * workspaceReclaimAlignmentBytes
	end := (free.Offset + free.Length) / workspaceReclaimAlignmentBytes * workspaceReclaimAlignmentBytes
	if end-start < workspaceReclaimMinimumBytes {
		return ext4ByteRange{}, false
	}
	return ext4ByteRange{Offset: start, Length: end - start}, true
}

func allocatedImageBytes(image *os.File, capacityBytes int64) (int64, error) {
	info, err := image.Stat()
	if err != nil {
		return 0, fmt.Errorf("SecondBox WorkspaceStore stat image for freed-block reclaim: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Size() != capacityBytes {
		return 0, fmt.Errorf("%w: Workspace image size or type is invalid", ErrCorruptState)
	}
	return stat.Blocks * 512, nil
}

func logFreeBlockReclaim(workspaceID string, result freeBlockReclaim, elapsed time.Duration) {
	outcome := result.Reason
	if outcome == "" {
		outcome = "reclaimed"
	}
	slog.Info(
		"SecondBox WorkspaceStore freed-block reclaim",
		"workspaceId", workspaceID,
		"outcome", outcome,
		"ranges", result.Ranges,
		"punchedBytes", result.PunchedBytes,
		"allocatedBeforeBytes", result.AllocatedBefore,
		"allocatedAfterBytes", result.AllocatedAfter,
		"elapsedMs", elapsed.Milliseconds(),
	)
}
