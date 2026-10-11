package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

// Pin the Notion API contract instead of following breaking provider changes.
const NotionVersion = "2025-09-03"

type Workspace struct {
	base
	action Action
}

func WorkspaceNodes(conns executor.Connections, policy netguard.Policy, ep Endpoints) []executor.Executor {
	out := []executor.Executor{}
	for _, kind := range []string{"slack", "notion"} {
		for _, a := range ActionsFor(kind) {
			out = append(out, &Workspace{base{conns, policy, ep}, a})
		}
	}
	return out
}
func (s *Workspace) Type() string { return s.action.Name }
func (s *Workspace) Describe() executor.Description {
	return executor.Description{Title: s.action.Title, Category: "integration", Description: s.action.Title + " through a saved connector. Reads include pagination cursors. Writes may be repeated on retries.", Output: "Provider JSON response"}
}
func (s *Workspace) ValidateConfig(raw json.RawMessage) error {
	return checkActionConfig(s.action, raw)
}
func (s *Workspace) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	name, args, err := decodeAction(s.action, req.Config)
	if err != nil {
		return executor.Result{}, err
	}
	text := func(key string) string { v, _ := args[key].(string); return v }
	for _, key := range []string{"page_id", "block_id"} {
		if v := text(key); v != "" && (len(v) > 64 || strings.ContainsAny(v, "/\\?#\r\n\x00")) {
			return executor.Result{}, executor.Permanent("bad_config", fmt.Errorf("invalid %s", key))
		}
	}
	method, target := http.MethodGet, ""
	var body any
	q := url.Values{}
	switch s.Type() {
	case "slack.history":
		target = s.ep.URL("slack_api", "") + "/conversations.history"
		q.Set("channel", text("channel"))
		q.Set("limit", "25")
		for _, key := range []string{"cursor", "oldest", "latest"} {
			if v := text(key); v != "" {
				q.Set(key, v)
			}
		}
		if n, ok := args["limit"]; ok {
			q.Set("limit", fmt.Sprint(n))
		}
	case "slack.send":
		method, target, body = http.MethodPost, s.ep.URL("slack_api", "")+"/chat.postMessage", args
	case "notion.search":
		method, target, body = http.MethodPost, s.ep.URL("notion_api", "")+"/v1/search", args
	case "notion.read":
		target = s.ep.URL("notion_api", "") + "/v1/pages/" + url.PathEscape(text("page_id"))
	case "notion.blocks":
		target = s.ep.URL("notion_api", "") + "/v1/blocks/" + url.PathEscape(text("block_id")) + "/children"
		q.Set("page_size", "100")
		if v := text("start_cursor"); v != "" {
			q.Set("start_cursor", v)
		}
	case "notion.append":
		if text("text") == "" || len([]rune(text("text"))) > 2000 {
			return executor.Result{}, executor.Permanent("bad_config", errors.New("text must contain 1 to 2000 characters"))
		}
		method, target = http.MethodPatch, s.ep.URL("notion_api", "")+"/v1/blocks/"+url.PathEscape(text("block_id"))+"/children"
		body = map[string]any{"children": []any{map[string]any{"object": "block", "type": "paragraph", "paragraph": map[string]any{"rich_text": []any{map[string]any{"type": "text", "text": map[string]string{"content": text("text")}}}}}}}
	}
	conn, err := s.connection(ctx, req, name, s.action.Kinds[0])
	if err != nil {
		return executor.Result{}, err
	}
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	data, err := s.request(ctx, conn, method, target, body)
	return executor.Result{Output: data}, err
}
func (s *Workspace) request(ctx context.Context, conn executor.Connection, method, target string, body any) ([]byte, error) {
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
		if len(raw) > MaxUploadBytes {
			return nil, executor.Permanent("bad_config", errors.New("request exceeds 8 MiB"))
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	authorize(req, conn)
	req.Header.Set("Content-Type", "application/json")
	if conn.Kind == "notion" {
		req.Header.Set("Notion-Version", NotionVersion)
	}
	data, err := s.do(ctx, req, conn.Kind)
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, executor.Retryable("invalid_response", errors.New("provider returned invalid JSON"))
	}
	if conn.Kind == "slack" {
		var reply struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &reply)
		if !reply.OK {
			switch reply.Error {
			case "ratelimited":
				return nil, executor.Retryable("rate_limited", errors.New("Slack rate limit"))
			case "internal_error", "fatal_error", "service_unavailable":
				return nil, executor.Retryable("upstream_5xx", errors.New("Slack service unavailable"))
			default:
				return nil, executor.Permanent("slack_error", fmt.Errorf("Slack: %s", reply.Error))
			}
		}
	}
	return data, nil
}

// ProbeToken uses the same transport, response checks and API version as actions.
func ProbeToken(ctx context.Context, kind, token string, policy netguard.Policy, ep Endpoints) error {
	s := Workspace{base: base{policy: policy, ep: ep}}
	target := ep.URL("slack_api", "") + "/auth.test"
	if kind == "notion" {
		target = ep.URL("notion_api", "") + "/v1/users/me"
	}
	_, err := s.request(ctx, executor.Connection{Kind: kind, Secret: map[string]string{"access_token": token}}, http.MethodGet, target, nil)
	return err
}
