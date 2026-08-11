package remnawave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testToken       = "test-bearer-token"
	testNodeUUID    = "3cb91cb7-b5dc-47da-a9c7-422c9a4cf47f"
	testProfileUUID = "a60d725f-17e9-4a50-9242-3dc223d5a0c9"
	testInboundUUID = "c55c6980-ac8e-4c6a-8579-dd3df8a29891"
)

func TestClientNormalResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Error("Authorization header mismatch")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/hosts":
			fmt.Fprint(w, `{"response":[{"uuid":"f22f0c53-22e7-4562-a14b-4f7f5f5adf35","remark":"Germany","address":"de.example.com","sni":"ignored.example.com","isDisabled":false,"inbound":{"configProfileUuid":"`+testProfileUUID+`","configProfileInboundUuid":"`+testInboundUUID+`"}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/keygen":
			fmt.Fprint(w, `{"response":{"pubKey":"generated-secret-key"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/nodes":
			fmt.Fprint(w, `{"response":[`+nodeJSON(testNodeUUID, "node-1", "192.0.2.10", true)+`]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/nodes/"+testNodeUUID:
			fmt.Fprint(w, `{"response":`+nodeJSON(testNodeUUID, "node-1", "192.0.2.10", true)+`}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/nodes/"+testNodeUUID:
			fmt.Fprint(w, `{"response":{"isDeleted":true}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)
	ctx := context.Background()

	hosts, err := client.GetHosts(ctx)
	if err != nil || len(hosts) != 1 || hosts[0].Address != "de.example.com" {
		t.Fatalf("GetHosts() = %#v, %v", hosts, err)
	}
	key, err := client.GenerateSecretKey(ctx)
	if err != nil || key != "generated-secret-key" {
		t.Fatalf("GenerateSecretKey() = %q, %v", key, err)
	}
	nodes, err := client.GetNodes(ctx)
	if err != nil || len(nodes) != 1 || !nodes[0].IsConnected {
		t.Fatalf("GetNodes() = %#v, %v", nodes, err)
	}
	node, err := client.GetNode(ctx, testNodeUUID)
	if err != nil || node.UUID != testNodeUUID || node.LastStatusChange == nil {
		t.Fatalf("GetNode() = %#v, %v", node, err)
	}
	deleted, err := client.DeleteNode(ctx, testNodeUUID)
	if err != nil || !deleted {
		t.Fatalf("DeleteNode() = %t, %v", deleted, err)
	}
}

func TestCreateNodeUsesContractMapping(t *testing.T) {
	var postCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/nodes" {
			fmt.Fprint(w, `{"response":[]}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/nodes" {
			http.NotFound(w, r)
			return
		}
		postCalls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["name"] != "manual-node" || body["address"] != "192.0.2.20" || body["port"] != float64(2222) {
			t.Errorf("create body identity = %#v", body)
		}
		profile, ok := body["configProfile"].(map[string]any)
		if !ok || profile["activeConfigProfileUuid"] != testProfileUUID {
			t.Errorf("configProfile = %#v", body["configProfile"])
		}
		inbounds, ok := profile["activeInbounds"].([]any)
		if !ok || len(inbounds) != 1 || inbounds[0] != testInboundUUID {
			t.Errorf("activeInbounds = %#v", profile["activeInbounds"])
		}
		if _, exists := body["sni"]; exists {
			t.Error("CreateNodeRequestDto must not contain an invented sni field")
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"response":`+nodeJSON(testNodeUUID, "manual-node", "192.0.2.20", false)+`}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, time.Second)
	sni := "must-not-be-used.example.com"
	node, err := client.CreateNode(context.Background(), CreateNodeInput{
		Name:    "manual-node",
		Address: netip.MustParseAddr("192.0.2.20"),
		Host: Host{
			Address: "de.example.com",
			SNI:     &sni,
			Inbound: HostInbound{
				ConfigProfileUUID:        stringPointer(testProfileUUID),
				ConfigProfileInboundUUID: stringPointer(testInboundUUID),
			},
		},
	})
	if err != nil || node.Name != "manual-node" || postCalls.Load() != 1 {
		t.Fatalf("CreateNode() = %#v, %v; post calls = %d", node, err, postCalls.Load())
	}
}

func TestUpdateNodeAddressSendsMinimalContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/nodes" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 2 || body["uuid"] != testNodeUUID || body["address"] != "198.51.100.20" {
			t.Errorf("PATCH body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"response":`+nodeJSON(testNodeUUID, "node-1", "198.51.100.20", true)+`}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)
	node, err := client.UpdateNodeAddress(context.Background(), UpdateNodeAddressInput{UUID: testNodeUUID, Address: netip.MustParseAddr("198.51.100.20")})
	if err != nil || node.Address != "198.51.100.20" {
		t.Fatalf("UpdateNodeAddress() = %#v, %v", node, err)
	}
}

func TestClientAuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)

	_, err := client.GetHosts(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("GetHosts() error = %v, want ErrUnauthorized", err)
	}
	if containsSecret(err, testToken) {
		t.Fatal("authorization error leaked bearer token")
	}
}

func TestClientInvalidResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed JSON", `{"response":`},
		{"missing response", `{}`},
		{"incomplete host", `{"response":[{"uuid":"f22f0c53-22e7-4562-a14b-4f7f5f5adf35"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, time.Second)

			_, err := client.GetHosts(context.Background())
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("GetHosts() error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestGetNodeRejectsInvalidStatusTimestamp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"response":{"uuid":"`+testNodeUUID+`","name":"node-1","address":"192.0.2.10","isConnected":false,"isDisabled":false,"isConnecting":true,"lastStatusChange":"not-a-date","lastStatusMessage":null}}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)

	_, err := client.GetNode(context.Background(), testNodeUUID)
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("GetNode() error = %v, want ErrInvalidResponse", err)
	}
}

func TestClientTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		fmt.Fprint(w, `{"response":[]}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, 20*time.Millisecond)

	_, err := client.GetNodes(context.Background())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetNodes() error = %v, want context deadline exceeded", err)
	}
}

func TestCreateNodeStopsOnDuplicate(t *testing.T) {
	var postCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			postCalls.Add(1)
		}
		fmt.Fprint(w, `{"response":[`+nodeJSON(testNodeUUID, "duplicate", "192.0.2.10", false)+`]}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)

	_, err := client.CreateNode(context.Background(), CreateNodeInput{
		Name:    "duplicate",
		Address: netip.MustParseAddr("192.0.2.10"),
		Host: Host{
			Address: "de.example.com",
			Inbound: HostInbound{
				ConfigProfileUUID:        stringPointer(testProfileUUID),
				ConfigProfileInboundUUID: stringPointer(testInboundUUID),
			},
		},
	})
	if !errors.Is(err, ErrDuplicateNode) {
		t.Fatalf("CreateNode() error = %v, want ErrDuplicateNode", err)
	}
	if postCalls.Load() != 0 {
		t.Fatalf("POST calls = %d, want 0", postCalls.Load())
	}
}

func newTestClient(t *testing.T, baseURL string, timeout time.Duration) *Client {
	t.Helper()
	client, err := NewClient(baseURL, testToken, timeout)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func nodeJSON(uuid, name, address string, connected bool) string {
	return fmt.Sprintf(`{"uuid":%q,"name":%q,"address":%q,"port":2222,"isConnected":%t,"isDisabled":false,"isConnecting":false,"lastStatusChange":"2026-08-07T12:00:00Z","lastStatusMessage":"ok","configProfile":{"activeConfigProfileUuid":%q,"activeInbounds":[{"uuid":%q}]}}`, uuid, name, address, connected, testProfileUUID, testInboundUUID)
}

func stringPointer(value string) *string { return &value }

func containsSecret(err error, secret string) bool {
	return err != nil && strings.Contains(err.Error(), secret)
}
