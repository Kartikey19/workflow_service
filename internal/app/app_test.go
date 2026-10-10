package app

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kartikey/workflow_service/pkg/config"
)

func testConfig() config.Config {
	return config.Config{
		App:      config.AppConfig{Environment: "testing", Mode: "api", HTTPAddress: "127.0.0.1:0", ServiceName: "test", BuildVersion: "test", ShutdownGrace: time.Second},
		Log:      config.LogConfig{LogLevel: "info"},
		Database: config.DatabaseConfig{URL: "postgres://user:secret@localhost:%zz/test", MaxConns: 1, ConnectTimeout: time.Second},
	}
}
func TestRunRejectsOccupiedPortBeforeOpeningDatabase(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	cfg := testConfig()
	cfg.App.HTTPAddress = listener.Addr().String()
	if err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "bind HTTP listener") {
		t.Fatalf("Run = %v", err)
	}
}
func TestRunReleasesListenerOnDependencyFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	cfg := testConfig()
	cfg.App.HTTPAddress = address
	if err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "open database") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unexpected startup error: %v", err)
	}
	replacement, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listener was not released: %v", err)
	}
	replacement.Close()
}
