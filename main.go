package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kartikey/workflow_service/internal/app"
	"github.com/kartikey/workflow_service/pkg/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "workflowd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Restore normal signal handling when graceful shutdown begins, so a second
	// interrupt can terminate a process whose dependencies fail to drain.
	unregister := context.AfterFunc(ctx, stop)
	defer unregister()
	return app.Run(ctx, *cfg)
}
