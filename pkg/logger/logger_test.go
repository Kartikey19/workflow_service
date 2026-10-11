package logger

import (
	"strings"
	"testing"
)

func TestNewLoggerAcceptsValidConfig(t *testing.T) {
	logger, err := NewLogger(Config{
		ServiceName: "workflow-service",
		Environment: "testing",
		Version:     "test",
		Level:       "info",
	})
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	if logger == nil {
		t.Fatal("NewLogger() logger = nil, want logger")
	}
	defer func() { _ = logger.Sync() }()
}

func TestNewLoggerRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{
			name:   "missing service name",
			config: Config{Environment: "testing", Version: "test", Level: "info"},
			want:   "log service name is required",
		},
		{
			name:   "invalid level",
			config: Config{ServiceName: "workflow-service", Environment: "testing", Version: "test", Level: "verbose"},
			want:   "parse log level",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewLogger(tt.config)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewLogger() error = %v, want %q", err, tt.want)
			}
		})
	}
}
