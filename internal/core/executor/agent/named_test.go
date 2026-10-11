package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/llm"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

type fakeDefs map[string]Definition

func (f fakeDefs) Definition(_ context.Context, _ uuid.UUID, name string) (Definition, error) {
	if d, ok := f[name]; ok {
		return d, nil
	}
	return Definition{}, ErrAgentNotFound
}

// recordingClients remembers which provider was asked for.
type recordingClients struct {
	client llm.Client
	refs   []ProviderRef
	err    error
}

func (r *recordingClients) Client(_ context.Context, _ uuid.UUID, ref ProviderRef) (Resolved, error) {
	r.refs = append(r.refs, ref)
	if r.err != nil {
		return Resolved{}, r.err
	}
	return Resolved{Client: r.client, Provider: "test", Pricing: llm.Pricing{"claude": {InputPerMTok: 1, OutputPerMTok: 1}}}, nil
}

func namedExecutor(t *testing.T, client llm.Client, defs Definitions) (*Executor, *recordingClients) {
	t.Helper()
	tools, err := NewTools(CurrentTime{}, &echoTool{})
	if err != nil {
		t.Fatal(err)
	}
	clients := &recordingClients{client: client}
	return New(clients, tools, defs), clients
}

func TestNamedAgentRunsItsDefinition(t *testing.T) {
	temperature := 0.1
	defs := fakeDefs{"classifier": {
		Name: "classifier", Revision: 4, Connection: "acme-openai", Model: "gpt-x", System: "You classify.",
		InputSchema:  json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}}}`),
		OutputSchema: json.RawMessage(`{"type":"object","required":["label"],"properties":{"label":{"type":"string"}}}`),
		Tools:        []string{"echo"}, MaxIterations: 3, MaxTokens: 512, Temperature: &temperature,
	}}
	client := &scripted{replies: []any{toolUse("t1", "echo", `{}`), text(`{"label":"billing"}`, 30, 10)}}
	e, clients := namedExecutor(t, client, defs)
	result, err := run(t, e, `{"agent":"classifier","input":{"text":"charged twice"},"prompt":"Be quick."}`)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `{"label":"billing"}` {
		t.Fatalf("output = %s", result.Output)
	}
	if got := clients.refs; len(got) != 1 || got[0].Connection != "acme-openai" || got[0].Model != "gpt-x" {
		t.Fatalf("provider asked for = %+v", got)
	}
	first := client.requests[0]
	if first.Model != "gpt-x" || first.MaxTokens != 512 || *first.Temperature != 0.1 || !strings.HasPrefix(first.System, "You classify.") ||
		!strings.Contains(first.System, `"required":["label"]`) {
		t.Fatalf("request = %+v", first)
	}
	user := first.Messages[0].Content[0].Text
	if !strings.HasPrefix(user, "Be quick.") || !strings.Contains(user, `"text": "charged twice"`) {
		t.Fatalf("user message = %q", user)
	}
	if len(first.Tools) != 1 || first.Tools[0].Name != "echo" {
		t.Fatalf("tools offered = %+v", first.Tools)
	}
	if result.Metrics["agent"] != "classifier" || result.Metrics["agent_revision"] != 4 || result.Metrics["iterations"] != 2 {
		t.Fatalf("metrics = %v", result.Metrics)
	}
}

func TestNamedAgentFailuresArePermanent(t *testing.T) {
	defs := fakeDefs{
		"strict": {Name: "strict", Model: "m", MaxIterations: 1, InputSchema: json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}}}`)},
		"stale":  {Name: "stale", Model: "m", MaxIterations: 1, Tools: []string{"gone"}},
	}
	for name, tt := range map[string]struct {
		config, code string
	}{
		"unknown agent":    {`{"agent":"nobody","input":{}}`, "E_AGENT_NOT_FOUND"},
		"input violates":   {`{"agent":"strict","input":{"text":5}}`, "invalid_input"},
		"input is missing": {`{"agent":"strict","input":{}}`, "invalid_input"},
		"removed tool":     {`{"agent":"stale","input":{}}`, "bad_config"},
	} {
		t.Run(name, func(t *testing.T) {
			client := &scripted{replies: []any{text("x", 1, 1)}}
			e, _ := namedExecutor(t, client, defs)
			_, err := run(t, e, tt.config)
			var permanent *executor.PermanentError
			if !errors.As(err, &permanent) || permanent.Code != tt.code {
				t.Fatalf("err = %v, want permanent %s", err, tt.code)
			}
			if len(client.requests) != 0 {
				t.Fatal("the model was called")
			}
		})
	}
	// An unknown connection is a permanent failure by name; a stores-down is retryable.
	e, clients := namedExecutor(t, &scripted{replies: []any{text("x", 1, 1)}}, fakeDefs{"a": {Name: "a", Connection: "ghost", Model: "m", MaxIterations: 1}})
	clients.err = ErrConnectionNotFound
	var permanent *executor.PermanentError
	if _, err := run(t, e, `{"agent":"a","input":{}}`); !errors.As(err, &permanent) || permanent.Code != "E_PROVIDER_NOT_FOUND" || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown connection = %v", err)
	}
	clients.err = errors.New("database down")
	var retryable *executor.RetryableError
	if _, err := run(t, e, `{"agent":"a","input":{}}`); !errors.As(err, &retryable) {
		t.Fatalf("a lookup error must be retryable: %v", err)
	}
	// Without a definitions store a named agent cannot run.
	noDefs, _ := namedExecutor(t, &scripted{replies: []any{text("x", 1, 1)}}, nil)
	if _, err := run(t, noDefs, `{"agent":"a","input":{}}`); !errors.As(err, &permanent) {
		t.Fatalf("named agent without definitions = %v", err)
	}
}

func TestValidateConfigNamedAgent(t *testing.T) {
	e, _ := namedExecutor(t, &scripted{replies: []any{text("x", 1, 1)}}, fakeDefs{})
	for _, ok := range []string{
		`{"agent":"classifier","input":{"text":"${input.t}"}}`,
		`{"agent":"${input.which}","prompt":"go"}`,
	} {
		if err := e.ValidateConfig(json.RawMessage(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for config, want := range map[string]string{
		`{"agent":"a"}`:                               "needs input or prompt",
		`{"agent":"a","input":{},"model":"m"}`:        "model cannot be set together with agent",
		`{"agent":"a","input":{},"max_iterations":3}`: "max_iterations cannot be set together with agent",
		`{"agent":"a","input":{},"tools":["echo"]}`:   "tools cannot be set together with agent",
		`{"agent":"  ","input":{}}`:                   "must name a stored agent",
		`{"agent":"a","input":{},"surprise":true}`:    "unknown field",
	} {
		if err := e.ValidateConfig(json.RawMessage(config)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", config, err, want)
		}
	}
}

func TestValidateDefinition(t *testing.T) {
	e, _ := namedExecutor(t, &scripted{replies: []any{text("x", 1, 1)}}, nil)
	good := Definition{Name: "a", Model: "m", MaxIterations: 3, Tools: []string{"echo"},
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`)}
	if err := e.ValidateDefinition(good); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Definition){
		"model required":      func(d *Definition) { d.Model = "" },
		"iterations required": func(d *Definition) { d.MaxIterations = 0 },
		"iterations capped":   func(d *Definition) { d.MaxIterations = 51 },
		"unknown tool":        func(d *Definition) { d.Tools = []string{"nope"} },
		"bad temperature":     func(d *Definition) { t := 2.0; d.Temperature = &t },
		"bad output schema":   func(d *Definition) { d.OutputSchema = json.RawMessage(`{"oneOf":[]}`) },
		"bad input schema":    func(d *Definition) { d.InputSchema = json.RawMessage(`{"oneOf":[]}`) },
	}
	for name, mutate := range bad {
		d := good
		mutate(&d)
		if err := e.ValidateDefinition(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHTTPRequestToolIsFailClosed(t *testing.T) {
	var redirects int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			redirects++
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", httpToolMaxBody+10)))
		default:
			body := make([]byte, 20)
			n, _ := r.Body.Read(body)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(r.Method + " " + string(body[:n])))
		}
	}))
	defer server.Close()
	tool := NewHTTPRequest(netguard.Policy{AllowPrivate: true})
	call := func(allowed []string, input string) (map[string]any, error) {
		out, err := tool.Call(context.Background(), Call{AllowedHosts: allowed}, json.RawMessage(input))
		if err != nil {
			return nil, err
		}
		var decoded map[string]any
		_ = json.Unmarshal(out, &decoded)
		return decoded, nil
	}
	if _, err := call(nil, `{"url":"`+server.URL+`"}`); err == nil || !strings.Contains(err.Error(), "no allowed_hosts") {
		t.Fatalf("no allow-list = %v, want a refusal", err)
	}
	if _, err := call([]string{"example.com"}, `{"url":"`+server.URL+`"}`); err == nil || !strings.Contains(err.Error(), "not in this agent's allowed_hosts") {
		t.Fatalf("a host outside the list = %v", err)
	}
	got, err := call([]string{"127.0.0.1"}, `{"method":"POST","url":"`+server.URL+`/echo","body":"hello"}`)
	if err != nil || got["status"] != float64(200) || got["body"] != "POST hello" || got["truncated"] != false {
		t.Fatalf("allowed call = %v, %v", got, err)
	}
	// A redirect is returned as is, never followed to a host outside the list.
	if got, err := call([]string{"127.0.0.1"}, `{"url":"`+server.URL+`/redirect"}`); err != nil || got["status"] != float64(302) {
		t.Fatalf("redirect = %v, %v", got, err)
	}
	if got, _ := call([]string{"127.0.0.1"}, `{"url":"`+server.URL+`/big"}`); got["truncated"] != true || len(got["body"].(string)) != httpToolMaxBody {
		t.Fatalf("a large body must be truncated: truncated=%v len=%d", got["truncated"], len(got["body"].(string)))
	}
	for _, bad := range []string{`{"url":"ftp://127.0.0.1/x"}`, `{"url":"not a url"}`, `{"method":"TRACE","url":"` + server.URL + `"}`, `[]`} {
		if _, err := call([]string{"127.0.0.1"}, bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	// The egress guard still applies: loopback is refused by the default policy.
	strict := NewHTTPRequest(netguard.Policy{})
	if _, err := strict.Call(context.Background(), Call{AllowedHosts: []string{"127.0.0.1"}}, json.RawMessage(`{"url":"`+server.URL+`"}`)); err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("loopback under the default policy = %v, want an egress refusal", err)
	}
}

func TestHostAllowed(t *testing.T) {
	for host, want := range map[string]bool{
		"api.example.com": true, "API.Example.com": true, "example.com": false, "evilexample.com": false,
		"a.b.example.com": true, "other.org": false,
	} {
		if got := hostAllowed(host, []string{"*.example.com"}); got != want {
			t.Errorf("hostAllowed(%q, *.example.com) = %v, want %v", host, got, want)
		}
	}
	if !hostAllowed("example.com", []string{" example.com "}) || hostAllowed("example.com", []string{"", "  "}) {
		t.Error("exact entries and blank entries misbehave")
	}
}
