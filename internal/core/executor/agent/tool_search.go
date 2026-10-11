package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"uuid"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/executor/connectors"
	"github.com/kartikey/workflow_service/pkg/dbx"
	"github.com/kartikey/workflow_service/pkg/vault"
)

// An agent's tools are the connectors its author attached, searched through
// three read-only tools, much like a coding agent searches files: glob lists
// what a connector holds, grep searches it, read opens one item. Writing (send
// an email, insert a row) is a workflow step, never an agent tool.

var connectorName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// IsToolReference says whether ref can name an agent tool: a saved connector,
// or "mcp:<server>" for a saved MCP server whose tools are discovered per attempt.
func IsToolReference(ref string) bool {
	return connectorName.MatchString(ref) || IsMCPReference(ref)
}

// operation maps one of glob, grep or read onto a connector action.
type operation struct {
	action string
	args   func(searchInput) (map[string]any, error)
	// filter post-processes the action's output, for kinds whose API cannot.
	filter func(json.RawMessage, searchInput) (json.RawMessage, error)
}

// searchable says how glob, grep and read work for one connector kind and how
// to describe its arguments to the model. A nil operation is not offered.
type searchable struct {
	glob, grep, read *operation
	usage            string
}

type searchInput struct {
	Connection string `json:"connection"`
	Pattern    string `json:"pattern"`
	Query      string `json:"query"`
	In         string `json:"in"`
	ID         string `json:"id"`
	Range      string `json:"range"`
	Limit      int    `json:"limit"`
}

func limit(in searchInput, fallback int) int {
	if in.Limit > 0 {
		return min(in.Limit, 100)
	}
	return fallback
}

func required(value, name string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required for this connector", name)
	}
	return nil
}

var database = searchable{
	glob: &operation{action: "db.list_tables", args: func(searchInput) (map[string]any, error) { return map[string]any{}, nil },
		filter: namesMatching("tables", func(t map[string]any) string { return fmt.Sprint(t["name"]) })},
	grep: &operation{action: "db.query", args: func(in searchInput) (map[string]any, error) {
		return map[string]any{"sql": in.Query, "max_rows": limit(in, 50)}, required(in.Query, "query (a read-only SQL statement)")
	}},
	read: &operation{action: "db.describe_table", args: func(in searchInput) (map[string]any, error) {
		return map[string]any{"table": in.ID}, required(in.ID, "id (a table name)")
	}},
	usage: "glob lists tables (pattern such as orders_*); grep runs one read-only SQL statement given as query; read describes the columns of the table named by id",
}

var searchableKinds = map[string]searchable{
	"postgres": database,
	"mysql":    database,
	"gmail": {
		glob: &operation{action: "gmail.search", args: func(in searchInput) (map[string]any, error) {
			return map[string]any{"query": in.Pattern, "max_results": limit(in, 20)}, nil
		}},
		grep: &operation{action: "gmail.search", args: func(in searchInput) (map[string]any, error) {
			return map[string]any{"query": in.Query, "max_results": limit(in, 20)}, required(in.Query, "query")
		}},
		read: &operation{action: "gmail.read", args: func(in searchInput) (map[string]any, error) {
			return map[string]any{"message_id": in.ID}, required(in.ID, "id (a message id)")
		}},
		usage: "glob lists recent messages; grep searches with Gmail syntax such as from:ana subject:invoice newer_than:7d; read opens the message whose id glob or grep returned",
	},
	"gcalendar": {
		glob: &operation{action: "gcalendar.list", args: func(in searchInput) (map[string]any, error) {
			return map[string]any{"max_results": limit(in, 20)}, nil
		}},
		grep: &operation{action: "gcalendar.list", args: func(in searchInput) (map[string]any, error) {
			return map[string]any{"query": in.Query, "max_results": limit(in, 20)}, required(in.Query, "query")
		}},
		usage: "glob lists upcoming events; grep finds events matching text",
	},
	"gdocs": {
		read: &operation{action: "gdocs.read", args: func(in searchInput) (map[string]any, error) {
			return map[string]any{"document_id": in.ID}, required(in.ID, "id (a document id)")
		}},
		usage: "read opens the document whose id is given",
	},
	"gsheets": {
		read: &operation{action: "gsheets.read", args: func(in searchInput) (map[string]any, error) {
			r := in.Range
			if r == "" {
				r = "A1:Z200"
			}
			return map[string]any{"spreadsheet_id": in.ID, "range": r}, required(in.ID, "id (a spreadsheet id)")
		}},
		usage: "read returns cells of the spreadsheet whose id is given, optionally a range such as Sheet1!A1:D50",
	},
	"mongo": {
		glob: &operation{action: "mongo.list_collections", args: func(searchInput) (map[string]any, error) { return map[string]any{}, nil },
			filter: namesMatching("collections", nil)},
		grep: &operation{action: "mongo.find", args: func(in searchInput) (map[string]any, error) {
			var filter map[string]any
			if err := json.Unmarshal([]byte(in.Query), &filter); err != nil {
				return nil, errors.New("query must be a MongoDB filter as a JSON object")
			}
			return map[string]any{"collection": in.In, "filter": filter, "max_rows": limit(in, 50)}, required(in.In, "in (a collection)")
		}},
		read: &operation{action: "mongo.find", args: func(in searchInput) (map[string]any, error) {
			return map[string]any{"collection": in.ID, "max_rows": limit(in, 20)}, required(in.ID, "id (a collection)")
		}},
		usage: `glob lists collections; grep finds documents in the collection named by in, with query a JSON filter such as {"status":"open"}; read returns the first documents of the collection named by id`,
	},
	"slack": {
		grep: &operation{action: "slack.history", args: func(in searchInput) (map[string]any, error) {
			return map[string]any{"channel": in.In, "limit": 100}, errors.Join(required(in.In, "in (a channel id)"), required(in.Query, "query"))
		}, filter: messagesContaining},
		read: &operation{action: "slack.history", args: func(in searchInput) (map[string]any, error) {
			return map[string]any{"channel": in.ID, "limit": limit(in, 50)}, required(in.ID, "id (a channel id)")
		}},
		usage: "grep finds recent messages containing query in the channel named by in; read returns recent messages of the channel named by id",
	},
	"notion": {
		glob: &operation{action: "notion.search", args: func(in searchInput) (map[string]any, error) {
			return map[string]any{"page_size": limit(in, 20)}, nil
		}},
		grep: &operation{action: "notion.search", args: func(in searchInput) (map[string]any, error) {
			return map[string]any{"query": in.Query, "page_size": limit(in, 20)}, required(in.Query, "query")
		}},
		read: &operation{action: "notion.blocks", args: func(in searchInput) (map[string]any, error) {
			return map[string]any{"block_id": in.ID}, required(in.ID, "id (a page id)")
		}},
		usage: "glob lists pages; grep searches pages by title; read returns the content of the page whose id is given",
	},
}

// searchableKindNames is SearchableKinds, computed once.
var searchableKindNames = func() []string {
	kinds := make([]string, 0, len(searchableKinds))
	for kind := range searchableKinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}()

// SearchableKinds lists the connector kinds an agent can be given, sorted.
// Callers must not modify the result.
func SearchableKinds() []string { return searchableKindNames }

// namesMatching keeps the entries of output[field] whose name matches the glob
// pattern. name extracts a name from an object entry; nil means plain strings.
func namesMatching(field string, name func(map[string]any) string) func(json.RawMessage, searchInput) (json.RawMessage, error) {
	return func(output json.RawMessage, in searchInput) (json.RawMessage, error) {
		if in.Pattern == "" {
			return output, nil
		}
		if _, err := path.Match(in.Pattern, ""); err != nil {
			return nil, fmt.Errorf("pattern: %w", err)
		}
		var result map[string][]any
		if err := json.Unmarshal(output, &result); err != nil {
			return nil, err
		}
		kept := []any{}
		for _, entry := range result[field] {
			text := fmt.Sprint(entry)
			if object, ok := entry.(map[string]any); ok && name != nil {
				text = name(object)
			}
			if ok, _ := path.Match(in.Pattern, text); ok {
				kept = append(kept, entry)
			}
		}
		return json.Marshal(map[string]any{field: kept})
	}
}

func messagesContaining(output json.RawMessage, in searchInput) (json.RawMessage, error) {
	var result struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, err
	}
	kept := []map[string]any{}
	for _, message := range result.Messages {
		if strings.Contains(strings.ToLower(fmt.Sprint(message["text"])), strings.ToLower(in.Query)) {
			kept = append(kept, message)
		}
	}
	return json.Marshal(map[string]any{"messages": kept})
}

// ConnectorTools resolves an agent's tool references per attempt: connectors
// become glob, grep and read; MCP servers are discovered through remote.
type ConnectorTools struct {
	catalog *connectors.Catalog
	conns   executor.Connections
	remote  DynamicTools
}

func NewConnectorTools(c *connectors.Catalog, conns executor.Connections, remote DynamicTools) *ConnectorTools {
	return &ConnectorTools{c, conns, remote}
}

func (s *ConnectorTools) Resolve(ctx context.Context, tenant uuid.UUID, refs []string) ([]Tool, func(), error) {
	var sources []source
	var remote []string
	for _, ref := range refs {
		if IsMCPReference(ref) {
			remote = append(remote, ref)
			continue
		}
		conn, err := s.conns.Connection(ctx, tenant, ref)
		if err != nil {
			cause := fmt.Errorf("load connector %q: %w", ref, err)
			if errors.Is(err, executor.ErrConnectionNotFound) || errors.Is(err, executor.ErrConnectionNeedsAuth) {
				return nil, func() {}, executor.Permanent("connector_tools", cause)
			}
			return nil, func() {}, executor.Retryable("connection_lookup", cause)
		}
		if _, ok := searchableKinds[conn.Kind]; !ok {
			return nil, func() {}, executor.Permanent("connector_tools", fmt.Errorf("connector %q (%s) cannot be searched by an agent", ref, conn.Kind))
		}
		sources = append(sources, source{ref, conn.Kind})
	}
	var tools []Tool
	for _, verb := range []string{"glob", "grep", "read"} {
		tool := searchTool{verb: verb, catalog: s.catalog}
		for _, src := range sources {
			if op(searchableKinds[src.kind], verb) != nil {
				tool.sources = append(tool.sources, src)
			}
		}
		if len(tool.sources) > 0 {
			tools = append(tools, tool)
		}
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

type source struct{ name, kind string }

func op(s searchable, verb string) *operation {
	switch verb {
	case "glob":
		return s.glob
	case "grep":
		return s.grep
	default:
		return s.read
	}
}

// searchTool is glob, grep or read over the connectors that support it.
type searchTool struct {
	verb    string
	catalog *connectors.Catalog
	sources []source
}

func (t searchTool) Name() string { return t.verb }

func (t searchTool) Description() string {
	summary := map[string]string{
		"glob": "Lists what a connector holds: tables, collections, recent messages, events or pages.",
		"grep": "Searches a connector and returns the matching items.",
		"read": "Opens one item of a connector by the id that glob or grep returned.",
	}[t.verb]
	lines := []string{summary + " Read only. Connectors:"}
	for _, src := range t.sources {
		lines = append(lines, fmt.Sprintf("- %s (%s): %s.", src.name, src.kind, searchableKinds[src.kind].usage))
	}
	return strings.Join(lines, "\n")
}

func (t searchTool) InputSchema() json.RawMessage {
	names := make([]string, 0, len(t.sources))
	for _, src := range t.sources {
		names = append(names, src.name)
	}
	properties := map[string]any{
		"connection": map[string]any{"type": "string", "enum": names},
		"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
	}
	required := []string{"connection"}
	switch t.verb {
	case "glob":
		properties["pattern"] = map[string]any{"type": "string", "description": "a name pattern such as orders_*, or search text for mail"}
	case "grep":
		properties["query"] = map[string]any{"type": "string"}
		properties["in"] = map[string]any{"type": "string", "description": "the collection or channel to search, where the connector needs one"}
		required = append(required, "query")
	case "read":
		properties["id"] = map[string]any{"type": "string"}
		properties["range"] = map[string]any{"type": "string", "description": "spreadsheet range, for spreadsheets only"}
		required = append(required, "id")
	}
	raw, _ := json.Marshal(map[string]any{"type": "object", "required": required, "properties": properties, "additionalProperties": false})
	return raw
}

func (t searchTool) Call(ctx context.Context, call Call, input json.RawMessage) (json.RawMessage, error) {
	var in searchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("tool input: %w", err)
	}
	i := slices.IndexFunc(t.sources, func(s source) bool { return s.name == in.Connection })
	if i < 0 {
		return nil, fmt.Errorf("connection %q is not attached to this agent", in.Connection)
	}
	operation := op(searchableKinds[t.sources[i].kind], t.verb)
	args, err := operation.args(in)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	output, err := invokeConnector(ctx, t.catalog, call, operation.action, in.Connection, raw)
	if err != nil || operation.filter == nil {
		return output, err
	}
	return operation.filter(output, in)
}

// invokeConnector runs one read-only connector action for a tool, bounded in
// time and size, with every credential the connector used scrubbed from the
// result and from errors.
func invokeConnector(ctx context.Context, catalog *connectors.Catalog, call Call, action, connection string, input json.RawMessage) (output json.RawMessage, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
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
	switch action {
	case "db.query":
		var rows dbx.Result
		if err = json.Unmarshal(output, &rows); err != nil {
			return nil, err
		}
		return databaseResult(rows)
	case "mongo.find":
		var found struct {
			Documents []json.RawMessage `json:"documents"`
			RowCount  int               `json:"row_count"`
			Truncated bool              `json:"truncated"`
		}
		_ = json.Unmarshal(output, &found)
		for len(output) > maxToolResultBytes && len(found.Documents) > 0 {
			found.Documents = found.Documents[:len(found.Documents)/2]
			found.RowCount = len(found.Documents)
			found.Truncated = true
			output, _ = json.Marshal(found)
		}
	}
	if len(output) > maxToolResultBytes {
		return nil, errors.New("result exceeds 32 KiB; narrow the query or lower limit")
	}
	return output, nil
}

// databaseResult keeps valid JSON and the truncated flag within the model's
// tool budget.
func databaseResult(result dbx.Result) (json.RawMessage, error) {
	for {
		raw, err := json.Marshal(map[string]any{"columns": result.Columns, "rows": result.Rows, "row_count": len(result.Rows), "truncated": result.Truncated})
		if err != nil {
			return nil, err
		}
		if len(raw) <= maxToolResultBytes {
			return raw, nil
		}
		if len(result.Rows) == 0 {
			return nil, errors.New("database column metadata exceeds the tool result limit")
		}
		result.Rows = result.Rows[:len(result.Rows)/2]
		result.Truncated = true
	}
}

type redactedToolError struct {
	error
	message string
}

func (e redactedToolError) Error() string { return e.message }
func (e redactedToolError) Unwrap() error { return e.error }
