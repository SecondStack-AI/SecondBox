package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func runManagementCommand(ctx context.Context, session cliSession, args []string, output io.Writer, httpClient *http.Client) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "tenant", "tenants":
		if len(args) >= 2 && (args[1] == "controller-authority" || args[1] == "controller-authorities") {
			return true, runTenantControllerAuthorityCommand(ctx, session, args[2:], output, httpClient)
		}
		if len(args) >= 2 && args[1] == "usage" {
			return true, runTenantUsageCommand(ctx, session, args[2:], output, httpClient)
		}
		return true, runTenantCommand(ctx, session, args[1:], output, httpClient)
	case "controller-authority", "controller-authorities":
		return true, runTenantControllerAuthorityCommand(ctx, session, args[1:], output, httpClient)
	case "subject", "subjects":
		return true, runSubjectCommand(ctx, session, args[1:], output, httpClient)
	case "application-authority", "application-authorities":
		return true, runApplicationAuthorityCommand(ctx, session, args[1:], output, httpClient)
	case "usage":
		return true, runTenantUsageCommand(ctx, session, args[1:], output, httpClient)
	case "deployment":
		if len(args) >= 2 && args[1] == "usage" {
			return true, runDeploymentUsageCommand(ctx, session, args[2:], output, httpClient)
		}
	}
	return false, nil
}

func requireManagementSession(session cliSession, required sessionAuthorityKind, httpClient *http.Client) (*secondboxclient.Client, error) {
	if session.url == "" || session.token == "" {
		return nil, errors.New("SecondBox management command requires --url and --token" + sessionSourceHint)
	}
	if session.authority != required {
		return nil, &cliSessionAuthorityError{Required: required, Actual: session.authority}
	}
	return clientForSession(session, httpClient)
}

func runTenantCommand(ctx context.Context, session cliSession, args []string, output io.Writer, httpClient *http.Client) error {
	client, err := requireManagementSession(session, sessionAuthorityPlatform, httpClient)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("SecondBox tenant requires create, get, list, egress-context, suspend, or reactivate")
	}
	var result any
	switch args[0] {
	case "create":
		var request secondboxclient.CreateTenantRequest
		idempotencyKey, parseErr := parseCreateManagementOptions("tenant create", args[1:], &request)
		if parseErr != nil {
			return parseErr
		}
		result, err = client.CreateTenant(ctx, request, idempotencyKey)
	case "get":
		ref, parseErr := requireManagementReference("tenant get", args[1:])
		if parseErr != nil {
			return parseErr
		}
		result, err = client.GetTenant(ctx, ref)
	case "list":
		options, parseErr := parseManagementPageOptions("tenant list", args[1:], false)
		if parseErr != nil {
			return parseErr
		}
		result, err = client.ListTenants(ctx, options.page)
	case "egress-context":
		ref, request, revision, idempotencyKey, parseErr := parseTenantEgressContextOptions(args[1:])
		if parseErr != nil {
			return parseErr
		}
		result, err = client.UpdateTenantEgressContext(ctx, ref, request, revision, idempotencyKey)
	case "suspend", "reactivate":
		ref, revision, idempotencyKey, parseErr := parseRevisionMutationOptions("tenant "+args[0], args[1:])
		if parseErr != nil {
			return parseErr
		}
		if args[0] == "suspend" {
			result, err = client.SuspendTenant(ctx, ref, revision, idempotencyKey)
		} else {
			result, err = client.ReactivateTenant(ctx, ref, revision, idempotencyKey)
		}
	default:
		return fmt.Errorf("SecondBox tenant unknown action %q", args[0])
	}
	if err != nil {
		return err
	}
	return writeManagementResult(ctx, output, result)
}

func parseTenantEgressContextOptions(args []string) (string, secondboxclient.UpdateTenantEgressContextRequest, int64, string, error) {
	ref, remaining, err := shiftManagementReference("tenant egress-context", args)
	if err != nil {
		return "", secondboxclient.UpdateTenantEgressContextRequest{}, 0, "", err
	}
	flags := flag.NewFlagSet("tenant egress-context", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	file := flags.String("file", "", "JSON request document, or - for stdin")
	revision := flags.Int64("revision", 0, "expected positive Tenant revision")
	idempotencyKey := flags.String("idempotency-key", "", "idempotency key")
	if err := flags.Parse(remaining); err != nil {
		return "", secondboxclient.UpdateTenantEgressContextRequest{}, 0, "", fmt.Errorf("SecondBox tenant egress-context options: %w", err)
	}
	if flags.NArg() != 0 || strings.TrimSpace(*file) == "" || *revision < 1 || strings.TrimSpace(*idempotencyKey) == "" {
		return "", secondboxclient.UpdateTenantEgressContextRequest{}, 0, "", errors.New("SecondBox tenant egress-context requires --file, --revision, and --idempotency-key")
	}
	var request secondboxclient.UpdateTenantEgressContextRequest
	if err := decodeManagementRequest(*file, &request); err != nil {
		return "", secondboxclient.UpdateTenantEgressContextRequest{}, 0, "", fmt.Errorf("SecondBox tenant egress-context request: %w", err)
	}
	return ref, request, *revision, strings.TrimSpace(*idempotencyKey), nil
}

func runTenantControllerAuthorityCommand(ctx context.Context, session cliSession, args []string, output io.Writer, httpClient *http.Client) error {
	client, err := requireManagementSession(session, sessionAuthorityPlatform, httpClient)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("SecondBox controller-authority requires create, get, list, rotate, or revoke")
	}
	var result any
	switch args[0] {
	case "create":
		tenantRef, remaining, parseErr := shiftManagementReference("controller-authority create", args[1:])
		if parseErr != nil {
			return parseErr
		}
		var request secondboxclient.CreateTenantControllerAuthorityRequest
		idempotencyKey, parseErr := parseCreateManagementOptions("controller-authority create", remaining, &request)
		if parseErr != nil {
			return parseErr
		}
		result, err = client.CreateTenantControllerAuthority(ctx, tenantRef, request, idempotencyKey)
	case "get":
		tenantRef, authorityID, parseErr := requireTwoManagementReferences("controller-authority get", args[1:])
		if parseErr != nil {
			return parseErr
		}
		result, err = client.GetTenantControllerAuthority(ctx, tenantRef, authorityID)
	case "list":
		tenantRef, remaining, parseErr := shiftManagementReference("controller-authority list", args[1:])
		if parseErr != nil {
			return parseErr
		}
		options, parseErr := parseManagementPageOptions("controller-authority list", remaining, false)
		if parseErr != nil {
			return parseErr
		}
		result, err = client.ListTenantControllerAuthorities(ctx, tenantRef, options.page)
	case "rotate", "revoke":
		tenantRef, authorityID, revision, idempotencyKey, parseErr := parseAuthorityMutationOptions("controller-authority "+args[0], args[1:])
		if parseErr != nil {
			return parseErr
		}
		if args[0] == "rotate" {
			result, err = client.RotateTenantControllerAuthority(ctx, tenantRef, authorityID, revision, idempotencyKey)
		} else {
			result, err = client.RevokeTenantControllerAuthority(ctx, tenantRef, authorityID, revision, idempotencyKey)
		}
	default:
		return fmt.Errorf("SecondBox controller-authority unknown action %q", args[0])
	}
	if err != nil {
		return err
	}
	return writeManagementResult(ctx, output, result)
}

func runSubjectCommand(ctx context.Context, session cliSession, args []string, output io.Writer, httpClient *http.Client) error {
	client, err := requireManagementSession(session, sessionAuthorityTenantController, httpClient)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("SecondBox subject requires create, get, list, close, or cleanup")
	}
	var result any
	switch args[0] {
	case "create":
		var request secondboxclient.CreateSubjectRequest
		idempotencyKey, parseErr := parseCreateManagementOptions("subject create", args[1:], &request)
		if parseErr != nil {
			return parseErr
		}
		result, err = client.CreateSubject(ctx, request, idempotencyKey)
	case "get":
		ref, parseErr := requireManagementReference("subject get", args[1:])
		if parseErr != nil {
			return parseErr
		}
		result, err = client.GetSubject(ctx, ref)
	case "list":
		options, parseErr := parseManagementPageOptions("subject list", args[1:], false)
		if parseErr != nil {
			return parseErr
		}
		result, err = client.ListSubjects(ctx, options.page)
	case "close", "cleanup":
		ref, revision, idempotencyKey, parseErr := parseRevisionMutationOptions("subject "+args[0], args[1:])
		if parseErr != nil {
			return parseErr
		}
		if args[0] == "close" {
			result, err = client.CloseSubject(ctx, ref, revision, idempotencyKey)
		} else {
			result, err = client.CleanupSubject(ctx, ref, revision, idempotencyKey)
		}
	default:
		return fmt.Errorf("SecondBox subject unknown action %q", args[0])
	}
	if err != nil {
		return err
	}
	return writeManagementResult(ctx, output, result)
}

func runApplicationAuthorityCommand(ctx context.Context, session cliSession, args []string, output io.Writer, httpClient *http.Client) error {
	client, err := requireManagementSession(session, sessionAuthorityTenantController, httpClient)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("SecondBox application-authority requires create, get, list, rotate, or revoke")
	}
	var result any
	switch args[0] {
	case "create":
		var request secondboxclient.CreateApplicationAuthorityRequest
		idempotencyKey, parseErr := parseCreateManagementOptions("application-authority create", args[1:], &request)
		if parseErr != nil {
			return parseErr
		}
		result, err = client.CreateApplicationAuthority(ctx, request, idempotencyKey)
	case "get":
		authorityID, parseErr := requireManagementReference("application-authority get", args[1:])
		if parseErr != nil {
			return parseErr
		}
		result, err = client.GetApplicationAuthority(ctx, authorityID)
	case "list":
		options, parseErr := parseManagementPageOptions("application-authority list", args[1:], true)
		if parseErr != nil {
			return parseErr
		}
		result, err = client.ListApplicationAuthorities(ctx, options.subjectRef, options.page)
	case "rotate", "revoke":
		authorityID, revision, idempotencyKey, parseErr := parseRevisionMutationOptions("application-authority "+args[0], args[1:])
		if parseErr != nil {
			return parseErr
		}
		if args[0] == "rotate" {
			result, err = client.RotateApplicationAuthority(ctx, authorityID, revision, idempotencyKey)
		} else {
			result, err = client.RevokeApplicationAuthority(ctx, authorityID, revision, idempotencyKey)
		}
	default:
		return fmt.Errorf("SecondBox application-authority unknown action %q", args[0])
	}
	if err != nil {
		return err
	}
	return writeManagementResult(ctx, output, result)
}

func runTenantUsageCommand(ctx context.Context, session cliSession, args []string, output io.Writer, httpClient *http.Client) error {
	client, err := requireManagementSession(session, sessionAuthorityTenantController, httpClient)
	if err != nil {
		return err
	}
	options, err := parseManagementPageOptions("tenant usage", args, false)
	if err != nil {
		return err
	}
	usage, err := client.GetTenantUsage(ctx, options.page)
	if err != nil {
		return err
	}
	return writeManagementResult(ctx, output, usage)
}

func runDeploymentUsageCommand(ctx context.Context, session cliSession, args []string, output io.Writer, httpClient *http.Client) error {
	client, err := requireManagementSession(session, sessionAuthorityPlatform, httpClient)
	if err != nil {
		return err
	}
	options, err := parseManagementPageOptions("deployment usage", args, false)
	if err != nil {
		return err
	}
	usage, err := client.GetDeploymentUsage(ctx, options.page)
	if err != nil {
		return err
	}
	return writeManagementResult(ctx, output, usage)
}

func parseCreateManagementOptions(command string, args []string, target any) (string, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	file := flags.String("file", "", "JSON request document, or - for stdin")
	idempotencyKey := flags.String("idempotency-key", "", "idempotency key")
	if err := flags.Parse(args); err != nil {
		return "", fmt.Errorf("SecondBox %s options: %w", command, err)
	}
	if flags.NArg() != 0 || strings.TrimSpace(*file) == "" || strings.TrimSpace(*idempotencyKey) == "" {
		return "", fmt.Errorf("SecondBox %s requires --file and --idempotency-key", command)
	}
	if err := decodeManagementRequest(*file, target); err != nil {
		return "", fmt.Errorf("SecondBox %s request: %w", command, err)
	}
	return strings.TrimSpace(*idempotencyKey), nil
}

func decodeManagementRequest(path string, target any) error {
	var content []byte
	if path == "-" {
		var err error
		content, err = io.ReadAll(io.LimitReader(os.Stdin, (4<<20)+1))
		if err != nil {
			return err
		}
	} else {
		var err error
		content, err = os.ReadFile(path)
		if err != nil {
			return err
		}
	}
	if len(content) > 4<<20 {
		return errors.New("request exceeds 4 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request contains trailing JSON content")
		}
		return err
	}
	return nil
}

type managementPageOptions struct {
	page       secondboxclient.PageOptions
	subjectRef secondboxclient.OwnershipRef
}

func parseManagementPageOptions(command string, args []string, subjectFilter bool) (managementPageOptions, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	limit := flags.Int("limit", 100, "maximum page size")
	cursor := flags.String("cursor", "", "opaque continuation cursor")
	subjectRef := flags.String("subject-ref", "", "exact subject filter")
	if err := flags.Parse(args); err != nil {
		return managementPageOptions{}, fmt.Errorf("SecondBox %s options: %w", command, err)
	}
	if flags.NArg() != 0 || *limit < 1 {
		return managementPageOptions{}, fmt.Errorf("SecondBox %s accepts --limit and --cursor only", command)
	}
	if !subjectFilter && *subjectRef != "" {
		return managementPageOptions{}, fmt.Errorf("SecondBox %s does not accept --subject-ref", command)
	}
	return managementPageOptions{page: secondboxclient.PageOptions{Limit: *limit, Cursor: strings.TrimSpace(*cursor)}, subjectRef: strings.TrimSpace(*subjectRef)}, nil
}

func requireManagementReference(command string, args []string) (string, error) {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return "", fmt.Errorf("SecondBox %s requires exactly one reference", command)
	}
	return strings.TrimSpace(args[0]), nil
}

func shiftManagementReference(command string, args []string) (string, []string, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return "", nil, fmt.Errorf("SecondBox %s requires a reference", command)
	}
	return strings.TrimSpace(args[0]), args[1:], nil
}

func requireTwoManagementReferences(command string, args []string) (string, string, error) {
	if len(args) != 2 || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[1]) == "" {
		return "", "", fmt.Errorf("SecondBox %s requires tenant and authority references", command)
	}
	return strings.TrimSpace(args[0]), strings.TrimSpace(args[1]), nil
}

func parseRevisionMutationOptions(command string, args []string) (string, int64, string, error) {
	ref, remaining, err := shiftManagementReference(command, args)
	if err != nil {
		return "", 0, "", err
	}
	revision, idempotencyKey, err := parseMutationFlags(command, remaining)
	return ref, revision, idempotencyKey, err
}

func parseAuthorityMutationOptions(command string, args []string) (string, string, int64, string, error) {
	if len(args) < 2 {
		return "", "", 0, "", fmt.Errorf("SecondBox %s requires tenant and authority references", command)
	}
	tenantRef, authorityID := strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	if tenantRef == "" || authorityID == "" {
		return "", "", 0, "", fmt.Errorf("SecondBox %s requires tenant and authority references", command)
	}
	revision, idempotencyKey, err := parseMutationFlags(command, args[2:])
	return tenantRef, authorityID, revision, idempotencyKey, err
}

func parseMutationFlags(command string, args []string) (int64, string, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	revision := flags.Int64("revision", 0, "expected positive resource revision")
	idempotencyKey := flags.String("idempotency-key", "", "idempotency key")
	if err := flags.Parse(args); err != nil {
		return 0, "", fmt.Errorf("SecondBox %s options: %w", command, err)
	}
	if flags.NArg() != 0 || *revision < 1 || strings.TrimSpace(*idempotencyKey) == "" {
		return 0, "", fmt.Errorf("SecondBox %s requires --revision and --idempotency-key", command)
	}
	return *revision, strings.TrimSpace(*idempotencyKey), nil
}

func writeManagementResult(ctx context.Context, output io.Writer, result any) error {
	view := presentationFromContext(ctx, output)
	if !view.renderer.HumanOutput() {
		return json.NewEncoder(output).Encode(result)
	}
	switch value := result.(type) {
	case secondboxclient.Tenant:
		return writeTenantSummary(view.renderer, value)
	case secondboxclient.TenantPage:
		rows := make([]cliui.Row, 0, len(value.Items))
		for _, tenant := range value.Items {
			rows = append(rows, cliui.Row{"ref": tenant.Ref, "state": tenant.State, "revision": strconv.FormatInt(tenant.Revision, 10)})
		}
		return view.renderer.WriteTable(cliui.Table{Columns: managementResourceColumns("Tenant"), Rows: rows, Empty: "No tenants.", ContinuationCursor: optionalCursor(value.NextCursor)})
	case secondboxclient.TenantControllerCredentialResponse:
		return writeCredentialSummary(view.renderer, "Tenant-controller credential", value.Authority.ID, value.Authority.State, value.Authority.Revision, value.BearerToken)
	case secondboxclient.TenantControllerAuthority:
		return writeAuthoritySummary(view.renderer, "Tenant-controller authority", value.ID, value.State, value.Revision)
	case secondboxclient.TenantControllerAuthorityPage:
		rows := make([]cliui.Row, 0, len(value.Items))
		for _, authority := range value.Items {
			rows = append(rows, cliui.Row{"ref": authority.ID, "state": authority.State, "revision": strconv.FormatInt(authority.Revision, 10)})
		}
		return view.renderer.WriteTable(cliui.Table{Columns: managementResourceColumns("Authority"), Rows: rows, Empty: "No tenant-controller authorities.", ContinuationCursor: optionalCursor(value.NextCursor)})
	case secondboxclient.Subject:
		return view.renderer.WriteSummary(cliui.Summary{Title: "Subject", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Reference", Value: value.Ref}, {Key: "Tenant", Value: value.TenantRef}, {Key: "State", Value: value.State}, {Key: "Cleanup", Value: value.CleanupState}, {Key: "Revision", Value: strconv.FormatInt(value.Revision, 10)}}})
	case secondboxclient.SubjectPage:
		rows := make([]cliui.Row, 0, len(value.Items))
		for _, subject := range value.Items {
			rows = append(rows, cliui.Row{"ref": subject.Ref, "state": subject.State, "revision": strconv.FormatInt(subject.Revision, 10)})
		}
		return view.renderer.WriteTable(cliui.Table{Columns: managementResourceColumns("Subject"), Rows: rows, Empty: "No subjects.", ContinuationCursor: optionalCursor(value.NextCursor)})
	case secondboxclient.ApplicationCredentialResponse:
		return writeCredentialSummary(view.renderer, "Application credential", value.Authority.ID, value.Authority.State, value.Authority.Revision, value.BearerToken)
	case secondboxclient.ApplicationAuthority:
		return writeAuthoritySummary(view.renderer, "Application authority", value.ID, value.State, value.Revision)
	case secondboxclient.ApplicationAuthorityPage:
		rows := make([]cliui.Row, 0, len(value.Items))
		for _, authority := range value.Items {
			rows = append(rows, cliui.Row{"ref": authority.ID, "state": authority.State, "revision": strconv.FormatInt(authority.Revision, 10)})
		}
		return view.renderer.WriteTable(cliui.Table{Columns: managementResourceColumns("Authority"), Rows: rows, Empty: "No application authorities.", ContinuationCursor: optionalCursor(value.NextCursor)})
	case secondboxclient.Operation:
		return view.renderer.WriteSummary(cliui.Summary{Title: "Subject cleanup operation", Status: cliui.StatusActive, Pairs: []cliui.Pair{{Key: "Operation", Value: value.ID}, {Key: "State", Value: string(value.State)}}})
	case secondboxclient.TenantUsage:
		return view.renderer.WriteSummary(cliui.Summary{Title: "Tenant usage", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Tenant", Value: value.TenantRef}, {Key: "Sandboxes", Value: strconv.FormatInt(value.Usage.Sandboxes, 10) + " / " + strconv.FormatInt(value.Limits.MaxSandboxes, 10)}, {Key: "Active subjects", Value: strconv.FormatInt(value.Usage.ActiveSubjects, 10) + " / " + strconv.FormatInt(value.Limits.MaxActiveSubjects, 10)}, {Key: "Application authorities", Value: strconv.FormatInt(value.Usage.ApplicationAuthorities, 10) + " / " + strconv.FormatInt(value.Limits.MaxApplicationAuthorities, 10)}, {Key: "Observed", Value: value.ObservedAt.Format("2006-01-02T15:04:05Z07:00")}}})
	case secondboxclient.DeploymentUsage:
		return view.renderer.WriteSummary(cliui.Summary{Title: "Deployment usage", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Tenants in page", Value: strconv.Itoa(len(value.Tenants))}, {Key: "Sandboxes", Value: strconv.FormatInt(value.Usage.Sandboxes, 10)}, {Key: "Observed", Value: value.ObservedAt.Format("2006-01-02T15:04:05Z07:00")}}, Next: managementContinuation(value.NextCursor)})
	default:
		return fmt.Errorf("SecondBox management result type %T has no stable human view", result)
	}
}

func writeTenantSummary(renderer cliui.Renderer, tenant secondboxclient.Tenant) error {
	egressContext := "none"
	if tenant.EgressContext != nil {
		egressContext = *tenant.EgressContext
	}
	return renderer.WriteSummary(cliui.Summary{Title: "Tenant", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Reference", Value: tenant.Ref}, {Key: "State", Value: tenant.State}, {Key: "Egress context", Value: egressContext}, {Key: "Revision", Value: strconv.FormatInt(tenant.Revision, 10)}, {Key: "Profile grants", Value: strings.Join(tenant.AllowedProfileGrants, ", ")}, {Key: "Application scopes", Value: strings.Join(tenant.AllowedApplicationScopes, ", ")}}})
}

func writeAuthoritySummary(renderer cliui.Renderer, title, id, state string, revision int64) error {
	return renderer.WriteSummary(cliui.Summary{Title: title, Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Authority", Value: id}, {Key: "State", Value: state}, {Key: "Revision", Value: strconv.FormatInt(revision, 10)}}})
}

func writeCredentialSummary(renderer cliui.Renderer, title, id, state string, revision int64, bearerToken string) error {
	return renderer.WriteSummary(cliui.Summary{Title: title, Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Authority", Value: id}, {Key: "State", Value: state}, {Key: "Revision", Value: strconv.FormatInt(revision, 10)}, {Key: "Bearer token (shown once)", Value: bearerToken}}, Warnings: []string{"Store this bearer token now. SecondBox cannot recover it later."}})
}

func managementResourceColumns(referenceTitle string) []cliui.Column {
	return []cliui.Column{{Key: "ref", Title: referenceTitle, Priority: 0, MinWidth: 12}, {Key: "state", Title: "State", Priority: 1, MinWidth: 8}, {Key: "revision", Title: "Revision", Priority: 2, MinWidth: 8}}
}

func optionalCursor(cursor *string) string {
	if cursor == nil {
		return ""
	}
	return *cursor
}

func managementContinuation(cursor *string) string {
	if cursor == nil || *cursor == "" {
		return ""
	}
	return "Continue with --cursor " + *cursor
}
