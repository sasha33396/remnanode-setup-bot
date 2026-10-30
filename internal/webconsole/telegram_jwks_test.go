package webconsole

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestTelegramRS256KeySetIgnoresUnsupportedCurve(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "oidc-1"
	publicKey, err := json.Marshal(jose.JSONWebKey{Key: &privateKey.PublicKey, KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[{"alg":"ES256K","kty":"EC","crv":"secp256k1","kid":"unsupported"},`))
		_, _ = w.Write(publicKey)
		_, _ = w.Write([]byte(`]}`))
	}))
	defer server.Close()

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: privateKey}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", keyID))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rawToken, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer: server.URL, Subject: "42", Audience: jwt.Audience{"client-id"},
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour)),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	verifier := oidc.NewVerifier(server.URL, newTelegramRS256KeySet(server.URL), &oidc.Config{ClientID: "client-id"})
	if _, err := verifier.Verify(context.Background(), rawToken); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
}
