package certmanager

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCloudflareEnsureARecordCreatesDedicatedUnproxiedRecord(t *testing.T) {
	var created bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/zones":
			_, _ = writer.Write([]byte(`{"success":true,"result":[{"id":"zone-id"}],"errors":[]}`))
		case request.Method == http.MethodGet && request.URL.Path == "/zones/zone-id/dns_records":
			_, _ = writer.Write([]byte(`{"success":true,"result":[],"errors":[]}`))
		case request.Method == http.MethodPost && request.URL.Path == "/zones/zone-id/dns_records":
			created = true
			_, _ = writer.Write([]byte(`{"success":true,"result":{"id":"record-id","type":"A","name":"gmb.example.com","content":"1.1.1.1"},"errors":[]}`))
		default:
			t.Fatalf("unexpected Cloudflare request: %s %s", request.Method, request.URL.String())
		}
	}))
	defer server.Close()
	provider := newTestCloudflareDNS(t, server.URL, "token")
	changed, err := provider.EnsureARecord(context.Background(), "gmb.example.com", netip.MustParseAddr("1.1.1.1"))
	if err != nil || !changed || !created {
		t.Fatalf("EnsureARecord() = %v, %v; created=%v", changed, err, created)
	}
}

func TestCloudflareEnsureARecordDoesNotOverwriteConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/zones" {
			_, _ = writer.Write([]byte(`{"success":true,"result":[{"id":"zone-id"}],"errors":[]}`))
			return
		}
		_, _ = writer.Write([]byte(`{"success":true,"result":[{"id":"record-id","type":"A","name":"gmb.example.com","content":"9.9.9.9"}],"errors":[]}`))
	}))
	defer server.Close()
	provider := newTestCloudflareDNS(t, server.URL, "token")
	_, err := provider.EnsureARecord(context.Background(), "gmb.example.com", netip.MustParseAddr("1.1.1.1"))
	if !errors.Is(err, ErrDistributionFailed) {
		t.Fatalf("EnsureARecord() error = %v", err)
	}
}

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
