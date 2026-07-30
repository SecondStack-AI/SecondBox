package microvmguest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	guestv1 "github.com/SecondStack-AI/SecondBox/runner/internal/guestprotocol"
	"github.com/SecondStack-AI/SecondBox/runner/internal/sandboxlimits"
	"golang.org/x/sys/unix"
)

type protocolFileState struct {
	binding      *guestv1.OperationBinding
	nextIncoming uint64
	request      *guestv1.FileRequest
	data         []byte
	credit       chan uint64
	ctx          context.Context
	cancel       context.CancelCauseFunc
}

func (c *protocolConnection) handleFileFrame(frame *guestv1.FileFrame) error {
	if !c.enabled[guestv1.GuestFeature_GUEST_FEATURE_DESCRIPTOR_PINNED_FILESYSTEM] {
		return fmt.Errorf("guest protocol filesystem feature was not negotiated")
	}
	if frame == nil || frame.Binding == nil || !sameProtocolConnectionBinding(frame.Binding.Connection, c.binding) {
		return fmt.Errorf("guest protocol file binding mismatch")
	}
	key := protocolOperationKey(frame.Binding)
	if key == "" {
		return fmt.Errorf("guest protocol file operation identity is incomplete")
	}
	if request := frame.GetRequest(); request != nil {
		if frame.Binding.Sequence != 1 {
			return fmt.Errorf("guest protocol file request sequence must begin at one")
		}
		if request.Operation == guestv1.FileOperation_FILE_OPERATION_UNSPECIFIED {
			return fmt.Errorf("guest protocol file operation is unspecified")
		}
		if hasParentSegment(request.WorkspaceRelativePath) || strings.ContainsRune(request.WorkspaceRelativePath, 0) {
			state := &protocolFileState{binding: cloneOperationBinding(frame.Binding)}
			return c.sendFile(state, &guestv1.FileFrame{Payload: &guestv1.FileFrame_Terminal{Terminal: &guestv1.FileTerminal{
				Kind:       guestv1.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH,
				SafeDetail: "workspace-relative path is invalid",
			}}})
		}
		fileCtx, cancel := context.WithCancelCause(c.stream.Context())
		state := &protocolFileState{
			binding:      cloneOperationBinding(frame.Binding),
			nextIncoming: 2,
			request:      request,
			credit:       make(chan uint64, 16),
			ctx:          fileCtx,
			cancel:       cancel,
		}
		c.mu.Lock()
		if _, exists := c.files[key]; exists {
			c.mu.Unlock()
			return fmt.Errorf("guest protocol file operation is already active")
		}
		c.files[key] = state
		c.mu.Unlock()
		if request.Operation == guestv1.FileOperation_FILE_OPERATION_WRITE {
			if request.ExpectedSize > uint64(sandboxlimits.FileTransferMaxBytes) {
				c.removeFile(key)
				return c.sendFileTerminal(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED, "file exceeds transfer limit")
			}
			if request.ExpectedSize == 0 {
				c.wait.Add(1)
				go func() {
					defer c.wait.Done()
					defer c.removeFile(key)
					c.commitProtocolFileWrite(state)
				}()
			}
			return nil
		}
		c.wait.Add(1)
		go func() {
			defer c.wait.Done()
			defer c.removeFile(key)
			c.runProtocolFileRequest(state)
		}()
		return nil
	}

	c.mu.Lock()
	state := c.files[key]
	if state == nil {
		c.mu.Unlock()
		return fmt.Errorf("guest protocol file operation is not active")
	}
	if frame.Binding.Sequence != state.nextIncoming {
		c.mu.Unlock()
		return fmt.Errorf("guest protocol file sequence mismatch: got %d, want %d", frame.Binding.Sequence, state.nextIncoming)
	}
	state.nextIncoming++
	c.mu.Unlock()

	switch {
	case frame.GetCredit() != nil:
		if frame.GetCredit().ByteCount == 0 {
			return fmt.Errorf("guest protocol file credit must be positive")
		}
		select {
		case state.credit <- frame.GetCredit().ByteCount:
			return nil
		case <-state.ctx.Done():
			return state.ctx.Err()
		}
	case frame.GetCancel() != nil:
		state.cancel(errProtocolExecCancelled)
		if state.request.Operation == guestv1.FileOperation_FILE_OPERATION_WRITE {
			c.removeFile(key)
			return c.sendFileTerminal(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED, "file write cancelled")
		}
		return nil
	case frame.GetChunk() != nil:
		if state.request.Operation != guestv1.FileOperation_FILE_OPERATION_WRITE {
			return fmt.Errorf("guest protocol file chunks are only valid for writes")
		}
		chunk := frame.GetChunk()
		if chunk.Offset != uint64(len(state.data)) {
			return fmt.Errorf("guest protocol file chunk offset mismatch")
		}
		if uint64(len(state.data))+uint64(len(chunk.Data)) > state.request.ExpectedSize {
			return c.sendFileTerminal(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED, "file exceeded declared size")
		}
		state.data = append(state.data, chunk.Data...)
		if uint64(len(state.data)) == state.request.ExpectedSize {
			c.wait.Add(1)
			go func() {
				defer c.wait.Done()
				defer c.removeFile(key)
				c.commitProtocolFileWrite(state)
			}()
		}
		return nil
	default:
		return fmt.Errorf("guest protocol file frame payload is unsupported")
	}
}

func (c *protocolConnection) runProtocolFileRequest(state *protocolFileState) {
	switch state.request.Operation {
	case guestv1.FileOperation_FILE_OPERATION_READ:
		c.readProtocolFile(state)
	case guestv1.FileOperation_FILE_OPERATION_STAT:
		c.statProtocolFile(state)
	case guestv1.FileOperation_FILE_OPERATION_LIST_DIRECT_CHILDREN:
		c.listProtocolDirectory(state)
	case guestv1.FileOperation_FILE_OPERATION_EXISTS:
		c.existsProtocolPath(state)
	case guestv1.FileOperation_FILE_OPERATION_MKDIR:
		resp := c.service.server.executeMkdir(toolExecRequest{
			Path: state.request.WorkspaceRelativePath, Recursive: state.request.Recursive,
		})
		c.sendLegacyFileResponse(state, resp)
	case guestv1.FileOperation_FILE_OPERATION_REMOVE:
		resp := c.service.server.executeRm(toolExecRequest{
			Path:      state.request.WorkspaceRelativePath,
			Recursive: state.request.Recursive, Force: state.request.Force,
		})
		c.sendLegacyFileResponse(state, resp)
	default:
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH, "file operation is unsupported")
	}
}

func (c *protocolConnection) readProtocolFile(state *protocolFileState) {
	root, target, err := c.service.server.workspacePath(state.request.WorkspaceRelativePath)
	if err != nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH, "workspace-relative path is invalid")
		return
	}
	file, err := openWorkspaceTarget(root, target, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		c.recordAsyncError("send guest file open failure", c.sendProtocolFileOpenError(state, err))
		return
	}
	defer c.closeProtocolFile("close guest file read descriptor", file)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, "path is not a readable regular file")
		return
	}
	if info.Size() > sandboxlimits.FileTransferMaxBytes {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_LIMIT_EXCEEDED, "file exceeds transfer limit")
		return
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, "file could not be read")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, "file could not be rewound")
		return
	}
	if err := c.sendFile(state, &guestv1.FileFrame{Payload: &guestv1.FileFrame_Metadata{Metadata: &guestv1.FileMetadata{
		Exists:   true,
		Size:     uint64(info.Size()),
		Mode:     uint32(info.Mode().Perm()),
		Checksum: fmt.Sprintf("sha256:%x", hash.Sum(nil)),
	}}}); err != nil {
		return
	}
	buffer := make([]byte, 64<<10)
	offset := uint64(0)
	credit := uint64(0)
	for {
		if credit == 0 {
			select {
			case credit = <-state.credit:
			case <-state.ctx.Done():
				c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED, "file read cancelled")
				return
			}
		}
		readSize := len(buffer)
		if uint64(readSize) > credit {
			readSize = int(credit)
		}
		n, readErr := file.Read(buffer[:readSize])
		if n > 0 {
			if err := c.sendFile(state, &guestv1.FileFrame{Payload: &guestv1.FileFrame_Chunk{Chunk: &guestv1.FileChunk{
				Offset: offset,
				Data:   append([]byte(nil), buffer[:n]...),
			}}}); err != nil {
				return
			}
			offset += uint64(n)
			credit -= uint64(n)
			if offset == uint64(info.Size()) {
				c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED, "")
				return
			}
		}
		if readErr == io.EOF {
			c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED, "")
			return
		}
		if readErr != nil {
			c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, "file read failed")
			return
		}
	}
}

func (c *protocolConnection) commitProtocolFileWrite(state *protocolFileState) {
	if err := state.ctx.Err(); err != nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_CANCELLED, "file write cancelled")
		return
	}
	actualChecksum := fmt.Sprintf("sha256:%x", sha256.Sum256(state.data))
	if state.request.ExpectedChecksum == "" || state.request.ExpectedChecksum != actualChecksum {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_CHECKSUM_MISMATCH, "file checksum mismatch")
		return
	}
	root, target, err := c.service.server.workspacePath(state.request.WorkspaceRelativePath)
	if err != nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH, "workspace-relative path is invalid")
		return
	}
	parent, err := openWorkspaceParent(root, target, true)
	if err != nil {
		c.recordAsyncError("send guest file parent-open failure", c.sendProtocolFileOpenError(state, err))
		return
	}
	defer parent.close()
	tmp, tmpName, err := createWorkspaceTemp(parent)
	if err != nil {
		c.recordAsyncError("send guest file temporary-create failure", c.sendProtocolFileOpenError(state, err))
		return
	}
	cleanup := true
	defer func() {
		if cleanup {
			unlinkWorkspaceTemp(parent, tmpName)
		}
	}()
	mode := os.FileMode(state.request.CreateMode)
	if mode == 0 || mode&^0o777 != 0 {
		c.closeProtocolFile("close guest file temporary descriptor", tmp)
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, "create mode is invalid")
		return
	}
	if err := tmp.Chmod(mode); err != nil {
		c.closeProtocolFile("close guest file temporary descriptor", tmp)
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, "file mode could not be applied")
		return
	}
	if _, err := tmp.Write(state.data); err != nil {
		c.closeProtocolFile("close guest file temporary descriptor", tmp)
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, "file could not be written")
		return
	}
	if err := tmp.Sync(); err != nil {
		c.closeProtocolFile("close guest file temporary descriptor", tmp)
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, "file could not be synced")
		return
	}
	if err := tmp.Close(); err != nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, "file could not be closed")
		return
	}
	if err := commitWorkspaceTemp(parent, tmpName); err != nil {
		c.recordAsyncError("send guest file commit failure", c.sendProtocolFileOpenError(state, err))
		return
	}
	cleanup = false
	c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED, "")
}

func (c *protocolConnection) statProtocolFile(state *protocolFileState) {
	root, target, err := c.service.server.workspacePath(state.request.WorkspaceRelativePath)
	if err != nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH, "workspace-relative path is invalid")
		return
	}
	file, err := openWorkspaceTarget(root, target, unix.O_PATH)
	if err != nil {
		c.recordAsyncError("send guest file stat open failure", c.sendProtocolFileOpenError(state, err))
		return
	}
	defer c.closeProtocolFile("close guest file stat descriptor", file)
	info, err := file.Stat()
	if err != nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, "path could not be stated")
		return
	}
	if err := c.sendFile(state, &guestv1.FileFrame{Payload: &guestv1.FileFrame_Metadata{Metadata: &guestv1.FileMetadata{
		Exists:           true,
		Size:             uint64(info.Size()),
		Mode:             uint32(info.Mode()),
		Kind:             protocolFileKind(info.Mode()),
		ModifiedAtUnixMs: uint64(info.ModTime().UnixMilli()),
	}}}); err == nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED, "")
	} else {
		c.recordAsyncError("send guest file stat metadata", err)
	}
}

func (c *protocolConnection) listProtocolDirectory(state *protocolFileState) {
	root, target, err := c.service.server.workspacePath(state.request.WorkspaceRelativePath)
	if err != nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH, "workspace-relative path is invalid")
		return
	}
	directory, err := openWorkspaceTarget(root, target, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		c.recordAsyncError("send guest directory open failure", c.sendProtocolFileOpenError(state, err))
		return
	}
	defer c.closeProtocolFile("close guest directory descriptor", directory)
	entries, err := directory.ReadDir(-1)
	if err != nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, "directory could not be listed")
		return
	}
	children := make([]string, 0, len(entries))
	metadataEntries := make([]*guestv1.FileMetadataEntry, 0, len(entries))
	for _, entry := range entries {
		children = append(children, entry.Name())
		info, err := entry.Info()
		if err != nil {
			c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, "directory entry could not be stated")
			return
		}
		metadataEntries = append(metadataEntries, &guestv1.FileMetadataEntry{
			Path: path.Join(state.request.WorkspaceRelativePath, entry.Name()),
			Kind: protocolFileKind(info.Mode()), Size: uint64(info.Size()),
			ModifiedAtUnixMs: uint64(info.ModTime().UnixMilli()),
		})
	}
	sort.Strings(children)
	sort.Slice(metadataEntries, func(left, right int) bool {
		return metadataEntries[left].Path < metadataEntries[right].Path
	})
	if err := c.sendFile(state, &guestv1.FileFrame{Payload: &guestv1.FileFrame_Metadata{Metadata: &guestv1.FileMetadata{
		Exists:             true,
		Kind:               guestv1.FileKind_FILE_KIND_DIRECTORY,
		DirectChildren:     children,
		DirectChildEntries: metadataEntries,
	}}}); err == nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED, "")
	} else {
		c.recordAsyncError("send guest directory metadata", err)
	}
}

func protocolFileKind(mode os.FileMode) guestv1.FileKind {
	switch {
	case mode&os.ModeSymlink != 0:
		return guestv1.FileKind_FILE_KIND_SYMBOLIC_LINK
	case mode.IsDir():
		return guestv1.FileKind_FILE_KIND_DIRECTORY
	default:
		return guestv1.FileKind_FILE_KIND_FILE
	}
}

func (c *protocolConnection) existsProtocolPath(state *protocolFileState) {
	root, target, err := c.service.server.workspacePath(state.request.WorkspaceRelativePath)
	if err != nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_INVALID_PATH, "workspace-relative path is invalid")
		return
	}
	file, err := openWorkspaceTarget(root, target, unix.O_PATH)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		c.recordAsyncError("send guest file existence open failure", c.sendProtocolFileOpenError(state, err))
		return
	}
	if file != nil {
		c.closeProtocolFile("close guest file existence descriptor", file)
	}
	if err := c.sendFile(state, &guestv1.FileFrame{Payload: &guestv1.FileFrame_Metadata{Metadata: &guestv1.FileMetadata{
		Exists: exists,
	}}}); err == nil {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED, "")
	} else {
		c.recordAsyncError("send guest file existence metadata", err)
	}
}

func (c *protocolConnection) sendLegacyFileResponse(state *protocolFileState, response toolExecResponse) {
	if response.Error != "" {
		c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED, response.Error)
		return
	}
	c.sendFileTerminalRecorded(state, guestv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED, "")
}

func (c *protocolConnection) sendFileTerminalRecorded(
	state *protocolFileState,
	kind guestv1.FileTerminalKind,
	detail string,
) {
	c.recordAsyncError("send guest file terminal", c.sendFileTerminal(state, kind, detail))
}

func (c *protocolConnection) closeProtocolFile(action string, file *os.File) {
	if file != nil {
		c.recordAsyncError(action, file.Close())
	}
}

func (c *protocolConnection) sendProtocolFileOpenError(state *protocolFileState, err error) error {
	kind := guestv1.FileTerminalKind_FILE_TERMINAL_KIND_PERMISSION_DENIED
	detail := "workspace path could not be opened"
	switch {
	case errors.Is(err, errUnsafeWorkspacePath), errors.Is(err, unix.ELOOP):
		kind = guestv1.FileTerminalKind_FILE_TERMINAL_KIND_SYMLINK_REJECTED
		detail = "workspace path contains a symlink"
	case os.IsNotExist(err):
		kind = guestv1.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND
		detail = "workspace path was not found"
	}
	return c.sendFileTerminal(state, kind, detail)
}

func (c *protocolConnection) sendFileTerminal(state *protocolFileState, kind guestv1.FileTerminalKind, detail string) error {
	return c.sendFile(state, &guestv1.FileFrame{Payload: &guestv1.FileFrame_Terminal{Terminal: &guestv1.FileTerminal{
		Kind:       kind,
		SafeDetail: detail,
	}}})
}

func (c *protocolConnection) sendFile(state *protocolFileState, frame *guestv1.FileFrame) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	frame.Binding = cloneOperationBinding(state.binding)
	frame.Binding.Sequence = state.binding.Sequence
	state.binding.Sequence++
	return c.stream.Send(&guestv1.GuestToRunner{Message: &guestv1.GuestToRunner_File{File: frame}})
}

func (c *protocolConnection) removeFile(key string) {
	c.mu.Lock()
	delete(c.files, key)
	c.mu.Unlock()
}

func (c *protocolConnection) cancelAllFiles() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, state := range c.files {
		state.cancel(errProtocolExecCancelled)
	}
}
