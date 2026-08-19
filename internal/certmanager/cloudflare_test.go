package certmanager

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCloudflareDNSReportsAuthenticationFailureSafely(t *testing.T) {
	const token = "protected-cloudflare-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization header = %q", got)
		}
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"secret provider detail"}]}`))
	}))
	defer server.Close()

	provider := newTestCloudflareDNS(t, server.URL, token)
	_, err := provider.Present(context.Background(), "_acme-challenge.edge.example.com", "challenge")
	if !errors.Is(err, ErrIssuanceFailed) {
		t.Fatalf("Present() error = %v, want ErrIssuanceFailed", err)
	}
	if got, want := SafeMessage(err, "fallback"), "Cloudflare API authentication failed (HTTP 401)"; got != want {
		t.Fatalf("safe message = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "secret provider detail") {
		t.Fatalf("error leaked protected detail: %q", err)
	}
}

func TestCloudflareDNSReportsProviderCodeWithoutProviderMessage(t *testing.T) {
	const token = "protected-cloudflare-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"success":false,"result":[],"errors":[{"code":9109,"message":"protected provider detail"}]}`))
	}))
	defer server.Close()

	provider := newTestCloudflareDNS(t, server.URL, token)
	_, err := provider.Present(context.Background(), "_acme-challenge.edge.example.com", "challenge")
	if got, want := SafeMessage(err, "fallback"), "Cloudflare rejected DNS zone lookup (code 9109)"; got != want {
		t.Fatalf("safe message = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), "protected provider detail") {
		t.Fatalf("error leaked provider message: %q", err)
	}
}

func TestSafeMessageRejectsArbitraryErrors(t *testing.T) {
	if got := SafeMessage(errors.New("password=protected"), "safe fallback"); got != "safe fallback" {
		t.Fatalf("SafeMessage() = %q", got)
	}
}

func newTestCloudflareDNS(t *testing.T, rawURL, token string) *CloudflareDNS {
	t.Helper()
	base, err := url.Parse(rawURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := newCloudflareDNS(base, token, time.Second, time.Second, time.Millisecond, &net.Resolver{})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}
