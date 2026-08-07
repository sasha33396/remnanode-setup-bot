// Package health provides liveness and readiness HTTP endpoints.
package health

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const shutdownTimeout = 10 * time.Second

// Server serves application health probes.
type Server struct {
	server *http.Server
	logger *slog.Logger
	ready  atomic.Bool
	check  func(context.Context) error
}

// NewServer creates a health server bound to addr.
func NewServer(addr string, logger *slog.Logger) *Server {
	return NewServerWithOptions(addr, logger, nil, nil)
}

// NewServerWithOptions adds dependency readiness and a metrics endpoint.
func NewServerWithOptions(addr string, logger *slog.Logger, check func(context.Context) error, metrics http.Handler) *Server {
	s := &Server{logger: logger, check: check}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.liveness)
	mux.HandleFunc("GET /readyz", s.readiness)
	if metrics != nil {
		mux.Handle("GET /metrics", metrics)
	}
	s.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	return s
}

// Run listens until ctx is cancelled, then gracefully shuts down.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return err
	}

	serveErrors := make(chan error, 1)
	s.ready.Store(true)
	go func() {
		serveErrors <- s.server.Serve(listener)
	}()

	select {
	case err = <-serveErrors:
		s.ready.Store(false)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.ready.Store(false)
		s.logger.Info("shutdown requested")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := s.server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-serveErrors; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) liveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) readiness(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !s.ready.Load() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	if s.check != nil {
		checkCtx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
		defer cancel()
		if err := s.check(checkCtx); err != nil {
			http.Error(w, "dependency unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}
