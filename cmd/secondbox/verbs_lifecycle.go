package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const verbWaitTimeout = 10 * time.Minute
const verbPollInterval = 250 * time.Millisecond

func verbFlags(command string) *flag.FlagSet {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func verbClient(session cliSession, transport *http.Client) (*sb.Client, error) {
	if err := requireSessionCredentials("Sandbox command", session); err != nil {
		return nil, err
	}
	return sb.NewSecondBoxSubjectClient(session.url, session.token, session.tenantRef, session.subjectRef, transport)
}

func emitVerb(ctx context.Context, output io.Writer, viewOperation string, raw []byte, value any) error {
	renderer := presentationFromContext(ctx, output).renderer
	if renderer.HumanOutput() {
		content, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("SecondBox CLI encode view: %w", err)
		}
		return renderBoundedOperation(viewOperation, content, renderer)
	}
	return cliui.WriteJSONPassthrough(output, raw)
}

func runLifecycleVerb(ctx context.Context, session cliSession, command string, args []string, output io.Writer, transport *http.Client) error {
	if command == "list" {
		command = "ls"
	}
	if command == "delete" {
		command = "rm"
	}
	var operand string
	var err error
	if command != "ls" {
		operand, args, err = splitLeadingOperand(command, "Profile or Sandbox", args)
		if err != nil {
			return err
		}
	}
	flags := verbFlags(command)
	var name, source, cursor string
	var metadata repeatedValues
	var noWait, force bool
	var limit int
	if command == "create" {
		flags.StringVar(&name, "name", "", "reserved Sandbox name")
		flags.StringVar(&source, "from", "", "Snapshot identifier or sandbox/name")
		flags.Var(&metadata, "metadata", "metadata key=value; repeatable")
	}
	if command == "start" || command == "stop" || command == "rm" {
		flags.BoolVar(&noWait, "no-wait", false, "return the admitted Operation without waiting")
	}
	if command == "rm" {
		flags.BoolVar(&force, "force", false, "skip terminal confirmation")
	}
	if command == "ls" {
		flags.StringVar(&name, "name", "", "exact reserved Sandbox name")
		flags.StringVar(&cursor, "cursor", "", "page continuation cursor")
		flags.IntVar(&limit, "limit", 100, "page size")
	}
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("SecondBox CLI %s options: %w", command, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("SecondBox CLI %s has unexpected arguments", command)
	}
	client, err := verbClient(session, transport)
	if err != nil {
		return err
	}
	var raw []byte
	capture := func(operation string) context.Context {
		return sb.CaptureJSONResponse(ctx, operation, func(content []byte) { raw = content })
	}
	if command == "ls" {
		options := sb.SandboxListOptions{PageOptions: sb.PageOptions{Limit: limit, Cursor: cursor}}
		if name != "" {
			options.Metadata = sb.Metadata{contracts.SandboxNameMetadataKey: name}
		}
		page, err := client.ListSandboxes(capture("listSandboxes"), options)
		if err != nil {
			return err
		}
		return emitVerb(ctx, output, "listSandboxes", raw, page)
	}
	if command == "create" {
		values, err := parsePairs(metadata)
		if err != nil {
			return fmt.Errorf("SecondBox CLI create metadata: %w", err)
		}
		if name != "" {
			if existing, ok := values[contracts.SandboxNameMetadataKey]; ok && existing != name {
				return errors.New("SecondBox CLI create --name conflicts with metadata")
			}
			values[contracts.SandboxNameMetadataKey] = name
		}
		if source != "" {
			source, err = resolveSnapshotReference(ctx, client, source)
			if err != nil {
				return err
			}
		}
		_, operation, err := client.CreateSandbox(capture("createSandbox"), sb.CreateSandboxRequest{Profile: operand, Metadata: values, SourceSnapshotID: source}, "")
		if err != nil {
			return err
		}
		return emitVerb(ctx, output, "getOperation", raw, operation)
	}
	handle, err := resolveSandboxReference(capture("getSandbox"), client, operand)
	if err != nil {
		return err
	}
	if command == "get" {
		// A name resolves through a list; fetch its own representation for exact JSON.
		sandbox, err := handle.Refresh(capture("getSandbox"))
		if err != nil {
			return err
		}
		return emitVerb(ctx, output, "getSandbox", raw, sandbox)
	}
	if command == "rm" && !force {
		if err := confirmSandboxRemoval(ctx, output, handle.Snapshot().ID); err != nil {
			return err
		}
	}
	var operation sb.Operation
	var target sb.SandboxState
	switch command {
	case "start":
		operation, err = handle.Start(capture("startSandbox"), sb.StartSandboxRequest{}, sb.LifecycleOptions{})
		target = sb.SandboxStateReady
	case "stop":
		operation, err = handle.Stop(capture("stopSandbox"), sb.LifecycleOptions{})
		target = sb.SandboxStateStopped
	case "rm":
		operation, err = handle.Delete(capture("deleteSandbox"), sb.LifecycleOptions{})
		target = sb.SandboxStateDeleted
	default:
		return fmt.Errorf("SecondBox CLI unknown lifecycle verb %q", command)
	}
	if err != nil {
		return err
	}
	if noWait {
		return emitVerb(ctx, output, "getOperation", raw, operation)
	}
	waitCtx, cancel := context.WithTimeout(ctx, verbWaitTimeout)
	defer cancel()
	if _, err := client.WaitOperation(waitCtx, operation.ID, verbPollInterval); err != nil {
		return err
	}
	sandbox, err := handle.WaitFor(waitCtx, target)
	if err != nil {
		return err
	}
	return emitVerb(ctx, output, "getSandbox", raw, sandbox)
}

func confirmSandboxRemoval(ctx context.Context, output io.Writer, id string) error {
	view := presentationFromContext(ctx, output)
	if !view.renderer.Capabilities.Input.TTY {
		return nil
	}
	accepted := false
	form := cliui.HuhForm{Groups: []cliui.GroupSpec{{Fields: []cliui.FieldSpec{{Kind: cliui.FieldConfirm, Title: "Delete Sandbox " + id + "?", BoolValue: &accepted}}}}}
	if err := form.Run(ctx, cliui.FormHandles{Input: view.input, Output: view.renderer.Diagnostic, Accessible: view.accessible}); err != nil {
		return err
	}
	if !accepted {
		return errors.New("SecondBox CLI Sandbox deletion cancelled")
	}
	return nil
}
