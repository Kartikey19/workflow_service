package connectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/jsonschema"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

type Google struct {
	base
	action Action
}

func GoogleNodes(conns executor.Connections, policy netguard.Policy, ep Endpoints) []executor.Executor {
	var out []executor.Executor
	for _, a := range actions {
		switch a.Name {
		case "gmail.search", "gmail.read", "gdocs.read", "gdocs.create", "gdocs.append", "gsheets.read", "gsheets.append", "gcalendar.list", "gcalendar.create":
			out = append(out, &Google{base{conns, policy, ep}, a})
		}
	}
	return out
}
func (g *Google) Type() string { return g.action.Name }
func (g *Google) Describe() executor.Description {
	return executor.Description{Title: g.action.Title, Category: "integration", Description: g.action.Title + " through a saved " + g.action.Kinds[0] + " connector. List actions return pagination tokens. Non-idempotent writes can be repeated if an attempt is retried.", Output: "Provider result, or normalized document/message content"}
}
func (g *Google) ValidateConfig(raw json.RawMessage) error {
	return checkActionConfig(g.action, raw)
}
func (g *Google) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var c map[string]any
	if err := executor.DecodeConfig(req.Config, &c); err != nil {
		return executor.Result{}, err
	}
	name, _ := c["connection"].(string)
	delete(c, "connection")
	var schema any
	_ = json.Unmarshal(g.action.InputSchema, &schema)
	if problems := jsonschema.Validate(schema, c); len(problems) > 0 {
		return executor.Result{}, executor.Permanent("bad_config", fmt.Errorf("invalid %s arguments: %v", g.Type(), problems))
	}
	text := func(key string) string { v, _ := c[key].(string); return v }
	for _, key := range []string{"message_id", "document_id", "spreadsheet_id"} {
		if v, exists := c[key]; exists {
			id, _ := v.(string)
			if id == "" || len(id) > 2048 || strings.ContainsAny(id, "/?#\r\n\x00") {
				return executor.Result{}, executor.Permanent("bad_config", fmt.Errorf("%s must be a resource ID", key))
			}
		}
	}
	query := url.Values{}
	count := func() string {
		if n, ok := c["max_results"].(float64); ok {
			return fmt.Sprint(int(n))
		}
		return "25"
	}
	if token := text("page_token"); token != "" {
		query.Set("pageToken", token)
	}
	method, target := http.MethodGet, ""
	var body any
	switch g.Type() {
	case "gmail.search":
		target = g.ep.URL("gmail_api", "") + "/gmail/v1/users/me/messages"
		query.Set("q", text("query"))
		query.Set("maxResults", count())
	case "gmail.read":
		target = g.ep.URL("gmail_api", "") + "/gmail/v1/users/me/messages/" + url.PathEscape(text("message_id"))
		query.Set("format", "full")
	case "gdocs.read":
		target = g.ep.URL("gdocs_api", "") + "/v1/documents/" + url.PathEscape(text("document_id"))
		query.Set("includeTabsContent", "true")
	case "gdocs.create":
		method, target, body = http.MethodPost, g.ep.URL("gdocs_api", "")+"/v1/documents", map[string]any{"title": text("title")}
	case "gdocs.append":
		method, target = http.MethodPost, g.ep.URL("gdocs_api", "")+"/v1/documents/"+url.PathEscape(text("document_id"))+":batchUpdate"
		body = map[string]any{"requests": []any{map[string]any{"insertText": map[string]any{"endOfSegmentLocation": map[string]any{}, "text": text("text")}}}}
	case "gsheets.read", "gsheets.append":
		if text("range") == "" {
			return executor.Result{}, executor.Permanent("bad_config", errors.New("range is required"))
		}
		target = g.ep.URL("gsheets_api", "") + "/v4/spreadsheets/" + url.PathEscape(text("spreadsheet_id")) + "/values/" + url.PathEscape(text("range"))
		if g.Type() == "gsheets.append" {
			method, target = http.MethodPost, target+":append"
			query.Set("valueInputOption", "RAW")
			query.Set("insertDataOption", "INSERT_ROWS")
			body = map[string]any{"values": c["values"], "majorDimension": "ROWS"}
		}
	case "gcalendar.list", "gcalendar.create":
		calendar := text("calendar_id")
		if calendar == "" {
			calendar = "primary"
		}
		target = g.ep.URL("gcalendar_api", "") + "/calendar/v3/calendars/" + url.PathEscape(calendar) + "/events"
		if g.Type() == "gcalendar.list" {
			query.Set("maxResults", count())
			query.Set("singleEvents", "true")
			query.Set("orderBy", "startTime")
			for key, apiName := range map[string]string{"query": "q", "time_min": "timeMin", "time_max": "timeMax"} {
				if v := text(key); v != "" {
					if key != "query" {
						if _, err := time.Parse(time.RFC3339, v); err != nil {
							return executor.Result{}, executor.Permanent("bad_config", fmt.Errorf("%s must be RFC 3339", key))
						}
					}
					query.Set(apiName, v)
				}
			}
		} else {
			start, err := time.Parse(time.RFC3339, text("start"))
			if err != nil {
				return executor.Result{}, executor.Permanent("bad_config", errors.New("start must be RFC 3339"))
			}
			end, err := time.Parse(time.RFC3339, text("end"))
			if err != nil || !end.After(start) {
				return executor.Result{}, executor.Permanent("bad_config", errors.New("end must be RFC 3339 and later than start"))
			}
			hash := sha256.Sum256([]byte(req.IdempotencyKey))
			method = http.MethodPost
			body = map[string]any{"id": hex.EncodeToString(hash[:16]), "summary": text("summary"), "description": text("description"), "start": map[string]string{"dateTime": text("start")}, "end": map[string]string{"dateTime": text("end")}}
		}
	}
	conn, err := g.connection(ctx, req, name, g.action.Kinds[0])
	if err != nil {
		return executor.Result{}, err
	}
	rawBody, err := json.Marshal(body)
	if body == nil {
		rawBody = nil
	}
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	if len(rawBody) > MaxUploadBytes {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("request exceeds 8 MiB"))
	}
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(rawBody))
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	authorize(httpReq, conn)
	httpReq.Header.Set("Content-Type", "application/json")
	data, err := g.do(ctx, httpReq, g.action.Title)
	if err != nil {
		return executor.Result{}, err
	}
	if !json.Valid(data) {
		return executor.Result{}, executor.Retryable("invalid_response", errors.New("provider returned invalid JSON"))
	}
	if g.Type() == "gdocs.read" {
		var doc map[string]any
		_ = json.Unmarshal(data, &doc)
		var content strings.Builder
		if tabs, ok := doc["tabs"]; ok {
			documentText(tabs, &content)
		} else {
			documentText(doc["body"], &content)
		}
		data, _ = json.Marshal(map[string]any{"document_id": doc["documentId"], "title": doc["title"], "text": content.String()})
	}
	if g.Type() == "gmail.read" {
		var message struct {
			ID       string    `json:"id"`
			ThreadID string    `json:"threadId"`
			Snippet  string    `json:"snippet"`
			Payload  gmailPart `json:"payload"`
		}
		_ = json.Unmarshal(data, &message)
		headers := map[string]string{}
		for _, h := range message.Payload.Headers {
			headers[strings.ToLower(h.Name)] = h.Value
		}
		var plain, html strings.Builder
		gmailText(message.Payload, &plain, &html)
		data, _ = json.Marshal(map[string]any{"id": message.ID, "thread_id": message.ThreadID, "snippet": message.Snippet, "headers": headers, "text": plain.String(), "html": html.String()})
	}
	return executor.Result{Output: data}, nil
}

func documentText(v any, out *strings.Builder) {
	switch node := v.(type) {
	case []any:
		for _, child := range node {
			documentText(child, out)
		}
	case map[string]any:
		if run, ok := node["textRun"].(map[string]any); ok {
			if text, ok := run["content"].(string); ok {
				out.WriteString(text)
			}
		}
		for _, key := range []string{"body", "content", "paragraph", "elements", "table", "tableRows", "tableCells", "tabs", "documentTab", "childTabs"} {
			if child, ok := node[key]; ok {
				documentText(child, out)
			}
		}
	}
}

type gmailPart struct {
	MimeType string `json:"mimeType"`
	Headers  []struct {
		Name  string
		Value string
	} `json:"headers"`
	Body  struct{ Data string } `json:"body"`
	Parts []gmailPart           `json:"parts"`
}

func gmailText(part gmailPart, plain, html *strings.Builder) {
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(part.Body.Data, "="))
	if err == nil {
		if part.MimeType == "text/plain" {
			plain.Write(data)
		}
		if part.MimeType == "text/html" {
			html.Write(data)
		}
	}
	for _, child := range part.Parts {
		gmailText(child, plain, html)
	}
}
