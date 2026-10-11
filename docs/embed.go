// Package docs provides the workflow language reference to the assistant.
package docs

import _ "embed"

//go:embed 02-workflow-spec.md
var WorkflowSpec string
