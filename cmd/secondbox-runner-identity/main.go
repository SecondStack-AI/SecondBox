// secondbox-runner-identity administers the private runner certificate authority.
package main

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/service"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("SecondBox runner identity command is required: create-enrollment, redeem, rotate, or revoke")
	}
	authority, err := loadAuthority(ctx)
	if err != nil {
		return err
	}
	defer authority.Close()
	switch arguments[0] {
	case "create-enrollment":
		return createEnrollment(ctx, authority, arguments[1:])
	case "redeem":
		return redeemEnrollment(ctx, authority, arguments[1:])
	case "rotate":
		return rotateCredential(ctx, authority, arguments[1:])
	case "revoke":
		return revokeCredential(ctx, authority, arguments[1:])
	default:
		return fmt.Errorf("SecondBox runner identity command %q is unsupported", arguments[0])
	}
}

func createEnrollment(
	ctx context.Context,
	authority *runnercontrol.CredentialAuthority,
	arguments []string,
) error {
	flags := flag.NewFlagSet("create-enrollment", flag.ContinueOnError)
	tokenID := flags.String("token-id", "", "unique enrollment token identifier")
	runnerID := flags.String("runner-id", "", "stable Runner identifier")
	poolName := flags.String("pool", "", "enrolled RunnerPool name")
	runnerName := flags.String("name", "", "operator-visible Runner name")
	expiresAtRaw := flags.String("expires-at", "", "RFC3339 enrollment expiry")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("SecondBox create-enrollment received unexpected positional arguments")
	}
	expiresAt, err := time.Parse(time.RFC3339, *expiresAtRaw)
	if err != nil {
		return fmt.Errorf("SecondBox create-enrollment --expires-at: %w", err)
	}
	enrollment, err := authority.CreateEnrollment(ctx, runnercontrol.EnrollmentRequest{
		TokenID: *tokenID, RunnerID: *runnerID, PoolName: *poolName,
		RunnerName: *runnerName, ExpiresAt: expiresAt,
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(enrollment)
}

func redeemEnrollment(
	ctx context.Context,
	authority *runnercontrol.CredentialAuthority,
	arguments []string,
) error {
	flags := flag.NewFlagSet("redeem", flag.ContinueOnError)
	csrPath := flags.String("csr", "", "runner PKCS#10 CSR PEM path")
	outputPath := flags.String("certificate-output", "", "new client certificate PEM path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("SecondBox redeem received unexpected positional arguments")
	}
	token, err := requiredEnvironment("SECONDBOX_RUNNER_ENROLLMENT_TOKEN")
	if err != nil {
		return err
	}
	csr, err := readRequiredFile(*csrPath, "runner CSR")
	if err != nil {
		return err
	}
	issued, err := authority.RedeemEnrollment(ctx, token, csr)
	if err != nil {
		return err
	}
	return writeNewCredential(*outputPath, issued.CertificatePEM)
}

func rotateCredential(
	ctx context.Context,
	authority *runnercontrol.CredentialAuthority,
	arguments []string,
) error {
	flags := flag.NewFlagSet("rotate", flag.ContinueOnError)
	currentPath := flags.String("current-certificate", "", "current runner certificate PEM path")
	csrPath := flags.String("csr", "", "replacement PKCS#10 CSR PEM path")
	outputPath := flags.String("certificate-output", "", "replacement client certificate PEM path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("SecondBox rotate received unexpected positional arguments")
	}
	currentPEM, err := readRequiredFile(*currentPath, "current runner certificate")
	if err != nil {
		return err
	}
	block, remainder := pem.Decode(currentPEM)
	if block == nil || block.Type != "CERTIFICATE" || len(remainder) != 0 {
		return errors.New("SecondBox current runner certificate is invalid PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("SecondBox current runner certificate parse: %w", err)
	}
	identity, err := authority.VerifyClientCertificate(ctx, certificate)
	if err != nil {
		return err
	}
	csr, err := readRequiredFile(*csrPath, "replacement runner CSR")
	if err != nil {
		return err
	}
	issued, err := authority.RotateCredential(ctx, identity, csr)
	if err != nil {
		return err
	}
	return writeNewCredential(*outputPath, issued.CertificatePEM)
}

func revokeCredential(
	ctx context.Context,
	authority *runnercontrol.CredentialAuthority,
	arguments []string,
) error {
	flags := flag.NewFlagSet("revoke", flag.ContinueOnError)
	serial := flags.String("serial", "", "runner certificate serial number")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*serial) == "" {
		return errors.New("SecondBox revoke requires exactly one --serial")
	}
	return authority.RevokeCredential(ctx, *serial)
}

func loadAuthority(ctx context.Context) (*runnercontrol.CredentialAuthority, error) {
	databaseURL, err := requiredEnvironment("SECONDBOX_DATABASE_URL")
	if err != nil {
		return nil, err
	}
	hashSecret, err := requiredEnvironment("SECONDBOX_RUNNER_ENROLLMENT_HASH_SECRET")
	if err != nil {
		return nil, err
	}
	if len(hashSecret) < 32 {
		return nil, errors.New("SecondBox SECONDBOX_RUNNER_ENROLLMENT_HASH_SECRET must contain at least 32 bytes")
	}
	caCertificatePath, err := requiredEnvironment("SECONDBOX_RUNNER_CA_CERTIFICATE")
	if err != nil {
		return nil, err
	}
	caPrivateKeyPath, err := requiredEnvironment("SECONDBOX_RUNNER_CA_PRIVATE_KEY")
	if err != nil {
		return nil, err
	}
	lifetimeRaw, err := requiredEnvironment("SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_SECONDS")
	if err != nil {
		return nil, err
	}
	lifetimeSeconds, err := strconv.ParseInt(lifetimeRaw, 10, 64)
	if err != nil || lifetimeSeconds <= 0 {
		return nil, errors.New("SecondBox SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_SECONDS must be a positive integer")
	}
	verificationTimeoutRaw, err := requiredEnvironment("SECONDBOX_RUNNER_CREDENTIAL_VERIFICATION_TIMEOUT_MILLISECONDS")
	if err != nil {
		return nil, err
	}
	verificationTimeoutMilliseconds, err := strconv.ParseInt(verificationTimeoutRaw, 10, 64)
	if err != nil || verificationTimeoutMilliseconds <= 0 {
		return nil, errors.New("SecondBox SECONDBOX_RUNNER_CREDENTIAL_VERIFICATION_TIMEOUT_MILLISECONDS must be a positive integer")
	}
	caCertificate, caSigner, err := runnercontrol.LoadCertificateAuthority(
		caCertificatePath, caPrivateKeyPath,
	)
	if err != nil {
		return nil, err
	}
	return runnercontrol.NewCredentialAuthority(ctx, runnercontrol.CredentialAuthorityConfig{
		DatabaseURL: databaseURL, EnrollmentHashSecret: []byte(hashSecret),
		CACertificate: caCertificate, CAPrivateKey: caSigner,
		CertificateLifetime:           time.Duration(lifetimeSeconds) * time.Second,
		CredentialVerificationTimeout: time.Duration(verificationTimeoutMilliseconds) * time.Millisecond,
		Now:                           service.SystemClock, NewToken: service.NewCredentialMaterial,
		NewSerial: newCertificateSerial,
	})
}

func requiredEnvironment(name string) (string, error) {
	value, exists := os.LookupEnv(name)
	if !exists || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("SecondBox runner identity missing required environment variable %s", name)
	}
	return value, nil
}

func readRequiredFile(path string, kind string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("SecondBox %s path is required", kind)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("SecondBox %s read: %w", kind, err)
	}
	return contents, nil
}

func writeNewCredential(path string, contents []byte) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("SecondBox certificate output path is required")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("SecondBox runner certificate output create: %w", err)
	}
	_, writeErr := file.Write(contents)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("SecondBox runner certificate output: %w", err)
	}
	return nil
}

func newCertificateSerial() *big.Int {
	maximum := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, maximum)
	if err != nil {
		panic(fmt.Sprintf("SecondBox runner certificate serial generation failed: %v", err))
	}
	if serial.Sign() == 0 {
		return big.NewInt(1)
	}
	return serial
}
