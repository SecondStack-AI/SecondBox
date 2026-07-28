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
	"sort"
	"strings"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

type commandAlias struct {
	operation string
	injected  []string
}

var commandAliases = map[string]commandAlias{
	"auth check":              {operation: "listProjects", injected: []string{"--query", "limit=1"}},
	"projects list":           {operation: "listProjects"},
	"projects create":         {operation: "createProject"},
	"projects get":            {operation: "getProject"},
	"projects update":         {operation: "updateProject"},
	"keys list":               {operation: "listAPIKeys"},
	"keys create":             {operation: "createAPIKey"},
	"keys revoke":             {operation: "revokeAPIKey"},
	"profiles list":           {operation: "listProfiles"},
	"profiles create":         {operation: "createProfile"},
	"profiles get":            {operation: "getProfile"},
	"profiles revise":         {operation: "reviseProfile"},
	"profiles disable":        {operation: "disableProfile"},
	"runner-pools list":       {operation: "listRunnerPools"},
	"runner-pools create":     {operation: "createRunnerPool"},
	"runner-pools get":        {operation: "getRunnerPool"},
	"runner-pools update":     {operation: "updateRunnerPool"},
	"runners list":            {operation: "listRunners"},
	"runners get":             {operation: "getRunner"},
	"sandboxes list":          {operation: "listSandboxes"},
	"sandboxes create":        {operation: "createSandbox"},
	"sandboxes get":           {operation: "getSandbox"},
	"sandboxes start":         {operation: "startSandbox"},
	"sandboxes drain":         {operation: "drainSandbox"},
	"sandboxes stop":          {operation: "stopSandbox"},
	"sandboxes checkpoint":    {operation: "checkpointSandbox"},
	"sandboxes delete":        {operation: "deleteSandbox"},
	"sandboxes inspect":       {operation: "inspectSandbox"},
	"sandboxes ping":          {operation: "pingSandbox"},
	"sandboxes touch":         {operation: "touchSandbox"},
	"sandboxes wait":          {operation: "waitForSandbox"},
	"operations get":          {operation: "getOperation"},
	"exec":                    {operation: "executeSandboxCommand"},
	"exec stream":             {operation: "createSandboxExecStream"},
	"exec cancel":             {operation: "cancelSandboxExecStream"},
	"shell create":            {operation: "createSandboxTerminal"},
	"shell reconnect":         {operation: "reconnectSandboxTerminal"},
	"shell close":             {operation: "cancelSandboxTerminal"},
	"files read":              {operation: "readSandboxFile"},
	"files write":             {operation: "writeSandboxFile"},
	"files stat":              {operation: "statSandboxFile"},
	"files exists":            {operation: "sandboxFileExists"},
	"files list":              {operation: "listSandboxDirectory"},
	"files mkdir":             {operation: "createSandboxDirectory"},
	"files rm":                {operation: "removeSandboxPath"},
	"checkpoints create":      {operation: "checkpointSandbox"},
	"artifacts list":          {operation: "listSandboxArtifacts"},
	"artifacts upload":        {operation: "uploadSandboxArtifact"},
	"artifacts get":           {operation: "getArtifact"},
	"artifacts download":      {operation: "downloadArtifactContent"},
	"artifacts delete":        {operation: "deleteArtifact"},
	"leases acquire":          {operation: "acquireSandboxLease"},
	"leases get":              {operation: "getSandboxLease"},
	"leases renew":            {operation: "renewSandboxLease"},
	"leases release":          {operation: "releaseSandboxLease"},
	"ports create":            {operation: "createSandboxPortSession"},
	"ports get":               {operation: "getSandboxPortSession"},
	"ports close":             {operation: "closeSandboxPortSession"},
	"service-accounts list":   {operation: "listServiceAccounts"},
	"service-accounts create": {operation: "createServiceAccount"},
	"service-accounts get":    {operation: "getServiceAccount"},
	"service-accounts update": {operation: "updateServiceAccount"},
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	global := flag.NewFlagSet("secondbox", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	rawURL := global.String("url", "", "absolute SecondBox API endpoint")
	token := global.String("token", "", "SecondBox service-account token")
	if err := global.Parse(args); err != nil {
		return fmt.Errorf("SecondBox CLI parse global options: %w", err)
	}
	handled, err := runOperationalCommand(ctx, *rawURL, *token, global.Args(), output)
	if handled {
		return err
	}
	if *rawURL == "" {
		return errors.New("SecondBox CLI requires --url")
	}
	if *token == "" {
		return errors.New("SecondBox CLI requires --token")
	}
	operationID, operationArgs, err := resolveCommand(global.Args())
	if err != nil {
		return err
	}

	client, err := secondboxclient.NewSecondBoxClient(*rawURL, *token, http.DefaultClient)
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
	return secondboxclient.CallOptions{
		PathParameters:  pathParameters,
		QueryParameters: query,
		Headers:         requestHeaders,
		Body:            body,
		ContentType:     *contentType,
	}, body, nil
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
	keys = append(keys, "diagnostics bundle", "logs follow", "logs tail", "sandbox shell")
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
