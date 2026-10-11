// Package config loads and validates service configuration from the
// environment (and an optional .env file) using struct tags.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"github.com/kartikey/workflow_service/internal/core/executor/connectors"
	"github.com/kartikey/workflow_service/pkg/vault"
)

type Config struct {
	App       AppConfig
	Log       LogConfig
	Database  DatabaseConfig
	Worker    WorkerConfig
	Egress    EgressConfig
	LLM       LLMConfig
	Auth      AuthConfig
	Vault     VaultConfig
	Services  ServicesConfig
	Assistant AssistantConfig
}

type AssistantConfig struct {
	Provider string `env:"ASSISTANT_PROVIDER" envDefault:"anthropic"`
	APIKey   string `env:"ASSISTANT_API_KEY"`
	Model    string `env:"ASSISTANT_MODEL"`
}

type AppConfig struct {
	Environment   string        `env:"APP_ENV" envDefault:"development"` // development | testing | production
	Mode          string        `env:"-"`                                // api | worker; set by the binary
	HTTPAddress   string        `env:"HTTP_ADDRESS" envDefault:":8080"`
	ServiceName   string        `env:"SERVICE_NAME" envDefault:"workflow-service"`
	BuildVersion  string        `env:"BUILD_VERSION" envDefault:"dev"`
	ShutdownGrace time.Duration `env:"SHUTDOWN_GRACE" envDefault:"30s"`
}

type LogConfig struct {
	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	AddSource bool   `env:"LOG_ADD_SOURCE" envDefault:"false"`
}

type DatabaseConfig struct {
	URL            string        `env:"DATABASE_URL,required,notEmpty"`
	MaxConns       int32         `env:"DB_MAX_CONNS" envDefault:"10"`
	MinConns       int32         `env:"DB_MIN_CONNS" envDefault:"0"`
	ConnectTimeout time.Duration `env:"DB_CONNECT_TIMEOUT" envDefault:"5s"`
}

type WorkerConfig struct {
	// ID defaults to the hostname plus a random suffix, so
	// `docker compose up --scale worker=N` gives every replica its own ID.
	ID            string        `env:"WORKER_ID"`
	Concurrency   int           `env:"WORKER_CONCURRENCY" envDefault:"32"`
	BatchSize     int           `env:"WORKER_BATCH_SIZE" envDefault:"8"`
	PollInterval  time.Duration `env:"WORKER_POLL_INTERVAL" envDefault:"200ms"`
	LeaseTTL      time.Duration `env:"WORKER_LEASE_TTL" envDefault:"60s"`
	SweepInterval time.Duration `env:"WORKER_SWEEP_INTERVAL" envDefault:"5s"`
	// MetricsAddress serves /metrics and /healthz in worker mode; empty disables it.
	MetricsAddress string `env:"WORKER_METRICS_ADDRESS"`
	// MaxNodesPerRun bounds node executions per run (E_RUN_BUDGET).
	MaxNodesPerRun int `env:"MAX_NODES_PER_RUN" envDefault:"1000"`
}

// EgressConfig widens internal/netguard for outbound calls made on a tenant's
// behalf. By default loopback, private and metadata addresses are blocked.
type EgressConfig struct {
	AllowCIDRs   string `env:"EGRESS_ALLOW_CIDRS"`                      // comma separated
	AllowPrivate bool   `env:"EGRESS_ALLOW_PRIVATE" envDefault:"false"` // local development only
}

// AuthConfig configures sign-in and the first administrator.
type AuthConfig struct {
	SessionTTL time.Duration `env:"SESSION_TTL" envDefault:"336h"`
	// CookieSecure marks the session cookie Secure; production requires it.
	CookieSecure bool `env:"COOKIE_SECURE" envDefault:"false"`
	// PublicURL is the externally visible base URL, used for invite links.
	PublicURL string `env:"PUBLIC_URL"`
	// The first platform admin is created from these when no user exists.
	BootstrapAdminEmail    string `env:"BOOTSTRAP_ADMIN_EMAIL"`
	BootstrapAdminPassword string `env:"BOOTSTRAP_ADMIN_PASSWORD"`
}

// VaultConfig configures secret encryption. Without a master key the vault is
// off and /be/v1/secrets answers 501. To rotate: set the new key as VAULT_MASTER_KEY
// with a higher VAULT_KEY_VERSION, list the previous key in VAULT_OLD_KEYS, restart,
// POST /be/v1/admin/vault/rotate, and remove the old key once nothing uses it.
type VaultConfig struct {
	MasterKey  string `env:"VAULT_MASTER_KEY"`                 // 32 bytes, base64
	KeyVersion int    `env:"VAULT_KEY_VERSION" envDefault:"1"` // version of VAULT_MASTER_KEY
	OldKeys    string `env:"VAULT_OLD_KEYS"`                   // "1:base64,2:base64"
}

// ServicesConfig points the built-in integrations at different endpoints, for a
// corporate proxy or a test server. Empty keeps the public endpoint. Tenants
// cannot set these.
type ServicesConfig struct {
	ResendBaseURL string `env:"RESEND_BASE_URL"` // default https://api.resend.com

	// OAuth apps registered with each provider. A provider without a client ID is
	// hidden: its integrations are not offered.
	GoogleClientID        string `env:"GOOGLE_CLIENT_ID"`
	GoogleClientSecret    string `env:"GOOGLE_CLIENT_SECRET"`
	DropboxAppKey         string `env:"DROPBOX_APP_KEY"`
	DropboxAppSecret      string `env:"DROPBOX_APP_SECRET"`
	MicrosoftClientID     string `env:"MICROSOFT_CLIENT_ID"`
	MicrosoftClientSecret string `env:"MICROSOFT_CLIENT_SECRET"`
	MicrosoftTenant       string `env:"MICROSOFT_TENANT" envDefault:"common"`

	// IntegrationEndpoints overrides provider URLs, as a JSON object, for a proxy
	// or a test server. Keys: google_auth, google_token, google_userinfo, gmail_api,
	// gdrive_api, gdrive_upload, dropbox_auth, dropbox_token, dropbox_api,
	// dropbox_content, microsoft_auth, microsoft_token, graph_api.
	IntegrationEndpoints string `env:"INTEGRATION_ENDPOINTS"`
}

// LLMConfig configures the platform-managed model provider used by agent
// nodes. Without a key, workflows with agent nodes still publish but their
// nodes fail permanently with llm_not_configured.
type LLMConfig struct {
	AnthropicAPIKey  string `env:"ANTHROPIC_API_KEY"`
	AnthropicBaseURL string `env:"ANTHROPIC_BASE_URL"` // default https://api.anthropic.com
	// OpenAIBaseURL and GeminiBaseURL let an operator route those providers
	// through a proxy. Tenants cannot choose a URL for them; only for
	// openai_compatible connections.
	OpenAIBaseURL string `env:"OPENAI_BASE_URL"` // default https://api.openai.com/v1
	GeminiBaseURL string `env:"GEMINI_BASE_URL"` // default https://generativelanguage.googleapis.com
	// PricingJSON overrides the built-in per-model price estimates, e.g.
	// {"claude-sonnet-5-5": {"input_per_mtok": 3, "output_per_mtok": 15}}.
	PricingJSON string `env:"LLM_PRICING_JSON"`
}

// LoadEnv loads an optional .env from the working directory. Shell values win.
func LoadEnv() error {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Parser errors may include secret values from the file.
		return errors.New("cannot load .env: check file permissions and syntax")
	}
	return nil
}

// LoadConfig reads .env and the environment for a dedicated entry point
// (cmd/api, cmd/worker): mode is "api" or "worker".
func LoadConfig(mode string) (*Config, error) {
	if err := LoadEnv(); err != nil {
		return nil, err
	}
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("parse environment: %w", err)
	}
	cfg.App.Mode = mode
	if strings.TrimSpace(cfg.Worker.ID) == "" {
		cfg.Worker.ID = defaultWorkerID()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// defaultWorkerID is the hostname plus a random suffix.
func defaultWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	return host + "-" + hex.EncodeToString(suffix)
}

// Validate collects every configuration error rather than the first.
func (c *Config) Validate() error {
	var errs []error
	add := func(message string) { errs = append(errs, errors.New(message)) }
	switch c.App.Environment {
	case "development", "testing", "production":
	default:
		add("APP_ENV must be development, testing, or production")
	}
	switch c.App.Mode {
	case "api", "worker":
	default:
		add("mode must be api or worker")
	}
	switch c.Log.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add("LOG_LEVEL must be debug, info, warn, or error")
	}
	if c.Assistant.APIKey != "" {
		switch c.Assistant.Provider {
		case "anthropic", "openai", "gemini":
		default:
			add("ASSISTANT_PROVIDER must be anthropic, openai or gemini")
		}
		if strings.TrimSpace(c.Assistant.Model) == "" {
			add("ASSISTANT_MODEL is required with ASSISTANT_API_KEY")
		}
	}
	if c.Database.URL == "" {
		add("DATABASE_URL is required")
	}
	if c.Database.MaxConns <= 0 {
		add("DB_MAX_CONNS must be greater than 0")
	}
	if c.Database.MinConns < 0 {
		add("DB_MIN_CONNS cannot be negative")
	}
	if c.Database.MinConns > c.Database.MaxConns {
		add("DB_MIN_CONNS cannot exceed DB_MAX_CONNS")
	}
	if c.Database.ConnectTimeout <= 0 {
		add("DB_CONNECT_TIMEOUT must be greater than 0")
	}
	if c.App.Mode != "worker" && strings.TrimSpace(c.App.HTTPAddress) == "" {
		add("HTTP_ADDRESS is required for api/all mode")
	}
	if c.App.Mode != "api" {
		if strings.TrimSpace(c.Worker.ID) == "" {
			add("WORKER_ID is required for worker/all mode")
		}
		if c.Worker.Concurrency <= 0 {
			add("WORKER_CONCURRENCY must be greater than 0")
		}
		if c.Worker.BatchSize < 1 || c.Worker.BatchSize > c.Worker.Concurrency {
			add("WORKER_BATCH_SIZE must be between 1 and WORKER_CONCURRENCY")
		}
		if c.Worker.PollInterval <= 0 {
			add("WORKER_POLL_INTERVAL must be greater than 0")
		}
		if c.Worker.LeaseTTL <= c.Worker.PollInterval {
			add("WORKER_LEASE_TTL must exceed WORKER_POLL_INTERVAL")
		}
		if c.Worker.SweepInterval < 0 {
			add("WORKER_SWEEP_INTERVAL cannot be negative")
		}
	}
	if c.App.ShutdownGrace <= 0 {
		add("SHUTDOWN_GRACE must be greater than 0")
	}
	if c.Auth.SessionTTL < 0 {
		add("SESSION_TTL cannot be negative")
	}
	if c.App.Environment == "production" && !c.Auth.CookieSecure {
		add("COOKIE_SECURE must be true in production")
	}
	if c.Vault.MasterKey != "" {
		if _, err := vault.ParseKeyring(c.Vault.MasterKey, c.Vault.KeyVersion, c.Vault.OldKeys); err != nil {
			add("VAULT_MASTER_KEY / VAULT_KEY_VERSION / VAULT_OLD_KEYS: " + strings.TrimPrefix(err.Error(), "vault: "))
		}
	}
	if c.Services.GoogleClientID != "" || c.Services.DropboxAppKey != "" || c.Services.MicrosoftClientID != "" {
		if c.Auth.PublicURL == "" {
			add("PUBLIC_URL is required when a Google, Dropbox or Microsoft OAuth app is configured: providers redirect back to it")
		}
		if (c.Services.GoogleClientID == "") != (c.Services.GoogleClientSecret == "") ||
			(c.Services.DropboxAppKey == "") != (c.Services.DropboxAppSecret == "") ||
			(c.Services.MicrosoftClientID == "") != (c.Services.MicrosoftClientSecret == "") {
			add("each OAuth app needs both its client ID and its secret")
		}
	}
	if _, err := connectors.ParseEndpoints(c.Services.IntegrationEndpoints); err != nil {
		add(err.Error())
	}
	if (c.Auth.BootstrapAdminEmail == "") != (c.Auth.BootstrapAdminPassword == "") {
		add("BOOTSTRAP_ADMIN_EMAIL and BOOTSTRAP_ADMIN_PASSWORD must be set together")
	}
	return errors.Join(errs...)
}
