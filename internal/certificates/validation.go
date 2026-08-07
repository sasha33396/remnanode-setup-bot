package certificates

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"strings"
	"time"
)

var ErrInvalidMaterial = errors.New("certificate material is invalid")

type Readiness string

const (
	ReadinessUnknown  Readiness = "unknown"
	ReadinessReady    Readiness = "ready"
	ReadinessNotReady Readiness = "not_ready"
)

// Info is safe certificate metadata. It never contains certificate or key
// bytes and is suitable for persistence and operator output.
type Info struct {
	Fingerprint string
	Serial      string
	IssuedAt    time.Time
	ExpiresAt   time.Time
}

// Validate verifies the complete leaf/key/SAN/time contract and returns only
// safe metadata. The fingerprint is SHA-256 over the leaf DER certificate.
func Validate(material Material, hostname string, now time.Time) (Info, error) {
	leaf, err := parseLeaf(material.FullchainPEM)
	if err != nil {
		return Info{}, ErrInvalidMaterial
	}
	privateKey, err := parsePrivateKey(material.PrivateKeyPEM)
	if err != nil {
		return Info{}, ErrInvalidMaterial
	}
	privatePublic, err := x509.MarshalPKIXPublicKey(privateKey.Public())
	if err != nil {
		return Info{}, ErrInvalidMaterial
	}
	certificatePublic, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || !bytes.Equal(privatePublic, certificatePublic) {
		return Info{}, ErrInvalidMaterial
	}
	hostname = strings.TrimSuffix(strings.TrimSpace(hostname), ".")
	if hostname == "" || leaf.VerifyHostname(hostname) != nil {
		return Info{}, ErrInvalidMaterial
	}
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return Info{}, ErrInvalidMaterial
	}
	fingerprint := sha256.Sum256(leaf.Raw)
	return Info{
		Fingerprint: hex.EncodeToString(fingerprint[:]),
		Serial:      leaf.SerialNumber.Text(16),
		IssuedAt:    leaf.NotBefore.UTC(),
		ExpiresAt:   leaf.NotAfter.UTC(),
	}, nil
}

func parseLeaf(fullchain []byte) (*x509.Certificate, error) {
	var leaf *x509.Certificate
	remaining := fullchain
	for len(bytes.TrimSpace(remaining)) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, ErrInvalidMaterial
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, ErrInvalidMaterial
		}
		if leaf == nil {
			leaf = certificate
		}
		remaining = rest
	}
	if leaf == nil {
		return nil, ErrInvalidMaterial
	}
	return leaf, nil
}

func parsePrivateKey(value []byte) (crypto.Signer, error) {
	block, rest := pem.Decode(value)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, ErrInvalidMaterial
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if signer, ok := key.(crypto.Signer); ok {
			return signer, nil
		}
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, ErrInvalidMaterial
}

var (
	_ crypto.Signer = (*rsa.PrivateKey)(nil)
	_ crypto.Signer = (*ecdsa.PrivateKey)(nil)
)
