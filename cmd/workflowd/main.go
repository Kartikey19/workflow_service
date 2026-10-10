// Command workflowd runs the API, the worker, or both (APP_MODE=api|worker|all).
// APP_MODE=all is the single-process setup for local development.
package main

import (
	"os"

	"github.com/kartikey/workflow_service/internal/app"
)

func main() { os.Exit(app.Main("workflowd", "")) }
