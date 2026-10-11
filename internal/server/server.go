package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kartikey/workflow_service/internal/api"
	"github.com/kartikey/workflow_service/pkg/events"
	"github.com/kartikey/workflow_service/pkg/obs"
	"go.uber.org/zap"
)

const (
	defaultReadinessTimeout = 5 * time.Second
	idleTimeout             = 60 * time.Second
	readHeaderTimeout       = 5 * time.Second
)

type ReadinessChecker interface {
	Ping(context.Context) error
}

type Config struct {
	Address          string
	ReadinessTimeout time.Duration
	// API enables the /v1 API; nil serves health endpoints only.
	API *api.Services
	// Metrics serves /metrics without the API, for worker processes.
	Metrics bool
}

type Server struct {
	httpServer       *http.Server
	readiness        ReadinessChecker
	readinessTimeout time.Duration
	logger           *zap.Logger
	services         *api.Services
	events           *events.Hub
	// closing ends SSE streams when shutdown starts, so they do not hold the
	// drain open until the grace period expires.
	closing chan struct{}
}

type statusResponse struct {
	Status string `json:"status"`
}

func New(cfg Config, readiness ReadinessChecker, logger *zap.Logger) (*Server, error) {
	if strings.TrimSpace(cfg.Address) == "" {
		return nil, errors.New("HTTP_ADDRESS is required")
	}

	if readiness == nil {
		return nil, errors.New("readiness checker is required")
	}
	if cfg.ReadinessTimeout < 0 {
		return nil, errors.New("readiness timeout cannot be negative")
	}

	if cfg.ReadinessTimeout == 0 {
		cfg.ReadinessTimeout = defaultReadinessTimeout
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	server := &Server{
		readiness:        readiness,
		readinessTimeout: cfg.ReadinessTimeout,
		logger:           logger,
		services:         cfg.API,
		events:           events.NewHub(),
		closing:          make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.healthz)
	mux.HandleFunc("GET /readyz", server.readyz)
	if cfg.Metrics || cfg.API != nil {
		mux.Handle("GET /metrics", obs.Handler())
	}
	if cfg.API != nil {
		if err := cfg.API.Validate(); err != nil {
			return nil, err
		}
		api.NewHandler(cfg.API, server.events, server.closing, logger).Register(router{server, mux})
	}

	server.httpServer = &http.Server{
		Addr:              cfg.Address,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	var closeOnce sync.Once
	server.httpServer.RegisterOnShutdown(func() { closeOnce.Do(func() { close(server.closing) }) })

	return server, nil
}

// Handler is exposed for in-process endpoint tests.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Run serves until cancellation, then allows active requests time to finish.
func (s *Server) Run(ctx context.Context, listener net.Listener, grace time.Duration) error {
	defer listener.Close()
	defer s.httpServer.Close()
	if grace <= 0 {
		return errors.New("shutdown grace must be positive")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// The event hub shares one LISTEN connection among SSE streams. It is owned
	// here and stops with the server.
	hubCtx, stopHub := context.WithCancel(context.WithoutCancel(ctx))
	hubDone := make(chan struct{})
	if s.services != nil && s.services.Pool != nil {
		go func() {
			defer close(hubDone)
			s.events.Run(hubCtx, s.services.Pool, s.logger)
		}()
	} else {
		close(hubDone)
	}
	defer func() {
		stopHub()
		<-hubDone
	}()

	served := make(chan error, 1)
	go func() {
		err := s.httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		served <- err
	}()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	drain, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	err := s.httpServer.Shutdown(drain)
	if err != nil {
		_ = s.httpServer.Close()
		err = fmt.Errorf("shutdown HTTP: %w", err)
	}
	return errors.Join(err, <-served)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	s.writeStatus(w, http.StatusOK, "ok")
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.readinessTimeout)
	defer cancel()

	if err := s.readiness.Ping(ctx); err != nil {
		// Do not return or log connection strings, credentials, or raw DB errors.
		s.logger.Warn("database readiness check failed")
		s.writeStatus(w, http.StatusServiceUnavailable, "not_ready")
		return
	}

	s.writeStatus(w, http.StatusOK, "ready")
}

func (s *Server) writeStatus(w http.ResponseWriter, status int, value string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(statusResponse{Status: value}); err != nil {
		s.logger.Debug("write health response failed")
	}
}
