package webconsole

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"remnanode-setup-bot/internal/networkinventory"
	"remnanode-setup-bot/internal/telegram"
)

type reportProvider struct{ NodeProvider }

func (p reportProvider) ListNetworkReports(context.Context) ([]networkinventory.Report, error) {
	return []networkinventory.Report{{NodeName: "test node", Status: "review"}}, nil
}
func (p reportProvider) CollectNetworkReports(context.Context, string, string, func(telegram.NodeActualizeProgress)) ([]networkinventory.Report, error) {
	return nil, nil
}

func TestNetworkReportRequiresSessionAndScanRequiresCSRF(t *testing.T) {
	server := newTestServer(t)
	server.nodes = reportProvider{server.nodes}
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, httptest.NewRequest("GET", "https://nodes.example.com/api/network-reports", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous report status=%d", response.Code)
	}
	recorder := httptest.NewRecorder()
	if err := server.issueSessionCookie(recorder, session{UserID: 42}); err != nil {
		t.Fatal(err)
	}
	cookie := recorder.Result().Cookies()[0]
	request := httptest.NewRequest("GET", "https://nodes.example.com/api/network-reports", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "test node") {
		t.Fatalf("report: %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest("POST", "https://nodes.example.com/api/network-reports/scan", strings.NewReader(`{}`))
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("scan without csrf=%d", response.Code)
	}
}
