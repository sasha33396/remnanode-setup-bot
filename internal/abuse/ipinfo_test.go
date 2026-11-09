package abuse

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

func TestIPInfoLiteLookup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lite/8.8.8.8" || r.URL.Query().Get("token") != "secret" {
			t.Errorf("request = %s", r.URL.RequestURI())
		}
		fmt.Fprint(w, `{"ip":"8.8.8.8","asn":"AS15169","as_name":"Google LLC","as_domain":"google.com"}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL + "/")
	client, err := newIPInfoClient(base, "secret", "lite", &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.Lookup(context.Background(), netip.MustParseAddr("8.8.8.8"))
	if err != nil || info.ASN != 15169 || info.Provider != "Google LLC" || info.Category != "UNKNOWN" {
		t.Fatalf("Lookup() = %#v, %v", info, err)
	}
}

func TestIPInfoCorePrefersAnonymousCategory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ip":"1.1.1.1","as":{"asn":"AS13335","name":"Cloudflare, Inc.","type":"hosting"},"is_hosting":true,"is_anonymous":true}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL + "/")
	client, _ := newIPInfoClient(base, "secret", "core", &http.Client{Timeout: time.Second})
	info, err := client.Lookup(context.Background(), netip.MustParseAddr("1.1.1.1"))
	if err != nil || info.ASN != 13335 || info.Category != "PROXY" {
		t.Fatalf("Lookup() = %#v, %v", info, err)
	}
}

func TestIPInfoErrorsDoNotExposeToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) }))
	defer server.Close()
	base, _ := url.Parse(server.URL + "/")
	client, _ := newIPInfoClient(base, "must-not-leak", "lite", &http.Client{Timeout: time.Second})
	_, err := client.Lookup(context.Background(), netip.MustParseAddr("8.8.8.8"))
	if err == nil || err.Error() != "IPinfo API returned HTTP 401" {
		t.Fatalf("error = %v", err)
	}
}
