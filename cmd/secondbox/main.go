package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

type commandAlias struct {
	operation string
	injected  []string
}

var commandAliases = map[string]commandAlias{
	"profiles list":       {operation: "listProfiles"},
	"profiles create":     {operation: "createProfile"},
	"profiles get":        {operation: "getProfile"},
	"profiles revise":     {operation: "reviseProfile"},
	"profiles disable":    {operation: "disableProfile"},
	"runner-pools list":   {operation: "listRunnerPools"},
	"runner-pools create": {operation: "createRunnerPool"},
	"runner-pools get":    {operation: "getRunnerPool"},
	"runner-pools update": {operation: "updateRunnerPool"},
	"runners list":        {operation: "listRunners"},
	"runners get":         {operation: "getRunner"},
	"sandboxes list":      {operation: "listSandboxes"},
	"sandboxes create":    {operation: "createSandbox"},
	"sandboxes get":       {operation: "getSandbox"},
	"sandboxes start":     {operation: "startSandbox"},
	"sandboxes drain":     {operation: "drainSandbox"},
	"sandboxes stop":      {operation: "stopSandbox"},
	"sandboxes relocate":  {operation: "relocateSandbox"},
	"sandboxes restore":   {operation: "restoreSandboxSnapshot"},
	"sandboxes delete":    {operation: "deleteSandbox"},
	"sandboxes inspect":   {operation: "inspectSandbox"},
	"sandboxes ping":      {operation: "pingSandbox"},
	"sandboxes touch":     {operation: "touchSandbox"},
	"sandboxes wait":      {operation: "waitForSandbox"},
	"operations get":      {operation: "getOperation"},
	"exec stream":         {operation: "createSandboxExecStream"},
	"exec cancel":         {operation: "cancelSandboxExecStream"},
	"shell create":        {operation: "createSandboxTerminal"},
	"shell reconnect":     {operation: "reconnectSandboxTerminal"},
	"shell close":         {operation: "cancelSandboxTerminal"},
	"files read":          {operation: "readSandboxFile"},
	"files write":         {operation: "writeSandboxFile"},
	"files stat":          {operation: "statSandboxFile"},
	"files exists":        {operation: "sandboxFileExists"},
	"files list":          {operation: "listSandboxDirectory"},
	"files mkdir":         {operation: "createSandboxDirectory"},
	"files rm":            {operation: "removeSandboxPath"},
	"snapshots create":    {operation: "createSandboxSnapshot"},
	"snapshots list":      {operation: "listSandboxSnapshots"},
	"snapshots get":       {operation: "getSnapshot"},
	"snapshots delete":    {operation: "deleteSnapshot"},
	"snapshots restore":   {operation: "restoreSandboxSnapshot"},
	"artifacts list":      {operation: "listSandboxArtifacts"},
	"artifacts upload":    {operation: "uploadSandboxArtifact"},
	"artifacts get":       {operation: "getArtifact"},
	"artifacts download":  {operation: "downloadArtifactContent"},
	"artifacts delete":    {operation: "deleteArtifact"},
	"leases acquire":      {operation: "acquireSandboxLease"},
	"leases get":          {operation: "getSandboxLease"},
	"leases renew":        {operation: "renewSandboxLease"},
	"leases release":      {operation: "releaseSandboxLease"},
	"ports create":        {operation: "createSandboxPortSession"},
	"ports get":           {operation: "getSandboxPortSession"},
	"ports close":         {operation: "closeSandboxPortSession"},
}

func main() {
	err := run(interruptibleContext(), os.Args[1:], os.Stdout)
	if err == nil {
		return
	}
	// A guest command that exited non-zero already wrote its own diagnosis to
	// standard error; the CLI reports it as an exit status and adds nothing.
	var exited *commandExitError
	if errors.As(err, &exited) {
		os.Exit(exited.code)
	}
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// interruptibleContext cancels on the signals a terminal actually sends, so the
// deferred cleanup that releases a Lease and cancels a Terminal still runs.
//
// Without this an interrupted `secondbox shell` left its Lease active until the
// service expired it, and the next attach failed with a state conflict. The
// second signal restores default handling so an unresponsive cleanup can still
// be abandoned; cleanup itself releases on a context of its own and so is
// unaffected by this cancellation.
func interruptibleContext() context.Context {
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt, syscall.SIGTERM, syscall.SIGHUP,
	)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx
}

func run(ctx context.Context, args []string, output io.Writer) error {
	global := flag.NewFlagSet("secondbox", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	rawURL := global.String("url", "", "absolute SecondBox API endpoint")
	token := global.String("token", "", "SecondBox platform token")
	tenantRef := global.String("tenant-ref", "", "trusted caller tenant reference")
	subjectRef := global.String("subject-ref", "", "trusted caller subject reference")
	if err := global.Parse(args); err != nil {
		return fmt.Errorf("SecondBox CLI parse global options: %w", err)
	}
	session, err := resolveSession(cliSession{
		url: *rawURL, token: *token, tenantRef: *tenantRef, subjectRef: *subjectRef,
	})
	if err != nil {
		return err
	}
	handled, err := runOperationalCommand(ctx, session, global.Args(), output)
	if handled {
		return err
	}
	if session.url == "" {
		return errors.New("SecondBox CLI requires --url" + sessionSourceHint)
	}
	if session.token == "" {
		return errors.New("SecondBox CLI requires --token" + sessionSourceHint)
	}
	if session.tenantRef == "" || session.subjectRef == "" {
		return errors.New("SecondBox CLI requires --tenant-ref and --subject-ref" + sessionSourceHint)
	}
	operationID, operationArgs, err := resolveCommand(global.Args())
	if err != nil {
		return err
	}

	client, err := secondboxclient.NewSecondBoxSubjectClient(
		session.url, session.token, session.tenantRef, session.subjectRef, http.DefaultClient,
	)
	if err != nil {
		return err
	}
	options, body, err := parseOperationOptions(operationID, operationArgs)
	if err != nil {
		return err
	}
	if body != nil && body != os.Stdin {
		defer body.Close()
	}
	response, err := client.Request(ctx, operationID, options)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, response.Body)
	closeErr := response.Body.Close()
	if copyErr != nil {
		return fmt.Errorf("SecondBox CLI copy %s response: %w", operationID, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("SecondBox CLI close %s response: %w", operationID, closeErr)
	}
	return nil
}

func runOperationalCommand(
	ctx context.Context,
	session cliSession,
	args []string,
	output io.Writer,
) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "login":
		return true, runLoginCommand(ctx, session, args[1:], output, http.DefaultClient)
	case "logout":
		return true, runLogoutCommand(session, args[1:], output)
	case "whoami":
		return true, runWhoamiCommand(session, args[1:], output)
	}
	if args[0] == "exec" && !isExecSubcommand(args) {
		return true, runExecCommand(ctx, session, args[1:], execCommandEnvironment{
			stdin: os.Stdin, stdout: output, stderr: os.Stderr,
			httpClient: http.DefaultClient,
		})
	}
	if args[0] == "run" {
		return true, runRunCommand(ctx, session, args[1:], execCommandEnvironment{
			stdin: os.Stdin, stdout: output, stderr: os.Stderr,
			httpClient: http.DefaultClient,
		}, sandboxShellEnvironment{
			input: os.Stdin, output: output,
			inputFD: int(os.Stdin.Fd()), outputFD: outputFileDescriptor(output),
			httpClient: http.DefaultClient,
		})
	}
	if args[0] == "shell" && !isShellSubcommand(args) {
		return true, runShellCommand(ctx, session, args[1:], sandboxShellEnvironment{
			input: os.Stdin, output: output,
			inputFD: int(os.Stdin.Fd()), outputFD: outputFileDescriptor(output),
			httpClient: http.DefaultClient,
		}, http.DefaultClient)
	}
	if len(args) < 2 {
		return false, nil
	}
	switch {
	case args[0] == "sandbox" && args[1] == "shell":
		return true, runSandboxShellCommand(
			ctx, session.url, session.token, session.tenantRef, session.subjectRef, args[2:],
			sandboxShellEnvironment{
				input: os.Stdin, output: output,
				inputFD: int(os.Stdin.Fd()), outputFD: outputFileDescriptor(output),
				httpClient: http.DefaultClient,
			},
		)
	case args[0] == "exec" && args[1] == "stream":
		return true, runExecStreamCommand(
			ctx, session.url, session.token, session.tenantRef, session.subjectRef,
			args[2:], os.Stdin, output, http.DefaultClient, nil,
		)
	case args[0] == "timings" &&
		(args[1] == "sandbox" || args[1] == "operation" || args[1] == "summary"):
		return true, runTimingCommand(
			ctx, session.url, session.token, session.tenantRef, session.subjectRef,
			args[1], args[2:], output, http.DefaultClient,
		)
	case args[0] == "diagnostics" && args[1] == "bundle":
		return true, runDiagnosticsBundleCommand(
			ctx, session.url, session.token, args[2:], output, http.DefaultClient,
		)
	case args[0] == "logs" && (args[1] == "tail" || args[1] == "follow"):
		return true, runLogsCommand(ctx, args[1], args[2:], output)
	default:
		return false, nil
	}
}

// isExecSubcommand distinguishes the streaming and cancellation subcommands
// from `exec <sandbox> -- command`.
func isExecSubcommand(args []string) bool {
	return len(args) >= 2 && (args[1] == "stream" || args[1] == "cancel")
}

// isShellSubcommand distinguishes the terminal negotiation aliases from
// `shell <sandbox>`.
func isShellSubcommand(args []string) bool {
	return len(args) >= 2 &&
		(args[1] == "create" || args[1] == "reconnect" || args[1] == "close")
}

func outputFileDescriptor(output io.Writer) int {
	file, ok := output.(*os.File)
	if !ok {
		return -1
	}
	return int(file.Fd())
}

func resolveCommand(args []string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, errors.New("SecondBox CLI requires a command")
	}
	if args[0] == "operation" {
		if len(args) < 2 {
			return "", nil, errors.New("SecondBox CLI operation requires an OpenAPI operationId")
		}
		return args[1], args[2:], nil
	}
	for width := 2; width >= 1; width-- {
		if len(args) < width {
			continue
		}
		key := strings.Join(args[:width], " ")
		if alias, exists := commandAliases[key]; exists {
			var rest []string
			rest = append(rest, alias.injected...)
			rest = append(rest, args[width:]...)
			return alias.operation, rest, nil
		}
	}
	return "", nil, fmt.Errorf("SecondBox CLI unknown command %q; available commands: %s", strings.Join(args, " "), commandSummary())
}

type repeatedValues []string

func (values *repeatedValues) String() string {
	return strings.Join(*values, ",")
}

func (values *repeatedValues) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func parseOperationOptions(operationID string, args []string) (secondboxclient.CallOptions, *os.File, error) {
	if _, found := secondboxclient.LookupOperation(operationID); !found {
		return secondboxclient.CallOptions{}, nil, fmt.Errorf("SecondBox CLI unknown OpenAPI operation %q", operationID)
	}
	flags := flag.NewFlagSet(operationID, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var paths repeatedValues
	var queries repeatedValues
	var headers repeatedValues
	flags.Var(&paths, "path", "path parameter name=value; repeatable")
	flags.Var(&queries, "query", "query parameter name=value; repeatable")
	flags.Var(&headers, "header", "request header name=value; repeatable")
	bodyPath := flags.String("body", "", "request body file, or - for stdin")
	contentType := flags.String("content-type", "", "request content type")
	if err := flags.Parse(args); err != nil {
		return secondboxclient.CallOptions{}, nil, fmt.Errorf("SecondBox CLI parse %s options: %w", operationID, err)
	}
	if len(flags.Args()) != 0 {
		return secondboxclient.CallOptions{}, nil, fmt.Errorf("SecondBox CLI unexpected %s arguments: %s", operationID, strings.Join(flags.Args(), " "))
	}
	pathParameters, err := parsePairs(paths)
	if err != nil {
		return secondboxclient.CallOptions{}, nil, fmt.Errorf("SecondBox CLI %s path parameter: %w", operationID, err)
	}
	queryPairs, err := parsePairs(queries)
	if err != nil {
		return secondboxclient.CallOptions{}, nil, fmt.Errorf("SecondBox CLI %s query parameter: %w", operationID, err)
	}
	headerPairs, err := parsePairs(headers)
	if err != nil {
		return secondboxclient.CallOptions{}, nil, fmt.Errorf("SecondBox CLI %s header: %w", operationID, err)
	}
	query := make(url.Values)
	for name, value := range queryPairs {
		query.Add(name, value)
	}
	requestHeaders := make(http.Header)
	for name, value := range headerPairs {
		requestHeaders.Add(name, value)
	}

	var body *os.File
	if *bodyPath == "-" {
		body = os.Stdin
	} else if *bodyPath != "" {
		body, err = os.Open(*bodyPath)
		if err != nil {
			return secondboxclient.CallOptions{}, nil, fmt.Errorf("SecondBox CLI open %s body %q: %w", operationID, *bodyPath, err)
		}
	}
	options := secondboxclient.CallOptions{
		PathParameters:  pathParameters,
		QueryParameters: query,
		Headers:         requestHeaders,
		ContentType:     *contentType,
	}
	// CallOptions.Body is an interface. Assigning a nil *os.File unconditionally
	// would produce a non-nil interface holding a nil pointer, which the HTTP
	// client then reads and fails with os.ErrInvalid before reaching the network.
	if body != nil {
		options.Body = body
	}
	return options, body, nil
}

func parsePairs(values []string) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		name, item, found := strings.Cut(value, "=")
		if !found || name == "" {
			return nil, fmt.Errorf("expected name=value, got %q", value)
		}
		result[name] = item
	}
	return result, nil
}

func commandSummary() string {
	keys := make([]string, 0, len(commandAliases))
	for key := range commandAliases {
		keys = append(keys, key)
	}
	keys = append(
		keys,
		"diagnostics bundle", "exec", "login", "logout", "logs follow", "logs tail",
		"run", "sandbox shell", "shell", "timings operation", "timings sandbox",
		"timings summary", "whoami",
	)
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
