package deployconfig

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/assetcatalog"
	controlconfig "github.com/SecondStack-AI/SecondBox/internal/config"
	"github.com/SecondStack-AI/SecondBox/internal/runnerfeatures"
	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
	"github.com/pelletier/go-toml/v2"
)

// DefaultComposeProjectName is the Compose project a manifest that states no
// deployment.compose_project_name deploys under.
const DefaultComposeProjectName = "secondbox"

var (
	artifactKeyPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	opaqueRunnerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	composeProjectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	configValidationMu    sync.Mutex
)

func ReadManifest(path string) (ManifestV1, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ManifestV1{}, manifestError("resolve path", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return ManifestV1{}, manifestError("open", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ManifestV1{}, manifestError("path must be a regular non-symbolic-link file", nil)
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return ManifestV1{}, manifestError("read", err)
	}
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest ManifestV1
	if err := decoder.Decode(&manifest); err != nil {
		return ManifestV1{}, manifestError("strict decode", err)
	}
	if manifest.SchemaVersion != 1 {
		return ManifestV1{}, manifestError(fmt.Sprintf("unsupported schema_version %d", manifest.SchemaVersion), nil)
	}
	return manifest, nil
}

// Resolve validates and resolves a manifest without consulting ambient process
// environment. Relative source references are anchored to the manifest.
func Resolve(path string) (ResolvedDeployment, error) {
	manifest, err := ReadManifest(path)
	if err != nil {
		return ResolvedDeployment{}, err
	}
	absolute, _ := filepath.Abs(path)
	base := filepath.Dir(absolute)
	return resolveManifest(manifest, base)
}

func resolveManifest(manifest ManifestV1, base string) (ResolvedDeployment, error) {
	return resolveManifestWithOptions(manifest, base, true)
}

func resolveManifestWithOptions(manifest ManifestV1, base string, validateSameHost bool) (ResolvedDeployment, error) {
	if err := validateManifestShape(manifest); err != nil {
		return ResolvedDeployment{}, err
	}
	environment := make(map[string]string)
	secretPaths := make(map[string]string)
	put := func(name, value string) { environment[name] = value }
	putInt := func(name string, value *int64) { environment[name] = strconv.FormatInt(*value, 10) }
	putBool := func(name string, value *bool) { environment[name] = strconv.FormatBool(*value) }

	deployment := manifest.Deployment
	put("SECONDBOX_DEPLOYMENT_MODE", deployment.Mode)
	put("SECONDBOX_PUBLIC_BASE_URL", deployment.PublicBaseURL)
	put("SECONDBOX_TLS_TERMINATION", deployment.TLSTermination)
	put("SECONDBOX_CONTROL_PLANE_IMAGE", deployment.ControlPlaneImage)
	put("SECONDBOX_RUNNER_IMAGE", deployment.RunnerImage)
	put("SECONDBOX_API_BIND_IP", deployment.APIBindIP)
	putInt("SECONDBOX_API_PUBLISHED_PORT", deployment.APIPublishedPort)
	put("SECONDBOX_LISTEN_ADDR", deployment.ListenAddress)
	put("SECONDBOX_RUNNER_BIND_IP", deployment.RunnerBindIP)
	putInt("SECONDBOX_RUNNER_PUBLISHED_PORT", deployment.RunnerPublishedPort)
	put("SECONDBOX_RUNNER_LISTEN_ADDR", deployment.RunnerListenAddress)
	put("SECONDBOX_LOG_PATH", deployment.LogPath)
	if deployment.DevelopmentWaitSeconds != nil {
		putInt("SECONDBOX_DEVELOPMENT_PREPARE_WAIT_TIMEOUT_SECONDS", deployment.DevelopmentWaitSeconds)
	}
	catalog, err := resolveRegularReference(base, deployment.SignedAssetCatalog)
	if err != nil {
		return ResolvedDeployment{}, manifestError("deployment.signed_asset_catalog", err)
	}
	verifiedCatalog, err := assetcatalog.LoadFileAssetCatalog(catalog)
	if err != nil {
		return ResolvedDeployment{}, manifestError("deployment.signed_asset_catalog", err)
	}
	if deployment.Mode == "production" && verifiedCatalog.UsesSignatureKeyID("secondbox-development-local-trust") {
		return ResolvedDeployment{}, manifestError("production requires an operator-supplied signed asset catalog", nil)
	}
	put("SECONDBOX_SIGNED_ASSET_CATALOG_HOST_PATH", catalog)
	put("SECONDBOX_SIGNED_ASSET_CATALOG_PATH", deployment.SignedAssetCatalogPath)

	database := manifest.Database
	databasePassword := ""
	if database.Mode == "bundled" {
		put("SECONDBOX_POSTGRES_IMAGE", deployment.PostgresImage)
		put("SECONDBOX_POSTGRES_BIND_IP", database.BindIP)
		putInt("SECONDBOX_POSTGRES_PUBLISHED_PORT", database.PublishedPort)
		put("SECONDBOX_POSTGRES_DATABASE", database.Name)
		put("SECONDBOX_POSTGRES_USER", database.User)
		password, path, err := readSecretReference(base, database.PasswordFile)
		if err != nil {
			return ResolvedDeployment{}, manifestError("database.password_file", err)
		}
		if len(password) < 24 {
			return ResolvedDeployment{}, manifestError("database.password_file must contain at least 24 bytes", nil)
		}
		secretPaths["database.password_file"] = path
		databasePassword = password
		put("SECONDBOX_POSTGRES_PASSWORD", password)
		databaseURL := url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(database.User, password),
			Host:     "postgres:5432",
			Path:     "/" + database.Name,
			RawQuery: "sslmode=disable",
		}
		put("SECONDBOX_DATABASE_URL", databaseURL.String())
	} else {
		databaseURL, path, err := readSecretReference(base, database.URLFile)
		if err != nil {
			return ResolvedDeployment{}, manifestError("database.url_file", err)
		}
		secretPaths["database.url_file"] = path
		if err := validateExternalDatabaseURL(databaseURL, deployment.Mode == "production"); err != nil {
			return ResolvedDeployment{}, manifestError("database.url_file", err)
		}
		put("SECONDBOX_DATABASE_URL", databaseURL)
	}

	objectStore := manifest.ObjectStore
	put("SECONDBOX_OBJECT_STORE_ENDPOINT", objectStore.Endpoint)
	put("SECONDBOX_OBJECT_STORE_BUCKET", objectStore.Bucket)
	put("SECONDBOX_OBJECT_STORE_REGION", objectStore.Region)
	putBool("SECONDBOX_OBJECT_STORE_USE_PATH_STYLE", objectStore.UsePathStyle)
	put("SECONDBOX_OBJECT_STORE_TEMP_DIRECTORY", objectStore.TempDirectory)
	access, accessPath, err := readSecretReference(base, objectStore.AccessKeyFile)
	if err != nil {
		return ResolvedDeployment{}, manifestError("object_store.access_key_file", err)
	}
	secretPaths["object_store.access_key_file"] = accessPath
	secret, secretPath, err := readSecretReference(base, objectStore.SecretKeyFile)
	if err != nil {
		return ResolvedDeployment{}, manifestError("object_store.secret_key_file", err)
	}
	secretPaths["object_store.secret_key_file"] = secretPath
	put("SECONDBOX_OBJECT_STORE_ROOT_USER", access)
	put("SECONDBOX_OBJECT_STORE_ROOT_PASSWORD", secret)
	if objectStore.Mode == "bundled" {
		clientEndpoint, err := url.Parse(objectStore.Endpoint)
		if err != nil {
			return ResolvedDeployment{}, manifestError("object_store.endpoint", err)
		}
		clientEndpoint.User = url.UserPassword(access, secret)
		put("SECONDBOX_OBJECT_STORE_MC_HOST", clientEndpoint.String())
		put("SECONDBOX_OBJECT_STORE_IMAGE", deployment.ObjectStoreImage)
		put("SECONDBOX_OBJECT_STORE_CLIENT_IMAGE", deployment.ObjectStoreClientImage)
		put("SECONDBOX_OBJECT_STORE_BIND_IP", objectStore.BindIP)
		putInt("SECONDBOX_OBJECT_STORE_PUBLISHED_PORT", objectStore.PublishedPort)
		putInt("SECONDBOX_OBJECT_STORE_CONSOLE_PUBLISHED_PORT", objectStore.ConsolePublishedPort)
	}

	trust := manifest.RunnerTrust
	credential, credentialPath, err := readSecretReference(base, trust.EnrollmentCredentialFile)
	if err != nil {
		return ResolvedDeployment{}, manifestError("runner_trust.enrollment_credential_file", err)
	}
	secretPaths["runner_trust.enrollment_credential_file"] = credentialPath
	caCert, err := resolveRegularReference(base, trust.CACertificateFile)
	if err != nil {
		return ResolvedDeployment{}, manifestError("runner_trust.ca_certificate_file", err)
	}
	caKey, err := resolvePrivateReference(base, trust.CAPrivateKeyFile)
	if err != nil {
		return ResolvedDeployment{}, manifestError("runner_trust.ca_private_key_file", err)
	}
	serverCert, err := resolveRegularReference(base, trust.ServerCertificateFile)
	if err != nil {
		return ResolvedDeployment{}, manifestError("runner_trust.server_certificate_file", err)
	}
	serverKey, err := resolvePrivateReference(base, trust.ServerPrivateKeyFile)
	if err != nil {
		return ResolvedDeployment{}, manifestError("runner_trust.server_private_key_file", err)
	}
	secretPaths["runner_trust.ca_private_key_file"] = caKey
	secretPaths["runner_trust.server_private_key_file"] = serverKey
	if err := validateRunnerTrustMaterial(caCert, caKey, serverCert, serverKey, trust.ServerName); err != nil {
		return ResolvedDeployment{}, manifestError("runner_trust cryptographic material", err)
	}
	if validateSameHost {
		for index, runner := range manifest.Runners {
			if runner.Placement == "same-host" {
				if err := validateSameHostRunnerHost(runner, caCert); err != nil {
					return ResolvedDeployment{}, manifestError(fmt.Sprintf("runners[%d] same-host preflight", index), err)
				}
			}
		}
	}
	if filepath.Dir(caCert) != filepath.Dir(caKey) || filepath.Dir(caCert) != filepath.Dir(serverCert) || filepath.Dir(caCert) != filepath.Dir(serverKey) || filepath.Base(caCert) != "runner-ca.crt" || filepath.Base(caKey) != "runner-ca.key" || filepath.Base(serverCert) != "server.crt" || filepath.Base(serverKey) != "server.key" {
		return ResolvedDeployment{}, manifestError("runner_trust files must use the canonical names in one protected directory", nil)
	}
	put("SECONDBOX_RUNNER_CREDENTIAL", credential)
	put("SECONDBOX_RUNNER_SERVER_NAME", trust.ServerName)
	put("SECONDBOX_RUNNER_PKI_HOST_DIR", filepath.Dir(caCert))
	put("SECONDBOX_RUNNER_CA_PRIVATE_KEY", caKey)
	put("SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS", strconv.FormatInt(*trust.CertificateLifetimeDays, 10))
	put("SECONDBOX_RUNNER_SERVER_CERTIFICATE", "/run/secondbox-runner-pki/server.crt")
	put("SECONDBOX_RUNNER_SERVER_PRIVATE_KEY", "/run/secondbox-runner-pki/server.key")
	put("SECONDBOX_RUNNER_CA_CERTIFICATE", "/run/secondbox-runner-pki/runner-ca.crt")
	_ = serverCert

	platformToken, platformPath, err := readSecretReference(base, manifest.Applications.PlatformTokenFile)
	if err != nil {
		return ResolvedDeployment{}, manifestError("applications.platform_token_file", err)
	}
	secretPaths["applications.platform_token_file"] = platformPath
	authorities, authoritiesPath, err := readSecretReference(base, manifest.Applications.ApplicationAuthoritiesFile)
	if err != nil {
		return ResolvedDeployment{}, manifestError("applications.application_authorities_file", err)
	}
	var authorityValue []controlconfig.ApplicationAuthority
	if err := json.Unmarshal([]byte(authorities), &authorityValue); err != nil {
		return ResolvedDeployment{}, manifestError("applications.application_authorities_file must contain a JSON array", err)
	}
	credentials := map[string]string{
		"applications.platform_token_file":        platformToken,
		"runner_trust.enrollment_credential_file": credential,
		"object_store.access_key_file":            access,
		"object_store.secret_key_file":            secret,
	}
	if databasePassword != "" {
		credentials["database.password_file"] = databasePassword
	}
	for i, authority := range authorityValue {
		credentials[fmt.Sprintf("applications.application_authorities_file[%d].token", i)] = authority.Token
	}
	if err := validateDistinctCredentials(credentials); err != nil {
		return ResolvedDeployment{}, err
	}
	secretPaths["applications.application_authorities_file"] = authoritiesPath
	put("SECONDBOX_PLATFORM_TOKEN", platformToken)
	put("SECONDBOX_APPLICATION_AUTHORITIES_JSON", authorities)

	addPolicyEnvironment(environment, manifest.Policy)
	overrides, err := resolvedOverrides(manifest.Overrides)
	if err != nil {
		return ResolvedDeployment{}, err
	}
	for name, value := range overrides {
		put(name, value)
	}

	remote := make(map[string]map[string]string)
	composeFiles := []string{"deploy/compose.yml"}
	if deployment.Mode == "development" {
		composeFiles = append(composeFiles, "deploy/compose.development.yml")
	} else {
		if database.Mode == "bundled" {
			composeFiles = append(composeFiles, "deploy/compose.bundled-database.yml")
		}
		if objectStore.Mode == "bundled" {
			composeFiles = append(composeFiles, "deploy/compose.bundled-object-store.yml")
		}
	}
	for _, runner := range manifest.Runners {
		runnerEnvironment := resolveRunnerEnvironment(runner, credential)
		if runner.Placement == "same-host" {
			for name, value := range runnerEnvironment {
				environment[name] = value
			}
			environment["SECONDBOX_SAME_HOST_RUNNER_ENABLED"] = "true"
			composeFiles = append(composeFiles, "deploy/compose.same-host-runner.yml")
		} else {
			remote[runner.RunnerID] = runnerEnvironment
		}
	}

	resources, err := resolveStandardResources(base, manifest, verifiedCatalog)
	if err != nil {
		return ResolvedDeployment{}, err
	}
	resolved := ResolvedDeployment{Manifest: manifest, Environment: environment, RemoteRunnerEnvironment: remote, ComposeFiles: composeFiles, SecretPaths: secretPaths, ResourceDocument: resources}
	if err := validateControlPlaneEnvironment(environment); err != nil {
		return ResolvedDeployment{}, manifestError("rendered control-plane environment failed production loader validation", err)
	}
	if _, err := EncodeComposeEnvironment(environment); err != nil {
		return ResolvedDeployment{}, err
	}
	remoteRunnerIDs := make([]string, 0, len(remote))
	for runnerID := range remote {
		remoteRunnerIDs = append(remoteRunnerIDs, runnerID)
	}
	sort.Strings(remoteRunnerIDs)
	for _, runnerID := range remoteRunnerIDs {
		if _, err := EncodeSystemdEnvironment(remote[runnerID]); err != nil {
			return ResolvedDeployment{}, manifestError("remote runner environment for "+runnerID, err)
		}
	}
	return resolved, nil
}

func validateManifestShape(manifest ManifestV1) error {
	require := func(path, value string) error {
		if strings.TrimSpace(value) == "" {
			return manifestError(path+" is required", nil)
		}
		return nil
	}
	requireInt := func(path string, value *int64, zero bool) error {
		if value == nil || *value < 0 || (!zero && *value == 0) {
			return manifestError(path+" must be "+map[bool]string{true: "non-negative", false: "positive"}[zero], nil)
		}
		return nil
	}
	requirePort := func(path string, value *int64) error {
		if value == nil || *value < 1 || *value > 65535 {
			return manifestError(path+" must be an integer from 1 through 65535", nil)
		}
		return nil
	}
	if manifest.SchemaVersion != 1 {
		return manifestError("schema_version must be 1", nil)
	}
	d := manifest.Deployment
	for path, value := range map[string]string{"deployment.mode": d.Mode, "deployment.public_base_url": d.PublicBaseURL, "deployment.tls_termination": d.TLSTermination, "deployment.control_plane_image": d.ControlPlaneImage, "deployment.runner_image": d.RunnerImage, "deployment.api_bind_ip": d.APIBindIP, "deployment.listen_address": d.ListenAddress, "deployment.runner_bind_ip": d.RunnerBindIP, "deployment.runner_listen_address": d.RunnerListenAddress, "deployment.log_path": d.LogPath, "deployment.signed_asset_catalog": d.SignedAssetCatalog, "deployment.signed_asset_catalog_path": d.SignedAssetCatalogPath} {
		if err := require(path, value); err != nil {
			return err
		}
	}
	if d.Mode != "development" && d.Mode != "production" {
		return manifestError("deployment.mode must be development or production", nil)
	}
	if d.ComposeProjectName != "" && !composeProjectPattern.MatchString(d.ComposeProjectName) {
		return manifestError("deployment.compose_project_name must start with a lowercase letter or digit and use at most 63 lowercase letters, digits, underscores, or hyphens", nil)
	}
	if d.Mode == "development" && (manifest.Database.Mode != "bundled" || manifest.ObjectStore.Mode != "bundled") {
		return manifestError("development mode requires the reviewed bundled database and object_store topology", nil)
	}
	if err := requirePort("deployment.api_published_port", d.APIPublishedPort); err != nil {
		return err
	}
	if err := requirePort("deployment.runner_published_port", d.RunnerPublishedPort); err != nil {
		return err
	}
	if d.ListenAddress != "0.0.0.0:8080" {
		return manifestError("deployment.listen_address must be 0.0.0.0:8080 for the packaged container mapping", nil)
	}
	if d.RunnerListenAddress != "0.0.0.0:9443" {
		return manifestError("deployment.runner_listen_address must be 0.0.0.0:9443 for the packaged container mapping", nil)
	}
	if d.SignedAssetCatalogPath != "/etc/secondbox/signed-assets.json" {
		return manifestError("deployment.signed_asset_catalog_path must be /etc/secondbox/signed-assets.json for the packaged container mapping", nil)
	}
	if d.DevelopmentWaitSeconds != nil {
		if err := requireInt("deployment.development_prepare_wait_timeout_seconds", d.DevelopmentWaitSeconds, false); err != nil {
			return err
		}
	} else if d.Mode == "development" {
		return manifestError("deployment.development_prepare_wait_timeout_seconds is required in development mode", nil)
	}
	if d.Mode == "development" && (d.APIBindIP != "127.0.0.1" || d.RunnerBindIP != "127.0.0.1" || manifest.Database.BindIP != "127.0.0.1" || manifest.ObjectStore.BindIP != "127.0.0.1") {
		return manifestError("development mode must bind every published port to 127.0.0.1", nil)
	}
	if !filepath.IsAbs(d.LogPath) || !filepath.IsAbs(d.SignedAssetCatalogPath) {
		return manifestError("deployment process paths must be absolute", nil)
	}
	parsedURL, err := url.Parse(d.PublicBaseURL)
	if err != nil || parsedURL.Host == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return manifestError("deployment.public_base_url must be an absolute HTTP URL", err)
	}
	if d.Mode == "production" {
		if d.TLSTermination != "external" || parsedURL.Scheme != "https" {
			return manifestError("production requires external TLS and an HTTPS public_base_url", nil)
		}
		for path, image := range map[string]string{"deployment.control_plane_image": d.ControlPlaneImage, "deployment.runner_image": d.RunnerImage} {
			if !strings.Contains(image, "@sha256:") {
				return manifestError(path+" must be an immutable digest reference in production", nil)
			}
		}
		if manifest.Database.Mode == "bundled" && !strings.Contains(d.PostgresImage, "@sha256:") {
			return manifestError("deployment.postgres_image must be an immutable digest reference for a production bundled database", nil)
		}
		if manifest.ObjectStore.Mode == "bundled" {
			for path, image := range map[string]string{"deployment.object_store_image": d.ObjectStoreImage, "deployment.object_store_client_image": d.ObjectStoreClientImage} {
				if !strings.Contains(image, "@sha256:") {
					return manifestError(path+" must be an immutable digest reference in production", nil)
				}
			}
		}
	}

	db := manifest.Database
	if db.Mode != "bundled" && db.Mode != "external" {
		return manifestError("database.mode must be bundled or external", nil)
	}
	if db.Mode == "bundled" {
		if db.URLFile != "" {
			return manifestError("database.url_file is ambiguous in bundled mode", nil)
		}
		for p, v := range map[string]string{"database.name": db.Name, "database.user": db.User, "database.password_file": db.PasswordFile, "database.bind_ip": db.BindIP} {
			if err := require(p, v); err != nil {
				return err
			}
		}
		if err := requirePort("database.published_port", db.PublishedPort); err != nil {
			return err
		}
		if err := require("deployment.postgres_image", d.PostgresImage); err != nil {
			return err
		}
	} else {
		if err := require("database.url_file", db.URLFile); err != nil {
			return err
		}
		if db.PasswordFile != "" || db.Name != "" || db.User != "" || db.BindIP != "" || db.PublishedPort != nil {
			return manifestError("external database contains bundled-only fields", nil)
		}
	}
	osConfig := manifest.ObjectStore
	if osConfig.Mode != "bundled" && osConfig.Mode != "external" {
		return manifestError("object_store.mode must be bundled or external", nil)
	}
	for p, v := range map[string]string{"object_store.endpoint": osConfig.Endpoint, "object_store.bucket": osConfig.Bucket, "object_store.region": osConfig.Region, "object_store.temp_directory": osConfig.TempDirectory, "object_store.access_key_file": osConfig.AccessKeyFile, "object_store.secret_key_file": osConfig.SecretKeyFile} {
		if err := require(p, v); err != nil {
			return err
		}
	}
	if osConfig.UsePathStyle == nil {
		return manifestError("object_store.use_path_style is required", nil)
	}
	if !filepath.IsAbs(osConfig.TempDirectory) {
		return manifestError("object_store.temp_directory must be absolute", nil)
	}
	endpoint, err := url.Parse(osConfig.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return manifestError("object_store.endpoint must be an absolute HTTP URL", err)
	}
	if endpoint.User != nil {
		return manifestError("object_store.endpoint must not contain userinfo; use the credential files", nil)
	}
	if osConfig.Mode == "external" && d.Mode == "production" && endpoint.Scheme != "https" {
		return manifestError("production external object_store.endpoint must use HTTPS", nil)
	}
	if osConfig.Mode == "bundled" {
		if osConfig.Endpoint != "http://object-store:9000" {
			return manifestError("bundled object_store.endpoint must be http://object-store:9000", nil)
		}
		for p, v := range map[string]string{"object_store.bind_ip": osConfig.BindIP, "deployment.object_store_image": d.ObjectStoreImage, "deployment.object_store_client_image": d.ObjectStoreClientImage} {
			if err := require(p, v); err != nil {
				return err
			}
		}
		for p, v := range map[string]*int64{"object_store.published_port": osConfig.PublishedPort, "object_store.console_published_port": osConfig.ConsolePublishedPort} {
			if err := requirePort(p, v); err != nil {
				return err
			}
		}
	} else if osConfig.BindIP != "" || osConfig.PublishedPort != nil || osConfig.ConsolePublishedPort != nil {
		return manifestError("external object_store contains bundled-only fields", nil)
	}

	t := manifest.RunnerTrust
	for p, v := range map[string]string{"runner_trust.enrollment_credential_file": t.EnrollmentCredentialFile, "runner_trust.ca_certificate_file": t.CACertificateFile, "runner_trust.ca_private_key_file": t.CAPrivateKeyFile, "runner_trust.server_certificate_file": t.ServerCertificateFile, "runner_trust.server_private_key_file": t.ServerPrivateKeyFile, "runner_trust.server_name": t.ServerName} {
		if err := require(p, v); err != nil {
			return err
		}
	}
	if err := requireInt("runner_trust.certificate_lifetime_days", t.CertificateLifetimeDays, false); err != nil {
		return err
	}
	for p, v := range map[string]string{"applications.platform_token_file": manifest.Applications.PlatformTokenFile, "applications.application_authorities_file": manifest.Applications.ApplicationAuthoritiesFile} {
		if err := require(p, v); err != nil {
			return err
		}
	}
	if err := validatePolicy(manifest.Policy); err != nil {
		return err
	}
	if err := validateStandardResources(manifest.StandardResources, manifest.Runners); err != nil {
		return err
	}
	seen := make(map[string]bool)
	sameHost := false
	for i, r := range manifest.Runners {
		prefix := fmt.Sprintf("runners[%d]", i)
		if err := require(prefix+".runner_id", r.RunnerID); err != nil {
			return err
		}
		if !opaqueRunnerIDPattern.MatchString(r.RunnerID) {
			return manifestError(prefix+".runner_id must be a valid opaque Runner ID", nil)
		}
		if seen[r.RunnerID] {
			return manifestError("duplicate runner_id "+r.RunnerID, nil)
		}
		seen[r.RunnerID] = true
		if r.Placement != "same-host" && r.Placement != "remote" {
			return manifestError(prefix+".placement must be same-host or remote", nil)
		}
		if r.Placement == "same-host" {
			if sameHost {
				return manifestError("at most one runner may use same-host placement", nil)
			}
			sameHost = true
		}
		if err := validateRunner(prefix, r); err != nil {
			return err
		}
	}
	return nil
}

func validateDistinctCredentials(credentials map[string]string) error {
	pathsByValue := make(map[string]string, len(credentials))
	for _, path := range sortedKeys(credentials) {
		value := credentials[path]
		if value == "" {
			continue
		}
		if other, exists := pathsByValue[value]; exists {
			return manifestError(path+" and "+other+" must use distinct credentials for separate trust boundaries", nil)
		}
		pathsByValue[value] = path
	}
	return nil
}

func validateExternalDatabaseURL(value string, requireVerifiedTLS bool) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("must be an absolute PostgreSQL URL: %w", err)
	}
	if parsed.Host == "" || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return fmt.Errorf("must be an absolute PostgreSQL URL")
	}
	if requireVerifiedTLS {
		modes := parsed.Query()["sslmode"]
		if len(modes) != 1 || modes[0] != "verify-full" {
			return fmt.Errorf("production must select exactly one sslmode=verify-full query option")
		}
	}
	return nil
}

func validatePolicy(p Policy) error {
	positive := map[string]*int64{"data_plane_retention_seconds": p.DataPlaneRetentionSeconds, "data_plane_poll_interval_milliseconds": p.DataPlanePollIntervalMilliseconds, "runner_command_poll_interval_milliseconds": p.RunnerCommandPollIntervalMilliseconds}
	for name, value := range positive {
		if value == nil || *value < 1 {
			return manifestError("policy."+name+" must be positive", nil)
		}
	}
	quotas := map[string]*int64{"default_subject_max_sandboxes": p.DefaultSubjectMaxSandboxes, "default_subject_max_active_instances": p.DefaultSubjectMaxActiveInstances, "default_subject_max_cpu_millis": p.DefaultSubjectMaxCPUMillis, "default_subject_max_memory_bytes": p.DefaultSubjectMaxMemoryBytes, "default_subject_max_artifact_bytes": p.DefaultSubjectMaxArtifactBytes, "default_subject_max_snapshots": p.DefaultSubjectMaxSnapshots, "default_subject_max_artifacts": p.DefaultSubjectMaxArtifacts, "default_subject_max_port_sessions": p.DefaultSubjectMaxPortSessions, "default_subject_max_concurrent_operations": p.DefaultSubjectMaxConcurrentOperations}
	for name, value := range quotas {
		if value == nil || *value < 0 {
			return manifestError("policy."+name+" must be non-negative", nil)
		}
	}
	for name, value := range map[string]string{"runner_enabled_features": p.RunnerEnabledFeatures} {
		if strings.TrimSpace(value) == "" {
			return manifestError("policy."+name+" is required", nil)
		}
	}
	featureNames := make([]string, 0)
	seenFeatures := make(map[string]bool)
	for _, entry := range strings.Split(p.RunnerEnabledFeatures, ",") {
		name := strings.TrimSpace(entry)
		if name == "" || seenFeatures[name] {
			return manifestError("policy.runner_enabled_features must contain unique non-empty comma-separated values", nil)
		}
		seenFeatures[name] = true
		featureNames = append(featureNames, name)
	}
	if _, err := runnerfeatures.Parse(featureNames); err != nil {
		return manifestError("policy.runner_enabled_features", err)
	}
	return nil
}

func validateStandardResources(resources StandardResources, runners []Runner) error {
	if strings.TrimSpace(resources.ArtifactManifest) == "" {
		return manifestError("standard_resources.artifact_manifest is required", nil)
	}
	if len(resources.Bundles) == 0 {
		return manifestError("standard_resources.bundles must explicitly select at least one bundle", nil)
	}
	if resources.ApplyWaitSeconds == nil || *resources.ApplyWaitSeconds < 1 {
		return manifestError("standard_resources.apply_wait_seconds must be positive", nil)
	}
	selected := map[string]bool{}
	for _, bundle := range resources.Bundles {
		if (bundle != standardresources.AgentCompartment && bundle != standardresources.DurableCoding) || selected[bundle] {
			return manifestError("standard_resources.bundles must contain unique agent-compartment or durable-coding names", nil)
		}
		selected[bundle] = true
	}
	bindings := map[string]StandardRunnerPool{}
	for index, pool := range resources.RunnerPools {
		prefix := fmt.Sprintf("standard_resources.runner_pools[%d]", index)
		if !selected[pool.Bundle] || bindings[pool.Bundle].Bundle != "" {
			return manifestError(prefix+" must bind one selected bundle exactly once", nil)
		}
		if pool.Name != standardresources.PoolAMD64 || pool.State == "" || len(pool.Architectures) == 0 || !slices.Contains(pool.Architectures, "amd64") || len(pool.Capabilities) == 0 {
			return manifestError(prefix+" requires name, ready state, capabilities and amd64 architecture inventory", nil)
		}
		for name, value := range map[string]*int64{"max_sandboxes": pool.MaxSandboxes, "max_cpu_millis": pool.MaxCPUMillis, "max_memory_bytes": pool.MaxMemoryBytes} {
			if value == nil || *value < 1 {
				return manifestError(prefix+"."+name+" must be positive", nil)
			}
		}
		gateway := map[string]string{standardresources.AgentCompartment: standardresources.AgentGateway, standardresources.DurableCoding: standardresources.PlatformGateway}[pool.Bundle]
		for runnerIndex, runner := range runners {
			if runner.PoolID == pool.Name && !runnerGatewayNames(runner.NetworkPolicyRunnerGateways)[gateway] {
				return manifestError(fmt.Sprintf("runners[%d].network_policy_runner_gateways must resolve %s for selected bundle %s", runnerIndex, gateway, pool.Bundle), nil)
			}
		}
		bindings[pool.Bundle] = pool
	}
	for bundle := range selected {
		if bindings[bundle].Bundle == "" {
			return manifestError("standard_resources.runner_pools must bind selected bundle "+bundle, nil)
		}
	}
	return nil
}

func runnerGatewayNames(value string) map[string]bool {
	result := map[string]bool{}
	for _, entry := range strings.Split(value, ",") {
		name, _, found := strings.Cut(strings.TrimSpace(entry), "=")
		if found {
			result[strings.TrimSpace(name)] = true
		}
	}
	return result
}

func validateRunner(prefix string, r Runner) error {
	required := map[string]string{"pool_id": r.PoolID, "software_version": r.SoftwareVersion, "control_plane_address": r.ControlPlaneAddress, "control_plane_server_name": r.ControlPlaneServerName, "identity_directory": r.IdentityDirectory, "log_path": r.LogPath, "firecracker_path": r.FirecrackerPath, "firecracker_jailer_path": r.FirecrackerJailerPath, "firecracker_jail_root": r.FirecrackerJailRoot, "firecracker_cgroup_parent": r.FirecrackerCgroupParent, "firecracker_kernel_path": r.FirecrackerKernelPath, "firecracker_rootfs_path": r.FirecrackerRootFSPath, "firecracker_shared_image_path": r.FirecrackerSharedImagePath, "firecracker_kernel_args": r.FirecrackerKernelArgs, "firecracker_cpu_template": r.FirecrackerCPUTemplate, "firecracker_run_directory": r.FirecrackerRunDirectory, "firecracker_log_directory": r.FirecrackerLogDirectory, "snapshot_template_cache_root": r.SnapshotTemplateCacheRoot, "artifact_public_key": r.ArtifactPublicKey, "artifact_public_key_sha256": r.ArtifactPublicKeySHA256, "workspace_root": r.WorkspaceRoot, "sandbox_guest_ip": r.SandboxGuestIP, "sandbox_bridge_name": r.SandboxBridgeName, "sandbox_bridge_cidr": r.SandboxBridgeCIDR, "sandbox_guest_cidr": r.SandboxGuestCIDR, "sandbox_tap_prefix": r.SandboxTapPrefix, "sandbox_network_state_directory": r.SandboxNetworkStateDir, "network_policy_nft_path": r.NetworkPolicyNFTPath, "network_policy_max_dns_ttl": r.NetworkPolicyMaxDNSTTL, "network_policy_runner_addresses": r.NetworkPolicyRunnerAddresses, "network_policy_management_cidrs": r.NetworkPolicyManagementCIDRs, "network_policy_runner_gateways": r.NetworkPolicyRunnerGateways, "network_policy_dns_upstream": r.NetworkPolicyDNSUpstream, "guest_heartbeat_interval": r.GuestHeartbeatInterval, "data_plane_listen_address": r.DataPlaneListenAddress, "data_plane_advertised_address": r.DataPlaneAdvertisedAddress}
	required["log_directory"] = r.LogDirectory
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return manifestError(prefix+"."+name+" is required", nil)
		}
	}
	for name, value := range map[string]string{
		"identity_directory":              r.IdentityDirectory,
		"log_path":                        r.LogPath,
		"log_directory":                   r.LogDirectory,
		"firecracker_path":                r.FirecrackerPath,
		"firecracker_jailer_path":         r.FirecrackerJailerPath,
		"firecracker_jail_root":           r.FirecrackerJailRoot,
		"firecracker_kernel_path":         r.FirecrackerKernelPath,
		"firecracker_rootfs_path":         r.FirecrackerRootFSPath,
		"firecracker_shared_image_path":   r.FirecrackerSharedImagePath,
		"firecracker_run_directory":       r.FirecrackerRunDirectory,
		"firecracker_log_directory":       r.FirecrackerLogDirectory,
		"snapshot_template_cache_root":    r.SnapshotTemplateCacheRoot,
		"artifact_public_key":             r.ArtifactPublicKey,
		"workspace_root":                  r.WorkspaceRoot,
		"sandbox_network_state_directory": r.SandboxNetworkStateDir,
		"network_policy_nft_path":         r.NetworkPolicyNFTPath,
	} {
		if !filepath.IsAbs(value) {
			return manifestError(prefix+"."+name+" must be an absolute Runner-host path", nil)
		}
	}
	for name, value := range map[string]string{
		"identity_host_directory":  r.IdentityHostDirectory,
		"artifact_host_directory":  r.ArtifactHostDirectory,
		"state_host_directory":     r.StateHostDirectory,
		"workspace_host_directory": r.WorkspaceHostDirectory,
	} {
		if value != "" && !filepath.IsAbs(value) {
			return manifestError(prefix+"."+name+" must be an absolute Runner-host path", nil)
		}
	}
	if r.Placement == "same-host" {
		for name, value := range map[string]string{"identity_host_directory": r.IdentityHostDirectory, "artifact_host_directory": r.ArtifactHostDirectory, "state_host_directory": r.StateHostDirectory, "workspace_host_directory": r.WorkspaceHostDirectory, "log_directory": r.LogDirectory} {
			if !filepath.IsAbs(value) {
				return manifestError(prefix+"."+name+" must be absolute", nil)
			}
		}
		if r.IdentityDirectory != "/run/secondbox-runner-identity" {
			return manifestError(prefix+".identity_directory must be /run/secondbox-runner-identity for same-host Compose placement", nil)
		}
		for name, value := range map[string]string{
			"firecracker_kernel_path":       r.FirecrackerKernelPath,
			"firecracker_rootfs_path":       r.FirecrackerRootFSPath,
			"firecracker_shared_image_path": r.FirecrackerSharedImagePath,
			"artifact_public_key":           r.ArtifactPublicKey,
		} {
			if !pathWithin("/opt/secondbox-artifacts", value) {
				return manifestError(prefix+"."+name+" must be within /opt/secondbox-artifacts for same-host Compose placement", nil)
			}
		}
		for name, value := range map[string]string{
			"log_path":                        r.LogPath,
			"log_directory":                   r.LogDirectory,
			"firecracker_jail_root":           r.FirecrackerJailRoot,
			"firecracker_run_directory":       r.FirecrackerRunDirectory,
			"firecracker_log_directory":       r.FirecrackerLogDirectory,
			"snapshot_template_cache_root":    r.SnapshotTemplateCacheRoot,
			"sandbox_network_state_directory": r.SandboxNetworkStateDir,
		} {
			if !pathWithin("/var/lib/secondbox-runner", value) {
				return manifestError(prefix+"."+name+" must be within /var/lib/secondbox-runner for same-host Compose placement", nil)
			}
		}
	}
	for name, value := range map[string]*int64{"firecracker_jailer_uid_start": r.FirecrackerJailerUIDStart, "firecracker_jailer_uid_count": r.FirecrackerJailerUIDCount, "firecracker_jailer_gid": r.FirecrackerJailerGID, "firecracker_cgroup_version": r.FirecrackerCgroupVersion, "storage_pressure_recovery_percent": r.StorageRecoveryPercent, "storage_pressure_warning_percent": r.StorageWarningPercent, "storage_pressure_admission_deny_percent": r.StorageAdmissionDenyPercent, "sandbox_max_vcpus": r.SandboxMaxVCPUs, "sandbox_max_memory_mib": r.SandboxMaxMemoryMiB, "sandbox_max_disk_mib": r.SandboxMaxDiskMiB, "sandbox_memory_budget_mib": r.SandboxMemoryBudgetMiB, "network_policy_max_dns_pins": r.NetworkPolicyMaxDNSPins, "max_concurrent_per_sandbox": r.MaxConcurrentPerSandbox, "max_concurrent_global": r.MaxConcurrentGlobal, "max_concurrent_starts": r.MaxConcurrentStarts, "max_concurrent_workspace_creates": r.MaxConcurrentWorkspaceCreates, "max_concurrent_operations_global": r.MaxConcurrentOperationsGlobal, "file_transfer_max_bytes": r.FileTransferMaxBytes, "guest_control_vsock_port": r.GuestControlVSockPort, "guest_protocol_vsock_port": r.GuestProtocolVSockPort} {
		if value == nil || *value < 1 {
			return manifestError(prefix+"."+name+" must be positive", nil)
		}
	}
	for name, value := range map[string]*bool{"firecracker_jailer_uid_allow_below_1000": r.FirecrackerJailerUIDAllowLow, "firecracker_allow_unjailed": r.FirecrackerAllowUnjailed, "sandbox_delete_bridge": r.SandboxDeleteBridge} {
		if value == nil {
			return manifestError(prefix+"."+name+" is required", nil)
		}
	}
	if r.StorageRecoveryPercent != nil && r.StorageWarningPercent != nil && r.StorageAdmissionDenyPercent != nil && !(*r.StorageRecoveryPercent < *r.StorageWarningPercent && *r.StorageWarningPercent < *r.StorageAdmissionDenyPercent) {
		return manifestError(prefix+" storage pressure thresholds must satisfy 0 < recovery < warning < admission deny < 100", nil)
	}
	if r.StorageAdmissionDenyPercent != nil && *r.StorageAdmissionDenyPercent >= 100 {
		return manifestError(prefix+" storage pressure thresholds must satisfy 0 < recovery < warning < admission deny < 100", nil)
	}
	if r.MaxConcurrentStarts != nil && r.MaxConcurrentGlobal != nil && *r.MaxConcurrentStarts > *r.MaxConcurrentGlobal {
		return manifestError(prefix+".max_concurrent_starts must not exceed max_concurrent_global", nil)
	}
	if r.FirecrackerJailerUIDCount != nil && r.MaxConcurrentGlobal != nil && *r.FirecrackerJailerUIDCount < *r.MaxConcurrentGlobal {
		return manifestError(prefix+".firecracker_jailer_uid_count must be at least max_concurrent_global", nil)
	}
	if r.FirecrackerJailerUIDStart != nil && r.FirecrackerJailerUIDAllowLow != nil &&
		*r.FirecrackerJailerUIDStart < 1000 && !*r.FirecrackerJailerUIDAllowLow {
		return manifestError(prefix+".firecracker_jailer_uid_start below 1000 requires firecracker_jailer_uid_allow_below_1000=true", nil)
	}
	if r.FirecrackerJailerUIDStart != nil && r.FirecrackerJailerUIDCount != nil &&
		*r.FirecrackerJailerUIDStart > int64(^uint32(0))-*r.FirecrackerJailerUIDCount+1 {
		return manifestError(prefix+" jailer UID range must fit within unsigned 32-bit user IDs", nil)
	}
	if r.GuestControlVSockPort != nil && r.GuestProtocolVSockPort != nil {
		if *r.GuestControlVSockPort > 65535 || *r.GuestProtocolVSockPort > 65535 || *r.GuestControlVSockPort == *r.GuestProtocolVSockPort {
			return manifestError(prefix+" guest control and protocol vsock ports must be distinct integers from 1 through 65535", nil)
		}
	}
	if r.FirecrackerAllowUnjailed != nil && *r.FirecrackerAllowUnjailed {
		return manifestError(prefix+".firecracker_allow_unjailed must be false for the packaged Runner", nil)
	}
	if !artifactKeyPattern.MatchString(r.ArtifactPublicKeySHA256) || r.ArtifactPublicKeySHA256 == strings.Repeat("0", 64) {
		return manifestError(prefix+".artifact_public_key_sha256 must identify a provisioned signed artifact key", nil)
	}
	for _, argument := range []string{"console=ttyS0", "reboot=k", "panic=1", "pci=off", "root=/dev/vda", "rw", "quiet", "loglevel=1", "i8042.noaux", "i8042.nomux", "i8042.nopnp", "i8042.dumbkbd", "init=/init"} {
		if !slices.Contains(strings.Fields(r.FirecrackerKernelArgs), argument) {
			return manifestError(prefix+".firecracker_kernel_args must include "+argument, nil)
		}
	}
	if err := validateRunnerDuration(prefix+".guest_heartbeat_interval", r.GuestHeartbeatInterval, time.Millisecond, time.Minute); err != nil {
		return err
	}
	if err := validateRunnerDuration(prefix+".network_policy_max_dns_ttl", r.NetworkPolicyMaxDNSTTL, time.Nanosecond, 0); err != nil {
		return err
	}
	if _, err := netip.ParseAddr(r.SandboxGuestIP); err != nil {
		return manifestError(prefix+".sandbox_guest_ip must be an IP address", err)
	}
	if _, err := netip.ParsePrefix(r.SandboxBridgeCIDR); err != nil {
		return manifestError(prefix+".sandbox_bridge_cidr must be a CIDR", err)
	}
	if _, err := netip.ParsePrefix(r.SandboxGuestCIDR); err != nil {
		return manifestError(prefix+".sandbox_guest_cidr must be a CIDR", err)
	}
	if err := validateAddressList(prefix+".network_policy_runner_addresses", r.NetworkPolicyRunnerAddresses, false); err != nil {
		return err
	}
	if err := validateAddressList(prefix+".network_policy_management_cidrs", r.NetworkPolicyManagementCIDRs, true); err != nil {
		return err
	}
	if err := validateRunnerGateways(prefix+".network_policy_runner_gateways", r.NetworkPolicyRunnerGateways); err != nil {
		return err
	}
	if upstream, err := netip.ParseAddrPort(r.NetworkPolicyDNSUpstream); err != nil || upstream.Port() == 0 {
		return manifestError(prefix+".network_policy_dns_upstream must be an IP:port", err)
	}
	if err := validateDataPlaneAddress(prefix+".data_plane_listen_address", r.DataPlaneListenAddress, true); err != nil {
		return err
	}
	if err := validateDataPlaneAddress(prefix+".data_plane_advertised_address", r.DataPlaneAdvertisedAddress, false); err != nil {
		return err
	}
	return nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validateRunnerDuration(path, value string, minimum, maximum time.Duration) error {
	duration, err := time.ParseDuration(value)
	if err != nil || duration < minimum || (maximum != 0 && duration > maximum) {
		return manifestError(path+" is outside the supported duration range", err)
	}
	return nil
}

func validateAddressList(path, value string, prefixes bool) error {
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		var err error
		if prefixes {
			_, err = netip.ParsePrefix(part)
		} else {
			_, err = netip.ParseAddr(part)
		}
		if err != nil {
			return manifestError(path+" contains an invalid network value", err)
		}
	}
	return nil
}

func validateRunnerGateways(path, value string) error {
	if value == "none" {
		return nil
	}
	seen := make(map[string]bool)
	for _, part := range strings.Split(value, ",") {
		domain, address, found := strings.Cut(strings.TrimSpace(part), "=")
		domain = strings.TrimSpace(domain)
		if !found || domain == "" || seen[domain] {
			return manifestError(path+" entries must be unique domain=IP pairs or none", nil)
		}
		seen[domain] = true
		if _, err := netip.ParseAddr(strings.TrimSpace(address)); err != nil {
			return manifestError(path+" contains an invalid gateway IP", err)
		}
	}
	return nil
}

func validateDataPlaneAddress(path, value string, listen bool) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return manifestError(path+" must be a host:port address", err)
	}
	number, err := strconv.Atoi(port)
	minimum := 1
	if listen {
		minimum = 0
	}
	if err != nil || number < minimum || number > 65535 {
		return manifestError(path+" must name a valid port", err)
	}
	if !listen && (strings.TrimSpace(host) == "" || host == "0.0.0.0" || host == "::") {
		return manifestError(path+" must name an explicit reachable host", nil)
	}
	return nil
}

func addPolicyEnvironment(environment map[string]string, p Policy) {
	values := map[string]*int64{"SECONDBOX_DATA_PLANE_RETENTION_SECONDS": p.DataPlaneRetentionSeconds, "SECONDBOX_DATA_PLANE_POLL_INTERVAL_MILLISECONDS": p.DataPlanePollIntervalMilliseconds, "SECONDBOX_RUNNER_COMMAND_POLL_INTERVAL_MILLISECONDS": p.RunnerCommandPollIntervalMilliseconds, "SECONDBOX_DEFAULT_SUBJECT_MAX_SANDBOXES": p.DefaultSubjectMaxSandboxes, "SECONDBOX_DEFAULT_SUBJECT_MAX_ACTIVE_INSTANCES": p.DefaultSubjectMaxActiveInstances, "SECONDBOX_DEFAULT_SUBJECT_MAX_CPU_MILLIS": p.DefaultSubjectMaxCPUMillis, "SECONDBOX_DEFAULT_SUBJECT_MAX_MEMORY_BYTES": p.DefaultSubjectMaxMemoryBytes, "SECONDBOX_DEFAULT_SUBJECT_MAX_ARTIFACT_BYTES": p.DefaultSubjectMaxArtifactBytes, "SECONDBOX_DEFAULT_SUBJECT_MAX_SNAPSHOTS": p.DefaultSubjectMaxSnapshots, "SECONDBOX_DEFAULT_SUBJECT_MAX_ARTIFACTS": p.DefaultSubjectMaxArtifacts, "SECONDBOX_DEFAULT_SUBJECT_MAX_PORT_SESSIONS": p.DefaultSubjectMaxPortSessions, "SECONDBOX_DEFAULT_SUBJECT_MAX_CONCURRENT_OPERATIONS": p.DefaultSubjectMaxConcurrentOperations}
	for name, value := range values {
		environment[name] = strconv.FormatInt(*value, 10)
	}
	environment["SECONDBOX_RUNNER_ENABLED_FEATURES"] = p.RunnerEnabledFeatures
}

func resolveStandardResources(base string, manifest ManifestV1, catalog assetcatalog.SignedAssetCatalog) (resourceapply.Document, error) {
	path, err := resolveRegularReference(base, manifest.StandardResources.ArtifactManifest)
	if err != nil {
		return resourceapply.Document{}, manifestError("standard_resources.artifact_manifest", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return resourceapply.Document{}, manifestError("standard_resources.artifact_manifest", err)
	}
	releaseManifest, err := releasecontract.DecodeArtifactManifest(data)
	if err != nil {
		return resourceapply.Document{}, manifestError("standard_resources.artifact_manifest", err)
	}
	expectedKeyID := strings.ToLower(strings.TrimPrefix(releaseManifest.MicroVM.SigningKeyFingerprint, "SHA256:"))
	components := []releasecontract.SignedComponent{releaseManifest.MicroVM.RuntimeBundle, releaseManifest.MicroVM.ToolchainBundle}
	for _, component := range components {
		asset, err := catalog.Resolve(component.ManifestDigest)
		if err != nil {
			return resourceapply.Document{}, manifestError("standard_resources artifact manifest component identity must exist in deployment.signed_asset_catalog", err)
		}
		if asset.ArtifactID != component.ArtifactID || asset.ManifestDigest != component.ManifestDigest ||
			asset.Architecture != standardresources.ArchitectureAMD64 || asset.GuestProtocolGeneration != releaseManifest.GuestProtocol.Maximum ||
			!slices.Equal(asset.MandatoryGuestFeatures, component.MandatoryGuestFeatures) {
			return resourceapply.Document{}, manifestError("standard_resources artifact manifest component identity differs from deployment.signed_asset_catalog", nil)
		}
		if manifest.Deployment.Mode == "production" && asset.SignatureKeyID != expectedKeyID {
			return resourceapply.Document{}, manifestError("standard_resources artifact manifest signing identity differs from deployment.signed_asset_catalog", nil)
		}
	}
	if manifest.Deployment.Mode == "production" {
		for index, runner := range manifest.Runners {
			if runner.PoolID == standardresources.PoolAMD64 && runner.ArtifactPublicKeySHA256 != expectedKeyID {
				return resourceapply.Document{}, manifestError(fmt.Sprintf("runners[%d].artifact_public_key_sha256 differs from standard_resources artifact manifest signing identity", index), nil)
			}
		}
	}
	pools := make(map[string]standardresources.PoolBinding, len(manifest.StandardResources.RunnerPools))
	for _, configured := range manifest.StandardResources.RunnerPools {
		pools[configured.Bundle] = standardresources.PoolBinding{Name: configured.Name, Architectures: configured.Architectures, Capabilities: configured.Capabilities, State: configured.State, CapacityPolicy: map[string]int64{"maxSandboxes": *configured.MaxSandboxes, "maxCpuMillis": *configured.MaxCPUMillis, "maxMemoryBytes": *configured.MaxMemoryBytes}}
	}
	document, err := standardresources.Build(releaseManifest, standardresources.Selection{Bundles: manifest.StandardResources.Bundles, Pools: pools})
	if err != nil {
		return resourceapply.Document{}, manifestError("standard_resources", err)
	}
	return document, nil
}

func resolveRegularReference(base, reference string) (string, error) {
	if strings.TrimSpace(reference) == "" {
		return "", fmt.Errorf("empty reference")
	}
	path := reference
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("must name an exact regular non-symbolic-link file")
	}
	return path, nil
}
func readSecretReference(base, reference string) (string, string, error) {
	path, err := resolveRegularReference(base, reference)
	if err != nil {
		return "", "", err
	}
	if err := validatePrivateFileMode(path); err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	if bytes.IndexByte(data, 0) >= 0 || bytes.IndexByte(data, '\r') >= 0 {
		return "", "", fmt.Errorf("secret contains a forbidden NUL or CR byte")
	}
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	if bytes.IndexByte(data, '\n') >= 0 {
		return "", "", fmt.Errorf("secret contains more than one line")
	}
	if len(data) == 0 {
		return "", "", fmt.Errorf("secret is empty")
	}
	return string(data), path, nil
}

func resolvePrivateReference(base, reference string) (string, error) {
	path, err := resolveRegularReference(base, reference)
	if err != nil {
		return "", err
	}
	if err := validatePrivateFileMode(path); err != nil {
		return "", err
	}
	return path, nil
}

func validatePrivateFileMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("private file permissions must not grant group or other access")
	}
	return nil
}

func validateRunnerTrustMaterial(caCertificatePath, caPrivateKeyPath, serverCertificatePath, serverPrivateKeyPath, serverName string) error {
	caPEM, err := os.ReadFile(caCertificatePath)
	if err != nil {
		return err
	}
	caBlock, remainder := pem.Decode(caPEM)
	if caBlock == nil || len(remainder) != 0 || caBlock.Type != "CERTIFICATE" {
		return fmt.Errorf("CA must contain exactly one PEM certificate")
	}
	caCertificate, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil || !caCertificate.IsCA {
		return fmt.Errorf("CA certificate is invalid: %w", err)
	}
	keyPEM, err := os.ReadFile(caPrivateKeyPath)
	if err != nil {
		return err
	}
	keyBlock, remainder := pem.Decode(keyPEM)
	if keyBlock == nil || len(remainder) != 0 {
		return fmt.Errorf("CA private key must contain exactly one PEM key")
	}
	caKey, err := parseRSAPrivateKey(keyBlock.Bytes)
	if err != nil {
		return fmt.Errorf("CA private key: %w", err)
	}
	if !caKey.PublicKey.Equal(caCertificate.PublicKey) {
		return fmt.Errorf("CA certificate and private key do not match")
	}
	serverPair, err := tls.LoadX509KeyPair(serverCertificatePath, serverPrivateKeyPath)
	if err != nil {
		return fmt.Errorf("server certificate and key: %w", err)
	}
	serverCertificate, err := x509.ParseCertificate(serverPair.Certificate[0])
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCertificate)
	if _, err := serverCertificate.Verify(x509.VerifyOptions{Roots: roots, DNSName: serverName, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return fmt.Errorf("server certificate verification: %w", err)
	}
	return nil
}

func parseRSAPrivateKey(der []byte) (*rsa.PrivateKey, error) {
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("is not valid PKCS#1 or PKCS#8 key material")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("must be RSA")
	}
	return key, nil
}

func resolveRunnerEnvironment(r Runner, credential string) map[string]string {
	env := map[string]string{"SECONDBOX_RUNNER_ID": r.RunnerID, "SECONDBOX_RUNNER_POOL_ID": r.PoolID, "SECONDBOX_RUNNER_SOFTWARE_VERSION": r.SoftwareVersion, "SECONDBOX_RUNNER_CONTROL_PLANE_ADDRESS": r.ControlPlaneAddress, "SECONDBOX_RUNNER_CONTROL_PLANE_SERVER_NAME": r.ControlPlaneServerName, "SECONDBOX_RUNNER_CREDENTIAL": credential, "SECONDBOX_RUNNER_LOG_PATH": r.LogPath, "SECONDBOX_RUNNER_FIRECRACKER_PATH": r.FirecrackerPath, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH": r.FirecrackerJailerPath, "SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT": r.FirecrackerJailRoot, "SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT": r.FirecrackerCgroupParent, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH": r.FirecrackerKernelPath, "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH": r.FirecrackerRootFSPath, "SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH": r.FirecrackerSharedImagePath, "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS": r.FirecrackerKernelArgs, "SECONDBOX_RUNNER_FIRECRACKER_CPU_TEMPLATE": r.FirecrackerCPUTemplate, "SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR": r.FirecrackerRunDirectory, "SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR": r.FirecrackerLogDirectory, "SECONDBOX_RUNNER_SNAPSHOT_TEMPLATE_CACHE_ROOT": r.SnapshotTemplateCacheRoot, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY": r.ArtifactPublicKey, "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256": r.ArtifactPublicKeySHA256, "SECONDBOX_RUNNER_WORKSPACE_ROOT": r.WorkspaceRoot, "SECONDBOX_RUNNER_SANDBOX_GUEST_IP": r.SandboxGuestIP, "SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME": r.SandboxBridgeName, "SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR": r.SandboxBridgeCIDR, "SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR": r.SandboxGuestCIDR, "SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX": r.SandboxTapPrefix, "SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR": r.SandboxNetworkStateDir, "SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH": r.NetworkPolicyNFTPath, "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL": r.NetworkPolicyMaxDNSTTL, "SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES": r.NetworkPolicyRunnerAddresses, "SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS": r.NetworkPolicyManagementCIDRs, "SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_GATEWAYS": r.NetworkPolicyRunnerGateways, "SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM": r.NetworkPolicyDNSUpstream, "SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL": r.GuestHeartbeatInterval, "SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS": r.DataPlaneListenAddress, "SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS": r.DataPlaneAdvertisedAddress}
	env["SECONDBOX_RUNNER_LOG_DIR"] = r.LogDirectory
	env["SECONDBOX_RUNNER_CLIENT_CERTIFICATE"] = filepath.Join(r.IdentityDirectory, "runner.crt")
	env["SECONDBOX_RUNNER_CLIENT_KEY"] = filepath.Join(r.IdentityDirectory, "runner.key")
	env["SECONDBOX_RUNNER_CONTROL_PLANE_CA"] = filepath.Join(r.IdentityDirectory, "runner-ca.crt")
	ints := map[string]*int64{"SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_START": r.FirecrackerJailerUIDStart, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_COUNT": r.FirecrackerJailerUIDCount, "SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID": r.FirecrackerJailerGID, "SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION": r.FirecrackerCgroupVersion, "SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT": r.StorageRecoveryPercent, "SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT": r.StorageWarningPercent, "SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT": r.StorageAdmissionDenyPercent, "SECONDBOX_RUNNER_SANDBOX_MAX_VCPUS": r.SandboxMaxVCPUs, "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB": r.SandboxMaxMemoryMiB, "SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB": r.SandboxMaxDiskMiB, "SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB": r.SandboxMemoryBudgetMiB, "SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS": r.NetworkPolicyMaxDNSPins, "SECONDBOX_RUNNER_MAX_CONCURRENT_PER_SANDBOX": r.MaxConcurrentPerSandbox, "SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL": r.MaxConcurrentGlobal, "SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS": r.MaxConcurrentStarts, "SECONDBOX_RUNNER_MAX_CONCURRENT_WORKSPACE_CREATES": r.MaxConcurrentWorkspaceCreates, "SECONDBOX_RUNNER_MAX_CONCURRENT_OPERATIONS_GLOBAL": r.MaxConcurrentOperationsGlobal, "SECONDBOX_RUNNER_FILE_TRANSFER_MAX_BYTES": r.FileTransferMaxBytes, "SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT": r.GuestControlVSockPort, "SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT": r.GuestProtocolVSockPort}
	for name, value := range ints {
		env[name] = strconv.FormatInt(*value, 10)
	}
	env["SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED"] = strconv.FormatBool(*r.FirecrackerAllowUnjailed)
	env["SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_ALLOW_BELOW_1000"] = strconv.FormatBool(*r.FirecrackerJailerUIDAllowLow)
	env["SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE"] = strconv.FormatBool(*r.SandboxDeleteBridge)
	if r.Placement == "same-host" {
		env["SECONDBOX_RUNNER_IDENTITY_HOST_DIR"] = r.IdentityHostDirectory
		env["SECONDBOX_RUNNER_ARTIFACT_HOST_DIR"] = r.ArtifactHostDirectory
		env["SECONDBOX_RUNNER_STATE_HOST_DIR"] = r.StateHostDirectory
		env["SECONDBOX_RUNNER_WORKSPACE_HOST_DIR"] = r.WorkspaceHostDirectory
	}
	return env
}

func validateControlPlaneEnvironment(environment map[string]string) (resultErr error) {
	configValidationMu.Lock()
	defer configValidationMu.Unlock()
	original := make(map[string]string)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "SECONDBOX_") {
			parts := strings.SplitN(entry, "=", 2)
			original[parts[0]] = parts[1]
		}
	}
	defer func() {
		for _, entry := range os.Environ() {
			if strings.HasPrefix(entry, "SECONDBOX_") {
				name := strings.SplitN(entry, "=", 2)[0]
				if err := os.Unsetenv(name); err != nil {
					resultErr = errors.Join(resultErr, fmt.Errorf("restore process environment unset %s: %w", name, err))
				}
			}
		}
		for name, value := range original {
			if err := os.Setenv(name, value); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("restore process environment set %s: %w", name, err))
			}
		}
	}()
	for name := range original {
		if err := os.Unsetenv(name); err != nil {
			return fmt.Errorf("clear process environment %s: %w", name, err)
		}
	}
	for name, value := range environment {
		if strings.HasPrefix(name, "SECONDBOX_") {
			if err := os.Setenv(name, value); err != nil {
				return fmt.Errorf("set process environment %s: %w", name, err)
			}
		}
	}
	_, resultErr = controlconfig.FromEnvironment()
	return resultErr
}

func manifestError(message string, err error) error {
	if err == nil {
		return fmt.Errorf("SecondBox deployment manifest: %s", message)
	}
	return fmt.Errorf("SecondBox deployment manifest: %s: %w", message, err)
}

// SecretFingerprint returns redacted identity evidence for inspect output.
func SecretFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
