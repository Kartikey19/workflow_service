package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	setValidEnvironment(t)

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if got.App.Environment != "testing" {
		t.Errorf("APP_ENV = %q, want testing", got.App.Environment)
	}
	if got.App.Mode != "all" {
		t.Errorf("APP_MODE = %q, want all", got.App.Mode)
	}
	if got.Database.MaxConns != 8 || got.Database.MinConns != 2 {
		t.Errorf("database connections = (%d, %d), want (8, 2)", got.Database.MaxConns, got.Database.MinConns)
	}
	if got.Worker.LeaseTTL != 30*time.Second {
		t.Errorf("WORKER_LEASE_TTL = %s, want 30s", got.Worker.LeaseTTL)
	}
}

func TestLoadConfigRejectsInvalidEnvironmentValue(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("APP_ENV", "test")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want invalid APP_ENV error")
	}
	if !strings.Contains(err.Error(), "APP_ENV must be development, testing, or production") {
		t.Fatalf("LoadConfig() error = %q, want testing listed as valid", err)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Config)
		wantError string
	}{
		{
			name:      "accepts testing environment",
			configure: func(*Config) {},
		},
		{
			name: "rejects invalid mode with environment variable name",
			configure: func(c *Config) {
				c.App.Mode = "server"
			},
			wantError: "APP_MODE must be api, worker, or all",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validConfig()
			tt.configure(&config)

			err := config.Validate()
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func setValidEnvironment(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"APP_ENV":              "testing",
		"APP_MODE":             "all",
		"HTTP_ADDRESS":         "127.0.0.1:8080",
		"SERVICE_NAME":         "workflow-service",
		"BUILD_VERSION":        "test",
		"LOG_LEVEL":            "info",
		"LOG_ADD_SOURCE":       "false",
		"DATABASE_URL":         "postgres://workflow:password@127.0.0.1:5432/workflow",
		"DB_MAX_CONNS":         "8",
		"DB_MIN_CONNS":         "2",
		"DB_CONNECT_TIMEOUT":   "5s",
		"WORKER_ID":            "test-worker",
		"WORKER_CONCURRENCY":   "4",
		"WORKER_BATCH_SIZE":    "2",
		"WORKER_POLL_INTERVAL": "1s",
		"WORKER_LEASE_TTL":     "30s",
		"SHUTDOWN_GRACE":       "10s",
	} {
		t.Setenv(name, value)
	}
}

func validConfig() Config {
	return Config{
		App: AppConfig{
			Environment:   "testing",
			Mode:          "all",
			HTTPAddress:   "127.0.0.1:0",
			ShutdownGrace: 10 * time.Second,
		},
		Log: LogConfig{LogLevel: "info"},
		Database: DatabaseConfig{
			URL:            "postgres://workflow:password@127.0.0.1:5432/workflow",
			MaxConns:       8,
			ConnectTimeout: time.Second,
			MinConns:       2,
		},
		Worker: WorkerConfig{
			ID:           "test-worker",
			Concurrency:  4,
			BatchSize:    2,
			PollInterval: time.Second,
			LeaseTTL:     30 * time.Second,
		},
	}
}

func TestLoadConfigDefaultsAndDedicatedMode(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, key := range []string{"APP_ENV", "APP_MODE", "HTTP_ADDRESS", "SERVICE_NAME", "BUILD_VERSION", "LOG_LEVEL",
		"LOG_ADD_SOURCE", "DB_MAX_CONNS", "DB_MIN_CONNS", "DB_CONNECT_TIMEOUT", "WORKER_ID", "WORKER_CONCURRENCY",
		"WORKER_BATCH_SIZE", "WORKER_POLL_INTERVAL", "WORKER_LEASE_TTL", "SHUTDOWN_GRACE"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	t.Setenv("DATABASE_URL", "postgres://localhost/workflow")
	got, err := LoadConfigMode("worker")
	if err != nil {
		t.Fatal(err)
	}
	if got.App.Mode != "worker" || got.App.Environment != "development" || got.Worker.Concurrency != 32 ||
		got.Worker.LeaseTTL != time.Minute || got.Database.MaxConns != 10 || got.Worker.MaxNodesPerRun != 1000 {
		t.Fatalf("defaults = %+v", got)
	}
	if !strings.Contains(got.Worker.ID, "-") {
		t.Fatalf("WORKER_ID default = %q, want hostname plus suffix", got.Worker.ID)
	}
	other, err := LoadConfigMode("worker")
	if err != nil || other.Worker.ID == got.Worker.ID {
		t.Fatalf("two replicas share WORKER_ID %q", got.Worker.ID)
	}
}

func TestLoadConfigRequiresDatabaseURL(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("LoadConfig() = %v, want missing DATABASE_URL", err)
	}
}

func TestLoadEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := LoadEnv(); err != nil {
		t.Fatalf("missing optional file: %v", err)
	}
	t.Setenv("DOTENV_TEST_FILE", "")
	if err := os.Unsetenv("DOTENV_TEST_FILE"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOTENV_TEST_OVERRIDE", "shell")
	if err := os.WriteFile(".env", []byte("DOTENV_TEST_FILE='from file'\nDOTENV_TEST_OVERRIDE=file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadEnv(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("DOTENV_TEST_FILE") != "from file" || os.Getenv("DOTENV_TEST_OVERRIDE") != "shell" {
		t.Fatal("incorrect dotenv values or precedence")
	}
	if err := os.WriteFile(".env", []byte("INVALID!KEY=secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadEnv(); err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatal("expected a sanitized parse error")
	}
}
