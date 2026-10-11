package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kartikey/workflow_service/internal/config"
)

// Main is the body of every service binary: it loads configuration, owns OS
// signals and returns the exit status. mode is "api" or "worker".
func Main(name, mode string) int {
	if len(os.Args) > 1 {
		if mode == "api" && len(os.Args) == 2 && os.Args[1] == "migrate" {
			return Migrate(name)
		}
		if mode == "api" && len(os.Args) == 3 && os.Args[1] == "healthcheck" {
			return healthcheck(os.Args[2])
		}
		fmt.Fprintln(os.Stderr, "usage: "+name+" [migrate | healthcheck URL]")
		return 2
	}
	cfg, err := config.LoadConfig(mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: load configuration: %v\n", name, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Restore normal signal handling when graceful shutdown begins, so a second
	// interrupt can terminate a process whose dependencies fail to drain.
	unregister := context.AfterFunc(ctx, stop)
	defer unregister()
	if err := Run(ctx, *cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}
