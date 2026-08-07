package health

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthEndpoints(t *testing.T) {
	server := NewServer(":0", slog.New(slog.NewTextHandler(io.Discard, nil)))

	live := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if live.Code != http.StatusOK {
		t.Errorf("liveness status = %d, want %d", live.Code, http.StatusOK)
	}

	notReady := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(notReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if notReady.Code != http.StatusServiceUnavailable {
		t.Errorf("initial readiness status = %d, want %d", notReady.Code, http.StatusServiceUnavailable)
	}

	server.ready.Store(true)
	ready := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Errorf("readiness status = %d, want %d", ready.Code, http.StatusOK)
	}
}
