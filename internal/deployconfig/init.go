package deployconfig

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const developmentBundleDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

func integer(value int64) *int64 { return &value }
func boolean(value bool) *bool   { return &value }

// InitDevelopment creates one complete reviewed loopback deployment. It never
// replaces an existing path or artifact.
func InitDevelopment(directory string) (string, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", manifestError("resolve initialization directory", err)
	}
	if _, err := os.Lstat(absolute); err == nil {
		return "", manifestError("initialization target must not exist; secondbox-deploy creates the deployment directory itself", nil)
	} else if !os.IsNotExist(err) {
		return "", manifestError("inspect initialization target", err)
	}
	parent := filepath.Dir(absolute)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", manifestError("initialization parent must be an existing non-symbolic-link directory", err)
	}
	if err := os.Mkdir(absolute, 0o700); err != nil {
		return "", manifestError("create initialization directory", err)
	}
	created := true
	defer func() {
		if created {
			_ = os.RemoveAll(absolute)
		}
	}()
	secrets := filepath.Join(absolute, "secrets")
	if err := os.Mkdir(secrets, 0o700); err != nil {
		return "", err
	}
	randomSecret := func(name string, bytes int) (string, error) {
		data := make([]byte, bytes)
		if _, err := rand.Read(data); err != nil {
			return "", err
		}
		path := filepath.Join(secrets, name)
		if err := writeAtomic(path, []byte(hex.EncodeToString(data)+"\n"), 0o600, false); err != nil {
			return "", err
		}
		return "secrets/" + name, nil
	}
	postgresPassword, err := randomSecret("postgres-password", 32)
	if err != nil {
		return "", err
	}
	objectAccess, err := randomSecret("object-store-access-key", 16)
	if err != nil {
		return "", err
	}
	objectSecret, err := randomSecret("object-store-secret-key", 32)
	if err != nil {
		return "", err
	}
	platformToken, err := randomSecret("platform-token", 32)
	if err != nil {
		return "", err
	}
	runnerCredential, err := randomSecret("runner-enrollment-credential", 48)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(filepath.Join(secrets, "application-authorities.json"), []byte("[]\n"), 0o600, false); err != nil {
		return "", err
	}
	pkiDirectory := filepath.Join(secrets, "runner-pki")
	if err := os.Mkdir(pkiDirectory, 0o700); err != nil {
		return "", err
	}
	if err := generateRunnerPKI(pkiDirectory, "control-plane", 825); err != nil {
		return "", err
	}
	asset := `{"assets":[{"artifactId":"secondbox-development-bootstrap","manifestDigest":"` + developmentBundleDigest + `","signatureKeyId":"secondbox-development-local-trust","architecture":"amd64","guestProtocolGeneration":1,"mandatoryGuestFeatures":[]}]}` + "\n"
	if err := writeAtomic(filepath.Join(secrets, "development-signed-assets.json"), []byte(asset), 0o600, false); err != nil {
		return "", err
	}
	manifest := developmentManifest(postgresPassword, objectAccess, objectSecret, platformToken, runnerCredential)
	encoded, err := encodeManifest(manifest)
	if err != nil {
		return "", err
	}
	manifestPath := filepath.Join(absolute, "secondbox.toml")
	if err := writeAtomic(manifestPath, encoded, 0o600, false); err != nil {
		return "", err
	}
	created = false
	return manifestPath, nil
}

func developmentManifest(postgresPassword, objectAccess, objectSecret, platformToken, runnerCredential string) ManifestV1 {
	return ManifestV1{SchemaVersion: 1, Deployment: Deployment{Mode: "development", PublicBaseURL: "http://127.0.0.1:8080", TLSTermination: "development-loopback", ControlPlaneImage: "secondbox-control-plane:development", RunnerImage: "secondbox-runner:development", PostgresImage: "docker.io/library/postgres:18.4-bookworm", ObjectStoreImage: "docker.io/rustfs/rustfs:1.0.0-beta.11", ObjectStoreClientImage: "quay.io/minio/mc:RELEASE.2025-08-13T08-35-41Z", APIBindIP: "127.0.0.1", APIPublishedPort: integer(8080), ListenAddress: "0.0.0.0:8080", RunnerBindIP: "127.0.0.1", RunnerPublishedPort: integer(9443), RunnerListenAddress: "0.0.0.0:9443", LogPath: "/var/log/secondbox/control-plane.jsonl", SignedAssetCatalog: "secrets/development-signed-assets.json", SignedAssetCatalogPath: "/etc/secondbox/signed-assets.json", DevelopmentWaitSeconds: integer(180)}, Database: Database{Mode: "bundled", BindIP: "127.0.0.1", PublishedPort: integer(5432), Name: "secondbox", User: "secondbox", PasswordFile: postgresPassword}, ObjectStore: ObjectStore{Mode: "bundled", Endpoint: "http://object-store:9000", Bucket: "secondbox-development", Region: "us-east-1", UsePathStyle: boolean(true), TempDirectory: "/tmp", AccessKeyFile: objectAccess, SecretKeyFile: objectSecret, BindIP: "127.0.0.1", PublishedPort: integer(9000), ConsolePublishedPort: integer(9001)}, RunnerTrust: RunnerTrust{EnrollmentCredentialFile: runnerCredential, CACertificateFile: "secrets/runner-pki/runner-ca.crt", CAPrivateKeyFile: "secrets/runner-pki/runner-ca.key", ServerCertificateFile: "secrets/runner-pki/server.crt", ServerPrivateKeyFile: "secrets/runner-pki/server.key", ServerName: "control-plane", CertificateLifetimeDays: integer(825)}, Applications: Applications{PlatformTokenFile: platformToken, ApplicationAuthoritiesFile: "secrets/application-authorities.json"}, Policy: Policy{DataPlaneRetentionSeconds: integer(86400), DataPlanePollIntervalMilliseconds: integer(250), RunnerCommandPollIntervalMilliseconds: integer(250), RunnerEnabledFeatures: "exec-streaming,file-streaming,pty,evidence,local-workspace,port-proxy", DefaultSubjectMaxSandboxes: integer(100), DefaultSubjectMaxActiveInstances: integer(20), DefaultSubjectMaxCPUMillis: integer(80000), DefaultSubjectMaxMemoryBytes: integer(171798691840), DefaultSubjectMaxArtifactBytes: integer(1099511627776), DefaultSubjectMaxSnapshots: integer(500), DefaultSubjectMaxArtifacts: integer(5000), DefaultSubjectMaxPortSessions: integer(100), DefaultSubjectMaxConcurrentOperations: integer(20), AgentCompartmentPool: "secondbox-local", AgentCompartmentRuntimeBundleDigest: developmentBundleDigest, AgentCompartmentToolchainBundleDigest: developmentBundleDigest, CodingEnvironmentPool: "secondbox-local", CodingEnvironmentRuntimeBundleDigest: developmentBundleDigest, CodingEnvironmentToolchainBundleDigest: developmentBundleDigest}}
}

func encodeManifest(manifest ManifestV1) ([]byte, error) {
	encoded, err := toml.Marshal(manifest)
	if err != nil {
		return nil, manifestError("encode", err)
	}
	const policyHeader = "[policy]\ndata_plane_retention_seconds = "
	const documentedPolicyHeader = "[policy]\n# " + dataPlaneRetentionHelp + "\ndata_plane_retention_seconds = "
	if !bytes.Contains(encoded, []byte(policyHeader)) {
		return nil, manifestError("encode retention policy help", nil)
	}
	encoded = bytes.Replace(encoded, []byte(policyHeader), []byte(documentedPolicyHeader), 1)
	var help strings.Builder
	help.WriteString("\n# Optional tuning overrides. Uncomment only an intentional override.\n# [overrides]\n")
	for _, definition := range OverrideRegistry() {
		fmt.Fprintf(&help, "# %s = %s # default; %s\n", definition.TOMLName, definition.Default, definition.Help)
	}
	return append(encoded, []byte(help.String())...), nil
}

func generateRunnerPKI(directory, serverName string, lifetimeDays int64) error {
	caKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	caSerial, err := randomSerial()
	if err != nil {
		return err
	}
	caTemplate := &x509.Certificate{SerialNumber: caSerial, Subject: pkix.Name{CommonName: "SecondBox Runner CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return err
	}
	serverSerial, err := randomSerial()
	if err != nil {
		return err
	}
	serverTemplate := &x509.Certificate{SerialNumber: serverSerial, Subject: pkix.Name{CommonName: serverName}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Duration(lifetimeDays) * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(serverName); ip != nil {
		serverTemplate.IPAddresses = []net.IP{ip}
	} else {
		serverTemplate.DNSNames = []string{serverName}
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	files := map[string]struct {
		data []byte
		mode os.FileMode
	}{"runner-ca.crt": {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644}, "runner-ca.key": {pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)}), 0o600}, "server.crt": {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), 0o644}, "server.key": {pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)}), 0o600}}
	for name, file := range files {
		if err := writeAtomic(filepath.Join(directory, name), file.data, file.mode, false); err != nil {
			return err
		}
	}
	return nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("SecondBox deployment manifest certificate serial generation failed: %w", err)
	}
	return serial, nil
}

// InitProduction writes an annotated, intentionally incomplete shape and
// reports all unresolved decision groups in one error.
func InitProduction(directory string) (string, error) {
	return initProduction(directory, func(path string, content []byte) error {
		return writeAtomic(path, content, 0o600, false)
	})
}

func initProduction(directory string, writeSkeleton func(string, []byte) error) (string, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(absolute); err == nil {
		return "", manifestError("production initialization target must not exist; secondbox-deploy creates the deployment directory itself", nil)
	} else if !os.IsNotExist(err) {
		return "", manifestError("inspect production initialization target", err)
	}
	parentInfo, err := os.Lstat(filepath.Dir(absolute))
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return "", manifestError("production initialization parent must be an existing non-symbolic-link directory", err)
	}
	if err := os.Mkdir(absolute, 0o700); err != nil {
		return "", manifestError("create production initialization directory", err)
	}
	created := true
	defer func() {
		if created {
			_ = os.RemoveAll(absolute)
		}
	}()
	skeleton := []byte("schema_version = 1\n\n# Production initialization is incomplete. Supply all eight decision groups:\n# deployment, database, object_store, runners, execution_asset_trust,\n# applications, tenancy_policy, lifecycle_policy.\n")
	path := filepath.Join(absolute, "secondbox.toml")
	if err := writeSkeleton(path, skeleton); err != nil {
		return "", err
	}
	created = false
	return path, manifestError("production initialization unresolved decision groups: deployment, database, object store, Runner topology, execution-asset trust, application authorities, tenancy policy, lifecycle policy", nil)
}

// InitProductionFromManifest is the non-interactive automation path. It
// validates a complete production input and materializes a create-only manifest
// whose local source references are absolute, so moving it into the protected
// deployment directory cannot change their meaning.
func InitProductionFromManifest(sourcePath, directory string) (string, error) {
	manifest, err := ReadManifest(sourcePath)
	if err != nil {
		return "", err
	}
	if manifest.Deployment.Mode != "production" {
		return "", manifestError("production automation input must select production mode", nil)
	}
	sourceAbsolute, _ := filepath.Abs(sourcePath)
	sourceBase := filepath.Dir(sourceAbsolute)
	absoluteReference := func(reference string) string {
		if reference == "" || filepath.IsAbs(reference) {
			return reference
		}
		return filepath.Clean(filepath.Join(sourceBase, reference))
	}
	manifest.Deployment.SignedAssetCatalog = absoluteReference(manifest.Deployment.SignedAssetCatalog)
	manifest.Database.URLFile = absoluteReference(manifest.Database.URLFile)
	manifest.Database.PasswordFile = absoluteReference(manifest.Database.PasswordFile)
	manifest.ObjectStore.AccessKeyFile = absoluteReference(manifest.ObjectStore.AccessKeyFile)
	manifest.ObjectStore.SecretKeyFile = absoluteReference(manifest.ObjectStore.SecretKeyFile)
	manifest.RunnerTrust.EnrollmentCredentialFile = absoluteReference(manifest.RunnerTrust.EnrollmentCredentialFile)
	manifest.RunnerTrust.CACertificateFile = absoluteReference(manifest.RunnerTrust.CACertificateFile)
	manifest.RunnerTrust.CAPrivateKeyFile = absoluteReference(manifest.RunnerTrust.CAPrivateKeyFile)
	manifest.RunnerTrust.ServerCertificateFile = absoluteReference(manifest.RunnerTrust.ServerCertificateFile)
	manifest.RunnerTrust.ServerPrivateKeyFile = absoluteReference(manifest.RunnerTrust.ServerPrivateKeyFile)
	manifest.Applications.PlatformTokenFile = absoluteReference(manifest.Applications.PlatformTokenFile)
	manifest.Applications.ApplicationAuthoritiesFile = absoluteReference(manifest.Applications.ApplicationAuthoritiesFile)
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(absolute); err == nil {
		return "", manifestError("production initialization target must not exist; secondbox-deploy creates the deployment directory itself", nil)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	parentInfo, err := os.Lstat(filepath.Dir(absolute))
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return "", manifestError("production initialization parent must be an existing non-symbolic-link directory", err)
	}
	if err := os.Mkdir(absolute, 0o700); err != nil {
		return "", err
	}
	created := true
	defer func() {
		if created {
			_ = os.RemoveAll(absolute)
		}
	}()
	encoded, err := encodeManifest(manifest)
	if err != nil {
		return "", err
	}
	target := filepath.Join(absolute, "secondbox.toml")
	if err := writeAtomic(target, encoded, 0o600, false); err != nil {
		return "", err
	}
	if _, err := Resolve(target); err != nil {
		return "", err
	}
	created = false
	return target, nil
}
