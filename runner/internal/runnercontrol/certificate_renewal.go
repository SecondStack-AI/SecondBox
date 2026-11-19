package runnercontrol

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"
)

// runnerCertificateReloader keeps the original private key and identity. Only
// the atomic leaf-file replacement is observed; CA files and keys are not read
// again. Control-plane peer verification remains responsible for its Runner CA.
type runnerCertificateReloader struct {
	path    string
	initial tls.Certificate
	leaf    *x509.Certificate
	issuer  *x509.Certificate
}

func newRunnerCertificateReloader(path string, initial tls.Certificate) (*runnerCertificateReloader, error) {
	leaf, err := x509.ParseCertificate(initial.Certificate[0])
	if err != nil {
		return nil, err
	}
	reload := &runnerCertificateReloader{path: path, initial: initial, leaf: leaf}
	for _, der := range initial.Certificate[1:] {
		issuer, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		if leaf.CheckSignatureFrom(issuer) == nil {
			reload.issuer = issuer
			break
		}
	}
	if reload.issuer == nil && bytes.Equal(leaf.RawSubject, leaf.RawIssuer) && leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil {
		reload.issuer = leaf
	}
	return reload, nil
}

func (reload *runnerCertificateReloader) load() (*tls.Certificate, error) {
	encoded, err := os.ReadFile(reload.path)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner renewed leaf read: %w", err)
	}
	// Reuse the startup private key, even if the on-disk key was replaced.
	certificate := reload.initial
	certificate.Certificate = nil
	encoded = bytes.TrimSpace(encoded)
	for len(encoded) > 0 {
		block, rest := pem.Decode(encoded)
		if block == nil {
			return nil, errors.New("SecondBox runner renewed leaf PEM is invalid")
		}
		encoded = bytes.TrimSpace(rest)
		if block.Type != "CERTIFICATE" {
			return nil, errors.New("SecondBox runner renewed leaf PEM contains a non-certificate")
		}
		certificate.Certificate = append(certificate.Certificate, block.Bytes)
	}
	if len(certificate.Certificate) == 0 {
		return nil, errors.New("SecondBox runner renewed leaf is empty")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, err
	}
	previous := reload.leaf
	if !bytes.Equal(previous.RawSubjectPublicKeyInfo, leaf.RawSubjectPublicKeyInfo) || !bytes.Equal(previous.RawSubject, leaf.RawSubject) || !bytes.Equal(previous.RawIssuer, leaf.RawIssuer) || !bytes.Equal(previous.AuthorityKeyId, leaf.AuthorityKeyId) || !reflect.DeepEqual(previous.URIs, leaf.URIs) || !reflect.DeepEqual(previous.DNSNames, leaf.DNSNames) || !reflect.DeepEqual(previous.IPAddresses, leaf.IPAddresses) || !reflect.DeepEqual(previous.EmailAddresses, leaf.EmailAddresses) || !reflect.DeepEqual(previous.ExtKeyUsage, leaf.ExtKeyUsage) || leaf.KeyUsage != previous.KeyUsage {
		return nil, errors.New("SecondBox runner renewed leaf changed its pinned key, identity or issuer")
	}
	if reload.issuer != nil {
		if err := reload.issuer.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
			return nil, fmt.Errorf("SecondBox runner renewed leaf changed signing authority: %w", err)
		}
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, errors.New("SecondBox runner renewed leaf is outside its validity interval")
	}
	certificate.Leaf = leaf
	return &certificate, nil
}
