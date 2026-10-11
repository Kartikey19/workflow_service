package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

// Dropbox uploads a file to Dropbox.
type Dropbox struct{ base }

var _ executor.Executor = (*Dropbox)(nil)

func NewDropbox(conns executor.Connections, policy netguard.Policy, ep Endpoints) *Dropbox {
	return &Dropbox{base{conns, policy, ep}}
}

func (*Dropbox) Type() string { return "dropbox.upload" }

func (*Dropbox) Describe() executor.Description {
	return executor.Description{
		Title: "Upload to Dropbox", Category: "integration",
		Description: "Uploads a file (text, or base64 for binary, up to 8 MiB) to a path in Dropbox. By default an existing file at the path is " +
			"replaced, which makes a retry harmless; set overwrite: false to fail instead if the path exists.",
		Example: `- id: archive
  type: dropbox.upload
  needs: [render_report]
  config:
    connection: team-dropbox
    path: "/Reports/report-${run.id}.csv"
    content: ${nodes.render_report.output.csv}`,
		Output: `{"id": "id:...", "name": "...", "path": "/Reports/...", "size": 123}`,
	}
}

func (*Dropbox) ValidateConfig(raw json.RawMessage) error {
	if err := executor.CheckFields(raw, append([]executor.Field{
		{Name: "connection", Kind: "string", Required: true},
		{Name: "path", Kind: "string", Required: true},
		{Name: "overwrite", Kind: "bool"},
	}, contentFields...)); err != nil {
		return err
	}
	return requireContent(raw)
}

func (d *Dropbox) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var c struct {
		Connection string `json:"connection"`
		Path       string `json:"path"`
		Overwrite  *bool  `json:"overwrite"`
		content
	}
	if err := executor.DecodeConfig(req.Config, &c); err != nil {
		return executor.Result{}, err
	}
	data, err := c.bytes(MaxUploadBytes)
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	if !strings.HasPrefix(c.Path, "/") || strings.ContainsAny(c.Path, "\r\n\x00") || strings.HasSuffix(c.Path, "/") {
		return executor.Result{}, executor.Permanent("bad_config", errors.New(`path must be a file path starting with "/"`))
	}
	conn, err := d.connection(ctx, req, c.Connection, "dropbox")
	if err != nil {
		return executor.Result{}, err
	}
	mode := "overwrite"
	if c.Overwrite != nil && !*c.Overwrite {
		mode = "add"
	}
	arg, _ := json.Marshal(map[string]any{"path": c.Path, "mode": mode, "autorename": false, "mute": true})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.ep.URL("dropbox_content", "")+"/2/files/upload", bytes.NewReader(data))
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	authorize(httpReq, conn)
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	httpReq.Header.Set("Dropbox-API-Arg", asciiJSON(arg)) // header values must be ASCII
	body, err := d.do(ctx, httpReq, "Dropbox")
	if err != nil {
		return executor.Result{}, err
	}
	var file struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		PathDisplay string `json:"path_display"`
		Size        int64  `json:"size"`
	}
	_ = json.Unmarshal(body, &file)
	output, _ := json.Marshal(map[string]any{"id": file.ID, "name": file.Name, "path": file.PathDisplay, "size": file.Size})
	return executor.Result{Output: output}, nil
}

// asciiJSON escapes non-ASCII characters as \uXXXX so the JSON can travel in an
// HTTP header.
func asciiJSON(data []byte) string {
	var out strings.Builder
	for _, r := range string(data) {
		switch {
		case r < 0x80:
			out.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(&out, "\\u%04x", r)
		default:
			r -= 0x10000
			fmt.Fprintf(&out, "\\u%04x\\u%04x", 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		}
	}
	return out.String()
}
