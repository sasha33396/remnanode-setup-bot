package dnsbalancer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testAPIKey = "test-dns-balancer-api-key"

func TestClientGetDomainsAndFindZone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != testAPIKey {
			t.Error("X-API-Key header mismatch")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, domainsJSON([]string{"192.0.2.1"}))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)

	domains, err := client.GetDomains(context.Background())
	if err != nil || len(domains) != 1 || len(domains[0].Zones) != 3 {
		t.Fatalf("GetDomains() = %#v, %v", domains, err)
	}
	match, err := client.FindZone(context.Background(), "de.example.com")
	if err != nil || match.Domain != "example.com" || match.ZoneName != "de" {
		t.Fatalf("FindZone() = %#v, %v", match, err)
	}
	if got := domains[0].Zones[2].Nodes[0].Address; got != "100.64.0.1" {
		t.Errorf("advanced node address = %q", got)
	}
}

func TestAddIPPreservesCompleteList(t *testing.T) {
	var patchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/config/domains":
			fmt.Fprint(w, domainsJSON([]string{"192.0.2.1", "192.0.2.2"}))
		case r.Method == http.MethodPatch && r.URL.Path == "/api/config/domains/example.com/zones/de":
			patchCalls.Add(1)
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode PATCH: %v", err)
			}
			if len(body) != 1 {
				t.Errorf("PATCH fields = %v, want only ips", body)
			}
			var ips []string
			if err := json.Unmarshal(body["ips"], &ips); err != nil {
				t.Errorf("decode PATCH ips: %v", err)
			}
			want := []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}
			if strings.Join(ips, ",") != strings.Join(want, ",") {
				t.Errorf("PATCH ips = %v, want %v", ips, want)
			}
			fmt.Fprint(w, `{"status":"ok"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)

	result, err := client.AddIP(context.Background(), "de.example.com", netip.MustParseAddr("192.0.2.3"))
	if err != nil || !result.Added || len(result.IPs) != 3 || patchCalls.Load() != 1 {
		t.Fatalf("AddIP() = %#v, %v; PATCH calls=%d", result, err, patchCalls.Load())
	}
}

func TestReplaceIPPreservesOtherAddresses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			fmt.Fprint(w, domainsJSON([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}))
			return
		}
		var request patchZoneRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		want := []string{"192.0.2.1", "192.0.2.3", "198.51.100.8"}
		if strings.Join(request.IPs, ",") != strings.Join(want, ",") {
			t.Errorf("replacement IPs = %v, want %v", request.IPs, want)
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)
	result, err := client.ReplaceIP(context.Background(), "de.example.com", netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("198.51.100.8"))
	if err != nil || !result.Changed {
		t.Fatalf("ReplaceIP() = %#v, %v", result, err)
	}
}

func TestFindAndReplaceAdvancedNodeIP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			fmt.Fprint(w, domainsJSON([]string{"198.51.100.1"}))
			return
		}
		var request patchZoneRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.IPs) != 0 || len(request.Nodes) != 1 || request.Nodes[0].IP != "198.51.100.8" || request.Nodes[0].Address != "100.64.0.1" {
			t.Fatalf("advanced PATCH = %#v", request)
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)
	zones, err := client.FindZonesByIP(context.Background(), netip.MustParseAddr("198.51.100.1"))
	if err != nil || len(zones) != 2 {
		t.Fatalf("FindZonesByIP() = %#v, %v", zones, err)
	}
	result, err := client.ReplaceIP(context.Background(), "vpn.example.com", netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("198.51.100.8"))
	if err != nil || !result.Changed {
		t.Fatalf("ReplaceIP() = %#v, %v", result, err)
	}
}

func TestAddIPApexUsesEncodedZoneAndSkipsExistingIP(t *testing.T) {
	var patchRequestURI string
	var patchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			fmt.Fprint(w, domainsJSON([]string{"192.0.2.1"}))
			return
		}
		patchCalls.Add(1)
		patchRequestURI = r.RequestURI
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)

	result, err := client.AddIP(context.Background(), "example.com", netip.MustParseAddr("192.0.2.6"))
	if err != nil || !result.Added || !strings.Contains(patchRequestURI, "/zones/%40") {
		t.Fatalf("apex AddIP() = %#v, %v; URI=%q", result, err, patchRequestURI)
	}
	result, err = client.AddIP(context.Background(), "example.com", netip.MustParseAddr("192.0.2.5"))
	if err != nil || result.Added {
		t.Fatalf("existing AddIP() = %#v, %v", result, err)
	}
	if patchCalls.Load() != 1 {
		t.Fatalf("PATCH calls = %d, want 1", patchCalls.Load())
	}
}

func TestAddIPSkipsIPAlreadyPresentAsAdvancedNode(t *testing.T) {
	var patchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			patchCalls.Add(1)
		}
		fmt.Fprint(w, domainsJSON([]string{"192.0.2.1"}))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)

	result, err := client.AddIP(context.Background(), "vpn.example.com", netip.MustParseAddr("198.51.100.1"))
	if err != nil || result.Added {
		t.Fatalf("AddIP() = %#v, %v; want unchanged", result, err)
	}
	if patchCalls.Load() != 0 {
		t.Fatalf("PATCH calls = %d, want 0", patchCalls.Load())
	}
}

func TestClientKnownHTTPErrors(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusNotFound, ErrNotFound},
		{http.StatusConflict, ErrConflict},
		{http.StatusUnprocessableEntity, ErrUnprocessable},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				fmt.Fprint(w, `{"detail":"safe server detail"}`)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, time.Second)

			_, err := client.GetDomains(context.Background())
			if !errors.Is(err, test.want) {
				t.Fatalf("GetDomains() error = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), testAPIKey) {
				t.Fatal("HTTP error leaked X-API-Key")
			}
		})
	}
}

func TestClientMalformedResponses(t *testing.T) {
	tests := []string{
		`{"domain": "not-an-array"}`,
		`null`,
		`[{"zones":[]}]`,
		`[{"domain":"example.com","zones":[{"name":"de","ttl":60,"proxied":false,"ips":["not-an-ip"]}]}]`,
	}
	for index, body := range tests {
		t.Run(fmt.Sprintf("case_%d", index), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, time.Second)
			_, err := client.GetDomains(context.Background())
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("GetDomains() error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestClientTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, 20*time.Millisecond)

	_, err := client.GetDomains(context.Background())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetDomains() error = %v, want deadline exceeded", err)
	}
}

func TestConcurrentAddIPCannotLoseUpdates(t *testing.T) {
	type state struct {
		sync.Mutex
		ips        []string
		patchSizes []int
	}
	current := &state{ips: []string{"192.0.2.1"}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			current.Lock()
			snapshot := append([]string(nil), current.ips...)
			current.Unlock()
			time.Sleep(25 * time.Millisecond)
			fmt.Fprint(w, domainsJSON(snapshot))
		case http.MethodPatch:
			var body patchZoneRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode concurrent PATCH: %v", err)
			}
			current.Lock()
			current.ips = append([]string(nil), body.IPs...)
			current.patchSizes = append(current.patchSizes, len(body.IPs))
			current.Unlock()
			fmt.Fprint(w, `{"status":"ok"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)

	start := make(chan struct{})
	errorsChannel := make(chan error, 2)
	for _, value := range []string{"192.0.2.2", "192.0.2.3"} {
		ip := netip.MustParseAddr(value)
		go func() {
			<-start
			_, err := client.AddIP(context.Background(), "de.example.com", ip)
			errorsChannel <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errorsChannel; err != nil {
			t.Fatalf("concurrent AddIP() error = %v", err)
		}
	}

	current.Lock()
	finalIPs := append([]string(nil), current.ips...)
	patchSizes := append([]int(nil), current.patchSizes...)
	current.Unlock()
	sort.Strings(finalIPs)
	sort.Ints(patchSizes)
	if got, want := strings.Join(finalIPs, ","), "192.0.2.1,192.0.2.2,192.0.2.3"; got != want {
		t.Fatalf("final IPs = %v, want all updates", finalIPs)
	}
	if got, want := fmt.Sprint(patchSizes), "[2 3]"; got != want {
		t.Fatalf("PATCH list sizes = %v, want %s", patchSizes, want)
	}
}

func newTestClient(t *testing.T, baseURL string, timeout time.Duration) *Client {
	t.Helper()
	client, err := NewClient(baseURL, testAPIKey, timeout, nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func domainsJSON(deIPs []string) string {
	encoded, _ := json.Marshal([]domainWireFixture{
		{
			Domain: "example.com",
			Zones: []zoneWireFixture{
				{Name: "de", TTL: 60, Proxied: false, IPs: deIPs},
				{Name: "@", TTL: 120, Proxied: true, IPs: []string{"192.0.2.5"}},
				{Name: "vpn", TTL: 60, Proxied: false, Nodes: []ZoneNode{{IP: "198.51.100.1", Address: "100.64.0.1"}}},
			},
		},
	})
	return string(encoded)
}

type domainWireFixture struct {
	Domain string            `json:"domain"`
	Zones  []zoneWireFixture `json:"zones"`
}

type zoneWireFixture struct {
	Name    string     `json:"name"`
	TTL     int        `json:"ttl"`
	Proxied bool       `json:"proxied"`
	IPs     []string   `json:"ips,omitempty"`
	Nodes   []ZoneNode `json:"nodes,omitempty"`
}
