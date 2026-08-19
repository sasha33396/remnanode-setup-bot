package certmanager

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/acme"

	"remnanode-setup-bot/internal/certificates"
)

type DNSChallengeProvider interface {
	Present(context.Context, string, string) (func(context.Context) error, error)
	WaitPropagation(context.Context, string, string) error
}

type ACMEIssuer struct {
	client *acme.Client
	email  string
	dns    DNSChallengeProvider

	accountMu    sync.Mutex
	accountReady bool
}

func NewACMEIssuer(directoryURL, email string, accountKey crypto.Signer, dns DNSChallengeProvider) (*ACMEIssuer, error) {
	if strings.TrimSpace(directoryURL) == "" || strings.TrimSpace(email) == "" || accountKey == nil || dns == nil {
		return nil, ErrInvalidInput
	}
	return &ACMEIssuer{
		client: &acme.Client{Key: accountKey, DirectoryURL: strings.TrimSpace(directoryURL), UserAgent: "remnanode-setup-bot/1"},
		email:  strings.TrimSpace(email), dns: dns,
	}, nil
}

func (i *ACMEIssuer) Issue(ctx context.Context, sni string) (certificates.Material, error) {
	domain, err := canonicalSNI(sni)
	if err != nil {
		return certificates.Material{}, err
	}
	if err := i.ensureAccount(ctx); err != nil {
		return certificates.Material{}, safe("ACME account registration failed", ErrIssuanceFailed)
	}
	order, err := i.client.AuthorizeOrder(ctx, []acme.AuthzID{{Type: "dns", Value: domain}})
	if err != nil {
		return certificates.Material{}, safe("ACME certificate order failed", ErrIssuanceFailed)
	}
	for _, authorizationURL := range order.AuthzURLs {
		authorization, err := i.client.GetAuthorization(ctx, authorizationURL)
		if err != nil {
			return certificates.Material{}, safe("ACME authorization lookup failed", ErrIssuanceFailed)
		}
		if authorization.Status == acme.StatusValid {
			continue
		}
		challenge := dnsChallenge(authorization)
		if challenge == nil {
			return certificates.Material{}, safe("ACME server did not offer a DNS-01 challenge", ErrIssuanceFailed)
		}
		value, err := i.client.DNS01ChallengeRecord(challenge.Token)
		if err != nil {
			return certificates.Material{}, safe("ACME DNS-01 challenge value could not be generated", ErrIssuanceFailed)
		}
		challengeName := "_acme-challenge." + domain
		cleanup, err := i.dns.Present(ctx, challengeName, value)
		if err != nil {
			return certificates.Material{}, safe(SafeMessage(err, "ACME DNS-01 challenge could not be presented"), ErrIssuanceFailed)
		}
		authorizationErr := func() error {
			defer func() { _ = cleanup(context.WithoutCancel(ctx)) }()
			if err := i.dns.WaitPropagation(ctx, challengeName, value); err != nil {
				return err
			}
			if _, err := i.client.Accept(ctx, challenge); err != nil {
				return err
			}
			_, err := i.client.WaitAuthorization(ctx, authorizationURL)
			return err
		}()
		if authorizationErr != nil {
			return certificates.Material{}, safe(SafeMessage(authorizationErr, "ACME DNS-01 authorization failed"), ErrIssuanceFailed)
		}
	}
	order, err = i.client.WaitOrder(ctx, order.URI)
	if err != nil {
		return certificates.Material{}, safe("ACME certificate order did not become ready", ErrIssuanceFailed)
	}
	certificateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return certificates.Material{}, ErrIssuanceFailed
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{domain}}, certificateKey)
	if err != nil {
		return certificates.Material{}, ErrIssuanceFailed
	}
	derChain, _, err := i.client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil || len(derChain) == 0 {
		return certificates.Material{}, safe("ACME certificate finalization failed", ErrIssuanceFailed)
	}
	fullchain := make([]byte, 0)
	for _, der := range derChain {
		fullchain = append(fullchain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(certificateKey)
	if err != nil {
		clear(fullchain)
		return certificates.Material{}, ErrIssuanceFailed
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	clear(privateDER)
	return certificates.Material{FullchainPEM: fullchain, PrivateKeyPEM: privatePEM}, nil
}

func (i *ACMEIssuer) ensureAccount(ctx context.Context) error {
	i.accountMu.Lock()
	defer i.accountMu.Unlock()
	if i.accountReady {
		return nil
	}
	_, err := i.client.Register(ctx, &acme.Account{Contact: []string{"mailto:" + i.email}}, acme.AcceptTOS)
	if err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return err
	}
	i.accountReady = true
	return nil
}

func dnsChallenge(authorization *acme.Authorization) *acme.Challenge {
	for _, challenge := range authorization.Challenges {
		if challenge.Type == "dns-01" {
			return challenge
		}
	}
	return nil
}

// LoadOrCreateAccountKey persists the central ACME account key with mode 0600.
// It is never copied to Nodes.
func LoadOrCreateAccountKey(path string) (crypto.Signer, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, ErrInvalidInput
	}
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, safe("ACME account key permissions could not be protected", ErrStorageFailed)
	}
	if value, err := os.ReadFile(path); err == nil {
		defer clear(value)
		return parseAccountKey(value)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, safe("ACME account key is unavailable", ErrStorageFailed)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, safe("ACME account key directory is unavailable", ErrStorageFailed)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, safe("ACME account key directory permissions could not be protected", ErrStorageFailed)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, safe("ACME account key could not be generated", ErrStorageFailed)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, safe("ACME account key could not be encoded", ErrStorageFailed)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	clear(der)
	defer clear(encoded)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			return nil, safe("ACME account key permissions could not be protected", ErrStorageFailed)
		}
		value, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, safe("ACME account key is unavailable", ErrStorageFailed)
		}
		defer clear(value)
		return parseAccountKey(value)
	}
	if err != nil {
		return nil, safe("ACME account key could not be created", ErrStorageFailed)
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return nil, safe("ACME account key could not be written", ErrStorageFailed)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, safe("ACME account key could not be synchronized", ErrStorageFailed)
	}
	if err := file.Close(); err != nil {
		return nil, safe("ACME account key could not be closed", ErrStorageFailed)
	}
	return key, nil
}

func parseAccountKey(value []byte) (crypto.Signer, error) {
	block, rest := pem.Decode(value)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, safe("ACME account key is invalid", ErrStorageFailed)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, safe("ACME account key is invalid", ErrStorageFailed)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, safe("ACME account key is invalid", ErrStorageFailed)
	}
	return signer, nil
}

var _ Issuer = (*ACMEIssuer)(nil)
