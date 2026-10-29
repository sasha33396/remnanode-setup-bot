package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	sshclient "remnanode-setup-bot/internal/ssh"
)

type terminalMessage struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Columns int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
}

func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.readSession(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		http.Error(w, "Достигнут лимит SSH-сессий", http.StatusTooManyRequests)
		return
	}
	target, ok := s.tickets.take(r.URL.Query().Get("ticket"), user.UserID)
	if !ok {
		http.Error(w, "SSH ticket недействителен", http.StatusForbidden)
		return
	}
	defer target.clear()

	ctx, cancel := context.WithTimeout(r.Context(), s.config.TerminalTimeout)
	defer cancel()
	var connection *sshclient.Connection
	var err error
	if target.Managed {
		connection, err = s.managedSSH.ConnectWithDeploymentKey(ctx, target.DeploymentID, target.Address, "root")
	} else {
		credentials := sshclient.NewInitialCredentials(target.Address, "root", target.Password)
		target.clear()
		connection, err = s.legacySSH.ConnectInitial(ctx, "legacy:"+target.PanelID+":"+target.NodeUUID, credentials)
	}
	if err != nil {
		s.config.Logger.Warn("web SSH connection failed", "panel_id", target.PanelID, "node_uuid", target.NodeUUID, "managed", target.Managed)
		http.Error(w, "Не удалось подключиться по SSH", http.StatusBadGateway)
		return
	}
	defer connection.Close()

	upgrader := websocket.Upgrader{
		HandshakeTimeout: 5 * time.Second,
		CheckOrigin: func(request *http.Request) bool {
			origin := request.Header.Get("Origin")
			return origin == s.publicURL.Scheme+"://"+s.publicURL.Host
		},
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(64 << 10)

	columns := boundedDimension(r.URL.Query().Get("cols"), 120)
	rows := boundedDimension(r.URL.Query().Get("rows"), 32)
	inputReader, inputWriter := io.Pipe()
	defer inputReader.Close()
	resize := make(chan sshclient.WindowSize, 8)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer inputWriter.Close()
		for {
			_, payload, readErr := ws.ReadMessage()
			if readErr != nil {
				cancel()
				return
			}
			var message terminalMessage
			if json.Unmarshal(payload, &message) != nil {
				continue
			}
			switch message.Type {
			case "input":
				if len(message.Data) <= 32<<10 {
					if _, writeErr := io.WriteString(inputWriter, message.Data); writeErr != nil {
						return
					}
				}
			case "resize":
				size := sshclient.WindowSize{Columns: message.Columns, Rows: message.Rows}
				select {
				case resize <- size:
				default:
				}
			}
		}
	}()

	output := &websocketWriter{connection: ws}
	err = connection.RunTerminal(ctx, inputReader, output, sshclient.WindowSize{Columns: columns, Rows: rows}, resize)
	cancel()
	_ = inputWriter.Close()
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		_ = output.writeText(`{"type":"error","message":"SSH-сессия завершена"}`)
	}
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	select {
	case <-readDone:
	case <-time.After(time.Second):
	}
}

func boundedDimension(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 1 || parsed > 1000 {
		return fallback
	}
	return parsed
}

type websocketWriter struct {
	mu         sync.Mutex
	connection *websocket.Conn
}

func (w *websocketWriter) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.connection.WriteMessage(websocket.BinaryMessage, value); err != nil {
		return 0, err
	}
	return len(value), nil
}

func (w *websocketWriter) writeText(value string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.connection.WriteMessage(websocket.TextMessage, []byte(value))
}
