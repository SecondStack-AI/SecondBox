package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

type remoteCopyPath struct{ sandbox, path string }

func parseRemoteCopyPath(value string) (*remoteCopyPath, error) {
	sandbox, absolute, remote := strings.Cut(value, ":")
	if !remote {
		return nil, nil
	}
	if sandbox == "" || !strings.HasPrefix(absolute, "/") {
		return nil, errors.New("SecondBox CLI cp remote operand requires sandbox:/workspace/path")
	}
	for _, part := range strings.Split(absolute, "/") {
		if part == ".." {
			return nil, errors.New("SecondBox CLI cp refuses parent traversal")
		}
	}
	absolute = path.Clean(absolute)
	if absolute != "/workspace" && !strings.HasPrefix(absolute, "/workspace/") {
		return nil, errors.New("SecondBox CLI cp remote path must be inside /workspace")
	}
	relative := strings.TrimPrefix(strings.TrimPrefix(absolute, "/workspace"), "/")
	if relative == "" {
		relative = "."
	}
	return &remoteCopyPath{sandbox: sandbox, path: relative}, nil
}

func runCopyVerb(ctx context.Context, session cliSession, args []string, output io.Writer, transport *http.Client) error {
	recursive := false
	operands := make([]string, 0, 2)
	options := true
	for _, arg := range args {
		if options && arg == "--" {
			options = false
			continue
		}
		if options && (arg == "-r" || arg == "--recursive") {
			recursive = true
			continue
		}
		if options && strings.HasPrefix(arg, "-") {
			return fmt.Errorf("SecondBox CLI cp unknown option %q", arg)
		}
		operands = append(operands, arg)
	}
	if len(operands) != 2 {
		return errors.New("SecondBox CLI cp requires a source and destination")
	}
	source, err := parseRemoteCopyPath(operands[0])
	if err != nil {
		return err
	}
	destination, err := parseRemoteCopyPath(operands[1])
	if err != nil {
		return err
	}
	if (source == nil) == (destination == nil) {
		return errors.New("SecondBox CLI cp requires exactly one remote operand")
	}
	client, err := verbClient(session, transport)
	if err != nil {
		return err
	}
	remote := source
	if remote == nil {
		remote = destination
	}
	handle, err := resolveSandboxReference(ctx, client, remote.sandbox)
	if err != nil {
		return err
	}
	if destination != nil {
		exists, lookupErr := handle.FileExists(ctx, destination.path, "")
		if lookupErr != nil {
			return lookupErr
		}
		if exists {
			stat, err := handle.StatFile(ctx, destination.path, "")
			if err != nil {
				return err
			}
			if stat.Kind == sb.FileKindDirectory {
				destination.path = path.Join(destination.path, filepath.Base(filepath.Clean(operands[0])))
			}
		}
		err = uploadCopyPath(ctx, handle, operands[0], destination.path, recursive, output)
	} else {
		local := operands[1]
		if stat, statErr := os.Stat(local); statErr == nil && stat.IsDir() {
			local = filepath.Join(local, path.Base(source.path))
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("SecondBox CLI cp destination: %w", statErr)
		}
		err = downloadCopyPath(ctx, handle, source.path, local, recursive)
	}
	if err != nil {
		return fmt.Errorf("SecondBox CLI cp: %w", err)
	}
	renderer := presentationFromContext(ctx, output).renderer
	if renderer.HumanOutput() {
		return renderer.WriteSummary(cliui.Summary{Title: "Files copied", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Source", Value: operands[0]}, {Key: "Destination", Value: operands[1]}}})
	}
	return nil
}

func uploadCopyPath(ctx context.Context, handle *sb.SandboxHandle, local, remote string, recursive bool, output io.Writer) error {
	info, err := os.Lstat(local)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if !recursive {
			return errors.New("directory copy requires -r")
		}
		if err := handle.CreateDirectory(ctx, remote, true, "", ""); err != nil {
			return err
		}
		entries, err := os.ReadDir(local)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := uploadCopyPath(ctx, handle, filepath.Join(local, entry.Name()), path.Join(remote, entry.Name()), true, output); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular source %q", local)
	}
	file, err := os.Open(local)
	if err != nil {
		return err
	}
	defer file.Close()
	var raw []byte
	capture := sb.CaptureJSONResponse(ctx, "writeSandboxFile", func(content []byte) { raw = content })
	_, err = handle.WriteFileFrom(capture, remote, file, "", "")
	if err != nil {
		return err
	}
	if !presentationFromContext(ctx, output).renderer.HumanOutput() {
		return cliui.WriteJSONPassthrough(output, raw)
	}
	return nil
}

func downloadCopyPath(ctx context.Context, handle *sb.SandboxHandle, remote, local string, recursive bool) error {
	stat, err := handle.StatFile(ctx, remote, "")
	if err != nil {
		return err
	}
	if stat.Kind == sb.FileKindDirectory {
		if !recursive {
			return errors.New("directory copy requires -r")
		}
		if info, err := os.Lstat(local); err == nil && !info.IsDir() {
			return fmt.Errorf("destination %q is not a directory", local)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(local, 0755); err != nil {
			return err
		}
		listing, err := handle.ListDirectory(ctx, remote, "")
		if err != nil {
			return err
		}
		for _, entry := range listing.Entries {
			clean := path.Clean(entry.Path)
			if clean != entry.Path || path.Dir(clean) != path.Clean(remote) || clean == "." || path.Base(clean) == ".." {
				return fmt.Errorf("invalid directory child %q", entry.Path)
			}
			if err := downloadCopyPath(ctx, handle, clean, filepath.Join(local, path.Base(clean)), true); err != nil {
				return err
			}
		}
		return nil
	}
	if stat.Kind != sb.FileKindFile {
		return fmt.Errorf("refusing non-regular remote source %q", remote)
	}
	// Replace atomically, so interrupted transfers do not truncate an existing file
	// and an existing destination symlink is replaced rather than followed.
	file, err := os.CreateTemp(filepath.Dir(local), ".secondbox-copy-*")
	if err != nil {
		return err
	}
	name := file.Name()
	transferErr := handle.ReadFileTo(ctx, remote, file, "")
	closeErr := file.Close()
	if err := errors.Join(transferErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(name))
	}
	if err := os.Rename(name, local); err != nil {
		return errors.Join(err, os.Remove(name))
	}
	return nil
}

func runListFilesVerb(ctx context.Context, session cliSession, args []string, output io.Writer, transport *http.Client) error {
	if len(args) != 2 {
		return errors.New("SecondBox CLI ls-files requires a Sandbox and absolute /workspace path")
	}
	remote, err := parseRemoteCopyPath(args[0] + ":" + args[1])
	if err != nil {
		return err
	}
	client, err := verbClient(session, transport)
	if err != nil {
		return err
	}
	handle, err := resolveSandboxReference(ctx, client, remote.sandbox)
	if err != nil {
		return err
	}
	var raw []byte
	listing, err := handle.ListDirectory(sb.CaptureJSONResponse(ctx, "listSandboxDirectory", func(content []byte) { raw = content }), remote.path, "")
	if err != nil {
		return err
	}
	return emitVerb(ctx, output, "listSandboxDirectory", raw, listing)
}
