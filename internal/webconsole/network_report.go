package webconsole

import (
	"context"
	"encoding/json"
	"net/http"
	"remnanode-setup-bot/internal/networkinventory"
	"remnanode-setup-bot/internal/telegram"
)

type networkReporter interface {
	ListNetworkReports(context.Context) ([]networkinventory.Report, error)
	CollectNetworkReports(context.Context, string, string, func(telegram.NodeActualizeProgress)) ([]networkinventory.Report, error)
}

func (s *Server) handleNetworkReports(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(w, r) {
		return
	}
	app, ok := s.nodes.(networkReporter)
	if !ok {
		writeJSON(w, 503, map[string]string{"error": "Сборщик недоступен"})
		return
	}
	reports, err := app.ListNetworkReports(r.Context())
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "Не удалось прочитать отчёт"})
		return
	}
	writeJSON(w, 200, map[string]any{"reports": reports})
}

func (s *Server) handleNetworkScan(w http.ResponseWriter, r *http.Request) {
	user, ok := s.mutationUser(w, r)
	if !ok {
		return
	}
	app, ok := s.nodes.(networkReporter)
	if !ok {
		writeJSON(w, 503, map[string]string{"error": "Сборщик недоступен"})
		return
	}
	var input struct {
		PanelID  string `json:"panelId"`
		NodeUUID string `json:"nodeUuid"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	if json.NewDecoder(r.Body).Decode(&input) != nil || (input.PanelID == "") != (input.NodeUUID == "") || len(input.PanelID) > 128 || len(input.NodeUUID) > 128 {
		writeJSON(w, 400, map[string]string{"error": "Некорректный выбор ноды"})
		return
	}
	id, err := s.startBackgroundJob(user.UserID, "network-scan", input.PanelID+":"+input.NodeUUID, nil, func(ctx context.Context, progress func(telegram.NodeActualizeProgress)) (any, error) {
		reports, err := app.CollectNetworkReports(ctx, input.PanelID, input.NodeUUID, progress)
		return map[string]any{"reports": reports}, err
	})
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": "Не удалось запустить сбор"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "jobId": id})
}
