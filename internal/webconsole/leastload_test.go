package webconsole

import (
	"context"
	"net/http/httptest"
	"remnanode-setup-bot/internal/leastload"
	"remnanode-setup-bot/internal/telegram"
	"strings"
	"testing"
)

type leastloadProvider struct{ NodeProvider }

func (p leastloadProvider) ListLeastloadCandidates(context.Context, string) ([]leastload.Candidate, error) {
	return []leastload.Candidate{}, nil
}
func (p leastloadProvider) ListLeastloadJobs(context.Context) ([]leastload.Job, error) {
	return []leastload.Job{}, nil
}
func (p leastloadProvider) PrepareLeastload(context.Context, string, string, int64, []leastload.Mapping, func(telegram.NodeActualizeProgress)) (leastload.Job, error) {
	return leastload.Job{}, nil
}
func (p leastloadProvider) ApplyLeastload(context.Context, string, int64, func(telegram.NodeActualizeProgress)) (leastload.Job, error) {
	return leastload.Job{}, nil
}
func TestLeastloadEndpointsRequireSessionCSRFAndAssignmentConfirmation(t *testing.T) {
	s := newTestServer(t)
	s.nodes = leastloadProvider{s.nodes}
	response := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(response, httptest.NewRequest("GET", "https://nodes.example.com/api/leastload/jobs", nil))
	if response.Code != 401 {
		t.Fatal(response.Code)
	}
	cookieResponse := httptest.NewRecorder()
	if err := s.issueSessionCookie(cookieResponse, session{UserID: 42}); err != nil {
		t.Fatal(err)
	}
	cookie := cookieResponse.Result().Cookies()[0]
	for _, csrf := range []bool{false, true} {
		req := httptest.NewRequest("POST", "https://nodes.example.com/api/leastload/apply", strings.NewReader(`{"jobId":"12345678-1234-4234-8234-123456789abc","assigned":false}`))
		req.AddCookie(cookie)
		if csrf {
			req.Header.Set("X-CSRF-Token", s.csrfToken(cookie.Value))
		}
		response = httptest.NewRecorder()
		s.http.Handler.ServeHTTP(response, req)
		expected := 403
		if csrf {
			expected = 400
		}
		if response.Code != expected {
			t.Fatalf("status=%d expected=%d", response.Code, expected)
		}
	}
}
