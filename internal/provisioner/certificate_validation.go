package provisioner

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"time"

	"remnanode-setup-bot/internal/certificates"
)

var ErrInvalidCertificateMaterial = errors.New("certificate material is invalid")

func validateCertificateMaterial(material certificates.Material, hostname string, now time.Time) error {
	var leaf *x509.Certificate
	remainingCertificates := material.FullchainPEM
	for len(bytes.TrimSpace(remainingCertificates)) > 0 {
		certificateBlock, rest := pem.Decode(remainingCertificates)
		if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" {
			return ErrInvalidCertificateMaterial
		}
		certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
		if err != nil {
			return ErrInvalidCertificateMaterial
		}
		if leaf == nil {
			leaf = certificate
		}
		remainingCertificates = rest
	}
	if leaf == nil {
		return ErrInvalidCertificateMaterial
	}
	privateKeyBlock, rest := pem.Decode(material.PrivateKeyPEM)
	if privateKeyBlock == nil || len(bytes.TrimSpace(rest)) != 0 {
		return ErrInvalidCertificateMaterial
	}
	privateKey, err := parsePrivateKey(privateKeyBlock.Bytes)
	if err != nil {
		return ErrInvalidCertificateMaterial
	}
	privatePublic, err := x509.MarshalPKIXPublicKey(privateKey.Public())
	if err != nil {
		return ErrInvalidCertificateMaterial
	}
	certificatePublic, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || !bytes.Equal(privatePublic, certificatePublic) {
		return ErrInvalidCertificateMaterial
	}
	if err := leaf.VerifyHostname(hostname); err != nil {
		return ErrInvalidCertificateMaterial
	}
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return ErrInvalidCertificateMaterial
	}
	return nil
}

func parsePrivateKey(der []byte) (crypto.Signer, error) {
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		signer, ok := key.(crypto.Signer)
		if ok {
			return signer, nil
		}
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	return nil, errors.New("unsupported private key")
}

var (
	_ crypto.Signer = (*rsa.PrivateKey)(nil)
	_ crypto.Signer = (*ecdsa.PrivateKey)(nil)
)
