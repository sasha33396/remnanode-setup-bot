package webconsole

import (
	"context"
	"encoding/json"
	"net/http"
	"remnanode-setup-bot/internal/leastload"
	"remnanode-setup-bot/internal/telegram"
)

type leastloadApp interface {
	ListLeastloadCandidates(context.Context, string) ([]leastload.Candidate, error)
	ListLeastloadJobs(context.Context) ([]leastload.Job, error)
	PrepareLeastload(context.Context, string, string, int64, []leastload.Mapping, func(telegram.NodeActualizeProgress)) (leastload.Job, error)
	ApplyLeastload(context.Context, string, int64, func(telegram.NodeActualizeProgress)) (leastload.Job, error)
}

func (s *Server) handleLeastloadRead(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(w, r) {
		return
	}
	app, ok := s.nodes.(leastloadApp)
	if !ok {
		writeJSON(w, 503, map[string]string{"error": "Смена IP leastload недоступна"})
		return
	}
	if r.URL.Path == "/api/leastload/jobs" {
		jobs, e := app.ListLeastloadJobs(r.Context())
		if e != nil {
			writeJSON(w, 502, map[string]string{"error": "Не удалось прочитать операции. Проверьте миграцию 000011"})
			return
		}
		writeJSON(w, 200, map[string]any{"jobs": jobs})
		return
	}
	candidates, e := app.ListLeastloadCandidates(r.Context(), r.URL.Query().Get("panel"))
	if e != nil {
		writeJSON(w, 502, map[string]string{"error": "Не удалось получить ноды со скрытыми Host"})
		return
	}
	writeJSON(w, 200, map[string]any{"candidates": candidates})
}
func (s *Server) handleLeastloadWrite(w http.ResponseWriter, r *http.Request) {
	user, ok := s.mutationUser(w, r)
	if !ok {
		return
	}
	app, ok := s.nodes.(leastloadApp)
	if !ok {
		writeJSON(w, 503, map[string]string{"error": "Смена IP leastload недоступна"})
		return
	}
	var body struct {
		PanelID  string              `json:"panelId"`
		Mode     string              `json:"mode"`
		Mappings []leastload.Mapping `json:"mappings"`
		JobID    string              `json:"jobId"`
		Assigned bool                `json:"assigned"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]string{"error": "Некорректный запрос"})
		return
	}
	scope := "leastload-preview"
	target := body.PanelID
	apply := r.URL.Path == "/api/leastload/apply"
	if apply {
		scope = "leastload-apply"
		target = body.JobID
		if !body.Assigned || len(body.JobID) != 36 {
			writeJSON(w, 400, map[string]string{"error": "Подтвердите назначение новых IP серверам у хостера"})
			return
		}
	} else if body.PanelID == "" || len(body.PanelID) > 128 || len(body.Mappings) == 0 || len(body.Mappings) > 50 {
		writeJSON(w, 400, map[string]string{"error": "Выберите от 1 до 50 нод"})
		return
	}
	id, err := s.startBackgroundJob(user.UserID, scope, target, nil, func(ctx context.Context, progress func(telegram.NodeActualizeProgress)) (any, error) {
		if apply {
			return app.ApplyLeastload(ctx, body.JobID, user.UserID, progress)
		}
		return app.PrepareLeastload(ctx, body.PanelID, body.Mode, user.UserID, body.Mappings, progress)
	})
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": "Операция уже выполняется"})
		return
	}
	writeJSON(w, 202, map[string]any{"jobId": id})
}
