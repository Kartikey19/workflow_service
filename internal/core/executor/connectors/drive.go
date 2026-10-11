package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/netguard"
)

// idempotencyProperty is the Drive appProperty that records which node attempt
// family created a file, so a redelivered upload finds its file instead of
// creating a second one.
const idempotencyProperty = "wf_key"

// GDrive uploads a file to Google Drive. The connection's scope (drive.file)
// limits it to files this application created.
type GDrive struct{ base }

var _ executor.Executor = (*GDrive)(nil)

func NewGDrive(conns executor.Connections, policy netguard.Policy, ep Endpoints) *GDrive {
	return &GDrive{base{conns, policy, ep}}
}

func (*GDrive) Type() string { return "gdrive.upload" }

func (*GDrive) Describe() executor.Description {
	return executor.Description{
		Title: "Upload to Google Drive", Category: "integration",
		Description: "Uploads a file (text, or base64 for binary, up to 8 MiB) to Google Drive. The upload is idempotent: the file is tagged with " +
			"the node's stable key, and a retry that finds it returns the existing file instead of creating a copy.",
		Example: `- id: archive
  type: gdrive.upload
  needs: [render_report]
  config:
    connection: team-drive
    name: "report-${run.id}.csv"
    content: ${nodes.render_report.output.csv}
    mime_type: text/csv
    folder_id: 1AbCdEfG`,
		Output: `{"id": "<file id>", "name": "...", "web_view_link": "https://drive.google.com/..."}`,
	}
}

func (*GDrive) ValidateConfig(raw json.RawMessage) error {
	if err := executor.CheckFields(raw, append([]executor.Field{
		{Name: "connection", Kind: "string", Required: true},
		{Name: "name", Kind: "string", Required: true},
		{Name: "mime_type", Kind: "string"},
		{Name: "folder_id", Kind: "string"},
	}, contentFields...)); err != nil {
		return err
	}
	return requireContent(raw)
}

func (g *GDrive) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	var c struct {
		Connection string `json:"connection"`
		Name       string `json:"name"`
		MimeType   string `json:"mime_type"`
		FolderID   string `json:"folder_id"`
		content
	}
	if err := executor.DecodeConfig(req.Config, &c); err != nil {
		return executor.Result{}, err
	}
	data, err := c.bytes(MaxUploadBytes)
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	if strings.TrimSpace(c.Name) == "" || strings.ContainsAny(c.Name, "\r\n\x00/") {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("name must be a file name without slashes or line breaks"))
	}
	conn, err := g.connection(ctx, req, c.Connection, "gdrive")
	if err != nil {
		return executor.Result{}, err
	}
	api := g.ep.URL("gdrive_api", "")

	// A previous attempt may have created the file: look for it first.
	query := url.Values{
		"q":      {fmt.Sprintf("appProperties has { key='%s' and value='%s' } and trashed=false", idempotencyProperty, req.IdempotencyKey)},
		"fields": {"files(id,name,webViewLink)"}, "spaces": {"drive"}, "pageSize": {"1"},
	}
	lookup, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/drive/v3/files?"+query.Encode(), nil)
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	authorize(lookup, conn)
	found, err := g.do(ctx, lookup, "Google Drive")
	if err != nil {
		return executor.Result{}, err
	}
	var existing struct {
		Files []driveFile `json:"files"`
	}
	if json.Unmarshal(found, &existing) == nil && len(existing.Files) > 0 {
		return driveResult(existing.Files[0], true)
	}

	mimeType := c.MimeType
	if mimeType == "" {
		mimeType = mime.TypeByExtension(strings.ToLower(extension(c.Name)))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	metadata := map[string]any{"name": c.Name, "appProperties": map[string]string{idempotencyProperty: req.IdempotencyKey}}
	if c.FolderID != "" {
		metadata["parents"] = []string{c.FolderID}
	}
	if c.MimeType != "" {
		metadata["mimeType"] = c.MimeType
	}
	encoded, _ := json.Marshal(metadata)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	_, _ = part.Write(encoded)
	part, _ = writer.CreatePart(textproto.MIMEHeader{"Content-Type": {mimeType}})
	_, _ = part.Write(data)
	_ = writer.Close()

	upload, err := http.NewRequestWithContext(ctx, http.MethodPost,
		g.ep.URL("gdrive_upload", "")+"/upload/drive/v3/files?uploadType=multipart&fields=id,name,webViewLink", &body)
	if err != nil {
		return executor.Result{}, executor.Permanent("bad_config", err)
	}
	authorize(upload, conn)
	upload.Header.Set("Content-Type", "multipart/related; boundary="+writer.Boundary())
	created, err := g.do(ctx, upload, "Google Drive")
	if err != nil {
		return executor.Result{}, err
	}
	var file driveFile
	_ = json.Unmarshal(created, &file)
	return driveResult(file, false)
}

type driveFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	WebViewLink string `json:"webViewLink"`
}

func driveResult(f driveFile, reused bool) (executor.Result, error) {
	output, _ := json.Marshal(map[string]any{"id": f.ID, "name": f.Name, "web_view_link": f.WebViewLink, "reused": reused})
	return executor.Result{Output: output}, nil
}

func extension(name string) string {
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		return name[dot:]
	}
	return ""
}
