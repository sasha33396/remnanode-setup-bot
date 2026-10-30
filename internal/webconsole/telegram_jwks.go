package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/go-jose/go-jose/v4"
)

// telegramRS256KeySet ignores JWKS entries for algorithms that this service
// does not accept. Telegram currently publishes an ES256K key alongside its
// RSA key, and go-jose rejects the entire document when it encounters the
// unsupported secp256k1 curve.
type telegramRS256KeySet struct {
	url string

	mu      sync.RWMutex
	keys    []jose.JSONWebKey
	fetchMu sync.Mutex
}

func newTelegramRS256KeySet(url string) *telegramRS256KeySet {
	return &telegramRS256KeySet{url: url}
}

func (s *telegramRS256KeySet) VerifySignature(ctx context.Context, rawJWT string) ([]byte, error) {
	jws, err := jose.ParseSigned(rawJWT, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return nil, fmt.Errorf("parse RS256 JWT: %w", err)
	}
	keyID := ""
	if len(jws.Signatures) > 0 {
		keyID = jws.Signatures[0].Header.KeyID
	}
	if payload, ok := verifyWithTelegramKeys(jws, s.cachedKeys(), keyID); ok {
		return payload, nil
	}

	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	// Another request may have refreshed the cache while this request waited.
	if payload, ok := verifyWithTelegramKeys(jws, s.cachedKeys(), keyID); ok {
		return payload, nil
	}
	keys, err := s.fetch(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
	if payload, ok := verifyWithTelegramKeys(jws, keys, keyID); ok {
		return payload, nil
	}
	return nil, errors.New("no Telegram RS256 key could verify ID token")
}

func (s *telegramRS256KeySet) cachedKeys() []jose.JSONWebKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]jose.JSONWebKey(nil), s.keys...)
}

func (s *telegramRS256KeySet) fetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("create Telegram JWKS request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Telegram JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch Telegram JWKS: unexpected HTTP status %s", resp.Status)
	}
	var document struct {
		Keys []json.RawMessage `json:"keys"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode Telegram JWKS: %w", err)
	}
	keys := make([]jose.JSONWebKey, 0, len(document.Keys))
	for _, rawKey := range document.Keys {
		var metadata struct {
			Algorithm string `json:"alg"`
			KeyType   string `json:"kty"`
		}
		if json.Unmarshal(rawKey, &metadata) != nil || metadata.Algorithm != string(jose.RS256) || metadata.KeyType != "RSA" {
			continue
		}
		var key jose.JSONWebKey
		if json.Unmarshal(rawKey, &key) != nil || !key.Valid() || !key.IsPublic() {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, errors.New("Telegram JWKS contains no valid RS256 public keys")
	}
	return keys, nil
}

func verifyWithTelegramKeys(jws *jose.JSONWebSignature, keys []jose.JSONWebKey, keyID string) ([]byte, bool) {
	for i := range keys {
		if keyID != "" && keys[i].KeyID != keyID {
			continue
		}
		payload, err := jws.Verify(&keys[i])
		if err == nil {
			return payload, true
		}
	}
	return nil, false
}
