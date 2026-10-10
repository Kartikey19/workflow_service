// Command api serves the HTTP API. Run any number of replicas behind a proxy.
package main

import (
	"os"

	"github.com/kartikey/workflow_service/internal/app"
)

func main() { os.Exit(app.Main("api", "api")) }
