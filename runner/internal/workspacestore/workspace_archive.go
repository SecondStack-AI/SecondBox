package workspacestore

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"

	"golang.org/x/sys/unix"
)

const workspaceArchiveChunkBytes = 64 << 10

var workspaceArchiveMagic = [8]byte{'S', 'B', 'X', 'A', 'R', 'C', '0', '1'}
var workspaceArchiveZeros [workspaceArchiveChunkBytes]byte

// WorkspaceArchiveDescriptor pins both the transport and the logical disk.
// Object locations and transfer credentials belong to the trusted transport.
type WorkspaceArchiveDescriptor struct {
	ArchiveBytes    int64  `json:"archiveBytes"`
	ArchiveChecksum string `json:"archiveChecksum"`
	ImageChecksum   string `json:"imageChecksum"`
}

type WorkspaceArchiveImportRequest struct {
	RelocationImportRequest
	Archive WorkspaceArchiveDescriptor
}

type WorkspaceArchiveStore interface {
	ExportWorkspaceArchive(context.Context, RelocationExportRequest, io.Writer) (WorkspaceArchiveDescriptor, error)
	ImportWorkspaceArchive(context.Context, WorkspaceArchiveImportRequest, io.Reader) (Receipt, error)
}

type workspaceArchiveHeader struct {
	Magic          [8]byte
	Generation     uint64
	CapacityBytes  uint64
	WorkspaceBytes uint16
}

type workspaceArchiveRecord struct {
	Offset uint64
	Length uint32
}

type workspaceArchiveWriter struct {
	writer io.Writer
	hash   hash.Hash
	bytes  int64
}

func (writer *workspaceArchiveWriter) Write(data []byte) (int, error) {
	n, err := writer.writer.Write(data)
	writer.bytes += int64(n)
	_, hashErr := writer.hash.Write(data[:n])
	return n, errors.Join(err, hashErr)
}

// ExportWorkspaceArchive leaves the source sealed until the control plane
// commits archived ownership or explicitly aborts the mutation.
func (store *Store) ExportWorkspaceArchive(
	ctx context.Context,
	request RelocationExportRequest,
	destination io.Writer,
) (_ WorkspaceArchiveDescriptor, resultErr error) {
	if destination == nil {
		return WorkspaceArchiveDescriptor{}, errors.New("SecondBox Workspace archive destination is required")
	}
	export, err := store.OpenRelocationExport(ctx, request)
	if err != nil {
		return WorkspaceArchiveDescriptor{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, export.Close()) }()
	image := export.(*relocationExport).file
	transport := &workspaceArchiveWriter{writer: destination, hash: sha256.New()}
	compressed, err := gzip.NewWriterLevel(transport, gzip.BestSpeed)
	if err != nil {
		return WorkspaceArchiveDescriptor{}, err
	}
	header := workspaceArchiveHeader{
		Magic: workspaceArchiveMagic, Generation: request.ExpectedGeneration,
		CapacityBytes: uint64(export.SizeBytes()), WorkspaceBytes: uint16(len(request.WorkspaceID)),
	}
	if err := binary.Write(compressed, binary.LittleEndian, header); err != nil {
		return WorkspaceArchiveDescriptor{}, fmt.Errorf("SecondBox Workspace archive header: %w", err)
	}
	if _, err := io.WriteString(compressed, request.WorkspaceID); err != nil {
		return WorkspaceArchiveDescriptor{}, fmt.Errorf("SecondBox Workspace archive identity: %w", err)
	}
	imageHash := sha256.New()
	buffer := make([]byte, workspaceArchiveChunkBytes)
	position := int64(0)
	for position < export.SizeBytes() {
		if err := ctx.Err(); err != nil {
			return WorkspaceArchiveDescriptor{}, err
		}
		dataOffset, err := unix.Seek(int(image.Fd()), position, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if err != nil {
			return WorkspaceArchiveDescriptor{}, fmt.Errorf("SecondBox Workspace archive sparse extent: %w", err)
		}
		holeOffset, err := unix.Seek(int(image.Fd()), dataOffset, unix.SEEK_HOLE)
		if err != nil {
			return WorkspaceArchiveDescriptor{}, fmt.Errorf("SecondBox Workspace archive extent end: %w", err)
		}
		if dataOffset < position || dataOffset >= export.SizeBytes() || holeOffset <= dataOffset {
			return WorkspaceArchiveDescriptor{}, fmt.Errorf("%w: invalid archive source extent", ErrCorruptState)
		}
		if err := hashWorkspaceArchiveZeros(ctx, imageHash, dataOffset-position); err != nil {
			return WorkspaceArchiveDescriptor{}, err
		}
		position = dataOffset
		for position < min(holeOffset, export.SizeBytes()) {
			if err := ctx.Err(); err != nil {
				return WorkspaceArchiveDescriptor{}, err
			}
			size := int(min(int64(len(buffer)), min(holeOffset, export.SizeBytes())-position))
			if _, err := image.ReadAt(buffer[:size], position); err != nil {
				return WorkspaceArchiveDescriptor{}, fmt.Errorf("SecondBox Workspace archive disk read: %w", err)
			}
			if err := binary.Write(compressed, binary.LittleEndian, workspaceArchiveRecord{uint64(position), uint32(size)}); err != nil {
				return WorkspaceArchiveDescriptor{}, err
			}
			if _, err := io.MultiWriter(compressed, imageHash).Write(buffer[:size]); err != nil {
				return WorkspaceArchiveDescriptor{}, fmt.Errorf("SecondBox Workspace archive extent write: %w", err)
			}
			position += int64(size)
		}
	}
	if err := hashWorkspaceArchiveZeros(ctx, imageHash, export.SizeBytes()-position); err != nil {
		return WorkspaceArchiveDescriptor{}, err
	}
	if err := binary.Write(compressed, binary.LittleEndian, workspaceArchiveRecord{uint64(export.SizeBytes()), 0}); err != nil {
		return WorkspaceArchiveDescriptor{}, err
	}
	if _, err := compressed.Write(imageHash.Sum(nil)); err != nil {
		return WorkspaceArchiveDescriptor{}, err
	}
	if err := compressed.Close(); err != nil {
		return WorkspaceArchiveDescriptor{}, fmt.Errorf("SecondBox Workspace archive compression finish: %w", err)
	}
	return WorkspaceArchiveDescriptor{
		ArchiveBytes: transport.bytes, ArchiveChecksum: workspaceArchiveChecksum(transport.hash),
		ImageChecksum: workspaceArchiveChecksum(imageHash),
	}, nil
}

func (store *Store) ImportWorkspaceArchive(
	ctx context.Context,
	request WorkspaceArchiveImportRequest,
	source io.Reader,
) (_ Receipt, resultErr error) {
	if source == nil || request.Archive.ArchiveBytes <= 0 || request.Archive.ArchiveBytes == math.MaxInt64 ||
		!validWorkspaceArchiveChecksum(request.Archive.ArchiveChecksum) ||
		!validWorkspaceArchiveChecksum(request.Archive.ImageChecksum) {
		return Receipt{}, fmt.Errorf("%w: invalid archive descriptor", ErrCorruptState)
	}
	if receipt, found, err := store.ReplayRelocationImport(ctx, request.RelocationImportRequest); found || err != nil {
		if err == nil && receipt.Checksum != request.Archive.ImageChecksum {
			return Receipt{}, ErrConflictingReplay
		}
		return receipt, err
	}
	compressedHash := sha256.New()
	limited := &io.LimitedReader{R: source, N: request.Archive.ArchiveBytes + 1}
	buffered := bufio.NewReader(io.TeeReader(limited, compressedHash))
	compressed, err := gzip.NewReader(buffered)
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: archive compression header: %w", ErrCorruptState, err)
	}
	defer func() { resultErr = errors.Join(resultErr, compressed.Close()) }()
	compressed.Multistream(false)
	var header workspaceArchiveHeader
	if err := binary.Read(compressed, binary.LittleEndian, &header); err != nil {
		return Receipt{}, fmt.Errorf("%w: archive header: %w", ErrCorruptState, err)
	}
	if header.Magic != workspaceArchiveMagic || header.Generation != request.Generation ||
		header.CapacityBytes != uint64(request.CapacityBytes) || header.WorkspaceBytes == 0 || header.WorkspaceBytes > 128 {
		return Receipt{}, fmt.Errorf("%w: archive identity or capacity mismatch", ErrCorruptState)
	}
	identity := make([]byte, header.WorkspaceBytes)
	if _, err := io.ReadFull(compressed, identity); err != nil || string(identity) != request.WorkspaceID {
		return Receipt{}, fmt.Errorf("%w: archive Workspace mismatch", ErrCorruptState)
	}
	importer, err := store.BeginRelocationImport(ctx, request.RelocationImportRequest)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, importer.Abort()) }()
	buffer := make([]byte, workspaceArchiveChunkBytes)
	zeros := workspaceArchiveZeros[:]
	position := uint64(0)
	for {
		if err := ctx.Err(); err != nil {
			return Receipt{}, err
		}
		var record workspaceArchiveRecord
		if err := binary.Read(compressed, binary.LittleEndian, &record); err != nil {
			return Receipt{}, fmt.Errorf("%w: archive extent header: %w", ErrCorruptState, err)
		}
		if record.Offset < position || record.Offset > header.CapacityBytes ||
			record.Length > workspaceArchiveChunkBytes || uint64(record.Length) > header.CapacityBytes-record.Offset ||
			(record.Length == 0 && record.Offset != header.CapacityBytes) {
			return Receipt{}, fmt.Errorf("%w: archive extent exceeds disk bounds", ErrCorruptState)
		}
		for position < record.Offset {
			if err := ctx.Err(); err != nil {
				return Receipt{}, err
			}
			size := min(uint64(len(zeros)), record.Offset-position)
			if err := importer.WriteChunk(position, zeros[:size]); err != nil {
				return Receipt{}, err
			}
			position += size
		}
		if record.Length == 0 {
			break
		}
		if _, err := io.ReadFull(compressed, buffer[:record.Length]); err != nil {
			return Receipt{}, fmt.Errorf("%w: archive extent data: %w", ErrCorruptState, err)
		}
		if err := importer.WriteChunk(position, buffer[:record.Length]); err != nil {
			return Receipt{}, err
		}
		position += uint64(record.Length)
	}
	var imageDigest [sha256.Size]byte
	if _, err := io.ReadFull(compressed, imageDigest[:]); err != nil ||
		"sha256:"+hex.EncodeToString(imageDigest[:]) != request.Archive.ImageChecksum {
		return Receipt{}, fmt.Errorf("%w: archive image digest mismatch", ErrCorruptState)
	}
	var extra [1]byte
	if n, err := compressed.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return Receipt{}, fmt.Errorf("%w: archive compression checksum or trailing data", ErrCorruptState)
	}
	if _, err := buffered.ReadByte(); !errors.Is(err, io.EOF) ||
		request.Archive.ArchiveBytes+1-limited.N != request.Archive.ArchiveBytes ||
		workspaceArchiveChecksum(compressedHash) != request.Archive.ArchiveChecksum {
		return Receipt{}, fmt.Errorf("%w: archive transport size or checksum mismatch", ErrCorruptState)
	}
	return importer.Complete(header.CapacityBytes, request.Archive.ImageChecksum)
}

func hashWorkspaceArchiveZeros(ctx context.Context, digest hash.Hash, size int64) error {
	zeros := workspaceArchiveZeros[:]
	for size > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(int64(len(zeros)), size)
		if _, err := digest.Write(zeros[:n]); err != nil {
			return err
		}
		size -= n
	}
	return nil
}

func workspaceArchiveChecksum(digest hash.Hash) string {
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func validWorkspaceArchiveChecksum(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || value[:len("sha256:")] != "sha256:" {
		return false
	}
	_, err := hex.DecodeString(value[len("sha256:"):])
	return err == nil
}

var _ WorkspaceArchiveStore = (*Store)(nil)
