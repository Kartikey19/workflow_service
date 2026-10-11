package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	setValidEnvironment(t)

	got, err := LoadConfig("api")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if got.App.Environment != "testing" {
		t.Errorf("APP_ENV = %q, want testing", got.App.Environment)
	}
	if got.App.Mode != "api" {
		t.Errorf("mode = %q, want api", got.App.Mode)
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

	_, err := LoadConfig("api")
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
			wantError: "mode must be api or worker",
		},
		{
			name:      "production requires secure cookies",
			configure: func(c *Config) { c.App.Environment = "production" },
			wantError: "COOKIE_SECURE must be true in production",
		},
		{
			name: "production with secure cookies is accepted",
			configure: func(c *Config) {
				c.App.Environment = "production"
				c.Auth.CookieSecure = true
			},
		},
		{
			name:      "bootstrap email needs a password",
			configure: func(c *Config) { c.Auth.BootstrapAdminEmail = "root@example.com" },
			wantError: "BOOTSTRAP_ADMIN_EMAIL and BOOTSTRAP_ADMIN_PASSWORD must be set together",
		},
		{
			name:      "bootstrap password needs an email",
			configure: func(c *Config) { c.Auth.BootstrapAdminPassword = "a-long-password" },
			wantError: "BOOTSTRAP_ADMIN_EMAIL and BOOTSTRAP_ADMIN_PASSWORD must be set together",
		},
		{
			name:      "vault key must be valid",
			configure: func(c *Config) { c.Vault = VaultConfig{MasterKey: "not-a-key", KeyVersion: 1} },
			wantError: "VAULT_MASTER_KEY",
		},
		{
			name: "vault with a valid key and an old key is accepted",
			configure: func(c *Config) {
				key := "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
				c.Vault = VaultConfig{MasterKey: key, KeyVersion: 2, OldKeys: "1:" + key}
			},
		},
		{
			name:      "an OAuth app needs a public URL",
			configure: func(c *Config) { c.Services.GoogleClientID, c.Services.GoogleClientSecret = "id", "secret" },
			wantError: "PUBLIC_URL is required when a Google, Dropbox or Microsoft OAuth app is configured",
		},
		{
			name: "an OAuth app with a public URL is accepted",
			configure: func(c *Config) {
				c.Services.DropboxAppKey, c.Services.DropboxAppSecret, c.Auth.PublicURL = "key", "secret", "https://app.example.com"
			},
		},
		{
			name: "an OAuth app needs its secret",
			configure: func(c *Config) {
				c.Services.MicrosoftClientID, c.Auth.PublicURL = "id", "https://app.example.com"
			},
			wantError: "each OAuth app needs both its client ID and its secret",
		},
		{
			name:      "unknown integration endpoint key",
			configure: func(c *Config) { c.Services.IntegrationEndpoints = `{"gmial_api": "http://x"}` },
			wantError: `unknown key "gmial_api"`,
		},
		{
			name:      "integration endpoints must be JSON",
			configure: func(c *Config) { c.Services.IntegrationEndpoints = `not json` },
			wantError: "INTEGRATION_ENDPOINTS must be a JSON object",
		},
		{
			name:      "negative session lifetime",
			configure: func(c *Config) { c.Auth.SessionTTL = -time.Hour },
			wantError: "SESSION_TTL cannot be negative",
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
			Mode:          "api",
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
	for _, key := range []string{"APP_ENV", "HTTP_ADDRESS", "SERVICE_NAME", "BUILD_VERSION", "LOG_LEVEL",
		"LOG_ADD_SOURCE", "DB_MAX_CONNS", "DB_MIN_CONNS", "DB_CONNECT_TIMEOUT", "WORKER_ID", "WORKER_CONCURRENCY",
		"WORKER_BATCH_SIZE", "WORKER_POLL_INTERVAL", "WORKER_LEASE_TTL", "SHUTDOWN_GRACE"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	t.Setenv("DATABASE_URL", "postgres://localhost/workflow")
	got, err := LoadConfig("worker")
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
	other, err := LoadConfig("worker")
	if err != nil || other.Worker.ID == got.Worker.ID {
		t.Fatalf("two replicas share WORKER_ID %q", got.Worker.ID)
	}
}

func TestLoadConfigRequiresDatabaseURL(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "")
	if _, err := LoadConfig("api"); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
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
