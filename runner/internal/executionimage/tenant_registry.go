package executionimage

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// TenantRegistry grants exact repositories and one authentication configuration.
type TenantRegistry struct {
	Registry       string                 `json:"registry"`
	Repositories   []string               `json:"repositories"`
	Authentication RegistryAuthentication `json:"authentication"`
}

type RegistryAuthentication struct {
	Mode             string `json:"mode"`
	Username         string `json:"username,omitempty"`
	TokenFile        string `json:"tokenFile,omitempty"`
	DockerConfigFile string `json:"dockerConfigFile,omitempty"`
}

type registryAuthEntry struct {
	Auth          string `json:"auth,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
}

// AuthenticationDocument returns only this Tenant's configured registry entry.
// An explicit empty auths map prevents anonymous requests from using host logins.
func (registry TenantRegistry) AuthenticationDocument(reference string) ([]byte, error) {
	if !imageReferencePattern.MatchString(reference) {
		return nil, errors.New("SecondBox tenant image reference is invalid")
	}
	host, repository, _ := strings.Cut(reference, "/")
	repository, _, _ = strings.Cut(repository, "@")
	repository, _, _ = strings.Cut(repository, ":")
	if host != registry.Registry || !slices.Contains(registry.Repositories, repository) {
		return nil, errors.New("SecondBox tenant image repository is not granted")
	}
	entries := map[string]registryAuthEntry{}
	auth := registry.Authentication
	switch auth.Mode {
	case "anonymous":
		if auth.Username != "" || auth.TokenFile != "" || auth.DockerConfigFile != "" {
			return nil, errors.New("SecondBox anonymous registry configuration contains credentials")
		}
	case "token":
		if auth.Username == "" || strings.ContainsAny(auth.Username, ":\r\n") || auth.DockerConfigFile != "" {
			return nil, errors.New("SecondBox token registry configuration requires a username and token file")
		}
		token, err := readRegistrySecret(auth.TokenFile)
		if err != nil {
			return nil, err
		}
		token = bytes.TrimSuffix(token, []byte("\n"))
		if len(token) == 0 || bytes.ContainsAny(token, "\r\n") {
			return nil, errors.New("SecondBox registry token must contain one nonempty line")
		}
		entries[host] = registryAuthEntry{Auth: base64.StdEncoding.EncodeToString(append([]byte(auth.Username+":"), token...))}
	case "docker_config":
		if auth.Username != "" || auth.TokenFile != "" {
			return nil, errors.New("SecondBox Docker registry configuration must use only a config file")
		}
		content, err := readRegistrySecret(auth.DockerConfigFile)
		if err != nil {
			return nil, err
		}
		var config struct {
			Auths       map[string]registryAuthEntry `json:"auths"`
			CredsStore  string                       `json:"credsStore"`
			CredHelpers map[string]string            `json:"credHelpers"`
		}
		if err := json.Unmarshal(content, &config); err != nil {
			return nil, errors.New("SecondBox Docker registry configuration is invalid JSON")
		}
		if config.CredsStore != "" || len(config.CredHelpers) != 0 {
			return nil, errors.New("SecondBox Docker registry configuration must contain exported credentials, not external helpers")
		}
		entry, ok := config.Auths[host]
		if !ok || (entry.Auth == "") == (entry.IdentityToken == "") {
			return nil, errors.New("SecondBox Docker registry configuration requires one credential for the configured registry host")
		}
		if entry.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			username, password, found := bytes.Cut(decoded, []byte(":"))
			if err != nil || !found || len(username) == 0 || len(password) == 0 {
				return nil, errors.New("SecondBox Docker registry authentication is invalid")
			}
		}
		entries[host] = entry
	default:
		return nil, errors.New("SecondBox registry authentication mode must be anonymous, token, or docker_config")
	}
	return json.Marshal(struct {
		Auths map[string]registryAuthEntry `json:"auths"`
	}{entries})
}

func readRegistrySecret(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("SecondBox registry secret path must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("SecondBox registry secret open failed: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("SecondBox registry secret inspection failed: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 || info.Mode().Perm()&0o007 != 0 {
		return nil, errors.New("SecondBox registry secret must be a private regular file of at most 1 MiB")
	}
	content := make([]byte, info.Size())
	if _, err := file.ReadAt(content, 0); err != nil && len(content) != 0 {
		return nil, fmt.Errorf("SecondBox registry secret read failed: %w", err)
	}
	return content, nil
}
