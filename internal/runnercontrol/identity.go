package runnercontrol

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LoadCertificateAuthority loads an explicit PEM certificate and signing key.
func LoadCertificateAuthority(
	certificatePath string,
	privateKeyPath string,
) (*x509.Certificate, crypto.Signer, error) {
	keyPair, err := tls.LoadX509KeyPair(certificatePath, privateKeyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox runner certificate authority load: %w", err)
	}
	if len(keyPair.Certificate) != 1 {
		return nil, nil, errors.New("SecondBox runner certificate authority requires exactly one certificate")
	}
	certificate, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("SecondBox runner certificate authority parse: %w", err)
	}
	signer, ok := keyPair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("SecondBox runner certificate authority key cannot sign certificates")
	}
	if !certificate.IsCA {
		return nil, nil, errors.New("SecondBox runner certificate authority certificate is not a CA")
	}
	return certificate, signer, nil
}

var (
	ErrRunnerEnrollmentInvalid = errors.New("SecondBox runner enrollment credential is invalid")
	ErrRunnerCredentialRevoked = errors.New("SecondBox runner credential is revoked")
)

// CredentialAuthorityConfig contains explicit runner-only enrollment and certificate authority.
type CredentialAuthorityConfig struct {
	DatabaseURL                   string
	EnrollmentHashSecret          []byte
	CACertificate                 *x509.Certificate
	CAPrivateKey                  crypto.Signer
	CertificateLifetime           time.Duration
	CredentialVerificationTimeout time.Duration
	Now                           func() time.Time
	NewToken                      func() string
	NewSerial                     func() *big.Int
}

// CredentialAuthority issues and verifies runner credentials independently of application keys.
type CredentialAuthority struct {
	pool                          *pgxpool.Pool
	enrollmentHashSecret          []byte
	caCertificate                 *x509.Certificate
	caPrivateKey                  crypto.Signer
	certificateLifetime           time.Duration
	credentialVerificationTimeout time.Duration
	now                           func() time.Time
	newToken                      func() string
	newSerial                     func() *big.Int
}

// EnrollmentRequest deliberately creates one single-use runner bootstrap identity.
type EnrollmentRequest struct {
	TokenID    string
	RunnerID   string
	PoolName   string
	RunnerName string
	ExpiresAt  time.Time
}

// Enrollment contains plaintext bootstrap material returned exactly once.
type Enrollment struct {
	TokenID  string
	RunnerID string
	Token    string
}

// RunnerIdentity is certificate-derived control-plane authority.
type RunnerIdentity struct {
	RunnerID               string
	CredentialSerial       string
	CertificateFingerprint string
}

// IssuedRunnerCertificate is PEM material and its durable identity evidence.
type IssuedRunnerCertificate struct {
	Identity       RunnerIdentity
	CertificatePEM []byte
	NotBefore      time.Time
	NotAfter       time.Time
}

// NewCredentialAuthority connects the runner-only credential authority.
func NewCredentialAuthority(
	ctx context.Context,
	config CredentialAuthorityConfig,
) (*CredentialAuthority, error) {
	if strings.TrimSpace(config.DatabaseURL) == "" ||
		len(config.EnrollmentHashSecret) < 32 ||
		config.CACertificate == nil ||
		config.CAPrivateKey == nil ||
		config.CertificateLifetime <= 0 ||
		config.CredentialVerificationTimeout <= 0 ||
		config.Now == nil ||
		config.NewToken == nil ||
		config.NewSerial == nil {
		return nil, errors.New("SecondBox runner credential authority requires explicit database, CA, lifetime, clock, token, and serial configuration")
	}
	if !config.CACertificate.IsCA {
		return nil, errors.New("SecondBox runner credential authority certificate is not a CA")
	}
	pool, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner credential PostgreSQL pool: %w", err)
	}
	authority := &CredentialAuthority{
		pool: pool, enrollmentHashSecret: append([]byte(nil), config.EnrollmentHashSecret...),
		caCertificate: config.CACertificate, caPrivateKey: config.CAPrivateKey,
		certificateLifetime:           config.CertificateLifetime,
		credentialVerificationTimeout: config.CredentialVerificationTimeout,
		now:                           config.Now,
		newToken:                      config.NewToken, newSerial: config.NewSerial,
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("SecondBox runner credential PostgreSQL readiness: %w", err)
	}
	return authority, nil
}

func (authority *CredentialAuthority) Close() {
	authority.pool.Close()
}

// CreateEnrollment persists one expiring single-use bootstrap token without plaintext.
func (authority *CredentialAuthority) CreateEnrollment(
	ctx context.Context,
	request EnrollmentRequest,
) (Enrollment, error) {
	if strings.TrimSpace(request.TokenID) == "" ||
		strings.TrimSpace(request.RunnerID) == "" ||
		strings.TrimSpace(request.PoolName) == "" ||
		strings.TrimSpace(request.RunnerName) == "" ||
		!request.ExpiresAt.After(authority.now()) {
		return Enrollment{}, errors.New("SecondBox runner enrollment requires identity, pool, name, and future expiry")
	}
	secret := authority.newToken()
	if len(secret) < 32 {
		return Enrollment{}, errors.New("SecondBox runner enrollment token generator returned fewer than 32 bytes")
	}
	token := request.TokenID + "." + secret
	tokenHash := authority.hashToken(token)
	now := authority.now().UTC()
	tx, err := authority.pool.Begin(ctx)
	if err != nil {
		return Enrollment{}, fmt.Errorf("SecondBox runner enrollment transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"secondbox-runner-enrollment\x1f"+request.RunnerID,
	); err != nil {
		return Enrollment{}, fmt.Errorf("SecondBox runner enrollment lock: %w", err)
	}
	var poolState string
	if err := tx.QueryRow(ctx, `
		SELECT state FROM secondbox.runner_pools WHERE name=$1`, request.PoolName,
	).Scan(&poolState); err != nil {
		return Enrollment{}, fmt.Errorf("SecondBox runner enrollment pool lookup: %w", err)
	}
	if poolState != "ready" {
		return Enrollment{}, errors.New("SecondBox RunnerPool is not accepting enrollment")
	}
	emptyJSON := []byte(`{}`)
	emptyListJSON := []byte(`[]`)
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,
			drain_phase,reserved_capacity_json,artifact_cache_json,last_seen_at,revision,
			created_at,updated_at
		) VALUES ($1,$2,$3,'enrolling',$4,$5,$5,$4,0,0,'','',0,'active',$5,$4,NULL,1,$6,$6)`,
		request.RunnerID, request.PoolName, request.RunnerName, emptyListJSON, emptyJSON, now,
	); err != nil {
		return Enrollment{}, fmt.Errorf("SecondBox runner enrollment identity insert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_enrollment_tokens (
			id,runner_id,pool_name,runner_name,token_hash,state,expires_at,
			redeemed_at,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,'active',$6,NULL,$7,$7)`,
		request.TokenID, request.RunnerID, request.PoolName, request.RunnerName,
		tokenHash, request.ExpiresAt.UTC(), now,
	); err != nil {
		return Enrollment{}, fmt.Errorf("SecondBox runner enrollment token insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Enrollment{}, fmt.Errorf("SecondBox runner enrollment commit: %w", err)
	}
	return Enrollment{TokenID: request.TokenID, RunnerID: request.RunnerID, Token: token}, nil
}

// RedeemEnrollment exchanges a single-use runner token and CSR for an mTLS client certificate.
func (authority *CredentialAuthority) RedeemEnrollment(
	ctx context.Context,
	token string,
	certificateRequestPEM []byte,
) (IssuedRunnerCertificate, error) {
	tokenHash := authority.hashToken(token)
	now := authority.now().UTC()
	tx, err := authority.pool.Begin(ctx)
	if err != nil {
		return IssuedRunnerCertificate{}, fmt.Errorf("SecondBox runner enrollment redemption transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var tokenID, runnerID, state string
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT id,runner_id,state,expires_at
		FROM secondbox.runner_enrollment_tokens
		WHERE token_hash=$1 FOR UPDATE`, tokenHash,
	).Scan(&tokenID, &runnerID, &state, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) || state != "active" || !expiresAt.After(now) {
		return IssuedRunnerCertificate{}, ErrRunnerEnrollmentInvalid
	}
	if err != nil {
		return IssuedRunnerCertificate{}, fmt.Errorf("SecondBox runner enrollment token lookup: %w", err)
	}
	issued, err := authority.issueCertificate(runnerID, "", certificateRequestPEM, now)
	if err != nil {
		return IssuedRunnerCertificate{}, err
	}
	if err := insertRunnerCredential(ctx, tx, issued, "", now); err != nil {
		return IssuedRunnerCertificate{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_enrollment_tokens
		SET state='redeemed',redeemed_at=$2,updated_at=$2 WHERE id=$1`,
		tokenID, now,
	); err != nil {
		return IssuedRunnerCertificate{}, fmt.Errorf("SecondBox runner enrollment redemption update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runners SET state='enrolled',revision=revision+1,updated_at=$2 WHERE id=$1`,
		runnerID, now,
	); err != nil {
		return IssuedRunnerCertificate{}, fmt.Errorf("SecondBox runner enrollment identity update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IssuedRunnerCertificate{}, fmt.Errorf("SecondBox runner enrollment redemption commit: %w", err)
	}
	return issued, nil
}

// RotateCredential issues an overlapping replacement and marks the old credential retiring.
func (authority *CredentialAuthority) RotateCredential(
	ctx context.Context,
	current RunnerIdentity,
	certificateRequestPEM []byte,
) (IssuedRunnerCertificate, error) {
	now := authority.now().UTC()
	tx, err := authority.pool.Begin(ctx)
	if err != nil {
		return IssuedRunnerCertificate{}, fmt.Errorf("SecondBox runner credential rotation transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var runnerID, fingerprint, state string
	if err := tx.QueryRow(ctx, `
		SELECT runner_id,certificate_fingerprint_sha256,state FROM secondbox.runner_credentials
		WHERE serial_number=$1 FOR UPDATE`, current.CredentialSerial,
	).Scan(&runnerID, &fingerprint, &state); err != nil {
		return IssuedRunnerCertificate{}, fmt.Errorf("SecondBox runner credential rotation lookup: %w", err)
	}
	if runnerID != current.RunnerID ||
		fingerprint != current.CertificateFingerprint ||
		(state != "active" && state != "retiring") {
		return IssuedRunnerCertificate{}, ErrRunnerCredentialRevoked
	}
	issued, err := authority.issueCertificate(
		runnerID, current.CredentialSerial, certificateRequestPEM, now,
	)
	if err != nil {
		return IssuedRunnerCertificate{}, err
	}
	if err := insertRunnerCredential(ctx, tx, issued, current.CredentialSerial, now); err != nil {
		return IssuedRunnerCertificate{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_credentials
		SET state='retiring',updated_at=$2 WHERE serial_number=$1`,
		current.CredentialSerial, now,
	); err != nil {
		return IssuedRunnerCertificate{}, fmt.Errorf("SecondBox retiring runner credential update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IssuedRunnerCertificate{}, fmt.Errorf("SecondBox runner credential rotation commit: %w", err)
	}
	return issued, nil
}

// RevokeCredential immediately removes one certificate's runner authority.
func (authority *CredentialAuthority) RevokeCredential(
	ctx context.Context,
	serial string,
) error {
	now := authority.now().UTC()
	command, err := authority.pool.Exec(ctx, `
		UPDATE secondbox.runner_credentials
		SET state='revoked',revoked_at=$2,updated_at=$2
		WHERE serial_number=$1 AND state IN ('active','retiring')`,
		serial, now,
	)
	if err != nil {
		return fmt.Errorf("SecondBox runner credential revocation: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrRunnerCredentialRevoked
	}
	return nil
}

// VerifyClientCertificate proves CA trust, intended client use, identity, and live revocation state.
func (authority *CredentialAuthority) VerifyClientCertificate(
	ctx context.Context,
	certificate *x509.Certificate,
) (RunnerIdentity, error) {
	roots := x509.NewCertPool()
	roots.AddCert(authority.caCertificate)
	if _, err := certificate.Verify(x509.VerifyOptions{
		Roots: roots, CurrentTime: authority.now(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return RunnerIdentity{}, fmt.Errorf("SecondBox runner certificate verification: %w", err)
	}
	runnerID, err := runnerIDFromCertificate(certificate)
	if err != nil {
		return RunnerIdentity{}, err
	}
	fingerprint := certificateFingerprint(certificate.Raw)
	var storedRunnerID, state string
	err = authority.pool.QueryRow(ctx, `
		SELECT runner_id,state FROM secondbox.runner_credentials
		WHERE serial_number=$1 AND certificate_fingerprint_sha256=$2`,
		certificate.SerialNumber.String(), fingerprint,
	).Scan(&storedRunnerID, &state)
	if errors.Is(err, pgx.ErrNoRows) || storedRunnerID != runnerID ||
		(state != "active" && state != "retiring") {
		return RunnerIdentity{}, ErrRunnerCredentialRevoked
	}
	if err != nil {
		return RunnerIdentity{}, fmt.Errorf("SecondBox runner credential verification lookup: %w", err)
	}
	return RunnerIdentity{
		RunnerID: runnerID, CredentialSerial: certificate.SerialNumber.String(),
		CertificateFingerprint: fingerprint,
	}, nil
}

// ServerTLSConfig requires a verified runner certificate and checks revocation per handshake.
func (authority *CredentialAuthority) ServerTLSConfig(
	serverCertificate tls.Certificate,
) (*tls.Config, error) {
	if len(serverCertificate.Certificate) == 0 {
		return nil, errors.New("SecondBox runner control server certificate is required")
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(authority.caCertificate)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 {
				return errors.New("SecondBox runner mTLS requires exactly one client certificate")
			}
			ctx, cancel := context.WithTimeout(
				context.Background(), authority.credentialVerificationTimeout,
			)
			defer cancel()
			_, err := authority.VerifyClientCertificate(ctx, state.PeerCertificates[0])
			return err
		},
	}, nil
}

func (authority *CredentialAuthority) issueCertificate(
	runnerID string,
	rotatedFrom string,
	certificateRequestPEM []byte,
	now time.Time,
) (IssuedRunnerCertificate, error) {
	block, remainder := pem.Decode(certificateRequestPEM)
	if block == nil || len(remainder) != 0 || block.Type != "CERTIFICATE REQUEST" {
		return IssuedRunnerCertificate{}, errors.New("SecondBox runner certificate request is invalid PEM")
	}
	request, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return IssuedRunnerCertificate{}, fmt.Errorf("SecondBox runner certificate request parse: %w", err)
	}
	if err := request.CheckSignature(); err != nil {
		return IssuedRunnerCertificate{}, fmt.Errorf("SecondBox runner certificate request signature: %w", err)
	}
	serial := authority.newSerial()
	if serial == nil || serial.Sign() <= 0 {
		return IssuedRunnerCertificate{}, errors.New("SecondBox runner credential serial generator returned an invalid serial")
	}
	identityURI := &url.URL{Scheme: "spiffe", Host: "secondbox", Path: "/runner/" + url.PathEscape(runnerID)}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: runnerID},
		NotBefore: now, NotAfter: now.Add(authority.certificateLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:                  []*url.URL{identityURI},
		BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(
		rand.Reader, template, authority.caCertificate, request.PublicKey, authority.caPrivateKey,
	)
	if err != nil {
		return IssuedRunnerCertificate{}, fmt.Errorf("SecondBox runner certificate signing: %w", err)
	}
	fingerprint := certificateFingerprint(certificateDER)
	return IssuedRunnerCertificate{
		Identity: RunnerIdentity{
			RunnerID: runnerID, CredentialSerial: serial.String(),
			CertificateFingerprint: fingerprint,
		},
		CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		NotBefore:      now, NotAfter: template.NotAfter,
	}, nil
}

func insertRunnerCredential(
	ctx context.Context,
	tx pgx.Tx,
	issued IssuedRunnerCertificate,
	rotatedFrom string,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_credentials (
			serial_number,runner_id,certificate_fingerprint_sha256,state,not_before,
			not_after,rotated_from_serial,revoked_at,created_at,updated_at
		) VALUES ($1,$2,$3,'active',$4,$5,$6,NULL,$7,$7)`,
		issued.Identity.CredentialSerial, issued.Identity.RunnerID,
		issued.Identity.CertificateFingerprint, issued.NotBefore, issued.NotAfter,
		rotatedFrom, now,
	); err != nil {
		return fmt.Errorf("SecondBox runner credential insert: %w", err)
	}
	return nil
}

func (authority *CredentialAuthority) hashToken(token string) []byte {
	hash := hmac.New(sha256.New, authority.enrollmentHashSecret)
	hash.Write([]byte(token))
	return hash.Sum(nil)
}

func certificateFingerprint(certificateDER []byte) string {
	digest := sha256.Sum256(certificateDER)
	return hex.EncodeToString(digest[:])
}

func runnerIDFromCertificate(certificate *x509.Certificate) (string, error) {
	for _, identityURI := range certificate.URIs {
		if identityURI.Scheme != "spiffe" || identityURI.Host != "secondbox" {
			continue
		}
		const prefix = "/runner/"
		if !strings.HasPrefix(identityURI.Path, prefix) {
			continue
		}
		runnerID, err := url.PathUnescape(strings.TrimPrefix(identityURI.Path, prefix))
		if err == nil && runnerID != "" && !strings.Contains(runnerID, "/") {
			return runnerID, nil
		}
	}
	return "", errors.New("SecondBox runner certificate has no valid runner identity URI")
}
