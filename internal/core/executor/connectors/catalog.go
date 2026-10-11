package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/executor/database"
	"github.com/kartikey/workflow_service/internal/core/executor/email"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

// Action is the shared contract used by the workflow builder and agent tools.
// Connection credentials never form part of an action's input.
type Action struct {
	Name        string          `json:"name"`
	Kinds       []string        `json:"kinds"`
	Title       string          `json:"title"`
	ReadOnly    bool            `json:"read_only"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func action(name, kinds, title string, read bool, schema string) Action {
	var accepted []string
	switch kinds {
	case "database":
		accepted = []string{"postgres", "mysql"}
	case "email":
		accepted = []string{"smtp", "resend"}
	default:
		accepted = []string{kinds}
	}
	return Action{name, accepted, title, read, json.RawMessage(schema)}
}

var actions = []Action{
	action("db.list_tables", "database", "List database tables", true, `{"type":"object","properties":{},"additionalProperties":false}`),
	action("db.describe_table", "database", "Describe a database table", true, `{"type":"object","required":["table"],"properties":{"table":{"type":"string"}},"additionalProperties":false}`),
	action("db.query", "database", "Query database (read only)", true, `{"type":"object","required":["sql"],"properties":{"sql":{"type":"string"},"params":{"type":"array"},"max_rows":{"type":"integer","minimum":1,"maximum":1000},"timeout":{"type":"string"},"fail_on_truncate":{"type":"boolean"}},"additionalProperties":false}`),
	action("db.insert", "database", "Insert database rows", false, `{"type":"object","required":["table"],"properties":{"table":{"type":"string"},"row":{"type":"object"},"rows":{"type":"array"},"ignore_conflicts":{"type":"boolean"},"timeout":{"type":"string"}},"additionalProperties":false}`),
	action("gmail.search", "gmail", "Search Gmail messages", true, `{"type":"object","properties":{"query":{"type":"string"},"max_results":{"type":"integer","minimum":1,"maximum":100},"page_token":{"type":"string"}},"additionalProperties":false}`),
	action("gmail.read", "gmail", "Read a Gmail message", true, `{"type":"object","required":["message_id"],"properties":{"message_id":{"type":"string"}},"additionalProperties":false}`),
	action("gmail.send", "gmail", "Send Gmail", false, `{"type":"object","required":["to","subject"],"properties":{"to":{"type":"array","items":{"type":"string"}},"cc":{"type":"array","items":{"type":"string"}},"bcc":{"type":"array","items":{"type":"string"}},"reply_to":{"type":"string"},"subject":{"type":"string"},"text":{"type":"string"},"html":{"type":"string"}},"additionalProperties":false}`),
	action("gdocs.read", "gdocs", "Read a Google document", true, `{"type":"object","required":["document_id"],"properties":{"document_id":{"type":"string"}},"additionalProperties":false}`),
	action("gdocs.create", "gdocs", "Create a Google document", false, `{"type":"object","required":["title"],"properties":{"title":{"type":"string"}},"additionalProperties":false}`),
	action("gdocs.append", "gdocs", "Append text to a Google document", false, `{"type":"object","required":["document_id","text"],"properties":{"document_id":{"type":"string"},"text":{"type":"string"}},"additionalProperties":false}`),
	action("gsheets.read", "gsheets", "Read spreadsheet cells", true, `{"type":"object","required":["spreadsheet_id","range"],"properties":{"spreadsheet_id":{"type":"string"},"range":{"type":"string"}},"additionalProperties":false}`),
	action("gsheets.append", "gsheets", "Append spreadsheet rows", false, `{"type":"object","required":["spreadsheet_id","range","values"],"properties":{"spreadsheet_id":{"type":"string"},"range":{"type":"string"},"values":{"type":"array","items":{"type":"array"}}},"additionalProperties":false}`),
	action("gcalendar.list", "gcalendar", "List calendar events", true, `{"type":"object","properties":{"calendar_id":{"type":"string","default":"primary"},"query":{"type":"string"},"time_min":{"type":"string"},"time_max":{"type":"string"},"max_results":{"type":"integer","minimum":1,"maximum":100},"page_token":{"type":"string"}},"additionalProperties":false}`),
	action("gcalendar.create", "gcalendar", "Create a calendar event", false, `{"type":"object","required":["summary","start","end"],"properties":{"calendar_id":{"type":"string","default":"primary"},"summary":{"type":"string"},"description":{"type":"string"},"start":{"type":"string","description":"RFC 3339 date and time with timezone"},"end":{"type":"string","description":"RFC 3339 date and time with timezone"}},"additionalProperties":false}`),
	action("gdrive.upload", "gdrive", "Upload to Google Drive", false, `{"type":"object","required":["name"],"properties":{"name":{"type":"string"},"mime_type":{"type":"string"},"folder_id":{"type":"string"},"content":{"type":"string"},"content_base64":{"type":"string"}},"additionalProperties":false}`),
	action("dropbox.upload", "dropbox", "Upload to Dropbox", false, `{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"overwrite":{"type":"boolean"},"content":{"type":"string"},"content_base64":{"type":"string"}},"additionalProperties":false}`),
	action("onedrive.upload", "onedrive", "Upload to OneDrive", false, `{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"content":{"type":"string"},"content_base64":{"type":"string"}},"additionalProperties":false}`),
	action("email.send", "email", "Send email", false, `{"type":"object","required":["from","to","subject"],"properties":{"from":{"type":"string"},"to":{"type":"array","items":{"type":"string"}},"subject":{"type":"string"},"text":{"type":"string"},"html":{"type":"string"}},"additionalProperties":false}`),
	action("mongo.list_collections", "mongo", "List MongoDB collections", true, `{"type":"object","properties":{},"additionalProperties":false}`),
	action("mongo.find", "mongo", "Find MongoDB documents", true, `{"type":"object","properties":{"collection":{"type":"string"},"filter":{"type":"object"},"projection":{"type":"object"},"sort":{"type":"object"},"max_rows":{"type":"integer","minimum":1,"maximum":1000}},"additionalProperties":false,"required":["collection"]}`),
	action("mongo.insert", "mongo", "Insert MongoDB documents", false, `{"type":"object","properties":{"collection":{"type":"string"},"documents":{"type":"array","minItems":1,"maxItems":1000,"items":{"type":"object"}}},"additionalProperties":false,"required":["collection","documents"]}`),
	action("slack.history", "slack", "Read Slack channel messages", true, `{"type":"object","properties":{"channel":{"type":"string"},"cursor":{"type":"string"},"oldest":{"type":"string"},"latest":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":100}},"additionalProperties":false,"required":["channel"]}`),
	action("slack.send", "slack", "Send a Slack message", false, `{"type":"object","properties":{"channel":{"type":"string"},"text":{"type":"string","minLength":1,"maxLength":4000},"thread_ts":{"type":"string"}},"additionalProperties":false,"required":["channel","text"]}`),
	action("notion.search", "notion", "Search Notion pages", true, `{"type":"object","properties":{"query":{"type":"string"},"start_cursor":{"type":"string"},"page_size":{"type":"integer","minimum":1,"maximum":100}},"additionalProperties":false}`),
	action("notion.read", "notion", "Read a Notion page", true, `{"type":"object","properties":{"page_id":{"type":"string"}},"additionalProperties":false,"required":["page_id"]}`),
	action("notion.blocks", "notion", "Read Notion block content", true, `{"type":"object","properties":{"block_id":{"type":"string"},"start_cursor":{"type":"string"}},"additionalProperties":false,"required":["block_id"]}`),
	action("notion.append", "notion", "Append text to Notion", false, `{"type":"object","properties":{"block_id":{"type":"string"},"text":{"type":"string","minLength":1,"maxLength":2000}},"additionalProperties":false,"required":["block_id","text"]}`),
}

func Actions() []Action { return slices.Clone(actions) }
func LookupAction(name string) (Action, bool) {
	for _, a := range actions {
		if a.Name == name {
			return a, true
		}
	}
	return Action{}, false
}
func ActionsFor(kind string) []Action {
	out := []Action{}
	for _, a := range actions {
		if slices.Contains(a.Kinds, kind) {
			out = append(out, a)
		}
	}
	return out
}

type Catalog struct{ nodes map[string]executor.Executor }

func NewCatalog(conns executor.Connections, policy netguard.Policy, ep Endpoints, resendURL string) *Catalog {
	nodes := []executor.Executor{NewGmail(conns, policy, ep), NewGDrive(conns, policy, ep), NewDropbox(conns, policy, ep), NewOneDrive(conns, policy, ep),
		database.NewQuery(conns, policy), database.NewInsert(conns, policy), database.NewInspect(conns, policy, false), database.NewInspect(conns, policy, true), email.New(conns, policy, resendURL)}
	nodes = append(nodes, GoogleNodes(conns, policy, ep)...)
	nodes = append(nodes, WorkspaceNodes(conns, policy, ep)...)
	nodes = append(nodes, MongoNodes(conns, policy)...)
	c := &Catalog{nodes: map[string]executor.Executor{}}
	for _, n := range nodes {
		c.nodes[n.Type()] = n
	}
	return c
}

// Nodes registers the same executor instances used by bound agent tools.
func (c *Catalog) Nodes() []executor.Executor {
	out := []executor.Executor{}
	for _, a := range actions {
		if n, ok := c.nodes[a.Name]; ok {
			out = append(out, n)
		}
	}
	return append(out, NewCaller(c))
}

func (c *Catalog) config(actionName, connection string, args json.RawMessage) (executor.Executor, json.RawMessage, error) {
	node, ok := c.nodes[actionName]
	if !ok {
		return nil, nil, fmt.Errorf("unknown connector action %q", actionName)
	}
	if connection == "" {
		return nil, nil, errors.New("connection is required")
	}
	input := map[string]json.RawMessage{}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &input); err != nil || input == nil {
			return nil, nil, errors.New("arguments must be an object")
		}
	}
	if _, exists := input["connection"]; exists {
		return nil, nil, errors.New("connection cannot be overridden in arguments")
	}
	input["connection"], _ = json.Marshal(connection)
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, nil, err
	}
	if err := node.ValidateConfig(raw); err != nil {
		return nil, nil, err
	}
	return node, raw, nil
}
func (c *Catalog) Invoke(ctx context.Context, req executor.Request, name, connection string, args json.RawMessage) (executor.Result, error) {
	node, raw, err := c.config(name, connection, args)
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	action, _ := LookupAction(name)
	if _, _, err := decodeAction(action, raw); err != nil {
		return executor.Result{}, err
	}
	req.Config = raw
	return node.Execute(ctx, req)
}

// Caller routes a workflow step through exactly the same actions as agent tools.
type Caller struct{ catalog *Catalog }

func NewCaller(c *Catalog) *Caller { return &Caller{c} }
func (*Caller) Type() string       { return "connector.call" }
func (*Caller) Describe() executor.Description {
	return executor.Description{Title: "Saved connector", Category: "integration", Description: "Uses an action on a saved connector. Credentials are loaded by name from this organization.", Example: "- id: read\n  type: connector.call\n  config:\n    connection: warehouse\n    action: db.query\n    arguments: {sql: 'select 1 as n'}", Output: "The selected connector action's result"}
}
func (c *Caller) ValidateConfig(raw json.RawMessage) error {
	if err := executor.CheckFields(raw, []executor.Field{{Name: "connection", Kind: "string", Required: true}, {Name: "action", Kind: "string", Required: true}, {Name: "arguments", Kind: "object"}}); err != nil {
		return err
	}
	var cfg struct {
		Connection string
		Action     string
		Arguments  json.RawMessage
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if executor.IsExpression(cfg.Action) {
		return errors.New("action must be a literal connector action")
	}
	_, _, err := c.catalog.config(cfg.Action, cfg.Connection, cfg.Arguments)
	return err
}
func (c *Caller) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var cfg struct {
		Connection string
		Action     string
		Arguments  json.RawMessage
	}
	if err := executor.DecodeConfig(req.Config, &cfg); err != nil {
		return executor.Result{}, err
	}
	return c.catalog.Invoke(ctx, req, cfg.Action, cfg.Connection, cfg.Arguments)
}
