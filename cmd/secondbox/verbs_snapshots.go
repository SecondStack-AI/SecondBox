package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func resolveSnapshotReference(ctx context.Context, client *sb.Client, reference string) (string, error) {
	if strings.HasPrefix(reference, "snp_") {
		return reference, nil
	}
	sandbox, name, found := strings.Cut(reference, "/")
	if !found || sandbox == "" || name == "" {
		return "", errors.New("SecondBox CLI Snapshot reference requires sandbox/name or snp_ identifier")
	}
	handle, err := resolveSandboxReference(ctx, client, sandbox)
	if err != nil {
		return "", err
	}
	cursor, match := "", ""
	for range nameResolutionPageBound {
		page, err := handle.ListSnapshots(ctx, sb.PageOptions{Limit: nameResolutionPageLimit, Cursor: cursor})
		if err != nil {
			return "", err
		}
		for _, snapshot := range page.Items {
			if snapshot.Name == name && snapshot.State == "ready" {
				if match != "" {
					return "", fmt.Errorf("SecondBox CLI Snapshot reference %q is ambiguous; use an identifier", reference)
				}
				match = snapshot.ID
			}
		}
		if page.NextCursor == nil {
			if match != "" {
				return match, nil
			}
			return "", fmt.Errorf("SecondBox CLI found no ready Snapshot %q", reference)
		}
		cursor = *page.NextCursor
	}
	return "", fmt.Errorf("SecondBox CLI Snapshot reference exceeds %d pages; use an identifier", nameResolutionPageBound)
}

func runSnapshotVerb(ctx context.Context, session cliSession, command string, args []string, output io.Writer, transport *http.Client) error {
	if command == "snapshot" && len(args) > 0 && args[0] == "rm" {
		command, args = "snapshot rm", args[1:]
	}
	reference, args, err := splitLeadingOperand(command, "Sandbox or Snapshot", args)
	if err != nil {
		return err
	}
	var source string
	if command == "restore" {
		source, args, err = splitLeadingOperand(command, "Snapshot", args)
		if err != nil {
			return err
		}
	}
	flags := verbFlags(command)
	var name, cursor string
	var limit int
	if command == "snapshot" {
		flags.StringVar(&name, "name", "", "Snapshot name")
	}
	if command == "snapshots" {
		flags.StringVar(&cursor, "cursor", "", "page continuation cursor")
		flags.IntVar(&limit, "limit", 100, "page size")
	}
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("SecondBox CLI %s options: %w", command, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("SecondBox CLI %s has unexpected arguments", command)
	}
	if command == "snapshot" && strings.TrimSpace(name) == "" {
		return errors.New("SecondBox CLI snapshot requires --name")
	}
	client, err := verbClient(session, transport)
	if err != nil {
		return err
	}
	var raw []byte
	capture := func(operation string) context.Context {
		return sb.CaptureJSONResponse(ctx, operation, func(content []byte) { raw = content })
	}
	if command == "snapshot rm" {
		id, err := resolveSnapshotReference(ctx, client, reference)
		if err != nil {
			return err
		}
		operation, err := client.DeleteSnapshot(capture("deleteSnapshot"), id, "")
		if err != nil {
			return err
		}
		return emitVerb(ctx, output, "getOperation", raw, operation)
	}
	handle, err := resolveSandboxReference(ctx, client, reference)
	if err != nil {
		return err
	}
	if command == "snapshots" {
		page, err := handle.ListSnapshots(capture("listSandboxSnapshots"), sb.PageOptions{Limit: limit, Cursor: cursor})
		if err != nil {
			return err
		}
		return emitVerb(ctx, output, "listSandboxSnapshots", raw, page)
	}
	if command == "restore" {
		id, err := resolveSnapshotReference(ctx, client, source)
		if err != nil {
			return err
		}
		operation, err := handle.Restore(capture("restoreSandboxSnapshot"), sb.LifecycleOptions{}, id)
		if err != nil {
			return err
		}
		return emitVerb(ctx, output, "getOperation", raw, operation)
	}
	operation, err := handle.CreateSnapshot(capture("createSandboxSnapshot"), sb.LifecycleOptions{}, sb.CreateSnapshotRequest{Name: name, Metadata: sb.Metadata{}})
	if err != nil {
		return err
	}
	if operation.Snapshot == nil {
		return errors.New("SecondBox CLI Snapshot creation returned no Snapshot")
	}
	waitCtx, cancel := context.WithTimeout(ctx, verbWaitTimeout)
	defer cancel()
	if _, err := client.WaitOperation(waitCtx, operation.ID, verbPollInterval); err != nil {
		return err
	}
	snapshot, err := client.GetSnapshot(waitCtx, operation.Snapshot.ID)
	if err != nil {
		return err
	}
	if snapshot.State != "ready" {
		return fmt.Errorf("SecondBox CLI Snapshot %s is %s after creation", snapshot.ID, snapshot.State)
	}
	return emitVerb(ctx, output, "getSnapshot", raw, snapshot)
}
