package webconsole

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/repository"
	sshclient "remnanode-setup-bot/internal/ssh"
	"remnanode-setup-bot/internal/telegram"
)

func TestNodeInventoryClassifiesManagedAndLegacyNodes(t *testing.T) {
	server := newTestServer(t)
	recorder := httptest.NewRecorder()
	if err := server.issueSessionCookie(recorder, session{UserID: 42, Name: "Operator"}); err != nil {
		t.Fatal(err)
	}
	cookie := recorder.Result().Cookies()[0]

	request := httptest.NewRequest(http.MethodGet, "https://nodes.example.com/api/nodes", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body struct {
		Nodes []nodeResponse `json:"nodes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Nodes) != 2 || !body.Nodes[0].Managed || body.Nodes[1].Managed {
		t.Fatalf("inventory body = %s, error = %v", response.Body.String(), err)
	}
}

func TestLoginRendersTelegramWidget(t *testing.T) {
	server := newTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "https://nodes.example.com/login", nil)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `data-telegram-login="node_setup_bot"`) || len(response.Result().Cookies()) == 0 {
		t.Fatalf("login status = %d, cookies = %d, body = %s", response.Code, len(response.Result().Cookies()), response.Body.String())
	}
}

func TestActualizeNodeRequiresSessionCSRFAndReturnsInventory(t *testing.T) {
	server := newTestServer(t)
	recorder := httptest.NewRecorder()
	if err := server.issueSessionCookie(recorder, session{UserID: 42, Name: "Operator"}); err != nil {
		t.Fatal(err)
	}
	cookie := recorder.Result().Cookies()[0]
	request := httptest.NewRequest(http.MethodPost, "https://nodes.example.com/api/nodes/main/22222222-2222-4222-8222-222222222222/actualize", strings.NewReader(`{"password":"temporary"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", server.csrfToken(cookie.Value))
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "198.51.100.25") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestLegacySSHTicketAdoptsNodeAndUsesManagedCredentials(t *testing.T) {
	server := newTestServer(t)
	recorder := httptest.NewRecorder()
	if err := server.issueSessionCookie(recorder, session{UserID: 42, Name: "Operator"}); err != nil {
		t.Fatal(err)
	}
	cookie := recorder.Result().Cookies()[0]
	request := httptest.NewRequest(http.MethodPost, "https://nodes.example.com/api/nodes/main/22222222-2222-4222-8222-222222222222/ticket", strings.NewReader(`{"password":"temporary"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", server.csrfToken(cookie.Value))
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body struct {
		Ticket  string `json:"ticket"`
		Managed bool   `json:"managed"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Ticket == "" || !body.Managed {
		t.Fatalf("ticket body = %s, error = %v", response.Body.String(), err)
	}
	ticket, ok := server.tickets.take(body.Ticket, 42)
	if !ok || !ticket.Managed || ticket.DeploymentID == "" || len(ticket.Password) != 0 {
		t.Fatalf("managed ticket = %#v, found = %v", ticket, ok)
	}
}

func TestBulkActualizeManagedNodes(t *testing.T) {
	server := newTestServer(t)
	recorder := httptest.NewRecorder()
	if err := server.issueSessionCookie(recorder, session{UserID: 42, Name: "Operator"}); err != nil {
		t.Fatal(err)
	}
	cookie := recorder.Result().Cookies()[0]
	request := httptest.NewRequest(http.MethodPost, "https://nodes.example.com/api/nodes/actualize-managed", nil)
	request.Header.Set("X-CSRF-Token", server.csrfToken(cookie.Value))
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"updated":1`) || !strings.Contains(response.Body.String(), `"firewallChanged":1`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	client, err := sshclient.NewClient(sshclient.NewMemoryHostKeyStore(), signer, time.Second, time.Second, 1024, 1024)
	if err != nil {
		t.Fatal(err)
	}
	finder := &staticDeployments{managed: map[string]string{"11111111-1111-4111-8111-111111111111": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}}
	provider := &staticNodes{deployments: finder, nodes: []telegram.NodeSummary{
		{PanelID: "main", PanelName: "Main", UUID: "11111111-1111-4111-8111-111111111111", Name: "Managed", Address: "192.0.2.10", Connected: true},
		{PanelID: "main", PanelName: "Main", UUID: "22222222-2222-4222-8222-222222222222", Name: "Legacy", Address: "192.0.2.11"},
	}}
	server, err := New(Config{Addr: ":8081", PublicURL: "https://nodes.example.com", BotToken: "token", BotUsername: "node_setup_bot", SessionSecret: strings.Repeat("s", 32), SessionTTL: time.Hour, TerminalTimeout: time.Hour, MaxSessions: 2, AllowedUsers: []int64{42}}, provider, finder, client, client)
	if err != nil {
		t.Fatal(err)
	}
	server.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	return server
}

type staticNodes struct {
	nodes       []telegram.NodeSummary
	deployments *staticDeployments
}

func (s *staticNodes) ListNodes(context.Context) ([]telegram.NodeSummary, error) { return s.nodes, nil }

func (s *staticNodes) ActualizeNode(_ context.Context, input telegram.NodeActualizeInput) (telegram.NodeActualizeResult, error) {
	s.deployments.managed[input.NodeUUID] = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	return telegram.NodeActualizeResult{NodeName: "Legacy", Managed: true, Adopted: true, Addresses: []string{"198.51.100.25"}, FirewallChanged: true}, nil
}

func (s *staticNodes) ActualizeManagedNodes(context.Context) (telegram.ManagedNodesActualizeResult, error) {
	return telegram.ManagedNodesActualizeResult{Total: 1, Updated: 1, FirewallChanged: 1}, nil
}

type staticDeployments struct{ managed map[string]string }

func (s *staticDeployments) FindDeploymentByPanelNodeUUID(_ context.Context, _ string, uuid string) (deployment.Deployment, error) {
	if id := s.managed[uuid]; id != "" {
		return deployment.Deployment{ID: id}, nil
	}
	return deployment.Deployment{}, repository.ErrNotFound
}
