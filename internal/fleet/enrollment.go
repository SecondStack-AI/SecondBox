package fleet

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type EnrollmentRequest struct {
	InstanceID       string `json:"instanceId"`
	Nonce            string `json:"nonce"`
	RunnerCSR        string `json:"runnerCsrPem"`
	GatewayCSR       string `json:"gatewayCsrPem"`
	IdentityProofURL string `json:"identityProofUrl"`
}

type EnrollmentResponse struct {
	RunnerID           string            `json:"runnerId"`
	RunnerCertificate  string            `json:"runnerCertificatePem"`
	RunnerCA           string            `json:"runnerCertificateAuthorityPem"`
	GatewayCertificate string            `json:"gatewayCertificatePem"`
	GatewayCA          string            `json:"gatewayCertificateAuthorityPem"`
	ExpiresAt          time.Time         `json:"expiresAt"`
	RunnerEnvironment  string            `json:"runnerEnvironment"`
	GatewayEnvironment string            `json:"gatewayEnvironment"`
	EgressContexts     string            `json:"egressContexts"`
	RegistryFiles      map[string]string `json:"registryFiles"`
}

type EnrollmentVerifier interface {
	Verify(context.Context, EnrollmentRequest, string) error
}

type GatewayIssuer interface {
	Issue(context.Context, string, string, time.Duration) (string, string, error)
	Retire(context.Context, string) error
}

type EnrollmentAuthority struct {
	Database           *pgxpool.Pool
	Pool               string
	Verifier           EnrollmentVerifier
	Gateway            GatewayIssuer
	CACertificate      *x509.Certificate
	CAKey              crypto.Signer
	CAPEM              string
	Lifetime           time.Duration
	RunnerEnvironment  string
	GatewayEnvironment string
	EgressContexts     string
	RegistryFiles      map[string]string
	Now                func() time.Time
}

func EnrollmentBinding(request EnrollmentRequest) string {
	digest := sha256.Sum256([]byte(request.InstanceID + "\n" + request.Nonce + "\n" + request.RunnerCSR + "\n" + request.GatewayCSR))
	return hex.EncodeToString(digest[:])
}

func (authority EnrollmentAuthority) Enroll(ctx context.Context, request EnrollmentRequest) (EnrollmentResponse, error) {
	if authority.Database == nil || authority.Verifier == nil || authority.Gateway == nil || authority.CACertificate == nil || authority.CAKey == nil ||
		authority.Pool == "" || authority.Lifetime <= 0 || authority.Lifetime > 24*time.Hour || authority.Now == nil {
		return EnrollmentResponse{}, errors.New("SecondBox fleet enrollment authority is incomplete")
	}
	if len(request.Nonce) != 64 || len(request.RunnerCSR) > 8192 || len(request.GatewayCSR) > 8192 || !strings.HasPrefix(request.InstanceID, "i-") || len(request.InstanceID) > 32 {
		return EnrollmentResponse{}, errors.New("SecondBox fleet enrollment identity or CSR bounds are invalid")
	}
	if _, err := hex.DecodeString(request.Nonce); err != nil {
		return EnrollmentResponse{}, errors.New("SecondBox fleet enrollment nonce is invalid")
	}
	binding := EnrollmentBinding(request)
	if err := authority.Verifier.Verify(ctx, request, binding); err != nil {
		return EnrollmentResponse{}, err
	}
	block, rest := pem.Decode([]byte(request.RunnerCSR))
	if block == nil || len(rest) != 0 || block.Type != "CERTIFICATE REQUEST" {
		return EnrollmentResponse{}, errors.New("SecondBox fleet enrollment Runner CSR is invalid")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	if err := csr.CheckSignature(); err != nil {
		return EnrollmentResponse{}, err
	}
	runnerID := "runner-" + request.InstanceID
	now := authority.Now().UTC()
	expires := now.Add(authority.Lifetime)
	if expires.After(authority.CACertificate.NotAfter) {
		return EnrollmentResponse{}, errors.New("SecondBox fleet enrollment CA expires before requested leaf")
	}
	caPublicKey, err := x509.MarshalPKIXPublicKey(authority.CACertificate.PublicKey)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	signerPublicKey, err := x509.MarshalPKIXPublicKey(authority.CAKey.Public())
	if err != nil {
		return EnrollmentResponse{}, err
	}
	if !authority.CACertificate.IsCA || !bytes.Equal(caPublicKey, signerPublicKey) {
		return EnrollmentResponse{}, errors.New("SecondBox fleet enrollment CA key does not match its certificate")
	}
	tx, err := authority.Database.Begin(ctx)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	defer tx.Rollback(ctx)
	// One immutable public-key binding per cloud instance prevents a new CSR
	// from reusing a provisioned worker's still-valid cloud role session.
	csrHash := sha256.Sum256([]byte(request.RunnerCSR + "\n" + request.GatewayCSR))
	tokenHash := sha256.Sum256([]byte(request.Nonce))
	if _, err := tx.Exec(ctx, `INSERT INTO secondbox.fleet_enrollments(token_sha256,pool_name,runner_id,instance_id,csr_sha256,expires_at)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(runner_id) DO NOTHING`, tokenHash[:], authority.Pool, runnerID, request.InstanceID, csrHash[:], expires); err != nil {
		return EnrollmentResponse{}, err
	}
	var previousHash []byte
	var responseJSON []byte
	var grantPool string
	var grantExpiry time.Time
	if err := tx.QueryRow(ctx, `SELECT pool_name,csr_sha256,expires_at,response_json FROM secondbox.fleet_enrollments WHERE runner_id=$1 FOR UPDATE`, runnerID).Scan(&grantPool, &previousHash, &grantExpiry, &responseJSON); err != nil {
		return EnrollmentResponse{}, err
	}
	if grantPool != authority.Pool || !strings.EqualFold(hex.EncodeToString(previousHash), hex.EncodeToString(csrHash[:])) || !now.Before(grantExpiry) {
		return EnrollmentResponse{}, errors.New("SecondBox fleet enrollment grant is expired or bound to another key")
	}
	var retired bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secondbox.runners WHERE id=$1 AND fleet_state='retired')`, runnerID).Scan(&retired); err != nil {
		return EnrollmentResponse{}, err
	}
	if retired {
		return EnrollmentResponse{}, errors.New("SecondBox fleet enrollment Runner identity is retired")
	}
	var response EnrollmentResponse
	if len(responseJSON) > 0 {
		if err := json.Unmarshal(responseJSON, &response); err != nil {
			return response, err
		}
	} else {
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return response, err
		}
		identity, _ := url.Parse("spiffe://secondbox/runner/" + runnerID)
		leaf := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: runnerID}, NotBefore: now.Add(-time.Minute), NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{identity}}
		certificate, err := x509.CreateCertificate(rand.Reader, leaf, authority.CACertificate, csr.PublicKey, authority.CAKey)
		if err != nil {
			return response, err
		}
		gatewayCertificate, gatewayCA, err := authority.Gateway.Issue(ctx, runnerID, request.GatewayCSR, authority.Lifetime)
		if err != nil {
			return response, err
		}
		response = EnrollmentResponse{RunnerID: runnerID, RunnerCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate})), RunnerCA: authority.CAPEM, GatewayCertificate: gatewayCertificate, GatewayCA: gatewayCA, ExpiresAt: expires}
		encoded, err := json.Marshal(response)
		if err != nil {
			return response, err
		}
		if _, err := tx.Exec(ctx, `UPDATE secondbox.fleet_enrollments SET response_json=$2,used_at=$3 WHERE runner_id=$1`, runnerID, encoded, now); err != nil {
			return response, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return response, err
	}
	response.RunnerEnvironment = strings.ReplaceAll(authority.RunnerEnvironment, "{{RUNNER_ID}}", runnerID)
	response.GatewayEnvironment = strings.ReplaceAll(authority.GatewayEnvironment, "{{RUNNER_ID}}", runnerID)
	response.EgressContexts = strings.ReplaceAll(authority.EgressContexts, "{{RUNNER_ID}}", runnerID)
	response.RegistryFiles = authority.RegistryFiles
	return response, nil
}

func (authority EnrollmentAuthority) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/v1/fleet/enroll" {
		http.NotFound(writer, request)
		return
	}
	defer request.Body.Close()
	var enrollment EnrollmentRequest
	decoder := json.NewDecoder(io.LimitReader(request.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&enrollment); err != nil {
		http.Error(writer, "SecondBox fleet enrollment request is invalid", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		http.Error(writer, "SecondBox fleet enrollment request has trailing data", http.StatusBadRequest)
		return
	}
	response, err := authority.Enroll(request.Context(), enrollment)
	if err != nil {
		slog.Warn("SecondBox fleet enrollment denied", "instanceId", enrollment.InstanceID, "error", err)
		http.Error(writer, "SecondBox fleet enrollment denied", http.StatusForbidden)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		return
	}
}

func LoadEnrollmentSigner(certificatePEM, keyPEM []byte) (*x509.Certificate, crypto.Signer, error) {
	certificateBlock, _ := pem.Decode(certificatePEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certificateBlock == nil || keyBlock == nil {
		return nil, nil, errors.New("SecondBox fleet enrollment PEM is invalid")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	var key any
	if keyBlock.Type == "RSA PRIVATE KEY" {
		key, err = x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	} else {
		key, err = x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	}
	if err != nil {
		return nil, nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("SecondBox fleet enrollment key cannot sign")
	}
	return certificate, signer, nil
}
