package certmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestReplaceARecordPreservesAttributesAndRetries(t *testing.T) {
	current := "8.8.8.8"
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/zones" {
			fmt.Fprint(w, `{"success":true,"result":[{"id":"zone"}]}`)
			return
		}
		if r.Method == "PATCH" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body) != 1 || body["content"] != "1.1.1.1" {
				t.Errorf("unexpected mutation: %v", body)
			}
			current = "1.1.1.1"
			patches++
			fmt.Fprintf(w, `{"success":true,"result":{"content":%q}}`, current)
			return
		}
		fmt.Fprintf(w, `{"success":true,"result":[{"id":"record","name":"edge.example.com","type":"A","content":%q,"ttl":300}]}`, current)
	}))
	defer server.Close()
	c := newTestCloudflareDNS(t, server.URL, "token")
	old, new := netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")
	if err := c.ReplaceARecord(context.Background(), "edge.example.com", old, new, false); err != nil || patches != 0 {
		t.Fatal("preview mutated DNS", err)
	}
	for i := 0; i < 2; i++ {
		if err := c.ReplaceARecord(context.Background(), "edge.example.com", old, new, true); err != nil {
			t.Fatal(err)
		}
	}
	if patches != 1 {
		t.Fatal("retry patched twice")
	}
	current = "9.9.9.9"
	if err := c.ReplaceARecord(context.Background(), "edge.example.com", old, new, true); err == nil {
		t.Fatal("conflicting record overwritten")
	}
}
