package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kartikey/workflow_service/pkg/netguard"
)

const (
	httpToolTimeout  = 15 * time.Second
	httpToolMaxBody  = 32 << 10
	httpToolMaxBytes = 64 << 10 // request body the model may send
)

// HTTPRequest lets an agent call an HTTP API. It is deliberately fail-closed: an
// agent may only reach the hosts in its own allow-list (allowed_hosts), the
// request goes through the egress guard, and redirects are not followed, so a
// reply cannot steer the call to a host outside the list. Nothing here knows
// the organization's secrets, so a model cannot ask for one.
type HTTPRequest struct{ client *http.Client }

// NewHTTPRequest builds the tool on the egress policy.
func NewHTTPRequest(policy netguard.Policy) HTTPRequest {
	client := policy.Client(httpToolTimeout)
	checked := *client
	checked.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return HTTPRequest{client: &checked}
}

func (HTTPRequest) Name() string { return "http_request" }

func (HTTPRequest) Description() string {
	return "Sends an HTTP request to an allowed host and returns the status and response body (truncated)."
}

func (HTTPRequest) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["url"],"properties":{
		"method":{"type":"string","enum":["GET","POST","PUT","PATCH","DELETE"]},
		"url":{"type":"string","description":"absolute http or https URL"},
		"headers":{"type":"object"},
		"body":{"type":"string","description":"request body, sent as is"}}}`)
}

// hostAllowed matches a host against entries that are exact names or
// "*.suffix" wildcards (which match subdomains, not the bare suffix).
func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(host)
	for _, entry := range allowed {
		entry = strings.ToLower(strings.TrimSpace(entry))
		switch {
		case entry == "":
		case strings.HasPrefix(entry, "*."):
			if strings.HasSuffix(host, entry[1:]) && len(host) > len(entry)-1 {
				return true
			}
		case host == entry:
			return true
		}
	}
	return false
}

func (t HTTPRequest) Call(ctx context.Context, call Call, input json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, errors.New("input must be an object with a url")
	}
	target, err := url.Parse(in.URL)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, errors.New("url must be an absolute http or https URL")
	}
	if len(call.AllowedHosts) == 0 {
		return nil, errors.New("this agent has no allowed_hosts, so it may not make HTTP requests")
	}
	if !hostAllowed(target.Hostname(), call.AllowedHosts) {
		return nil, fmt.Errorf("host %q is not in this agent's allowed_hosts", target.Hostname())
	}
	method := strings.ToUpper(in.Method)
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil, fmt.Errorf("method %q is not allowed", in.Method)
	}
	if len(in.Body) > httpToolMaxBytes {
		return nil, fmt.Errorf("body is over %d bytes", httpToolMaxBytes)
	}
	var body io.Reader
	if in.Body != "" {
		body = bytes.NewReader([]byte(in.Body))
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	for key, value := range in.Headers {
		if strings.EqualFold(key, "host") {
			continue
		}
		req.Header.Set(key, value)
	}
	req.Header.Set("User-Agent", "workflow-service-agent/1")
	res, err := t.client.Do(req)
	if err != nil {
		if errors.Is(err, netguard.ErrBlocked) {
			return nil, errors.New("the egress policy blocks that address")
		}
		return nil, fmt.Errorf("request failed: %v", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, httpToolMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %v", err)
	}
	truncated := len(data) > httpToolMaxBody
	if truncated {
		data = data[:httpToolMaxBody]
	}
	return json.Marshal(map[string]any{
		"status": res.StatusCode, "content_type": res.Header.Get("Content-Type"),
		"body": string(data), "truncated": truncated,
	})
}
