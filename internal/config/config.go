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
)

type Config struct {
	App      AppConfig
	Log      LogConfig
	Database DatabaseConfig
	Worker   WorkerConfig
	Egress   EgressConfig
}

type AppConfig struct {
	Environment   string        `env:"APP_ENV" envDefault:"development"` // development | testing | production
	Mode          string        `env:"APP_MODE" envDefault:"all"`        // api | worker | all
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

// LoadEnv loads an optional .env from the working directory. Shell values win.
func LoadEnv() error {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Parser errors may include secret values from the file.
		return errors.New("cannot load .env: check file permissions and syntax")
	}
	return nil
}

// LoadConfig reads .env and the environment, applies defaults and validates.
func LoadConfig() (*Config, error) {
	return LoadConfigMode("")
}

// LoadConfigMode is LoadConfig for a dedicated entry point (cmd/api,
// cmd/worker): a non-empty mode replaces APP_MODE.
func LoadConfigMode(mode string) (*Config, error) {
	if err := LoadEnv(); err != nil {
		return nil, err
	}
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("parse environment: %w", err)
	}
	if mode != "" {
		cfg.App.Mode = mode
	}
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
	case "api", "worker", "all":
	default:
		add("APP_MODE must be api, worker, or all")
	}
	switch c.Log.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add("LOG_LEVEL must be debug, info, warn, or error")
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
	return errors.Join(errs...)
}
