package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/fleet"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
)

func main() {
	endpoint := flag.String("endpoint", "", "HTTPS fleet enrollment origin")
	region := flag.String("region", "", "AWS region")
	caFile := flag.String("ca-file", "", "enrollment server CA PEM file")
	output := flag.String("output", "", "private absolute handoff directory")
	flag.Parse()
	if err := run(*endpoint, *region, *caFile, *output); err != nil {
		fmt.Fprintln(os.Stderr, "SecondBox fleet enrollment failed:", err)
		os.Exit(1)
	}
}

func run(endpoint, region, caFile, output string) error {
	if !fleet.ValidEnrollmentEndpoint(endpoint) || region == "" || !filepath.IsAbs(output) {
		return errors.New("HTTPS endpoint, AWS region and absolute output directory are required")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(output)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("handoff directory must be private and cannot be a symlink")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	awsConfig, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return err
	}
	identity, err := imds.NewFromConfig(awsConfig).GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return errors.New("AWS instance identity could not be read")
	}
	runnerCSR, err := loadEnrollmentCSR(output, "runner.key")
	if err != nil {
		return err
	}
	gatewayCSR, err := loadEnrollmentCSR(output, "gateway.key")
	if err != nil {
		return err
	}
	noncePath := filepath.Join(output, "enrollment.nonce")
	nonce, err := os.ReadFile(noncePath)
	if errors.Is(err, os.ErrNotExist) {
		value := make([]byte, 32)
		if _, err := rand.Read(value); err != nil {
			return err
		}
		nonce = []byte(hex.EncodeToString(value))
		if err := writeEnrollmentFile(noncePath, nonce); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	enrollment := fleet.EnrollmentRequest{InstanceID: identity.InstanceID, Nonce: string(nonce), RunnerCSR: runnerCSR, GatewayCSR: gatewayCSR}
	binding := fleet.EnrollmentBinding(enrollment)
	proofURL := "https://sts." + region + ".amazonaws.com/?Action=GetCallerIdentity&Version=2011-06-15&X-Amz-Expires=60"
	proof, err := http.NewRequestWithContext(ctx, http.MethodGet, proofURL, nil)
	if err != nil {
		return err
	}
	proof.Header.Set("X-SecondBox-Enrollment", binding)
	credentials, err := awsConfig.Credentials.Retrieve(ctx)
	if err != nil {
		return errors.New("AWS instance credentials could not be read")
	}
	empty := sha256.Sum256(nil)
	signed, _, err := v4.NewSigner().PresignHTTP(ctx, credentials, proof, hex.EncodeToString(empty[:]), "sts", region, time.Now(), func(options *v4.SignerOptions) { options.DisableHeaderHoisting = true })
	if err != nil {
		return errors.New("AWS enrollment proof could not be signed")
	}
	enrollment.IdentityProofURL = signed
	encoded, err := json.Marshal(enrollment)
	if err != nil {
		return err
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return errors.New("enrollment CA is invalid")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}}, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("enrollment redirect refused") }}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/fleet/enroll", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("enrollment HTTPS connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("enrollment broker refused: HTTP %d", response.StatusCode)
	}
	var handoff fleet.EnrollmentResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&handoff); err != nil {
		return err
	}
	if handoff.RunnerID != "runner-"+identity.InstanceID || !time.Now().Before(handoff.ExpiresAt) {
		return errors.New("enrollment handoff identity or expiry differs")
	}
	runnerKey, err := os.ReadFile(filepath.Join(output, "runner.key"))
	if err != nil {
		return err
	}
	gatewayKey, err := os.ReadFile(filepath.Join(output, "gateway.key"))
	if err != nil {
		return err
	}
	if _, err := tls.X509KeyPair([]byte(handoff.RunnerCertificate), runnerKey); err != nil {
		return err
	}
	if _, err := tls.X509KeyPair([]byte(handoff.GatewayCertificate), gatewayKey); err != nil {
		return err
	}
	for name, content := range map[string]string{"runner.crt": handoff.RunnerCertificate, "runner-ca.crt": handoff.RunnerCA, "client.crt": handoff.GatewayCertificate, "workload-ca.crt": handoff.GatewayCA, "runner.env": handoff.RunnerEnvironment, "agent-runner-gateway.env": handoff.GatewayEnvironment, "egress-contexts.json": handoff.EgressContexts} {
		if err := writeEnrollmentFile(filepath.Join(output, name), []byte(content)); err != nil {
			return err
		}
	}
	if err := writeRegistryHandoff(filepath.Join(output, "image-registry"), handoff.RegistryFiles); err != nil {
		return err
	}
	return nil
}

func writeRegistryHandoff(directory string, files map[string]string) error {
	if files["tenants.json"] == "" || len(files) > 64 {
		return errors.New("enrollment registry handoff is incomplete")
	}
	total := 0
	for name, content := range files {
		total += len(content)
		if !filepath.IsLocal(name) || filepath.ToSlash(filepath.Clean(name)) != name || strings.Contains(name, "\\") || len(content) > 128<<10 || total > 512<<10 {
			return errors.New("enrollment registry handoff path or bounds are invalid")
		}
		parent := directory
		parts := strings.Split(name, "/")
		for _, part := range append([]string{""}, parts[:len(parts)-1]...) {
			parent = filepath.Join(parent, part)
			if err := os.Mkdir(parent, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err := os.Lstat(parent)
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
				return errors.New("enrollment registry directory is not private")
			}
		}
		if err := writeEnrollmentFile(filepath.Join(directory, name), []byte(content)); err != nil {
			return err
		}
	}
	return nil
}

func loadEnrollmentCSR(directory, name string) (string, error) {
	path := filepath.Join(directory, name)
	encoded, err := os.ReadFile(path)
	var key *rsa.PrivateKey
	if errors.Is(err, os.ErrNotExist) {
		key, err = rsa.GenerateKey(rand.Reader, 3072)
		if err != nil {
			return "", err
		}
		encoded = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		if err := writeEnrollmentFile(path, encoded); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	block, _ := pem.Decode(encoded)
	if block == nil {
		return "", errors.New("enrollment key PEM is invalid")
	}
	key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "secondbox-fleet-bootstrap"}}, key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})), nil
}

func writeEnrollmentFile(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("enrollment existing handoff file is not private")
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(existing, content) {
			return errors.New("enrollment refuses to overwrite a different handoff")
		}
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}
