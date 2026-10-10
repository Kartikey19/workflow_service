// Package migrations contains the service's immutable SQL migrations.
package migrations

import "embed"

// Files is embedded so migration commands work independently of the working directory.
//
//go:embed *.sql
var Files embed.FS
