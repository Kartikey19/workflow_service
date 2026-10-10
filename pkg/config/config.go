// Package config loads and validates service configuration.
package config

import (
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
	if err := LoadEnv(); err != nil {
		return nil, err
	}

	env, err := required("APP_ENV")
	if err != nil {
		return nil, err
	}
	mode, err := required("APP_MODE")
	if err != nil {
		return nil, err
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
		worker.ID, err = required("WORKER_ID")
		if err != nil {
			return nil, err
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
