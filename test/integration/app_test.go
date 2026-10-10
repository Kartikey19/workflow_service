//go:build integration

package integration_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/kartikey/workflow_service/internal/app"
	"github.com/kartikey/workflow_service/internal/config"
)

func TestApplicationReadinessAndShutdown(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Fatal("DATABASE_URL is required")
	}
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	cfg := config.Config{
		App:      config.AppConfig{Environment: "testing", Mode: "api", HTTPAddress: address, ServiceName: "integration", BuildVersion: "test", ShutdownGrace: time.Second},
		Log:      config.LogConfig{LogLevel: "error"},
		Database: config.DatabaseConfig{URL: url, MaxConns: 2, ConnectTimeout: time.Second},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("application: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("application did not stop")
		}
	})
	client := &http.Client{Timeout: time.Second}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		res, err := client.Get("http://" + address + "/readyz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("readiness = %d", res.StatusCode)
			}
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("application never became ready")
		}
	}
	// Cleanup cancels and joins the application, including dependency cleanup.
}
