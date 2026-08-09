package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const maximumCLIConfigurationBytes = 1 << 20

type cliConfiguration struct {
	URL        string `json:"url"`
	Token      string `json:"token"`
	TenantRef  string `json:"tenantRef"`
	SubjectRef string `json:"subjectRef"`
}

// LoginCLI verifies the generated platform authority, then writes the invoking
// user's ordinary CLI configuration without ever putting the token in process
// arguments, the receipt, or diagnostic output.
func LoginCLI(ctx context.Context, plan InstallPlan, httpClient *http.Client) ([]CreatedResource, error) {
	if httpClient == nil {
		return nil, installerError("CLI login HTTP client is required", nil)
	}
	tokenPath := ""
	for _, target := range plan.SecretTargets {
		if target.Category == "platform-authority" {
			tokenPath = target.Path
			break
		}
	}
	token, err := readInstallerSecret(tokenPath, plan.HostFacts.InvokingUID)
	if err != nil {
		return nil, err
	}
	client, err := secondboxclient.NewSecondBoxSubjectClient("http://"+plan.Network.APIAddress, token, plan.CLI.TenantRef, plan.CLI.SubjectRef, httpClient)
	if err != nil {
		return nil, err
	}
	response, err := client.Request(ctx, "listSandboxes", secondboxclient.CallOptions{QueryParameters: url.Values{"limit": {"1"}}})
	if err != nil {
		return nil, installerError("verify generated CLI authority", err)
	}
	if err := response.Body.Close(); err != nil {
		return nil, installerError("close CLI authority verification response", err)
	}
	created := []CreatedResource{}
	for _, name := range []string{"cli-config-root", "cli-config-directory"} {
		planned, found := plannedPathByName(plan.Paths, name)
		if !found {
			return nil, installerError("CLI configuration directory is absent from plan: "+name, nil)
		}
		wasCreated, err := ensureOwnedDirectory(planned)
		if err != nil {
			return nil, err
		}
		if wasCreated {
			created = append(created, resourceFromPath(planned, StageCLILogin))
		}
	}
	content, err := cliConfigurationBytes(plan, token)
	if err != nil {
		return nil, err
	}
	planned, found := plannedPathByName(plan.Paths, "cli-config")
	if !found || planned.Path != plan.CLI.ConfigPath {
		return nil, installerError("CLI configuration file is absent from plan", nil)
	}
	if err := publishCLIConfiguration(planned, content); err != nil {
		return nil, err
	}
	resource := resourceFromPath(planned, StageCLILogin)
	resource.Digest = Digest(content)
	created = append(created, resource)
	return created, nil
}

// ValidateCLIConfig checks the protected stored authority without contacting
// the control plane or exposing the token through arguments or diagnostics.
func ValidateCLIConfig(plan InstallPlan) error {
	tokenPath := ""
	for _, target := range plan.SecretTargets {
		if target.Category == "platform-authority" {
			tokenPath = target.Path
			break
		}
	}
	token, err := readInstallerSecret(tokenPath, plan.HostFacts.InvokingUID)
	if err != nil {
		return err
	}
	expected, err := cliConfigurationBytes(plan, token)
	if err != nil {
		return err
	}
	planned, found := plannedPathByName(plan.Paths, "cli-config")
	if !found || planned.Path != plan.CLI.ConfigPath {
		return installerError("CLI configuration file is absent from plan", nil)
	}
	info, err := os.Lstat(planned.Path)
	if err != nil {
		return installerError("inspect CLI configuration", err)
	}
	actual, readErr := os.ReadFile(planned.Path)
	stat, ok := info.Sys().(*syscall.Stat_t)
	if readErr != nil || !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != os.FileMode(planned.Mode) || int64(stat.Uid) != planned.OwnerUID || int64(stat.Gid) != planned.OwnerGID || !bytes.Equal(actual, expected) {
		return installerError("existing CLI configuration differs from the verified accepted authority", readErr)
	}
	return nil
}

func cliConfigurationBytes(plan InstallPlan, token string) ([]byte, error) {
	configuration := cliConfiguration{URL: "http://" + plan.Network.APIAddress, Token: token, TenantRef: plan.CLI.TenantRef, SubjectRef: plan.CLI.SubjectRef}
	content, err := json.MarshalIndent(configuration, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

func validateCLIConfigurationTarget(plan InstallPlan) error {
	planned, found := plannedPathByName(plan.Paths, "cli-config")
	if !found || planned.Path != plan.CLI.ConfigPath {
		return installerError("CLI configuration file is absent from plan", nil)
	}
	_, err := os.Lstat(planned.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return installerError("inspect CLI configuration", err)
	}
	return validateReplaceableCLIConfiguration(planned)
}

func validateReplaceableCLIConfiguration(planned PlannedPath) error {
	if err := ValidatePlannedPath(planned); err != nil {
		return err
	}
	info, err := os.Lstat(planned.Path)
	if err != nil || info.Size() <= 0 || info.Size() > maximumCLIConfigurationBytes {
		return installerError("pre-existing CLI configuration size is invalid", err)
	}
	content, err := os.ReadFile(planned.Path)
	if err != nil {
		return installerError("read pre-existing CLI configuration", err)
	}
	var configuration cliConfiguration
	if err := decodeStrict(content, &configuration); err != nil {
		return installerError("pre-existing CLI configuration is not a SecondBox session document", err)
	}
	parsed, err := url.ParseRequestURI(configuration.URL)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return installerError("pre-existing CLI configuration URL is invalid", err)
	}
	for name, value := range map[string]string{"token": configuration.Token, "tenantRef": configuration.TenantRef, "subjectRef": configuration.SubjectRef} {
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
			return installerError("pre-existing CLI configuration "+name+" is invalid", nil)
		}
	}
	return nil
}

func publishCLIConfiguration(planned PlannedPath, content []byte) error {
	_, err := os.Lstat(planned.Path)
	if errors.Is(err, os.ErrNotExist) {
		return writePrivateCreateOnly(planned.Path, content, os.FileMode(planned.Mode))
	}
	if err != nil {
		return installerError("inspect CLI configuration", err)
	}
	if err := validateReplaceableCLIConfiguration(planned); err != nil {
		return err
	}
	actual, err := os.ReadFile(planned.Path)
	if err != nil {
		return installerError("read existing CLI configuration", err)
	}
	if bytes.Equal(actual, content) {
		return nil
	}
	temporary, err := os.CreateTemp(filepath.Dir(planned.Path), ".secondbox-cli-config-")
	if err != nil {
		return installerError("stage CLI configuration", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() { _ = os.Remove(temporaryPath) }
	defer cleanup()
	modeErr := temporary.Chmod(os.FileMode(planned.Mode))
	_, writeErr := temporary.Write(content)
	closeErr := errors.Join(temporary.Sync(), temporary.Close())
	if err := errors.Join(modeErr, writeErr, closeErr); err != nil {
		return installerError("stage CLI configuration", err)
	}
	if err := validateReplaceableCLIConfiguration(planned); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, planned.Path); err != nil {
		return installerError("atomically replace SecondBox CLI configuration", err)
	}
	if err := syncInstallDirectory(filepath.Dir(planned.Path)); err != nil {
		return installerError("sync replaced SecondBox CLI configuration", err)
	}
	return nil
}

func readInstallerSecret(path string, ownerUID int64) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", installerError("platform authority must be a protected regular file", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != ownerUID {
		return "", installerError("platform authority owner differs from accepted invoking user", nil)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(string(content), "\n")
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", installerError("platform authority is empty or malformed", nil)
	}
	return value, nil
}

func ensureOwnedDirectory(planned PlannedPath) (bool, error) {
	_, err := os.Lstat(planned.Path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(planned.Path, os.FileMode(planned.Mode)); err != nil {
			return false, installerError("create "+planned.Name, err)
		}
		if err := os.Chmod(planned.Path, os.FileMode(planned.Mode)); err != nil {
			return false, errors.Join(installerError("secure "+planned.Name, err), os.Remove(planned.Path))
		}
		return true, nil
	}
	return false, validateOwnedDirectoryBoundary(planned)
}

// validateOwnedDirectoryBoundary admits an invoking-user directory that
// predates the installation without changing or claiming it. The plan's mode
// remains the create-time mode for a missing directory; an existing parent may
// be more restrictive, but must remain owner-accessible and non-writable by
// group or other users.
func validateOwnedDirectoryBoundary(planned PlannedPath) error {
	info, err := os.Lstat(planned.Path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return installerError(planned.Name+" must be an existing non-symbolic-link directory", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != planned.OwnerUID || int64(stat.Gid) != planned.OwnerGID {
		return installerError(planned.Name+" ownership differs from the accepted plan", nil)
	}
	mode := info.Mode().Perm()
	if mode&0o700 != 0o700 || mode&0o022 != 0 {
		return installerError(planned.Name+" permissions do not form a safe invoking-user directory boundary", nil)
	}
	return nil
}

func writePrivateCreateOnly(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return installerError("create protected file "+path, err)
	}
	_, writeErr := file.Write(content)
	closeErr := errors.Join(file.Sync(), file.Close())
	if err := errors.Join(writeErr, closeErr); err != nil {
		return errors.Join(installerError("write protected file "+path, err), os.Remove(path))
	}
	return syncInstallDirectory(filepath.Dir(path))
}
