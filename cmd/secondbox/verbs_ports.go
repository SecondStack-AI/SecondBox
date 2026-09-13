package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func parseForwardPorts(value string) (int, int, error) {
	local, remote, found := strings.Cut(value, ":")
	if !found {
		remote = local
	}
	parse := func(value string) (int, error) {
		number, err := strconv.Atoi(value)
		if err != nil || number < 1 || number > 65535 {
			return 0, fmt.Errorf("SecondBox CLI ports forward port %q must be from 1 through 65535", value)
		}
		return number, nil
	}
	localPort, err := parse(local)
	if err != nil {
		return 0, 0, err
	}
	remotePort, err := parse(remote)
	return localPort, remotePort, err
}

func runPortForwardVerb(ctx context.Context, session cliSession, args []string, output io.Writer, transport *http.Client) error {
	reference, args, err := splitLeadingOperand("ports forward", "Sandbox", args)
	if err != nil {
		return err
	}
	ports, args, err := splitLeadingOperand("ports forward", "local:remote port", args)
	if err != nil {
		return err
	}
	local, remote, err := parseForwardPorts(ports)
	if err != nil {
		return err
	}
	flags := verbFlags("ports forward")
	bind := flags.String("bind", "127.0.0.1", "local listen address")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("SecondBox CLI ports forward options: %w", err)
	}
	if flags.NArg() != 0 {
		return errors.New("SecondBox CLI ports forward has unexpected arguments")
	}
	client, err := verbClient(session, transport)
	if err != nil {
		return err
	}
	handle, err := resolveSandboxReference(ctx, client, reference)
	if err != nil {
		return err
	}
	policy, err := handle.PortPolicyForNumber(ctx, remote)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(*bind, strconv.Itoa(local)))
	if err != nil {
		return fmt.Errorf("SecondBox CLI ports forward listen: %w", err)
	}
	var raw []byte
	var once sync.Once
	ctx = sb.CaptureJSONResponse(ctx, "createSandboxPortSession", func(content []byte) { once.Do(func() { raw = content }) })
	return handle.ForwardPort(ctx, listener, policy, func(port sb.PortSession) error {
		renderer := presentationFromContext(ctx, output).renderer
		if renderer.HumanOutput() {
			return renderer.WriteSummary(cliui.Summary{Title: "Port forwarding", Status: cliui.StatusActive, Pairs: []cliui.Pair{{Key: "Local", Value: listener.Addr().String()}, {Key: "Sandbox", Value: handle.Snapshot().ID}, {Key: "Remote port", Value: strconv.Itoa(remote)}, {Key: "Policy", Value: port.Name}}})
		}
		return cliui.WriteJSONPassthrough(output, raw)
	})
}
