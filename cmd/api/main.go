// Command api serves the HTTP API. Run any number of replicas behind a proxy.
// "api migrate" applies pending database migrations and exits; it is the
// one-shot used before a deploy.
package main

import (
	"fmt"
	"os"

	"github.com/kartikey/workflow_service/internal/app"
)

func main() {
	switch {
	case len(os.Args) == 1:
		os.Exit(app.Main("api", "api"))
	case len(os.Args) == 2 && os.Args[1] == "migrate":
		os.Exit(app.Migrate("api"))
	default:
		fmt.Fprintln(os.Stderr, "usage: api [migrate]")
		os.Exit(2)
	}
}
