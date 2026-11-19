package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/fleet"
)

// Validate every leaf before writing any file. Keys, authority, environment and
// registry grants remain the bootstrap handoff; renewal changes no identity.
func applyRenewalHandoff(directory, gatewayCopy string, handoff fleet.EnrollmentResponse, now time.Time) error {
	if !filepath.IsAbs(gatewayCopy) {
		return errors.New("renewal requires an absolute existing gateway certificate copy")
	}
	runnerCA, err := readPrivateEnrollmentFile(filepath.Join(directory, "runner-ca.crt"))
	if err != nil {
		return err
	}
	gatewayCA, err := readPrivateEnrollmentFile(filepath.Join(directory, "workload-ca.crt"))
	if err != nil {
		return err
	}
	if !bytes.Equal(runnerCA, []byte(handoff.RunnerCA)) || !bytes.Equal(gatewayCA, []byte(handoff.GatewayCA)) {
		return errors.New("renewal refuses a changed certificate authority")
	}
	type leafUpdate struct {
		path, key       string
		leaf, authority []byte
	}
	updates := []leafUpdate{
		{filepath.Join(directory, "runner.crt"), filepath.Join(directory, "runner.key"), []byte(handoff.RunnerCertificate), runnerCA},
		{filepath.Join(directory, "client.crt"), filepath.Join(directory, "gateway.key"), []byte(handoff.GatewayCertificate), gatewayCA},
		{gatewayCopy, filepath.Join(directory, "gateway.key"), []byte(handoff.GatewayCertificate), gatewayCA},
	}
	for _, update := range updates {
		current, err := readPrivateEnrollmentFile(update.path)
		if err != nil {
			return err
		}
		key, err := readPrivateEnrollmentFile(update.key)
		if err != nil {
			return err
		}
		if err := validateRenewedEnrollmentLeaf(current, update.leaf, key, update.authority, now); err != nil {
			return fmt.Errorf("renewal leaf %s: %w", filepath.Base(update.path), err)
		}
	}
	for _, update := range updates {
		if err := replaceEnrollmentCertificate(update.path, update.leaf); err != nil {
			return err
		}
	}
	return nil
}

func readPrivateEnrollmentFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("renewal requires private regular existing identity files")
	}
	return os.ReadFile(path)
}

func validateRenewedEnrollmentLeaf(currentPEM, renewedPEM, keyPEM, authorityPEM []byte, now time.Time) error {
	previous, err := tls.X509KeyPair(currentPEM, keyPEM)
	if err != nil {
		return err
	}
	next, err := tls.X509KeyPair(renewedPEM, keyPEM)
	if err != nil {
		return err
	}
	oldLeaf, err := x509.ParseCertificate(previous.Certificate[0])
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(next.Certificate[0])
	if err != nil {
		return err
	}
	if !bytes.Equal(oldLeaf.RawSubjectPublicKeyInfo, leaf.RawSubjectPublicKeyInfo) || !bytes.Equal(oldLeaf.RawSubject, leaf.RawSubject) || !bytes.Equal(oldLeaf.RawIssuer, leaf.RawIssuer) || !bytes.Equal(oldLeaf.AuthorityKeyId, leaf.AuthorityKeyId) || !reflect.DeepEqual(oldLeaf.URIs, leaf.URIs) || !reflect.DeepEqual(oldLeaf.DNSNames, leaf.DNSNames) || !reflect.DeepEqual(oldLeaf.IPAddresses, leaf.IPAddresses) || !reflect.DeepEqual(oldLeaf.EmailAddresses, leaf.EmailAddresses) || !reflect.DeepEqual(oldLeaf.ExtKeyUsage, leaf.ExtKeyUsage) || oldLeaf.KeyUsage != leaf.KeyUsage || leaf.NotAfter.Before(oldLeaf.NotAfter) {
		return errors.New("renewal refuses changed leaf identity, issuer, key or regressed expiry")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(authorityPEM) {
		return errors.New("renewal authority is invalid")
	}
	intermediates := x509.NewCertPool()
	for _, der := range next.Certificate[1:] {
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			return err
		}
		intermediates.AddCert(certificate)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return err
	}
	return nil
}

// The directory mount, rather than an individual-file bind, makes each rename
// visible to existing containers without restarting active guest processes.
func replaceEnrollmentCertificate(path string, content []byte) (resultErr error) {
	block, _ := pem.Decode(content)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("renewal leaf PEM is invalid")
	}
	previous, err := readPrivateEnrollmentFile(path)
	if err != nil {
		return err
	}
	if bytes.Equal(previous, content) {
		return nil
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".secondbox-leaf-*")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	_, writeErr := file.Write(content)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
