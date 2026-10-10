// Package logger configures the process logger.
package logger

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Config struct {
	ServiceName string
	Environment string
	Version     string
	Level       string
	AddSource   bool
}

func NewLogger(cfg Config) (*zap.Logger, error) {
	if cfg.ServiceName == "" {
		return nil, fmt.Errorf("log service name is required")
	}
	if cfg.Environment == "" {
		return nil, fmt.Errorf("log environment is required")
	}
	if cfg.Version == "" {
		return nil, fmt.Errorf("log version is required")
	}

	level := zap.NewAtomicLevel()
	if err := level.UnmarshalText([]byte(strings.ToLower(cfg.Level))); err != nil {
		return nil, fmt.Errorf("parse log level: %w", err)
	}

	config := zap.NewProductionConfig()
	config.Level = level
	config.Encoding = "json"
	config.OutputPaths = []string{"stdout"}
	config.ErrorOutputPaths = []string{"stderr"}
	config.DisableCaller = !cfg.AddSource
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.LevelKey = "level"
	config.EncoderConfig.MessageKey = "message"
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	config.EncoderConfig.EncodeLevel = zapcore.LowercaseLevelEncoder

	return config.Build(zap.Fields(
		zap.String("service.name", cfg.ServiceName),
		zap.String("deployment.environment.name", cfg.Environment),
		zap.String("service.version", cfg.Version),
	))
}
