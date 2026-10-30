package webconsole

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
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

func TestLoginStartsTelegramOIDCWithPKCE(t *testing.T) {
	server := newTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "https://nodes.example.com/login", nil)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `https://oauth.telegram.org/auth?`) || !strings.Contains(body, `code_challenge`) || !strings.Contains(body, `nonce`) || len(response.Result().Cookies()) == 0 {
		t.Fatalf("login status = %d, cookies = %d, body = %s", response.Code, len(response.Result().Cookies()), response.Body.String())
	}
}

func TestTelegramOIDCCallbackCreatesAllowedSession(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	server.now = func() time.Time { return now }
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "test-key"
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: privateKey}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", keyID))
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := newOIDCLoginAttempt(now)
	if err != nil {
		t.Fatal(err)
	}
	issuer := ""
	var rawIDToken string
	fakeOIDC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jwks":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &privateKey.PublicKey, KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig"}}})
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 3600, "id_token": rawIDToken})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeOIDC.Close()
	issuer = fakeOIDC.URL
	rawIDToken, err = jwt.Signed(signer).Claims(jwt.Claims{Issuer: issuer, Subject: "42", Audience: jwt.Audience{"123456789"}, IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour))}).Claims(map[string]any{"nonce": attempt.Nonce, "id": int64(42), "name": "OIDC Operator"}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	server.oauth2Config.Endpoint.TokenURL = fakeOIDC.URL + "/token"
	server.oidcVerifier = oidc.NewVerifier(issuer, oidc.NewRemoteKeySet(context.Background(), fakeOIDC.URL+"/jwks"), &oidc.Config{ClientID: "123456789"})
	attemptCookie, err := server.signOIDCLoginAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://nodes.example.com/auth/oidc/callback?state="+url.QueryEscape(attempt.State)+"&code=test-code", nil)
	request.AddCookie(&http.Cookie{Name: loginStateCookie, Value: attemptCookie})
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, body = %s", response.Code, response.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			sessionCookie = cookie
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("session cookie was not issued")
	}
	sessionRequest := httptest.NewRequest(http.MethodGet, "https://nodes.example.com/api/session", nil)
	sessionRequest.AddCookie(sessionCookie)
	value, _, ok := server.readSession(sessionRequest)
	if !ok || value.UserID != 42 || value.Name != "OIDC Operator" {
		t.Fatalf("session = %#v, ok = %v", value, ok)
	}
}

func TestTelegramOIDCCallbackAcceptsVerifiedTokenWithoutNonce(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	server.now = func() time.Time { return now }
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "test-key-without-nonce"
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: privateKey}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", keyID))
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := newOIDCLoginAttempt(now)
	if err != nil {
		t.Fatal(err)
	}
	var rawIDToken string
	fakeOIDC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jwks":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &privateKey.PublicKey, KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig"}}})
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 3600, "id_token": rawIDToken})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeOIDC.Close()
	rawIDToken, err = jwt.Signed(signer).Claims(jwt.Claims{Issuer: fakeOIDC.URL, Subject: "42", Audience: jwt.Audience{"123456789"}, IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour))}).Claims(map[string]any{"id": int64(42), "name": "OIDC Operator"}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	server.oauth2Config.Endpoint.TokenURL = fakeOIDC.URL + "/token"
	server.oidcVerifier = oidc.NewVerifier(fakeOIDC.URL, oidc.NewRemoteKeySet(context.Background(), fakeOIDC.URL+"/jwks"), &oidc.Config{ClientID: "123456789"})
	attemptCookie, err := server.signOIDCLoginAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://nodes.example.com/auth/oidc/callback?state="+url.QueryEscape(attempt.State)+"&code=test-code", nil)
	request.AddCookie(&http.Cookie{Name: loginStateCookie, Value: attemptCookie})
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, body = %s", response.Code, response.Body.String())
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
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.JobID == "" {
		t.Fatalf("job body = %s, error = %v", response.Body.String(), err)
	}
	job := waitTestJob(t, server, body.JobID, 42)
	result, resultOK := job.Result.(telegram.NodeActualizeResult)
	if job.Status != jobCompleted || !resultOK || len(result.Addresses) != 1 || result.Addresses[0] != "198.51.100.25" {
		t.Fatalf("completed job = %#v", job)
	}
	jobRequest := httptest.NewRequest(http.MethodGet, "https://nodes.example.com/api/jobs/"+body.JobID, nil)
	jobRequest.AddCookie(cookie)
	jobResponse := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(jobResponse, jobRequest)
	if jobResponse.Code != http.StatusOK || !strings.Contains(jobResponse.Body.String(), `"percent":100`) || !strings.Contains(jobResponse.Body.String(), `"message":"Операция успешно завершена"`) {
		t.Fatalf("job status = %d, body = %s", jobResponse.Code, jobResponse.Body.String())
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
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.JobID == "" {
		t.Fatalf("job body = %s, error = %v", response.Body.String(), err)
	}
	if job := waitTestJob(t, server, body.JobID, 42); job.Status != jobCompleted {
		t.Fatalf("completed job = %#v", job)
	}
	request = httptest.NewRequest(http.MethodPost, "https://nodes.example.com/api/nodes/main/22222222-2222-4222-8222-222222222222/ticket", strings.NewReader(`{"password":""}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", server.csrfToken(cookie.Value))
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	var ticketBody struct {
		Ticket  string `json:"ticket"`
		Managed bool   `json:"managed"`
	}
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &ticketBody) != nil || ticketBody.Ticket == "" || !ticketBody.Managed {
		t.Fatalf("ticket status = %d, body = %s", response.Code, response.Body.String())
	}
	ticket, ok := server.tickets.take(ticketBody.Ticket, 42)
	if !ok || !ticket.Managed || ticket.DeploymentID == "" || len(ticket.Password) != 0 {
		t.Fatalf("managed ticket = %#v, found = %v", ticket, ok)
	}
}

func waitTestJob(t *testing.T, server *Server, token string, userID int64) nodeJob {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		job, ok := server.jobs.get(token, userID)
		if ok && job.Status != jobRunning {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Node job did not finish")
	return nodeJob{}
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
	server, err := New(Config{Addr: ":8081", PublicURL: "https://nodes.example.com", OIDCClientID: "123456789", OIDCClientSecret: strings.Repeat("o", 32), SessionSecret: strings.Repeat("s", 32), SessionTTL: time.Hour, TerminalTimeout: time.Hour, MaxSessions: 2, AllowedUsers: []int64{42}}, provider, finder, client, client)
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
	if input.Progress != nil {
		input.Progress(telegram.NodeActualizeProgress{Step: "firewall", Message: "Проверка firewall", Completed: 2, Total: 4})
	}
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
