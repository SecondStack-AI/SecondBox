package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const (
	sessionURLEnvironment        = "SECONDBOX_URL"
	sessionTokenEnvironment      = "SECONDBOX_TOKEN"
	sessionTenantRefEnvironment  = "SECONDBOX_TENANT_REF"
	sessionSubjectRefEnvironment = "SECONDBOX_SUBJECT_REF"
	sessionPathEnvironment       = "SECONDBOX_CONFIG"
)

// sessionSourceHint names every supported way to supply one CLI credential.
const sessionSourceHint = "; supply it as a flag, in the environment, or with secondbox login"

// sessionOrigin records which source supplied one resolved value.
type sessionOrigin string

const (
	sessionOriginUnset         sessionOrigin = "unset"
	sessionOriginFlag          sessionOrigin = "flag"
	sessionOriginEnvironment   sessionOrigin = "environment"
	sessionOriginConfiguration sessionOrigin = "configuration"
)

// cliSession is one resolved CLI authority and the provenance of each value.
type cliSession struct {
	url        string
	token      string
	tenantRef  string
	subjectRef string
	origins    sessionOrigins
	path       string
}

type sessionOrigins struct {
	url        sessionOrigin
	token      sessionOrigin
	tenantRef  sessionOrigin
	subjectRef sessionOrigin
}

// sessionFile is the stored configuration document written by secondbox login.
type sessionFile struct {
	URL        string `json:"url"`
	Token      string `json:"token"`
	TenantRef  string `json:"tenantRef"`
	SubjectRef string `json:"subjectRef"`
}

// resolveSession merges explicit flags, the environment, and stored configuration.
// A missing value is not an error here; each command reports the values it requires.
func resolveSession(flagValues cliSession) (cliSession, error) {
	path, err := resolveSessionPath()
	if err != nil {
		return cliSession{}, err
	}
	stored, err := readSessionFile(path)
	if err != nil {
		return cliSession{}, err
	}
	session := cliSession{path: path}
	session.url, session.origins.url = selectSessionValue(
		flagValues.url, sessionURLEnvironment, stored.URL,
	)
	session.token, session.origins.token = selectSessionValue(
		flagValues.token, sessionTokenEnvironment, stored.Token,
	)
	session.tenantRef, session.origins.tenantRef = selectSessionValue(
		flagValues.tenantRef, sessionTenantRefEnvironment, stored.TenantRef,
	)
	session.subjectRef, session.origins.subjectRef = selectSessionValue(
		flagValues.subjectRef, sessionSubjectRefEnvironment, stored.SubjectRef,
	)
	return session, nil
}

func selectSessionValue(
	flagValue string,
	environmentName string,
	storedValue string,
) (string, sessionOrigin) {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value, sessionOriginFlag
	}
	if value := strings.TrimSpace(os.Getenv(environmentName)); value != "" {
		return value, sessionOriginEnvironment
	}
	if value := strings.TrimSpace(storedValue); value != "" {
		return value, sessionOriginConfiguration
	}
	return "", sessionOriginUnset
}

func resolveSessionPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv(sessionPathEnvironment)); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf(
				"SecondBox CLI %s must be an absolute path", sessionPathEnvironment,
			)
		}
		return override, nil
	}
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("SecondBox CLI user configuration directory: %w", err)
	}
	return filepath.Join(directory, "secondbox", "config.json"), nil
}

// readSessionFile returns the stored document, or a zero document when absent.
func readSessionFile(path string) (sessionFile, error) {
	pathInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return sessionFile{}, nil
	}
	if err != nil {
		return sessionFile{}, fmt.Errorf("SecondBox CLI configuration stat failed: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return sessionFile{}, fmt.Errorf(
			"SecondBox CLI configuration %s must be a non-symbolic-link regular file", path,
		)
	}
	if pathInfo.Mode().Perm()&0o077 != 0 {
		return sessionFile{}, fmt.Errorf(
			"SecondBox CLI configuration %s must not be readable by group or other", path,
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return sessionFile{}, fmt.Errorf("SecondBox CLI configuration open failed: %w", err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		return sessionFile{}, errors.Join(
			fmt.Errorf("SecondBox CLI configuration open-file stat failed: %w", err),
			file.Close(),
		)
	}
	if !fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) {
		return sessionFile{}, errors.Join(
			fmt.Errorf("SecondBox CLI configuration %s changed during secure open", path),
			file.Close(),
		)
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var stored sessionFile
	decodeErr := decoder.Decode(&stored)
	if decodeErr == nil && decoder.More() {
		decodeErr = errors.New("configuration contains trailing content")
	}
	closeErr := file.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return sessionFile{}, fmt.Errorf("SecondBox CLI read configuration %s: %w", path, err)
	}
	return stored, nil
}

// writeSessionFile replaces the stored document atomically at mode 0600.
func writeSessionFile(path string, stored sessionFile) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("SecondBox CLI create configuration directory: %w", err)
	}
	pathInfo, err := os.Lstat(path)
	switch {
	case err == nil:
		if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
			return fmt.Errorf(
				"SecondBox CLI configuration %s must be a non-symbolic-link regular file", path,
			)
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("SecondBox CLI configuration stat failed: %w", err)
	}
	content, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("SecondBox CLI encode configuration: %w", err)
	}
	content = append(content, '\n')
	temporary, err := os.CreateTemp(directory, "config-*.json")
	if err != nil {
		return fmt.Errorf("SecondBox CLI create temporary configuration: %w", err)
	}
	temporaryPath := temporary.Name()
	_, writeErr := temporary.Write(content)
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return errors.Join(
			fmt.Errorf("SecondBox CLI write temporary configuration: %w", err),
			os.Remove(temporaryPath),
		)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return errors.Join(
			fmt.Errorf("SecondBox CLI replace configuration %s: %w", path, err),
			os.Remove(temporaryPath),
		)
	}
	return nil
}

func runLoginCommand(
	ctx context.Context,
	session cliSession,
	args []string,
	output io.Writer,
	httpClient *http.Client,
) error {
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	rawURL := flags.String("url", session.url, "absolute SecondBox API endpoint")
	token := flags.String("token", session.token, "SecondBox API token")
	tenantRef := flags.String("tenant-ref", session.tenantRef, "trusted caller tenant reference")
	subjectRef := flags.String("subject-ref", session.subjectRef, "trusted caller subject reference")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("SecondBox CLI parse login options: %w", err)
	}
	if len(flags.Args()) != 0 {
		return fmt.Errorf(
			"SecondBox CLI unexpected login arguments: %s", strings.Join(flags.Args(), " "),
		)
	}
	stored := sessionFile{
		URL:        strings.TrimSpace(*rawURL),
		Token:      strings.TrimSpace(*token),
		TenantRef:  strings.TrimSpace(*tenantRef),
		SubjectRef: strings.TrimSpace(*subjectRef),
	}
	if stored.URL == "" || stored.Token == "" || stored.TenantRef == "" || stored.SubjectRef == "" {
		view := presentationFromContext(ctx, output)
		if view.renderer.OutputMode != cliui.OutputJSON && view.renderer.Capabilities.Input.TTY && view.renderer.Capabilities.Output.TTY && view.input != nil {
			fields := make([]cliui.FieldSpec, 0, 4)
			if stored.URL == "" {
				fields = append(fields, cliui.FieldSpec{Kind: cliui.FieldText, Title: "API endpoint", Description: "Absolute URL of the SecondBox control plane", StringValue: &stored.URL, ValidateString: requiredLoginValue("API endpoint")})
			}
			if stored.Token == "" {
				fields = append(fields, cliui.FieldSpec{Kind: cliui.FieldSecret, Title: "Platform token", Description: "Input is masked and is never printed", StringValue: &stored.Token, ValidateString: requiredLoginValue("platform token")})
			}
			if stored.TenantRef == "" {
				fields = append(fields, cliui.FieldSpec{Kind: cliui.FieldText, Title: "Tenant reference", StringValue: &stored.TenantRef, ValidateString: requiredLoginValue("tenant reference")})
			}
			if stored.SubjectRef == "" {
				fields = append(fields, cliui.FieldSpec{Kind: cliui.FieldText, Title: "Subject reference", StringValue: &stored.SubjectRef, ValidateString: requiredLoginValue("subject reference")})
			}
			form := cliui.HuhForm{Groups: []cliui.GroupSpec{{Title: "Store credentials in " + session.path, Fields: fields}}}
			if err := form.Run(ctx, cliui.FormHandles{Input: view.input, Output: output, Width: view.renderer.Capabilities.Output.Width, Accessible: view.accessible, Dark: view.renderer.Capabilities.Output.Background == cliui.BackgroundDark}); err != nil {
				return err
			}
			stored.URL = strings.TrimSpace(stored.URL)
			stored.Token = strings.TrimSpace(stored.Token)
			stored.TenantRef = strings.TrimSpace(stored.TenantRef)
			stored.SubjectRef = strings.TrimSpace(stored.SubjectRef)
		}
	}
	if stored.URL == "" || stored.Token == "" ||
		stored.TenantRef == "" || stored.SubjectRef == "" {
		return errors.New(
			"SecondBox CLI login requires --url, --token, --tenant-ref, and --subject-ref",
		)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client, err := secondboxclient.NewSecondBoxSubjectClient(
		stored.URL, stored.Token, stored.TenantRef, stored.SubjectRef, httpClient,
	)
	if err != nil {
		return err
	}
	if err := verifySessionCredentials(ctx, client); err != nil {
		return err
	}
	if err := writeSessionFile(session.path, stored); err != nil {
		return err
	}
	view := presentationFromContext(ctx, output)
	if view.renderer.HumanOutput() || view.renderer.OutputMode == cliui.OutputJSON {
		return nil
	}
	_, printErr := fmt.Fprintf(
		output, "SecondBox verified %s and stored credentials in %s\n", stored.URL, session.path,
	)
	return printErr
}

func runLoginCommandPresented(ctx context.Context, session cliSession, args []string, output io.Writer, httpClient *http.Client) error {
	if err := runLoginCommand(ctx, session, args, output, httpClient); err != nil {
		return err
	}
	stored, err := readSessionFile(session.path)
	if err != nil {
		return err
	}
	view := presentationFromContext(ctx, output)
	if view.renderer.OutputMode == cliui.OutputJSON {
		return json.NewEncoder(output).Encode(map[string]string{"configuration": session.path, "status": "verified", "url": stored.URL})
	}
	if view.renderer.HumanOutput() {
		return view.renderer.WriteSummary(cliui.Summary{Title: "Login complete", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Endpoint", Value: stored.URL}, {Key: "Credentials", Value: session.path}}})
	}
	return nil
}

func requiredLoginValue(name string) func(string) error {
	return func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
}

// verifySessionCredentials proves the authority before it is written to disk.
func verifySessionCredentials(ctx context.Context, client *secondboxclient.Client) error {
	response, err := client.Request(ctx, "listSandboxes", secondboxclient.CallOptions{
		QueryParameters: url.Values{"limit": []string{"1"}},
	})
	if err != nil {
		return fmt.Errorf("SecondBox CLI verify credentials: %w", err)
	}
	_, copyErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("SecondBox CLI verify credentials response: %w", err)
	}
	return nil
}

func runLogoutCommand(session cliSession, args []string, output io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf(
			"SecondBox CLI unexpected logout arguments: %s", strings.Join(args, " "),
		)
	}
	err := os.Remove(session.path)
	if errors.Is(err, os.ErrNotExist) {
		_, printErr := fmt.Fprintf(
			output, "SecondBox has no stored credentials at %s\n", session.path,
		)
		return printErr
	}
	if err != nil {
		return fmt.Errorf("SecondBox CLI remove configuration %s: %w", session.path, err)
	}
	_, printErr := fmt.Fprintf(
		output, "SecondBox removed stored credentials from %s\n", session.path,
	)
	return printErr
}

func runLogoutCommandPresented(ctx context.Context, session cliSession, args []string, output io.Writer) error {
	var receipt strings.Builder
	if err := runLogoutCommand(session, args, &receipt); err != nil {
		return err
	}
	view := presentationFromContext(ctx, output)
	status := "removed"
	if strings.Contains(receipt.String(), "no stored credentials") {
		status = "absent"
	}
	if view.renderer.OutputMode == cliui.OutputJSON {
		return json.NewEncoder(output).Encode(map[string]string{"configuration": session.path, "status": status})
	}
	if view.renderer.HumanOutput() {
		return view.renderer.WriteSummary(cliui.Summary{Title: "Logout complete", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Credentials", Value: session.path}, {Key: "Previous state", Value: status}}})
	}
	_, err := io.WriteString(output, receipt.String())
	return err
}

// runWhoamiCommand reports the resolved authority without disclosing the token.
func runWhoamiCommand(session cliSession, args []string, output io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf(
			"SecondBox CLI unexpected whoami arguments: %s", strings.Join(args, " "),
		)
	}
	token := "absent"
	if session.token != "" {
		token = "present"
	}
	_, err := fmt.Fprintf(
		output,
		"configuration  %s\nurl            %s (%s)\ntenant-ref     %s (%s)\nsubject-ref    %s (%s)\ntoken          %s (%s)\n",
		session.path,
		displaySessionValue(session.url), session.origins.url,
		displaySessionValue(session.tenantRef), session.origins.tenantRef,
		displaySessionValue(session.subjectRef), session.origins.subjectRef,
		token, session.origins.token,
	)
	return err
}

func runWhoamiCommandPresented(ctx context.Context, session cliSession, args []string, output io.Writer) error {
	if len(args) != 0 {
		return runWhoamiCommand(session, args, output)
	}
	view := presentationFromContext(ctx, output)
	token := "absent"
	if session.token != "" {
		token = "present"
	}
	if view.renderer.OutputMode == cliui.OutputJSON {
		return json.NewEncoder(output).Encode(map[string]any{"configuration": session.path, "url": map[string]string{"value": displaySessionValue(session.url), "source": string(session.origins.url)}, "tenantRef": map[string]string{"value": displaySessionValue(session.tenantRef), "source": string(session.origins.tenantRef)}, "subjectRef": map[string]string{"value": displaySessionValue(session.subjectRef), "source": string(session.origins.subjectRef)}, "token": map[string]string{"state": token, "source": string(session.origins.token)}})
	}
	if view.renderer.HumanOutput() {
		return view.renderer.WriteSummary(cliui.Summary{Title: "Current authority", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "Configuration", Value: session.path}, {Key: "URL", Value: displaySessionValue(session.url) + " (" + string(session.origins.url) + ")"}, {Key: "Tenant", Value: displaySessionValue(session.tenantRef) + " (" + string(session.origins.tenantRef) + ")"}, {Key: "Subject", Value: displaySessionValue(session.subjectRef) + " (" + string(session.origins.subjectRef) + ")"}, {Key: "Token", Value: token + " (" + string(session.origins.token) + ")"}}})
	}
	return runWhoamiCommand(session, args, output)
}

func displaySessionValue(value string) string {
	if value == "" {
		return "unset"
	}
	return value
}
