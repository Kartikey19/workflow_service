//go:build integration

package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kartikey/workflow_service/internal/config"
	"github.com/kartikey/workflow_service/pkg/mongox"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

func TestSavedConnectorWorkspaceActions(t *testing.T) {
	cloud := startFakeCloud(t)
	var mu sync.Mutex
	seen := map[string]int{}
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		auth := r.Header.Get("Authorization")
		if auth == "Bearer invalid-token" {
			if strings.Contains(r.URL.Path, "auth.test") {
				io.WriteString(w, `{"ok":false,"error":"invalid_auth"}`)
			} else {
				w.WriteHeader(401)
				io.WriteString(w, `{"error":"unauthorized"}`)
			}
			return
		}
		if !strings.HasPrefix(auth, "Bearer google-access-") && auth != "Bearer workspace-test-token" {
			t.Errorf("missing connector auth on %s", r.URL.Path)
			w.WriteHeader(401)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") && !strings.Contains(r.URL.Path, "documents") && r.Header.Get("Notion-Version") != "2025-09-03" {
			t.Error("missing Notion API version")
		}
		mu.Lock()
		seen[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		var body map[string]any
		if r.Method != "GET" {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
		}
		switch r.URL.Path {
		case "/auth.test":
			io.WriteString(w, `{"ok":true,"team_id":"T1"}`)
		case "/chat.postMessage":
			if body["channel"] != "C1" || body["text"] != "hello" {
				t.Errorf("Slack message: %v", body)
			}
			io.WriteString(w, `{"ok":true,"ts":"123.1"}`)
		case "/conversations.history":
			if r.URL.Query().Get("channel") != "C1" {
				t.Error("missing channel")
			}
			io.WriteString(w, `{"ok":true,"messages":[{"text":"hello"}],"response_metadata":{"next_cursor":"slack-next"}}`)
		case "/v1/users/me":
			io.WriteString(w, `{"object":"user","id":"bot"}`)
		case "/v1/search":
			io.WriteString(w, `{"results":[{"id":"page1"}],"has_more":true,"next_cursor":"notion-next"}`)
		case "/v1/pages/page1":
			io.WriteString(w, `{"object":"page","id":"page1","properties":{}}`)
		case "/v1/blocks/page1/children":
			if r.Method == "PATCH" && body["children"] == nil {
				t.Error("missing Notion paragraph")
			}
			io.WriteString(w, `{"results":[{"type":"paragraph","paragraph":{"rich_text":[{"plain_text":"hello"}]}}],"has_more":false}`)
		case "/gmail/v1/users/me/messages":
			if r.URL.Query().Get("q") != "label:inbox" {
				t.Error("missing Gmail query")
			}
			io.WriteString(w, `{"messages":[{"id":"m1"}],"nextPageToken":"gmail-next"}`)
		case "/gmail/v1/users/me/messages/m1":
			fmt.Fprintf(w, `{"id":"m1","payload":{"mimeType":"text/plain","body":{"data":%q}}}`, base64.RawURLEncoding.EncodeToString([]byte("hello mail")))
		case "/v1/documents":
			if body["title"] != "Notes" {
				t.Error("missing document title")
			}
			io.WriteString(w, `{"documentId":"doc1","title":"Notes"}`)
		case "/v1/documents/doc1":
			io.WriteString(w, `{"documentId":"doc1","title":"Notes","tabs":[{"documentTab":{"body":{"content":[{"paragraph":{"elements":[{"textRun":{"content":"hello docs"}}]}}]}}}]}`)
		case "/v1/documents/doc1:batchUpdate":
			if body["requests"] == nil {
				t.Error("missing Docs insertText")
			}
			io.WriteString(w, `{"documentId":"doc1","replies":[{}]}`)
		case "/v4/spreadsheets/sheet1/values/Sheet1!A1:B2":
			io.WriteString(w, `{"values":[["hello","sheets"]]}`)
		case "/v4/spreadsheets/sheet1/values/Sheet1!A1:B2:append":
			if body["values"] == nil || r.URL.Query().Get("valueInputOption") != "RAW" {
				t.Error("Sheets append request")
			}
			io.WriteString(w, `{"updates":{"updatedRows":1}}`)
		case "/calendar/v3/calendars/primary/events":
			if r.Method == "POST" {
				if body["summary"] != "Meeting" || body["id"] == nil {
					t.Error("Calendar event request")
				}
				io.WriteString(w, `{"id":"event1"}`)
			} else {
				io.WriteString(w, `{"items":[{"summary":"Meeting"}],"nextPageToken":"calendar-next"}`)
			}
		default:
			t.Errorf("unexpected provider action %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			io.WriteString(w, `{}`)
		}
	}))
	defer apiServer.Close()
	s := newStack(t, context.Background(), withCloud(cloud), func(c *config.Config) {
		ep := map[string]string{}
		_ = json.Unmarshal([]byte(c.Services.IntegrationEndpoints), &ep)
		for _, key := range []string{"gmail_api", "gdocs_api", "gsheets_api", "gcalendar_api", "slack_api", "notion_api"} {
			ep[key] = apiServer.URL
		}
		raw, _ := json.Marshal(ep)
		c.Services.IntegrationEndpoints = string(raw)
	})
	owner := s.owner(t, "Connector actions")
	for _, kind := range []string{"gmail", "gdocs", "gsheets", "gcalendar"} {
		if back := cloud.connect(t, s, owner, kind, kind+"-shared"); !strings.Contains(back, "connected=") {
			t.Fatalf("OAuth %s did not save", kind)
		}
	}
	for _, kind := range []string{"slack", "notion"} {
		owner.mustOK("POST", "/be/v1/connections", map[string]any{"name": kind + "-shared", "kind": kind, "secret": map[string]string{"access_token": "workspace-test-token"}}, 201)
	}
	// Both HTTP authentication failures and Slack's HTTP-200 logical failures stop save.
	for _, kind := range []string{"slack", "notion"} {
		status, _ := owner.call("POST", "/be/v1/connections", map[string]any{"name": "invalid-" + kind, "kind": kind, "secret": map[string]string{"access_token": "invalid-token"}})
		if status != 422 {
			t.Errorf("%s invalid credentials status %d", kind, status)
		}
	}
	cases := []struct{ action, connection, args, want string }{
		{"gmail.search", "gmail-shared", `{"query":"label:inbox"}`, "gmail-next"}, {"gmail.read", "gmail-shared", `{"message_id":"m1"}`, "hello mail"},
		{"gdocs.read", "gdocs-shared", `{"document_id":"doc1"}`, "hello docs"}, {"gdocs.create", "gdocs-shared", `{"title":"Notes"}`, "doc1"}, {"gdocs.append", "gdocs-shared", `{"document_id":"doc1","text":"hello"}`, "doc1"},
		{"gsheets.read", "gsheets-shared", `{"spreadsheet_id":"sheet1","range":"Sheet1!A1:B2"}`, "sheets"}, {"gsheets.append", "gsheets-shared", `{"spreadsheet_id":"sheet1","range":"Sheet1!A1:B2","values":[["hello","sheets"]]}`, "updatedRows"},
		{"gcalendar.list", "gcalendar-shared", `{}`, "calendar-next"}, {"gcalendar.create", "gcalendar-shared", `{"summary":"Meeting","start":"2026-10-12T12:00:00Z","end":"2026-10-12T13:00:00Z"}`, "event1"},
		{"slack.history", "slack-shared", `{"channel":"C1"}`, "slack-next"}, {"slack.send", "slack-shared", `{"channel":"C1","text":"hello"}`, "123.1"},
		{"notion.search", "notion-shared", `{"query":"Notes"}`, "notion-next"}, {"notion.read", "notion-shared", `{"page_id":"page1"}`, "page1"}, {"notion.blocks", "notion-shared", `{"block_id":"page1"}`, "hello"}, {"notion.append", "notion-shared", `{"block_id":"page1","text":"hello"}`, "paragraph"},
	}
	for index, tt := range cases {
		t.Run(tt.action, func(t *testing.T) {
			name := fmt.Sprintf("connector-case-%d", index)
			config := fmt.Sprintf(`{connection: %s, action: %s, arguments: %s}`, tt.connection, tt.action, tt.args)
			if status, body := owner.publish(nodeWorkflow(name, "connector.call", config)); status != 201 {
				t.Fatalf("publish %d %v", status, body)
			}
			id, run := runToEnd(t, owner, name)
			if run["status"] != "succeeded" {
				t.Fatalf("run %v", run)
			}
			var output string
			if err := s.h.pool.QueryRow(context.Background(), `select output::text from node_executions where run_id=$1 and node_id='act'`, mustParse(t, id)).Scan(&output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output, tt.want) {
				t.Fatalf("result lacks %q: %s", tt.want, output)
			}
		})
	}
	// Bound tools use those same saved connectors and action implementations.
	steps := []toolStep{}
	refs := []string{}
	for _, tt := range cases {
		ref := "connector:" + tt.connection + ":" + tt.action
		steps = append(steps, toolStep{"$connector:" + ref, tt.args})
		refs = append(refs, ref)
	}
	model, results := scriptedToolLLM(t, steps)
	owner.mustOK("POST", "/be/v1/providers", map[string]any{"name": "connector-model", "provider": "openai_compatible", "base_url": model.URL, "api_key": "connector-test-llm-key"}, 201)
	body := agentBody("connector-agent", "connector-model")
	body["tools"] = refs
	body["max_iterations"] = 20
	owner.mustOK("POST", "/be/v1/agents", body, 201)
	tested := owner.mustOK("POST", "/be/v1/agents/connector-agent/test", map[string]any{"input": map[string]any{}}, 200)
	if tested["ok"] != true {
		t.Fatalf("agent %v", tested)
	}
	if len(results()) != len(cases) {
		t.Fatal("agent did not call all connector actions")
	}
	for i, tt := range cases {
		if !strings.Contains(results()[i], tt.want) {
			t.Errorf("agent result %s: %s", tt.action, results()[i])
		}
	}
	body["name"] = "wrong-kind"
	body["tools"] = []string{"connector:slack-shared:db.query"}
	if status, _ := owner.call("POST", "/be/v1/agents", body); status != 400 {
		t.Errorf("wrong-kind %d", status)
	}
	other := s.owner(t, "Other connector org")
	body["connection"] = ""
	body["name"] = "foreign"
	body["tools"] = []string{"connector:slack-shared:slack.history"}
	if status, _ := other.call("POST", "/be/v1/agents", body); status != 400 {
		t.Errorf("foreign connector %d", status)
	}
	if status, _ := owner.publish(nodeWorkflow("override", "connector.call", `{connection: slack-shared, action: slack.history, arguments: {connection: notion-shared, channel: C1}}`)); status != 400 {
		t.Error("argument connection override accepted")
	}
	if n := s.h.occurrences("workspace-test-token"); n != 0 {
		t.Errorf("workspace token leaked into %d rows", n)
	}
}

func TestMongoSavedConnector(t *testing.T) {
	uri := os.Getenv("MONGO_URL")
	if uri == "" {
		t.Skip("set MONGO_URL for real MongoDB connector acceptance")
	}
	database := fmt.Sprintf("workflow_connector_%d", time.Now().UnixNano())
	cfg := mongox.Config{URI: uri, Database: database, TLS: "disable", Dial: (netguard.Policy{AllowPrivate: true}).DialContext}
	conn, err := mongox.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal("connect local MongoDB test server:", err)
	}
	defer conn.Close()
	cleanup, err := mongo.Connect(options.Client().ApplyURI(uri).SetTimeout(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cleanup.Database(database).Drop(ctx)
		_ = cleanup.Disconnect(ctx)
	})
	// The connector creates and uses its own collection in the local test database.
	collection := fmt.Sprintf("events_%d", len(t.Name()))
	s := newStack(t, context.Background())
	owner := s.owner(t, "Mongo connector")
	input := map[string]any{"name": "mongo-shared", "kind": "mongo", "config": map[string]any{"database": database, "tls": "disable"}, "secret": map[string]string{"uri": uri}}
	owner.mustOK("POST", "/be/v1/connections", input, 201)
	insert := fmt.Sprintf(`{connection: mongo-shared, action: mongo.insert, arguments: {collection: %s, documents: [{name: first, score: 10}, {name: second, score: 20}]}}`, collection)
	if status, body := owner.publish(nodeWorkflow("mongo-insert", "connector.call", insert)); status != 201 {
		t.Fatalf("publish %d %v", status, body)
	}
	if _, run := runToEnd(t, owner, "mongo-insert"); run["status"] != "succeeded" {
		t.Fatalf("insert %v", run)
	}
	query := fmt.Sprintf(`{connection: mongo-shared, collection: %s, filter: {score: {$gte: 10}}, sort: {score: 1}, max_rows: 1}`, collection)
	if status, body := owner.publish(nodeWorkflow("mongo-find", "mongo.find", query)); status != 201 {
		t.Fatalf("publish %d %v", status, body)
	}
	id, run := runToEnd(t, owner, "mongo-find")
	if run["status"] != "succeeded" {
		t.Fatalf("find %v", run)
	}
	var output map[string]any
	_ = s.h.pool.QueryRow(context.Background(), `select output from node_executions where run_id=$1 and node_id='act'`, mustParse(t, id)).Scan(&output)
	if output["row_count"] != float64(1) || output["truncated"] != true || !strings.Contains(toJSON(output), "first") {
		t.Fatalf("Mongo result %v", output)
	}
	model, results := scriptedToolLLM(t, []toolStep{{"$connector:connector:mongo-shared:mongo.list_collections", `{}`}, {"$connector:connector:mongo-shared:mongo.find", fmt.Sprintf(`{"collection":%q,"max_rows":1}`, collection)}})
	owner.mustOK("POST", "/be/v1/providers", map[string]any{"name": "mongo-model", "provider": "openai_compatible", "base_url": model.URL, "api_key": "mongo-test-llm-key"}, 201)
	body := agentBody("mongo-analyst", "mongo-model")
	body["tools"] = []string{"connector:mongo-shared:mongo.list_collections", "connector:mongo-shared:mongo.find"}
	owner.mustOK("POST", "/be/v1/agents", body, 201)
	tested := owner.mustOK("POST", "/be/v1/agents/mongo-analyst/test", map[string]any{"input": map[string]any{}}, 200)
	if tested["ok"] != true || len(results()) != 2 || !strings.Contains(results()[1], `"truncated":true`) {
		t.Fatalf("Mongo agent %v %v", tested, results())
	}
	// Executable JavaScript is refused in the read API, not sent as a command.
	bad := fmt.Sprintf(`{connection: mongo-shared, collection: %s, filter: {$where: 'function(){return true}'}}`, collection)
	owner.publish(nodeWorkflow("mongo-script", "mongo.find", bad))
	id, run = runToEnd(t, owner, "mongo-script")
	if run["status"] != "failed" || len(s.h.attempts(mustParse(t, id), "act")) != 1 {
		t.Fatal("Mongo JavaScript was not refused permanently")
	}
	// An invalid password fails test-before-save; credentials never enter the API response.
	u, _ := url.Parse(uri)
	u.User = url.UserPassword(u.User.Username(), "wrong-mongo-password")
	input["name"] = "mongo-invalid"
	input["secret"] = map[string]string{"uri": u.String()}
	if status, _ := owner.call("POST", "/be/v1/connections", input); status != 422 {
		t.Errorf("Mongo invalid password %d", status)
	}
	// Validate credential scopes/egress through the same action path.
	blocked := newStack(t, context.Background(), func(c *config.Config) { c.Egress.AllowPrivate = false })
	blockedOwner := blocked.owner(t, "Blocked Mongo")
	input["name"] = "mongo-shared"
	input["secret"] = map[string]string{"uri": uri}
	input["skip_test"] = true
	blockedOwner.mustOK("POST", "/be/v1/connections", input, 201)
	definition := strings.Replace(nodeWorkflow("mongo-blocked", "mongo.find", query), "type: mongo.find", "type: mongo.find\n    timeout: 300ms", 1)
	blockedOwner.publish(definition)
	id, run = runToEnd(t, blockedOwner, "mongo-blocked")
	attempts := blocked.h.attempts(mustParse(t, id), "act")
	if run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != "egress_blocked" {
		t.Fatalf("Mongo blocked address %v %v", run["status"], attempts)
	}
}

func TestSQLSavedConnectorActions(t *testing.T) {
	eachDatabase(t, func(t *testing.T, s *stack, owner *person, b backend, tb tables, connection string) {
		args, _ := json.Marshal(map[string]any{"sql": "select count(*) as n from " + tb.events})
		definition := nodeWorkflow("shared-query", "connector.call", fmt.Sprintf(`{connection: %s, action: db.query, arguments: %s}`, connection, args))
		if status, body := owner.publish(definition); status != 201 {
			t.Fatalf("publish %d %v", status, body)
		}
		if _, run := runToEnd(t, owner, "shared-query"); run["status"] != "succeeded" {
			t.Fatalf("query %v", run)
		}
		ref := "connector:" + connection + ":db.query"
		model, results := scriptedToolLLM(t, []toolStep{{"$connector:" + ref, string(args)}, {"$connector:" + ref, `{"sql":"select 1","connection":"another"}`}, {"$connector:" + ref, `{"sql":"delete from ` + tb.events + `"}`}})
		owner.mustOK("POST", "/be/v1/providers", map[string]any{"name": "shared-sql-model", "provider": "openai_compatible", "base_url": model.URL, "api_key": "shared-sql-test-key"}, 201)
		body := agentBody("shared-sql-agent", "shared-sql-model")
		body["max_iterations"] = 5
		body["tools"] = []string{ref}
		owner.mustOK("POST", "/be/v1/agents", body, 201)
		tested := owner.mustOK("POST", "/be/v1/agents/shared-sql-agent/test", map[string]any{"input": map[string]any{}}, 200)
		if tested["ok"] != true || len(results()) != 3 || !strings.Contains(results()[0], `"n":1`) || !strings.Contains(results()[1], "connection") || !strings.Contains(results()[2], "only read statements") {
			t.Fatalf("shared SQL tool results %v %v", tested, results())
		}
	})
}
