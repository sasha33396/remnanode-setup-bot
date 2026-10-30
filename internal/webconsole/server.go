// Package webconsole exposes an authenticated node inventory and browser SSH
// terminal for operators already allowed to use the Telegram bot.
package webconsole

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/repository"
	sshclient "remnanode-setup-bot/internal/ssh"
	"remnanode-setup-bot/internal/telegram"
)

//go:embed static/*
var assets embed.FS

type NodeProvider interface {
	ListNodes(context.Context) ([]telegram.NodeSummary, error)
}

type NodeActualizer interface {
	ActualizeNode(context.Context, telegram.NodeActualizeInput) (telegram.NodeActualizeResult, error)
	ActualizeManagedNodes(context.Context) (telegram.ManagedNodesActualizeResult, error)
}

type DeploymentFinder interface {
	FindDeploymentByPanelNodeUUID(context.Context, string, string) (deployment.Deployment, error)
}

type Config struct {
	Addr             string
	PublicURL        string
	OIDCClientID     string
	OIDCClientSecret string
	SessionSecret    string
	SessionTTL       time.Duration
	TerminalTimeout  time.Duration
	MaxSessions      int
	AllowedUsers     []int64
	Logger           *slog.Logger
}

type Server struct {
	config       Config
	nodes        NodeProvider
	actualizer   NodeActualizer
	deployments  DeploymentFinder
	managedSSH   *sshclient.Client
	legacySSH    *sshclient.Client
	allowedUsers map[int64]struct{}
	sessionKey   []byte
	publicURL    *url.URL
	login        *template.Template
	oauth2Config oauth2.Config
	oidcVerifier *oidc.IDTokenVerifier
	tickets      *ticketStore
	jobs         *nodeJobStore
	jobContext   context.Context
	cancelJobs   context.CancelFunc
	workers      sync.WaitGroup
	slots        chan struct{}
	now          func() time.Time
	http         *http.Server
}

func New(config Config, nodes NodeProvider, deployments DeploymentFinder, managedSSH, legacySSH *sshclient.Client) (*Server, error) {
	actualizer, _ := nodes.(NodeActualizer)
	if config.Addr == "" || config.PublicURL == "" || strings.TrimSpace(config.OIDCClientID) == "" || strings.TrimSpace(config.OIDCClientSecret) == "" || len(config.SessionSecret) < 32 || config.SessionTTL <= 0 || config.TerminalTimeout <= 0 || config.MaxSessions <= 0 || len(config.AllowedUsers) == 0 || nodes == nil || deployments == nil || managedSSH == nil || legacySSH == nil {
		return nil, errors.New("invalid web console configuration")
	}
	publicURL, err := url.Parse(config.PublicURL)
	if err != nil || publicURL.Scheme != "https" || publicURL.Host == "" {
		return nil, errors.New("invalid web console public URL")
	}
	login, err := template.ParseFS(assets, "static/login.html")
	if err != nil {
		return nil, errors.New("parse web console login template")
	}
	key := sha256.Sum256([]byte(config.SessionSecret))
	server := &Server{
		config: config, nodes: nodes, actualizer: actualizer, deployments: deployments, managedSSH: managedSSH, legacySSH: legacySSH,
		allowedUsers: make(map[int64]struct{}, len(config.AllowedUsers)), sessionKey: key[:], publicURL: publicURL,
		login: login, slots: make(chan struct{}, config.MaxSessions), now: time.Now,
	}
	server.oauth2Config = oauth2.Config{
		ClientID: config.OIDCClientID, ClientSecret: config.OIDCClientSecret,
		RedirectURL: strings.TrimRight(config.PublicURL, "/") + "/auth/oidc/callback",
		Endpoint:    oauth2.Endpoint{AuthURL: "https://oauth.telegram.org/auth", TokenURL: "https://oauth.telegram.org/token", AuthStyle: oauth2.AuthStyleInHeader},
		Scopes:      []string{oidc.ScopeOpenID, "profile"},
	}
	keySet := newTelegramRS256KeySet("https://oauth.telegram.org/.well-known/jwks.json")
	server.oidcVerifier = oidc.NewVerifier("https://oauth.telegram.org", keySet, &oidc.Config{ClientID: config.OIDCClientID})
	if server.config.Logger == nil {
		server.config.Logger = slog.Default()
	}
	for _, id := range config.AllowedUsers {
		if id > 0 {
			server.allowedUsers[id] = struct{}{}
		}
	}
	server.tickets = newTicketStore(func() time.Time { return server.now() })
	server.jobs = newNodeJobStore(func() time.Time { return server.now() })
	server.jobContext, server.cancelJobs = context.WithCancel(context.Background())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", server.handleIndex)
	mux.HandleFunc("GET /login", server.handleLogin)
	mux.HandleFunc("GET /auth/oidc/callback", server.handleOIDCCallback)
	mux.HandleFunc("GET /api/session", server.handleSession)
	mux.HandleFunc("GET /api/nodes", server.handleNodes)
	mux.HandleFunc("GET /api/jobs/", server.handleJob)
	mux.HandleFunc("GET /api/options/panels", server.handlePanels)
	mux.HandleFunc("GET /api/options/capabilities", server.handleCapabilities)
	mux.HandleFunc("GET /api/options/hosts", server.handleHosts)
	mux.HandleFunc("GET /api/options/balancer", server.handleBalancerOptions)
	mux.HandleFunc("GET /api/deployments", server.handleDeployments)
	mux.HandleFunc("GET /api/deployments/", server.handleDeploymentRead)
	mux.HandleFunc("POST /api/deployments/", server.handleDeploymentAction)
	mux.HandleFunc("GET /api/nodes/", server.handleNodeOperationPreview)
	mux.HandleFunc("POST /api/nodes/", server.handleNodeAction)
	mux.HandleFunc("GET /ws/terminal", server.handleTerminal)
	mux.HandleFunc("POST /logout", server.handleLogout)
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, errors.New("load web console assets")
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(staticFS))))
	server.http = &http.Server{Addr: config.Addr, Handler: server.securityHeaders(mux), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute}
	return server, nil
}

func (s *Server) Run(ctx context.Context) error {
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.cancelJobs()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = s.http.Shutdown(shutdownCtx)
		case <-stopped:
		}
	}()
	err := s.http.ListenAndServe()
	close(stopped)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) startNodeJob(userID int64, panelID, nodeUUID string, password []byte) (string, error) {
	return s.startBackgroundJob(userID, panelID, nodeUUID, password, func(ctx context.Context, progress func(telegram.NodeActualizeProgress)) (any, error) {
		return s.actualizer.ActualizeNode(ctx, telegram.NodeActualizeInput{
			PanelID: panelID, NodeUUID: nodeUUID, OperatorUserID: userID, Password: password, Progress: progress,
		})
	})
}

func (s *Server) startBackgroundJob(userID int64, scope, target string, secret []byte, run func(context.Context, func(telegram.NodeActualizeProgress)) (any, error)) (string, error) {
	token, existing, err := s.jobs.begin(userID, scope, target)
	if err != nil {
		clear(secret)
		return "", err
	}
	if existing {
		clear(secret)
		return token, nil
	}
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer clear(secret)
		result, runErr := run(s.jobContext, func(update telegram.NodeActualizeProgress) { s.jobs.progress(token, update) })
		if runErr != nil {
			s.config.Logger.Warn("background web console operation failed", "scope", scope, "target", target, "error", runErr)
			safeMessage := "Не удалось безопасно выполнить операцию"
			var operatorError interface{ OperatorMessage() string }
			if errors.As(runErr, &operatorError) && strings.TrimSpace(operatorError.OperatorMessage()) != "" {
				safeMessage = operatorError.OperatorMessage()
			}
			s.jobs.fail(token, safeMessage)
			return
		}
		s.jobs.complete(token, result)
	}()
	return token, nil
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.readSession(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/jobs/"))
	job, ok := s.jobs.get(token, user.UserID)
	if !ok || token == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Операция не найдена"})
		return
	}
	percent := 0
	if job.Progress.Total > 0 {
		percent = job.Progress.Completed * 100 / job.Progress.Total
	}
	if percent < 0 {
		percent = 0
	}
	if job.Status == jobRunning && percent >= 100 {
		percent = 99
	}
	if job.Status == jobCompleted {
		percent = 100
	}
	response := map[string]any{
		"status": job.Status,
		"progress": map[string]any{
			"step": job.Progress.Step, "message": job.Progress.Message,
			"completed": job.Progress.Completed, "total": job.Progress.Total, "percent": percent,
		},
	}
	if job.Status == jobCompleted {
		switch result := job.Result.(type) {
		case telegram.NodeActualizeResult:
			response["result"] = map[string]any{
				"nodeName": result.NodeName, "managed": result.Managed, "adopted": result.Adopted,
				"addresses": result.Addresses, "firewallChanged": result.FirewallChanged,
			}
		default:
			response["result"] = result
		}
	}
	if job.Status == jobFailed {
		response["error"] = job.SafeError
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' https://cdn.jsdelivr.net; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; frame-src 'none'; img-src 'self' data: https:; connect-src 'self' wss:")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if _, _, ok := s.readSession(r); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.ServeFileFS(w, r, assets, "static/index.html")
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.readSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	attempt, err := newOIDCLoginAttempt(s.now())
	if err != nil {
		http.Error(w, "Не удалось подготовить авторизацию", http.StatusInternalServerError)
		return
	}
	cookie, err := s.signOIDCLoginAttempt(attempt)
	if err != nil {
		http.Error(w, "Не удалось подготовить авторизацию", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: loginStateCookie, Value: cookie, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	authURL := s.oauth2Config.AuthCodeURL(attempt.State, oauth2.S256ChallengeOption(attempt.Verifier), oidc.Nonce(attempt.Nonce))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.login.ExecuteTemplate(w, "login.html", map[string]string{"AuthURL": authURL}); err != nil {
		s.config.Logger.Error("web login template failed")
	}
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	attempt, err := s.readOIDCLoginAttempt(r)
	clearCookie(w, loginStateCookie)
	if err != nil || !hmac.Equal([]byte(attempt.State), []byte(r.URL.Query().Get("state"))) || strings.TrimSpace(r.URL.Query().Get("code")) == "" {
		http.Error(w, "Проверка входа не пройдена", http.StatusForbidden)
		return
	}
	callbackCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	token, err := s.oauth2Config.Exchange(callbackCtx, r.URL.Query().Get("code"), oauth2.VerifierOption(attempt.Verifier))
	if err != nil {
		s.config.Logger.Warn("Telegram OIDC code exchange failed", "error", err)
		http.Error(w, "Telegram-авторизация недействительна", http.StatusForbidden)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || strings.TrimSpace(rawIDToken) == "" {
		http.Error(w, "Telegram не вернул ID token", http.StatusForbidden)
		return
	}
	idToken, err := s.oidcVerifier.Verify(callbackCtx, rawIDToken)
	if err != nil || idToken == nil {
		s.config.Logger.Warn("Telegram OIDC ID token verification failed", "error", err)
		http.Error(w, "ID token Telegram не прошёл проверку", http.StatusForbidden)
		return
	}
	if idToken.Nonce != "" && !hmac.Equal([]byte(idToken.Nonce), []byte(attempt.Nonce)) {
		s.config.Logger.Warn("Telegram OIDC nonce mismatch")
		http.Error(w, "ID token Telegram не прошёл проверку nonce", http.StatusForbidden)
		return
	}
	if idToken.Nonce == "" {
		s.config.Logger.Info("Telegram OIDC ID token omitted nonce; signed state and PKCE were verified")
	}
	value, err := sessionFromOIDCIDToken(idToken)
	if err != nil {
		s.config.Logger.Warn("Telegram OIDC user claims rejected", "error", err)
		http.Error(w, "Telegram не вернул корректного пользователя", http.StatusForbidden)
		return
	}
	if _, allowed := s.allowedUsers[value.UserID]; !allowed {
		http.Error(w, "Этот Telegram-пользователь не имеет доступа", http.StatusForbidden)
		return
	}
	if err := s.issueSessionCookie(w, value); err != nil {
		http.Error(w, "Не удалось создать сессию", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	value, cookie, ok := s.readSession(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": map[string]any{"id": value.UserID, "name": value.Name}, "csrfToken": s.csrfToken(cookie)})
}

type nodeResponse struct {
	PanelID          string     `json:"panelId"`
	PanelName        string     `json:"panelName"`
	UUID             string     `json:"uuid"`
	Name             string     `json:"name"`
	Address          string     `json:"address"`
	Connected        bool       `json:"connected"`
	Connecting       bool       `json:"connecting"`
	Disabled         bool       `json:"disabled"`
	Online           int        `json:"online"`
	OnlineKnown      bool       `json:"onlineKnown"`
	Managed          bool       `json:"managed"`
	Status           string     `json:"status"`
	LastChange       *time.Time `json:"lastStatusChange,omitempty"`
	NetworkAddresses []string   `json:"networkAddresses"`
	NetworkUpdatedAt *time.Time `json:"networkUpdatedAt,omitempty"`
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.readSession(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	nodes, err := s.nodes.ListNodes(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Не удалось загрузить список нод"})
		return
	}
	result := make([]nodeResponse, 0, len(nodes))
	for _, node := range nodes {
		managed := node.Managed
		if !managed {
			_, err := s.deployments.FindDeploymentByPanelNodeUUID(r.Context(), node.PanelID, node.UUID)
			managed = err == nil
			if err != nil && !errors.Is(err, repository.ErrNotFound) {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Не удалось определить тип ноды"})
				return
			}
		}
		status := "offline"
		if node.Disabled {
			status = "disabled"
		} else if node.Connected {
			status = "online"
		} else if node.Connecting {
			status = "connecting"
		}
		result = append(result, nodeResponse{PanelID: node.PanelID, PanelName: node.PanelName, UUID: node.UUID, Name: node.Name, Address: node.Address, Connected: node.Connected, Connecting: node.Connecting, Disabled: node.Disabled, Online: node.Online, OnlineKnown: node.OnlineKnown, Managed: managed, Status: status, LastChange: node.LastStatusChange, NetworkAddresses: node.NetworkAddresses, NetworkUpdatedAt: node.NetworkUpdatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": result})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	_, cookie, ok := s.readSession(r)
	if !ok || !s.validCSRF(r, cookie) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	clearCookie(w, sessionCookieName)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTicket(w http.ResponseWriter, r *http.Request) {
	value, cookie, ok := s.readSession(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !s.validCSRF(r, cookie) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
	parts := strings.Split(path, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] != "ticket" {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var input struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Password) > 1024 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный запрос"})
		return
	}
	target, err := s.resolveTarget(r.Context(), parts[0], parts[1])
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Нода не найдена"})
		return
	}
	if !target.Managed && input.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Для legacy-ноды нужен root-пароль"})
		return
	}
	if !target.Managed {
		if s.actualizer == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Перевод legacy-ноды под управление недоступен"})
			return
		}
		password := []byte(input.Password)
		input.Password = ""
		jobID, jobErr := s.startNodeJob(value.UserID, parts[0], parts[1], password)
		if jobErr != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Для этой ноды уже выполняется другая операция"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "jobId": jobID})
		return
	}
	target.UserID = value.UserID
	ticket, err := s.tickets.issue(target)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Не удалось подготовить SSH-сессию"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ticket": ticket, "managed": target.Managed})
}

func (s *Server) handleNodeAction(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/nodes/actualize-managed" {
		s.handleBulkActualize(w, r)
		return
	}
	if r.URL.Path == "/api/nodes/ip-bulk/preview" {
		s.handleBulkIPPreview(w, r)
		return
	}
	if r.URL.Path == "/api/nodes/ip-bulk/apply" {
		s.handleBulkIPApply(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/actualize") {
		s.handleActualize(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/move") {
		s.handleNodeMove(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/ip") {
		s.handleNodeIPChange(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/server-ip") {
		s.handleNodeServerIPChange(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/dns") {
		s.handleNodeDNSSync(w, r)
		return
	}
	s.handleTicket(w, r)
}

func (s *Server) handleBulkActualize(w http.ResponseWriter, r *http.Request) {
	_, cookie, ok := s.readSession(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !s.validCSRF(r, cookie) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	if s.actualizer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Синхронизация нод недоступна"})
		return
	}
	result, err := s.actualizer.ActualizeManagedNodes(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Не удалось запустить массовую синхронизацию"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": result.Total, "updated": result.Updated, "failed": result.Failed,
		"firewallChanged": result.FirewallChanged, "failures": result.Failures,
	})
}

func (s *Server) handleActualize(w http.ResponseWriter, r *http.Request) {
	user, cookie, ok := s.readSession(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !s.validCSRF(r, cookie) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	if s.actualizer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Синхронизация нод недоступна"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/nodes/"), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] != "actualize" {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Password) > 1024 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный запрос"})
		return
	}
	password := []byte(body.Password)
	body.Password = ""
	target, err := s.resolveTarget(r.Context(), parts[0], parts[1])
	if err != nil {
		clear(password)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Нода не найдена"})
		return
	}
	if !target.Managed && len(password) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Для legacy-ноды нужен root-пароль"})
		return
	}
	jobID, jobErr := s.startNodeJob(user.UserID, parts[0], parts[1], password)
	if jobErr != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Для этой ноды уже выполняется другая операция"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "jobId": jobID})
}

func (s *Server) resolveTarget(ctx context.Context, panelID, nodeUUID string) (terminalTicket, error) {
	nodes, err := s.nodes.ListNodes(ctx)
	if err != nil {
		return terminalTicket{}, err
	}
	for _, node := range nodes {
		if node.PanelID != panelID || node.UUID != nodeUUID {
			continue
		}
		address, err := netip.ParseAddr(strings.TrimSpace(node.Address))
		if err != nil || !address.IsGlobalUnicast() || address.IsPrivate() {
			return terminalTicket{}, errors.New("invalid node address")
		}
		result := terminalTicket{PanelID: panelID, NodeUUID: nodeUUID, NodeName: node.Name, Address: address.Unmap()}
		item, err := s.deployments.FindDeploymentByPanelNodeUUID(ctx, panelID, nodeUUID)
		if err == nil {
			result.Managed, result.DeploymentID = true, item.ID
			return result, nil
		}
		if errors.Is(err, repository.ErrNotFound) {
			return result, nil
		}
		return terminalTicket{}, err
	}
	return terminalTicket{}, errors.New("node not found")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) String() string { return fmt.Sprintf("webconsole(%s)", s.config.Addr) }
