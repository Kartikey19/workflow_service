package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"uuid"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/executor/connectors"
	"github.com/kartikey/workflow_service/pkg/dbx"
	"github.com/kartikey/workflow_service/pkg/jsonschema"
	"github.com/kartikey/workflow_service/pkg/vault"
)

var connectorReference = regexp.MustCompile(`^connector:([a-z0-9][a-z0-9_-]{0,62}):([a-z][a-z0-9_]*\.[a-z][a-z0-9_]*)$`)

func ConnectorReference(ref string) (connection, action string, ok bool) {
	parts := connectorReference.FindStringSubmatch(ref)
	if parts == nil {
		return "", "", false
	}
	_, ok = connectors.LookupAction(parts[2])
	return parts[1], parts[2], ok
}
func IsDynamicReference(ref string) bool {
	_, _, ok := ConnectorReference(ref)
	return ok || IsMCPReference(ref)
}

type ConnectorTools struct {
	catalog *connectors.Catalog
	conns   executor.Connections
	remote  DynamicTools
}

func NewConnectorTools(c *connectors.Catalog, conns executor.Connections, remote DynamicTools) *ConnectorTools {
	return &ConnectorTools{c, conns, remote}
}
func (s *ConnectorTools) Resolve(ctx context.Context, tenant uuid.UUID, refs []string) ([]Tool, func(), error) {
	tools := []Tool{}
	remote := []string{}
	for _, ref := range refs {
		if IsMCPReference(ref) {
			remote = append(remote, ref)
			continue
		}
		connection, name, ok := ConnectorReference(ref)
		if !ok {
			return nil, func() {}, executor.Permanent("bad_config", fmt.Errorf("invalid connector reference %q", ref))
		}
		a, _ := connectors.LookupAction(name)
		if _, err := s.conns.Connection(ctx, tenant, connection, a.Kinds...); err != nil {
			cause := fmt.Errorf("load connector %q: %w", connection, err)
			if errors.Is(err, executor.ErrConnectionNotFound) || errors.Is(err, executor.ErrConnectionNeedsAuth) {
				return nil, func() {}, executor.Permanent("connector_tools", cause)
			}
			return nil, func() {}, executor.Retryable("connection_lookup", cause)
		}
		tools = append(tools, connectorTool{s.catalog, connection, a, ref})
	}
	cleanup := func() {}
	if len(remote) > 0 {
		if s.remote == nil {
			return nil, cleanup, errors.New("MCP tools unavailable")
		}
		found, closeTools, err := s.remote.Resolve(ctx, tenant, remote)
		if closeTools != nil {
			cleanup = closeTools
		}
		if err != nil {
			return nil, cleanup, err
		}
		tools = append(tools, found...)
	}
	return tools, cleanup, nil
}

type connectorTool struct {
	catalog    *connectors.Catalog
	connection string
	action     connectors.Action
	ref        string
}

func (t connectorTool) Name() string {
	hash := sha256.Sum256([]byte(t.ref))
	return "connector_" + strings.ReplaceAll(t.action.Name, ".", "_") + "_" + hex.EncodeToString(hash[:8])
}
func (t connectorTool) Description() string {
	access := "Read only"
	if !t.action.ReadOnly {
		access = "Writes to the external service; retries can repeat writes"
	}
	return t.action.Title + " using " + t.ref + ". " + access + ". The connector is fixed by the agent author."
}
func (t connectorTool) InputSchema() json.RawMessage { return t.action.InputSchema }
func (t connectorTool) Call(ctx context.Context, call Call, input json.RawMessage) (json.RawMessage, error) {
	return invokeConnector(ctx, t.catalog, call, t.action.Name, t.connection, t.InputSchema(), input)
}

func invokeConnector(ctx context.Context, catalog *connectors.Catalog, call Call, action, connection string, schema, input json.RawMessage) (output json.RawMessage, err error) {
	if connection == "" {
		return nil, errors.New("this agent has no database_connection")
	}
	var args any
	if err = json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("tool input: %w", err)
	}
	var contract any
	_ = json.Unmarshal(schema, &contract)
	if problems := jsonschema.Validate(contract, args); len(problems) > 0 {
		return nil, fmt.Errorf("tool arguments: %v", problems)
	}
	// Every tool is bounded; SQL agents also cap a supplied query timeout.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if action == "db.query" {
		fields := args.(map[string]any)
		timeout := dbx.DefaultTimeout
		if value, ok := fields["timeout"].(string); ok {
			d, e := time.ParseDuration(value)
			if e != nil || d <= 0 {
				return nil, errors.New("timeout must be a positive duration")
			}
			timeout = min(d, timeout)
		}
		fields["timeout"] = timeout.String()
		input, _ = json.Marshal(fields)
	}
	values := []string{}
	defer func() {
		if len(values) > 0 {
			for _, value := range values {
				output = vault.RedactJSON(output, value)
			}
			if err != nil {
				message := err.Error()
				for _, v := range values {
					message = strings.ReplaceAll(message, v, "***")
				}
				err = redactedToolError{err, message}
			}
		}
	}()
	result, err := catalog.Invoke(ctx, executor.Request{TenantID: call.TenantID, RunID: call.RunID, NodeID: call.NodeID, IdempotencyKey: call.RunID.String() + ":" + call.NodeID + ":" + call.ToolCallID, Redact: func(secrets ...string) {
		values = append(values, secrets...)
		if call.RedactSecrets != nil {
			call.RedactSecrets(secrets...)
		}
	}}, action, connection, input)
	if err != nil {
		return nil, err
	}
	output = result.Output
	if action == "db.query" {
		var rows dbx.Result
		if err = json.Unmarshal(output, &rows); err != nil {
			return nil, err
		}
		return databaseResult(rows)
	}
	if action == "mongo.find" {
		var result struct {
			Documents []json.RawMessage `json:"documents"`
			RowCount  int               `json:"row_count"`
			Truncated bool              `json:"truncated"`
		}
		_ = json.Unmarshal(output, &result)
		for len(output) > maxToolResultBytes && len(result.Documents) > 0 {
			result.Documents = result.Documents[:len(result.Documents)/2]
			result.RowCount = len(result.Documents)
			result.Truncated = true
			output, _ = json.Marshal(result)
		}
	}
	if len(output) > maxToolResultBytes {
		return nil, errors.New("connector tool result exceeds 32 KiB; narrow the query or page size")
	}
	return output, nil
}

type redactedToolError struct {
	error
	message string
}

func (e redactedToolError) Error() string { return e.message }
func (e redactedToolError) Unwrap() error { return e.error }
