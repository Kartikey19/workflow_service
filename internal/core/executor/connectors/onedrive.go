package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

// maxOneDriveBytes is the largest file Microsoft Graph accepts in a single
// request; bigger files need an upload session.
const maxOneDriveBytes = 4 << 20

// OneDrive uploads a file to the connected account's OneDrive.
type OneDrive struct{ base }

var _ executor.Executor = (*OneDrive)(nil)

func NewOneDrive(conns executor.Connections, policy netguard.Policy, ep Endpoints) *OneDrive {
	return &OneDrive{base{conns, policy, ep}}
}

func (*OneDrive) Type() string { return "onedrive.upload" }

func (*OneDrive) Describe() executor.Description {
	return executor.Description{
		Title: "Upload to OneDrive", Category: "integration",
		Description: "Uploads a file (text, or base64 for binary, up to 4 MiB) to a path in OneDrive, replacing a file already there, so a retry is harmless.",
		Example: `- id: archive
  type: onedrive.upload
  needs: [render_report]
  config:
    connection: team-onedrive
    path: "Reports/report-${run.id}.csv"
    content: ${nodes.render_report.output.csv}`,
		Output: `{"id": "...", "name": "...", "web_url": "https://...", "size": 123}`,
	}
}

func (*OneDrive) ValidateConfig(raw json.RawMessage) error {
	if err := executor.CheckFields(raw, append([]executor.Field{
		{Name: "connection", Kind: "string", Required: true},
		{Name: "path", Kind: "string", Required: true},
	}, contentFields...)); err != nil {
		return err
	}
	return requireContent(raw)
}

func (o *OneDrive) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var c struct {
		Connection string `json:"connection"`
		Path       string `json:"path"`
		content
	}
	if err := executor.DecodeConfig(req.Config, &c); err != nil {
		return executor.Result{}, err
	}
	data, err := c.bytes(maxOneDriveBytes)
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	path := strings.Trim(c.Path, "/")
	if path == "" || strings.ContainsAny(path, "\r\n\x00") || strings.Contains(path, "..") {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("path must be a file path such as Reports/report.csv"))
	}
	conn, err := o.connection(ctx, req, c.Connection, "onedrive")
	if err != nil {
		return executor.Result{}, err
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	target := o.ep.URL("graph_api", "") + "/v1.0/me/drive/root:/" + strings.Join(segments, "/") + ":/content?@microsoft.graph.conflictBehavior=replace"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(data))
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	authorize(httpReq, conn)
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	body, err := o.do(ctx, httpReq, "OneDrive")
	if err != nil {
		return executor.Result{}, err
	}
	var item struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		WebURL string `json:"webUrl"`
		Size   int64  `json:"size"`
	}
	_ = json.Unmarshal(body, &item)
	output, _ := json.Marshal(map[string]any{"id": item.ID, "name": item.Name, "web_url": item.WebURL, "size": item.Size})
	return executor.Result{Output: output}, nil
}
