package remnawave

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestAdvancedBalancerConfigControlsSendThroughByIPMode(t *testing.T) {
	oneIP := AdvancedBalancerConfig("VLESS_TCP_REALITY_TEST", "edge.example.com", "private", nil)
	oneDirect := oneIP["outbounds"].([]any)[0].(map[string]any)
	if _, exists := oneDirect["sendThrough"]; exists {
		t.Fatalf("single-IP config unexpectedly contains sendThrough: %#v", oneDirect)
	}
	egress := netip.MustParseAddr("2.2.2.2")
	twoIP := AdvancedBalancerConfig("VLESS_TCP_REALITY_TEST", "edge.example.com", "private", &egress)
	twoDirect := twoIP["outbounds"].([]any)[0].(map[string]any)
	if got := twoDirect["sendThrough"]; got != "2.2.2.2" {
		t.Fatalf("dual-IP sendThrough = %#v", got)
	}
}

func TestCreateConfigProfileRejectsDotsInName(t *testing.T) {
	client := &Client{}
	if _, err := client.CreateConfigProfile(context.Background(), CreateConfigProfileInput{Name: "gmb.wey-land.net", Config: map[string]any{}}); err == nil {
		t.Fatal("CreateConfigProfile accepted a dotted name")
	}
}

func TestAdvancedAPIContractPathsAndPayloads(t *testing.T) {
	const squadUUID = "77777777-7777-4777-8777-777777777777"
	const hostUUID = "99999999-9999-4999-8999-999999999999"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/system/tools/x25519/generate":
			fmt.Fprint(w, `{"response":{"keypairs":[{"privateKey":"private","publicKey":"public"}]}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/config-profiles":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["name"] != "Balancer-test" || body["config"] == nil {
				t.Errorf("config profile body = %#v, %v", body, err)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"response":{"uuid":"`+testProfileUUID+`","name":"Balancer-test","config":{},"inbounds":[{"uuid":"`+testInboundUUID+`","tag":"VLESS_TCP_REALITY_TEST"}]}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/internal-squads":
			fmt.Fprint(w, `{"response":{"internalSquads":[{"uuid":"`+squadUUID+`","name":"Default","inbounds":[]}]}}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/internal-squads":
			var body updateInternalSquadRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UUID != squadUUID || len(body.Inbounds) != 1 || body.Inbounds[0] != testInboundUUID {
				t.Errorf("squad body = %#v, %v", body, err)
			}
			fmt.Fprint(w, `{"response":{"uuid":"`+squadUUID+`","name":"Default","inbounds":[{"uuid":"`+testInboundUUID+`","tag":"VLESS_TCP_REALITY_TEST"}]}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/hosts":
			var body createHostRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.IsHidden || body.Address != "edge.example.com" || body.Inbound.ConfigProfileUUID != testProfileUUID || len(body.Nodes) != 1 || body.Nodes[0] != testNodeUUID {
				t.Errorf("host body = %#v, %v", body, err)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"response":{"uuid":"`+hostUUID+`","remark":"edge.example.com","address":"edge.example.com","isDisabled":false,"isHidden":true,"tags":["BALANCER_2"],"nodes":["`+testNodeUUID+`"],"inbound":{"configProfileUuid":"`+testProfileUUID+`","configProfileInboundUuid":"`+testInboundUUID+`"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)
	ctx := context.Background()
	pair, err := client.GenerateX25519(ctx)
	if err != nil || pair.PrivateKey != "private" {
		t.Fatalf("GenerateX25519() = %#v, %v", pair, err)
	}
	profile, err := client.CreateConfigProfile(ctx, CreateConfigProfileInput{Name: "Balancer-test", Config: map[string]any{"inbounds": []any{}}})
	if err != nil || profile.UUID != testProfileUUID || len(profile.Inbounds) != 1 {
		t.Fatalf("CreateConfigProfile() = %#v, %v", profile, err)
	}
	squad, err := client.AddInboundToInternalSquad(ctx, squadUUID, testInboundUUID)
	if err != nil || len(squad.Inbounds) != 1 {
		t.Fatalf("AddInboundToInternalSquad() = %#v, %v", squad, err)
	}
	host, err := client.CreateHost(ctx, CreateHostInput{Remark: "edge.example.com", Address: "edge.example.com", Port: 443, Tag: "BALANCER_2", ConfigProfileUUID: testProfileUUID, InboundUUID: testInboundUUID, NodeUUID: testNodeUUID})
	if err != nil || host.UUID != hostUUID || !host.IsHidden {
		t.Fatalf("CreateHost() = %#v, %v", host, err)
	}
}
