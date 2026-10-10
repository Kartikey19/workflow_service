// Package config loads and validates service configuration.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	App      AppConfig
	Log      LogConfig
	Database DatabaseConfig
	Worker   WorkerConfig
	Egress   EgressConfig
}

// EgressConfig widens internal/netguard for outbound calls made on a tenant's
// behalf. By default loopback, private and metadata addresses are blocked.
type EgressConfig struct {
	AllowCIDRs   string // EGRESS_ALLOW_CIDRS, comma separated
	AllowPrivate bool   // EGRESS_ALLOW_PRIVATE; local development only
}

type AppConfig struct {
	Environment   string
	Mode          string
	HTTPAddress   string
	ServiceName   string
	BuildVersion  string
	ShutdownGrace time.Duration // SHUTDOWN_GRACE
}

type LogConfig struct {
	LogLevel  string
	AddSource bool
}

type DatabaseConfig struct {
	URL            string        // DATABASE_URL
	MaxConns       int32         // DB_MAX_CONNS
	MinConns       int32         // DB_MIN_CONNS
	ConnectTimeout time.Duration // DB_CONNECT_TIMEOUT
}

type WorkerConfig struct {
	ID           string        // WORKER_ID
	Concurrency  int           // WORKER_CONCURRENCY
	BatchSize    int           // WORKER_BATCH_SIZE
	PollInterval time.Duration // WORKER_POLL_INTERVAL
	LeaseTTL     time.Duration // WORKER_LEASE_TTL
	// SweepInterval is optional (WORKER_SWEEP_INTERVAL, default 5s).
	SweepInterval time.Duration
	// MetricsAddress serves /metrics and /healthz in worker mode
	// (WORKER_METRICS_ADDRESS, optional; empty disables it).
	MetricsAddress string
	// MaxNodesPerRun bounds node executions per run (MAX_NODES_PER_RUN, default 1000).
	MaxNodesPerRun int
}

// LoadEnv loads an optional .env from the working directory. Shell values win.
func LoadEnv() error {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Parser errors may include secret values from the file.
		return errors.New("cannot load .env: check file permissions and syntax")
	}
	return nil
}

func LoadConfig() (*Config, error) {
	return LoadConfigMode("")
}

// LoadConfigMode is LoadConfig for a dedicated entry point (cmd/api, cmd/worker):
// a non-empty mode replaces APP_MODE, which is then not required.
func LoadConfigMode(fixedMode string) (*Config, error) {
	if err := LoadEnv(); err != nil {
		return nil, err
	}

	env, err := required("APP_ENV")
	if err != nil {
		return nil, err
	}
	mode := fixedMode
	if mode == "" {
		mode, err = required("APP_MODE")
		if err != nil {
			return nil, err
		}
	}
	var addr string
	if mode != "worker" {
		addr, err = required("HTTP_ADDRESS")
		if err != nil {
			return nil, err
		}
	}
	serviceName, err := required("SERVICE_NAME")
	if err != nil {
		return nil, err
	}
	buildVersion, err := required("BUILD_VERSION")
	if err != nil {
		return nil, err
	}
	logLevel, err := required("LOG_LEVEL")
	if err != nil {
		return nil, err
	}
	addSource, err := boolFromEnv("LOG_ADD_SOURCE")
	if err != nil {
		return nil, err
	}
	dbURL, err := required("DATABASE_URL")
	if err != nil {
		return nil, err
	}
	dbMaxConns, err := int32FromEnv("DB_MAX_CONNS")
	if err != nil {
		return nil, err
	}
	dbMinConns, err := int32FromEnv("DB_MIN_CONNS")
	if err != nil {
		return nil, err
	}
	dbConnectTimeout, err := durationFromEnv("DB_CONNECT_TIMEOUT")
	if err != nil {
		return nil, err
	}
	var worker WorkerConfig
	if mode != "api" {
		// Unique per replica so `docker compose up --scale worker=N` works.
		worker.ID = strings.TrimSpace(os.Getenv("WORKER_ID"))
		if worker.ID == "" {
			worker.ID = defaultWorkerID()
		}
		worker.Concurrency, err = intFromEnv("WORKER_CONCURRENCY")
		if err != nil {
			return nil, err
		}
		worker.BatchSize, err = intFromEnv("WORKER_BATCH_SIZE")
		if err != nil {
			return nil, err
		}
		worker.PollInterval, err = durationFromEnv("WORKER_POLL_INTERVAL")
		if err != nil {
			return nil, err
		}
		worker.LeaseTTL, err = durationFromEnv("WORKER_LEASE_TTL")
		if err != nil {
			return nil, err
		}
		if worker.SweepInterval, err = optionalDuration("WORKER_SWEEP_INTERVAL", 5*time.Second); err != nil {
			return nil, err
		}
		worker.MetricsAddress = strings.TrimSpace(os.Getenv("WORKER_METRICS_ADDRESS"))
		if worker.MaxNodesPerRun, err = optionalInt("MAX_NODES_PER_RUN", 1000); err != nil {
			return nil, err
		}
	}
	allowPrivate, err := optionalBool("EGRESS_ALLOW_PRIVATE", false)
	if err != nil {
		return nil, err
	}
	shutdownGrace, err := durationFromEnv("SHUTDOWN_GRACE")
	if err != nil {
		return nil, err
	}
	config := &Config{
		App: AppConfig{
			Environment:   env,
			Mode:          mode,
			HTTPAddress:   addr,
			ServiceName:   serviceName,
			BuildVersion:  buildVersion,
			ShutdownGrace: shutdownGrace,
		},
		Log: LogConfig{
			LogLevel:  logLevel,
			AddSource: addSource,
		},
		Database: DatabaseConfig{
			URL:            dbURL,
			MaxConns:       dbMaxConns,
			MinConns:       dbMinConns,
			ConnectTimeout: dbConnectTimeout,
		},
		Worker: worker,
		Egress: EgressConfig{
			AllowCIDRs:   strings.TrimSpace(os.Getenv("EGRESS_ALLOW_CIDRS")),
			AllowPrivate: allowPrivate,
		},
	}

	if err := config.Validate(); err != nil {
		return nil, err
	}

	return config, nil

}

func required(name string) (string, error) {
	value, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", name)
	}

	return value, nil
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

func optionalDuration(name string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(os.Getenv(name)) == "" {
		return fallback, nil
	}
	return durationFromEnv(name)
}

func optionalInt(name string, fallback int) (int, error) {
	if strings.TrimSpace(os.Getenv(name)) == "" {
		return fallback, nil
	}
	return intFromEnv(name)
}

func optionalBool(name string, fallback bool) (bool, error) {
	if strings.TrimSpace(os.Getenv(name)) == "" {
		return fallback, nil
	}
	return boolFromEnv(name)
}

func optionalString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func boolFromEnv(name string) (bool, error) {
	value, err := required(name)
	if err != nil {
		return false, err
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}

	return parsed, nil
}

func int32FromEnv(name string) (int32, error) {
	value, err := required(name)
	if err != nil {
		return 0, err
	}

	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be a 32-bit integer: %w", name, err)
	}

	return int32(parsed), nil
}

func intFromEnv(name string) (int, error) {
	value, err := required(name)
	if err != nil {
		return 0, err
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}

	return parsed, nil
}

func durationFromEnv(name string) (time.Duration, error) {
	value, err := required(name)
	if err != nil {
		return 0, err
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration: %w", name, err)
	}

	return parsed, nil
}

func (c *Config) Validate() error {
	var errs []error
	switch c.App.Environment {
	case "development", "testing", "production":
		break
	default:
		errs = append(errs, fmt.Errorf(
			"APP_ENV must be development, testing, or production",
		))
	}

	switch c.App.Mode {
	case "api", "worker", "all":
		break
	default:
		errs = append(errs, fmt.Errorf(
			"APP_MODE must be api, worker, or all",
		))
	}

	switch c.Log.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf(
			"LOG_LEVEL must be debug, info, warn, or error",
		))
	}

	if c.Database.URL == "" {
		errs = append(errs, fmt.Errorf("DATABASE_URL is required"))
	}

	if c.Database.MaxConns <= 0 {
		errs = append(errs, fmt.Errorf("DB_MAX_CONNS must be greater than 0"))
	}

	if c.Database.MinConns < 0 {
		errs = append(errs, fmt.Errorf("DB_MIN_CONNS cannot be negative"))
	}

	if c.Database.MinConns > c.Database.MaxConns {
		errs = append(errs, fmt.Errorf(
			"DB_MIN_CONNS cannot exceed DB_MAX_CONNS",
		))
	}

	if c.App.Mode != "worker" && strings.TrimSpace(c.App.HTTPAddress) == "" {
		errs = append(errs, errors.New("HTTP_ADDRESS is required for api/all mode"))
	}
	if c.Database.ConnectTimeout <= 0 {
		errs = append(errs, errors.New("DB_CONNECT_TIMEOUT must be greater than 0"))
	}
	if c.App.Mode != "api" {
		if strings.TrimSpace(c.Worker.ID) == "" {
			errs = append(errs, errors.New("WORKER_ID is required for worker/all mode"))
		}
		if c.Worker.Concurrency <= 0 {
			errs = append(errs, fmt.Errorf(
				"WORKER_CONCURRENCY must be greater than 0",
			))
		}

		if c.Worker.BatchSize < 1 || c.Worker.BatchSize > c.Worker.Concurrency {
			errs = append(errs, fmt.Errorf(
				"WORKER_BATCH_SIZE must be between 1 and WORKER_CONCURRENCY",
			))
		}

		if c.Worker.PollInterval <= 0 {
			errs = append(errs, fmt.Errorf(
				"WORKER_POLL_INTERVAL must be greater than 0",
			))
		}

		if c.Worker.LeaseTTL <= c.Worker.PollInterval {
			errs = append(errs, fmt.Errorf(
				"WORKER_LEASE_TTL must exceed WORKER_POLL_INTERVAL",
			))
		}

	}
	if c.App.ShutdownGrace <= 0 {
		errs = append(errs, fmt.Errorf(
			"SHUTDOWN_GRACE must be greater than 0",
		))
	}

	return errors.Join(errs...)

}
