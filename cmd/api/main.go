// Command api serves HTTP; api migrate applies pending migrations.
package main

import (
	"github.com/kartikey/workflow_service/internal/app"
	"os"
)

func main() { os.Exit(app.Main("api", "api")) }
