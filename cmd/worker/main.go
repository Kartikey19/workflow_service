// Command worker claims and executes node executions and runs the sweeper.
// Replicas are interchangeable; SIGTERM drains and releases leases.
package main

import (
	"os"

	"github.com/kartikey/workflow_service/internal/app"
)

func main() { os.Exit(app.Main("worker", "worker")) }
