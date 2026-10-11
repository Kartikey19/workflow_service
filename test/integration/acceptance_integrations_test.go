//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kartikey/workflow_service/internal/config"
)

// fakeCloud stands in for Google, Dropbox and Microsoft: their OAuth token
// endpoints, account lookups and the file and mail APIs the nodes call. It
// records what it was sent so tests can check the nodes' requests.
type fakeCloud struct {
	*httptest.Server
	mu sync.Mutex

	challenge     string // PKCE challenge the test read from the authorize URL
	expiresIn     int    // lifetime of issued access tokens, seconds
	revoked       bool   // refresh requests answer invalid_grant
	tokensIssued  int
	refreshGrants atomic.Int32
	validTokens   map[string]bool
	rejectAPI     int // when set, API calls answer this status

	gmailRaw            []string
	gmailAuth           []string
	driveFiles          map[string]map[string]string // idempotency key -> file
	driveLookups        int
	driveUploads        int
	driveCreateThenFail bool
	driveParents        []string

	dropboxArgs       []string
	dropboxBodies     [][]byte
	dropboxStatus     int // answer this status once, then succeed
	dropboxRetryAfter string

	graphPaths  []string
	graphBodies [][]byte
	graphAuth   []string
}

func startFakeCloud(t *testing.T) *fakeCloud {
	f := &fakeCloud{expiresIn: 3600, validTokens: map[string]bool{}, driveFiles: map[string]map[string]string{}}
	mux := http.NewServeMux()
	token := func(prefix string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			f.mu.Lock()
			defer f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if r.Form.Get("client_id") != prefix+"-client" || r.Form.Get("client_secret") != prefix+"-secret" {
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
				return
			}
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
				if r.Form.Get("code") != "auth-code-1" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"bad code or verifier"}`)
					return
				}
			case "refresh_token":
				f.refreshGrants.Add(1)
				if f.revoked || r.Form.Get("refresh_token") != prefix+"-refresh" {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
					return
				}
			}
			f.tokensIssued++
			access := fmt.Sprintf("%s-access-%d", prefix, f.tokensIssued)
			f.validTokens[access] = true
			resp := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": f.expiresIn, "scope": "granted-scope"}
			if r.Form.Get("grant_type") == "authorization_code" {
				resp["refresh_token"] = prefix + "-refresh"
			}
			_ = json.NewEncoder(w).Encode(resp)
		}
	}
	bearer := func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.rejectAPI != 0 {
			w.WriteHeader(f.rejectAPI)
			_, _ = io.WriteString(w, `{"error":{"message":"api says no"}}`)
			return false
		}
		if !f.validTokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid credentials"}}`)
			return false
		}
		return true
	}
	mux.HandleFunc("/google/token", token("google"))
	mux.HandleFunc("/dropbox/token", token("dropbox"))
	mux.HandleFunc("/microsoft/token", token("microsoft"))

	mux.HandleFunc("/google/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if bearer(w, r) {
			_, _ = io.WriteString(w, `{"email":"agent@example.com"}`)
		}
	})
	mux.HandleFunc("/2/users/get_current_account", func(w http.ResponseWriter, r *http.Request) {
		if bearer(w, r) {
			_, _ = io.WriteString(w, `{"email":"dbx@example.com"}`)
		}
	})
	mux.HandleFunc("/v1.0/me", func(w http.ResponseWriter, r *http.Request) {
		if bearer(w, r) {
			_, _ = io.WriteString(w, `{"mail":"","userPrincipalName":"ms@example.com"}`)
		}
	})

	mux.HandleFunc("/gmail/v1/users/me/messages/send", func(w http.ResponseWriter, r *http.Request) {
		if !bearer(w, r) {
			return
		}
		var body struct{ Raw string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := base64.RawURLEncoding.DecodeString(body.Raw)
		f.mu.Lock()
		f.gmailRaw = append(f.gmailRaw, string(raw))
		f.gmailAuth = append(f.gmailAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"msg-1","threadId":"thread-1"}`)
	})

	mux.HandleFunc("/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		if !bearer(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.driveLookups++
		q := r.URL.Query().Get("q")
		files := []map[string]string{}
		for key, file := range f.driveFiles {
			if strings.Contains(q, "value='"+key+"'") {
				files = append(files, file)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
	})
	mux.HandleFunc("/upload/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		if !bearer(w, r) {
			return
		}
		mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != "multipart/related" {
			w.WriteHeader(400)
			return
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		metaPart, _ := reader.NextPart()
		metaRaw, _ := io.ReadAll(metaPart)
		var meta struct {
			Name          string            `json:"name"`
			Parents       []string          `json:"parents"`
			AppProperties map[string]string `json:"appProperties"`
		}
		_ = json.Unmarshal(metaRaw, &meta)
		dataPart, _ := reader.NextPart()
		data, _ := io.ReadAll(dataPart)
		f.mu.Lock()
		f.driveUploads++
		f.driveParents = append(f.driveParents, strings.Join(meta.Parents, ","))
		id := fmt.Sprintf("file-%d", f.driveUploads)
		file := map[string]string{"id": id, "name": meta.Name, "webViewLink": "https://drive.example.com/" + id, "content": string(data)}
		f.driveFiles[meta.AppProperties["wf_key"]] = file
		failAfter := f.driveCreateThenFail
		f.driveCreateThenFail = false
		f.mu.Unlock()
		if failAfter { // the file exists but the caller never learns of it
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(file)
	})

	mux.HandleFunc("/2/files/upload", func(w http.ResponseWriter, r *http.Request) {
		if !bearer(w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.dropboxArgs = append(f.dropboxArgs, r.Header.Get("Dropbox-API-Arg"))
		f.dropboxBodies = append(f.dropboxBodies, body)
		status, retryAfter := f.dropboxStatus, f.dropboxRetryAfter
		f.dropboxStatus = 0
		f.mu.Unlock()
		if status != 0 {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error_summary":"too_many_requests/..","error":{".tag":"too_many_requests"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"id:abc","name":"résumé.txt","path_display":"/Reports/résumé.txt","size":`+strconv.Itoa(len(body))+`}`)
	})

	mux.HandleFunc("/v1.0/me/drive/root:/", func(w http.ResponseWriter, r *http.Request) {
		if !bearer(w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.graphPaths = append(f.graphPaths, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		f.graphBodies = append(f.graphBodies, body)
		f.graphAuth = append(f.graphAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"item-1","name":"report.csv","webUrl":"https://onedrive.example.com/item-1","size":`+strconv.Itoa(len(body))+`}`)
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeCloud) endpoints() string {
	m := map[string]string{
		"google_auth": f.URL + "/google/auth", "google_token": f.URL + "/google/token", "google_userinfo": f.URL + "/google/userinfo",
		"gmail_api": f.URL, "gdrive_api": f.URL, "gdrive_upload": f.URL,
		"dropbox_auth": f.URL + "/dropbox/auth", "dropbox_token": f.URL + "/dropbox/token", "dropbox_api": f.URL, "dropbox_content": f.URL,
		"microsoft_auth": f.URL + "/microsoft/auth", "microsoft_token": f.URL + "/microsoft/token", "graph_api": f.URL,
	}
	data, _ := json.Marshal(m)
	return string(data)
}

func withCloud(f *fakeCloud) func(*config.Config) {
	return func(c *config.Config) {
		c.Auth.PublicURL = "http://app.test"
		c.Services.GoogleClientID, c.Services.GoogleClientSecret = "google-client", "google-secret"
		c.Services.DropboxAppKey, c.Services.DropboxAppSecret = "dropbox-client", "dropbox-secret"
		c.Services.MicrosoftClientID, c.Services.MicrosoftClientSecret = "microsoft-client", "microsoft-secret"
		c.Services.IntegrationEndpoints = f.endpoints()
	}
}

// connect runs the whole OAuth dance for one kind: start, then the provider's
// redirect to the callback, returning the callback's Location.
func (f *fakeCloud) connect(t *testing.T, s *stack, owner *person, kind, name string) string {
	t.Helper()
	started := owner.mustOK("POST", "/be/v1/connections/oauth/start", map[string]any{"kind": kind, "name": name}, 200)
	authorize, err := url.Parse(started["authorize_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	query := authorize.Query()
	f.mu.Lock()
	f.challenge = query.Get("code_challenge")
	f.mu.Unlock()
	anon := newPerson(t, s.base)
	status, _, header := anon.do("GET", "/be/v1/oauth/callback?code=auth-code-1&state="+url.QueryEscape(query.Get("state")), nil)
	if status != http.StatusFound {
		t.Fatalf("callback = %d", status)
	}
	return header.Get("Location")
}

func nodeWorkflow(name, nodeType, nodeConfig string) string {
	return "apiVersion: workflow/v1\nname: " + name + "\nnodes:\n  - id: act\n    type: " + nodeType +
		"\n    retry: {max_attempts: 3, backoff: fixed, initial: 10ms}\n    config: " + nodeConfig + "\n"
}

// M14: the OAuth sign-in. The authorize URL carries a PKCE challenge, the
// callback needs no session and is single-use, and a connection stores its
// account but never its tokens.
func TestOAuthConnectFlow(t *testing.T) {
	ctx := context.Background()
	cloud := startFakeCloud(t)
	s := newStack(t, ctx, withCloud(cloud))
	owner := s.owner(t, "OAuth Co")

	// A provider the operator has not configured is not offered.
	plain := newStack(t, ctx)
	plainOwner := plain.owner(t, "No OAuth Co")
	_, kinds := plainOwner.call("GET", "/be/v1/integrations", nil)
	for _, k := range kinds["items"].([]any) {
		if k.(map[string]any)["oauth"] == true {
			t.Fatalf("an unconfigured OAuth kind is offered: %v", k)
		}
	}
	if status, _ := plainOwner.call("POST", "/be/v1/connections/oauth/start", map[string]any{"kind": "gmail", "name": "g"}); status != 400 {
		t.Fatalf("starting an unconfigured kind = %d, want 400", status)
	}

	_, kinds = owner.call("GET", "/be/v1/integrations", nil)
	offered := []string{}
	for _, k := range kinds["items"].([]any) {
		offered = append(offered, k.(map[string]any)["kind"].(string))
	}
	if strings.Join(offered, ",") != "smtp,resend,gmail,gdrive,gdocs,gsheets,gcalendar,dropbox,onedrive,postgres,mysql,mongo,slack,notion,mcp" {
		t.Fatalf("offered kinds = %v", offered)
	}
	// OAuth kinds cannot be created with pasted credentials.
	if status, _ := owner.call("POST", "/be/v1/connections", map[string]any{"name": "pasted", "kind": "gmail", "secret": map[string]any{"access_token": "x"}}); status != 400 {
		t.Fatalf("creating an OAuth kind directly = %d, want 400", status)
	}

	started := owner.mustOK("POST", "/be/v1/connections/oauth/start", map[string]any{"kind": "gmail", "name": "my-gmail"}, 200)
	authorize, _ := url.Parse(started["authorize_url"].(string))
	q := authorize.Query()
	if authorize.Path != "/google/auth" || q.Get("client_id") != "google-client" || q.Get("redirect_uri") != "http://app.test/be/v1/oauth/callback" ||
		q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("access_type") != "offline" || q.Get("state") == "" ||
		!strings.Contains(q.Get("scope"), "gmail.send") || strings.Contains(q.Get("scope"), "drive") {
		t.Fatalf("authorize URL = %s", started["authorize_url"])
	}
	cloud.challenge = q.Get("code_challenge")

	anon := newPerson(t, s.base)
	callback := "/be/v1/oauth/callback?code=auth-code-1&state=" + url.QueryEscape(q.Get("state"))
	status, _, header := anon.do("GET", callback, nil)
	if status != http.StatusFound || header.Get("Location") != "http://app.test/ui/connectors?connected=my-gmail" {
		t.Fatalf("callback = %d %s", status, header.Get("Location"))
	}
	got := owner.mustOK("GET", "/be/v1/connections/my-gmail", nil, 200)
	if got["kind"] != "gmail" || got["account"] != "agent@example.com" || got["expires_at"] == nil || got["config"].(map[string]any)["account"] != "agent@example.com" {
		t.Fatalf("connection = %v", got)
	}
	if strings.Contains(toJSON(got), "access-") || strings.Contains(toJSON(got), "refresh") {
		t.Fatalf("a token leaked into the connection view: %v", got)
	}
	owner.mustOK("POST", "/be/v1/connections/my-gmail/test", nil, 200)

	// The state is single use; unknown and tampered states and provider errors
	// land back on the UI with a message and create nothing.
	for name, path := range map[string]string{
		"replayed":       callback,
		"unknown state":  "/be/v1/oauth/callback?code=auth-code-1&state=forged",
		"provider error": "/be/v1/oauth/callback?error=access_denied&error_description=user+said+no&state=" + url.QueryEscape(q.Get("state")),
	} {
		_, _, header := anon.do("GET", path, nil)
		location, _ := url.Parse(header.Get("Location"))
		if location.Path != "/ui/connectors" || location.Query().Get("error") == "" || location.Query().Get("connected") != "" {
			t.Errorf("%s: Location = %s", name, header.Get("Location"))
		}
	}
	// A wrong code (so the token exchange fails) creates nothing.
	started = owner.mustOK("POST", "/be/v1/connections/oauth/start", map[string]any{"kind": "dropbox", "name": "my-dropbox"}, 200)
	authorize, _ = url.Parse(started["authorize_url"].(string))
	cloud.challenge = "not-the-challenge"
	_, _, header = anon.do("GET", "/be/v1/oauth/callback?code=auth-code-1&state="+url.QueryEscape(authorize.Query().Get("state")), nil)
	if location, _ := url.Parse(header.Get("Location")); location.Query().Get("error") == "" {
		t.Fatalf("a failed token exchange should come back as an error: %s", header.Get("Location"))
	}
	if status, _ := owner.call("GET", "/be/v1/connections/my-dropbox", nil); status != 404 {
		t.Fatalf("a failed sign-in created a connection: %d", status)
	}

	// Another organization cannot use the name or see the connection.
	other := s.owner(t, "Other OAuth Co")
	if status, _ := other.call("GET", "/be/v1/connections/my-gmail", nil); status != 404 {
		t.Fatalf("another organization reads the connection: %d", status)
	}
	// Reconnecting the same name replaces its tokens; another kind cannot take the name.
	if status, body := owner.call("POST", "/be/v1/connections/oauth/start", map[string]any{"kind": "dropbox", "name": "my-gmail"}); status != 409 {
		t.Fatalf("reusing a name for another kind = %d %v, want 409", status, body)
	}
	if location := cloud.connect(t, s, owner, "gmail", "my-gmail"); !strings.Contains(location, "connected=my-gmail") {
		t.Fatalf("reconnect: %s", location)
	}
	if _, list := owner.call("GET", "/be/v1/connections", nil); len(list["items"].([]any)) != 1 {
		t.Fatalf("reconnecting must not create a second connection: %v", list)
	}
}

// gmail.send: the message reaches the Gmail API as RFC 5322 with the connected
// account as sender, and the token is stored nowhere.
func TestGmailSend(t *testing.T) {
	ctx := context.Background()
	cloud := startFakeCloud(t)
	s := newStack(t, ctx, withCloud(cloud))
	owner := s.owner(t, "Gmail Co")
	cloud.connect(t, s, owner, "gmail", "my-gmail")

	if status, body := owner.publish(nodeWorkflow("gmail-it", "gmail.send", `{connection: my-gmail, to: [a@example.com], bcc: hidden@example.com, subject: "Hi ${run.id}", text: "hello there"}`)); status != 201 {
		t.Fatalf("publish = %d %v", status, body)
	}
	id, run := runToEnd(t, owner, "gmail-it")
	if run["status"] != "succeeded" {
		t.Fatalf("run = %v", run)
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if len(cloud.gmailRaw) != 1 || !strings.HasPrefix(cloud.gmailAuth[0], "Bearer google-access-") {
		t.Fatalf("Gmail received %d messages with auth %v", len(cloud.gmailRaw), cloud.gmailAuth)
	}
	msg := cloud.gmailRaw[0]
	for _, want := range []string{"From: <agent@example.com>", "To: <a@example.com>", "Subject: Hi " + id, "hello there", "Message-ID: <"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "hidden@example.com") {
		t.Fatal("the Bcc recipient is in the message headers")
	}
	for _, secret := range []string{"google-access-1", "google-refresh"} {
		if n := s.h.occurrences(secret); n != 0 {
			t.Errorf("%q is stored in %d row(s)", secret, n)
		}
	}
}

// Tokens are refreshed shortly before they expire, once however many nodes need
// it; a revoked grant fails the node permanently and says to reconnect.
func TestOAuthTokenRefresh(t *testing.T) {
	ctx := context.Background()
	cloud := startFakeCloud(t)
	cloud.expiresIn = 1 // issued tokens are about to expire immediately
	s := newStack(t, ctx, withCloud(cloud))
	owner := s.owner(t, "Refresh Co")
	cloud.connect(t, s, owner, "gmail", "my-gmail")
	cloud.expiresIn = 3600 // refreshed tokens last
	if status, body := owner.publish(nodeWorkflow("refresh-it", "gmail.send", `{connection: my-gmail, to: a@example.com, subject: hi, text: body}`)); status != 201 {
		t.Fatalf("publish = %d %v", status, body)
	}

	// Many runs at once: the stored token is stale, one refresh serves them all.
	var wg sync.WaitGroup
	statuses := make([]string, 12)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, run := runToEnd(t, owner, "refresh-it")
			statuses[i], _ = run["status"].(string)
		}()
	}
	wg.Wait()
	for i, status := range statuses {
		if status != "succeeded" {
			t.Fatalf("run %d = %s", i, status)
		}
	}
	if grants := cloud.refreshGrants.Load(); grants < 1 || grants > 3 {
		t.Fatalf("%d refresh grants for 12 concurrent runs, want one (a race with another process may add one or two)", grants)
	}
	cloud.mu.Lock()
	for _, auth := range cloud.gmailAuth {
		if auth == "Bearer google-access-1" {
			cloud.mu.Unlock()
			t.Fatal("a send used the expired access token")
		}
	}
	cloud.mu.Unlock()
	got := owner.mustOK("GET", "/be/v1/connections/my-gmail", nil, 200)
	if expires, _ := time.Parse(time.RFC3339, got["expires_at"].(string)); time.Until(expires) < 30*time.Minute {
		t.Fatalf("the refreshed token expiry was not stored: %v", got["expires_at"])
	}

	// Revoke the grant, then make the stored token look stale.
	cloud.mu.Lock()
	cloud.revoked = true
	cloud.mu.Unlock()
	if _, err := s.h.pool.Exec(ctx, `update connections set token_expires_at = now() - interval '1 minute' where name = 'my-gmail'`); err != nil {
		t.Fatal(err)
	}
	id, run := runToEnd(t, owner, "refresh-it")
	if run["status"] != "failed" {
		t.Fatalf("run = %v", run)
	}
	attempts := s.h.attempts(mustParse(t, id), "act")
	if len(attempts) != 1 || attempts[0]["code"] != "E_CONNECTION_REAUTH" {
		t.Fatalf("attempts = %v, want one attempt failing E_CONNECTION_REAUTH", attempts)
	}
	if e := s.h.nodeError(mustParse(t, id), "act"); !strings.Contains(e["message"].(string), "reconnect") {
		t.Fatalf("error = %v", e)
	}
}

// gdrive.upload tags the file with the node's key, so a retry after a lost
// response finds the file instead of uploading a copy.
func TestGDriveUploadIsIdempotent(t *testing.T) {
	ctx := context.Background()
	cloud := startFakeCloud(t)
	s := newStack(t, ctx, withCloud(cloud))
	owner := s.owner(t, "Drive Co")
	cloud.connect(t, s, owner, "gdrive", "team-drive")
	if status, body := owner.publish(nodeWorkflow("drive-it", "gdrive.upload",
		`{connection: team-drive, name: "report.csv", content: "a,b\n1,2\n", folder_id: folder-9}`)); status != 201 {
		t.Fatalf("publish = %d %v", status, body)
	}

	// The first attempt's upload succeeds on the server but the response is lost.
	cloud.mu.Lock()
	cloud.driveCreateThenFail = true
	cloud.mu.Unlock()
	id, run := runToEnd(t, owner, "drive-it")
	if run["status"] != "succeeded" {
		t.Fatalf("run = %v", run)
	}
	cloud.mu.Lock()
	uploads, lookups := cloud.driveUploads, cloud.driveLookups
	file := cloud.driveFiles[firstKey(cloud.driveFiles)]
	parents := cloud.driveParents[0]
	cloud.mu.Unlock()
	if uploads != 1 || lookups != 2 {
		t.Fatalf("%d uploads and %d lookups; the retry must find the file, not upload again", uploads, lookups)
	}
	if file["content"] != "a,b\n1,2\n" || parents != "folder-9" {
		t.Fatalf("file = %v in %q", file, parents)
	}
	var output map[string]any
	if err := s.h.pool.QueryRow(ctx, `select output from node_executions where run_id = $1 and node_id = 'act' order by attempt desc limit 1`, mustParse(t, id)).Scan(&output); err != nil {
		t.Fatal(err)
	}
	if output["id"] != "file-1" || output["reused"] != true || !strings.HasPrefix(output["web_view_link"].(string), "https://drive.example.com/") {
		t.Fatalf("output = %v", output)
	}
	if attempts := s.h.attempts(mustParse(t, id), "act"); len(attempts) != 2 || attempts[0]["code"] != "upstream_5xx" {
		t.Fatalf("attempts = %v", attempts)
	}
}

func firstKey(m map[string]map[string]string) string {
	for k := range m {
		return k
	}
	return ""
}

// dropbox.upload: the path travels in an ASCII-safe header, overwrite is the
// default, binary content works, and rate limits honour Retry-After.
func TestDropboxUpload(t *testing.T) {
	ctx := context.Background()
	cloud := startFakeCloud(t)
	s := newStack(t, ctx, withCloud(cloud))
	owner := s.owner(t, "Dropbox Co")
	cloud.connect(t, s, owner, "dropbox", "team-dropbox")
	binary := base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 0xff, 0xfe})
	for _, step := range []struct{ name, cfg string }{ // in this order: the assertions below index the uploads
		{"dbx-default", `{connection: team-dropbox, path: "/Reports/résumé.txt", content: "hello"}`},
		{"dbx-add", `{connection: team-dropbox, path: "/Reports/new.txt", content: "hello", overwrite: false}`},
		{"dbx-binary", `{connection: team-dropbox, path: "/Reports/b.bin", content_base64: "` + binary + `"}`},
	} {
		if status, body := owner.publish(nodeWorkflow(step.name, "dropbox.upload", step.cfg)); status != 201 {
			t.Fatalf("publish %s = %d %v", step.name, status, body)
		}
		if _, run := runToEnd(t, owner, step.name); run["status"] != "succeeded" {
			t.Fatalf("%s: %v", step.name, run)
		}
	}
	cloud.mu.Lock()
	args, bodies := append([]string(nil), cloud.dropboxArgs...), cloud.dropboxBodies
	cloud.mu.Unlock()
	if len(args) != 3 {
		t.Fatalf("Dropbox received %d uploads", len(args))
	}
	for _, arg := range args {
		for _, r := range arg {
			if r > 0x7f {
				t.Fatalf("Dropbox-API-Arg is not ASCII: %q", arg)
			}
		}
	}
	var first, second map[string]any
	_ = json.Unmarshal([]byte(args[0]), &first)
	_ = json.Unmarshal([]byte(args[1]), &second)
	if first["path"] != "/Reports/résumé.txt" || first["mode"] != "overwrite" || first["autorename"] != false || second["mode"] != "add" || second["path"] != "/Reports/new.txt" {
		t.Fatalf("args = %v / %v", first, second)
	}
	if string(bodies[0]) != "hello" || len(bodies[2]) != 5 || bodies[2][3] != 0xff {
		t.Fatalf("bodies = %q / %v", bodies[0], bodies[2])
	}

	// A rate limit is retried and honours Retry-After.
	cloud.mu.Lock()
	cloud.dropboxStatus, cloud.dropboxRetryAfter = 429, "1"
	cloud.mu.Unlock()
	start := time.Now()
	id, run := runToEnd(t, owner, "dbx-default")
	if run["status"] != "succeeded" || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("run = %v after %v, want a success after the 1s Retry-After", run["status"], time.Since(start))
	}
	if attempts := s.h.attempts(mustParse(t, id), "act"); len(attempts) != 2 || attempts[0]["code"] != "rate_limited" {
		t.Fatalf("attempts = %v", attempts)
	}
	// A conflict is permanent.
	cloud.mu.Lock()
	cloud.dropboxStatus = 409
	cloud.mu.Unlock()
	id, run = runToEnd(t, owner, "dbx-add")
	if attempts := s.h.attempts(mustParse(t, id), "act"); run["status"] != "failed" || len(attempts) != 1 {
		t.Fatalf("a 409 must fail once: %v %v", run["status"], attempts)
	}
}

// onedrive.upload: path segments are escaped, files are replaced, big files and
// bad paths are refused before any request, and a rejected token is permanent.
func TestOneDriveUpload(t *testing.T) {
	ctx := context.Background()
	cloud := startFakeCloud(t)
	s := newStack(t, ctx, withCloud(cloud))
	owner := s.owner(t, "OneDrive Co")
	cloud.connect(t, s, owner, "onedrive", "team-onedrive")
	if status, body := owner.publish(nodeWorkflow("od-ok", "onedrive.upload", `{connection: team-onedrive, path: "Reports/Q3 2026/résumé #1.csv", content: "x,y"}`)); status != 201 {
		t.Fatalf("publish = %d %v", status, body)
	}
	if _, run := runToEnd(t, owner, "od-ok"); run["status"] != "succeeded" {
		t.Fatalf("run = %v", run)
	}
	cloud.mu.Lock()
	path, body, auth := cloud.graphPaths[0], string(cloud.graphBodies[0]), cloud.graphAuth[0]
	cloud.mu.Unlock()
	if !strings.HasPrefix(path, "/v1.0/me/drive/root:/Reports/Q3%202026/r%C3%A9sum%C3%A9%20%231.csv:/content?") || !strings.Contains(path, "conflictBehavior=replace") ||
		body != "x,y" || !strings.HasPrefix(auth, "Bearer microsoft-access-") {
		t.Fatalf("request = %s body %q auth %q", path, body, auth)
	}

	for name, cfg := range map[string]string{
		"od-traversal": `{connection: team-onedrive, path: "../etc/passwd", content: "x"}`,
		"od-badb64":    `{connection: team-onedrive, path: "a.bin", content_base64: "***"}`,
	} {
		if status, resp := owner.publish(nodeWorkflow(name, "onedrive.upload", cfg)); status != 201 {
			t.Fatalf("publish %s = %d %v", name, status, resp)
		}
		id, run := runToEnd(t, owner, name)
		attempts := s.h.attempts(mustParse(t, id), "act")
		if run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != "bad_config" {
			t.Errorf("%s: %v %v, want one bad_config failure", name, run["status"], attempts)
		}
	}
	cloud.mu.Lock()
	uploadsBefore := len(cloud.graphPaths)
	cloud.rejectAPI = 401
	cloud.mu.Unlock()
	id, run := runToEnd(t, owner, "od-ok")
	attempts := s.h.attempts(mustParse(t, id), "act")
	if run["status"] != "failed" || len(attempts) != 1 || attempts[0]["code"] != "auth" || uploadsBefore != 1 {
		t.Fatalf("a rejected token: %v %v", run["status"], attempts)
	}
}

// Publish-time validation of the app nodes.
func TestAppNodeConfigValidation(t *testing.T) {
	ctx := context.Background()
	cloud := startFakeCloud(t)
	s := newStack(t, ctx, withCloud(cloud))
	owner := s.owner(t, "Validate Apps Co")
	for name, tt := range map[string]struct{ nodeType, config string }{
		"gmail no body":           {"gmail.send", `{connection: c, to: a@example.com, subject: hi}`},
		"gmail no recipient":      {"gmail.send", `{connection: c, subject: hi, text: x}`},
		"gmail no connection":     {"gmail.send", `{to: a@example.com, subject: hi, text: x}`},
		"drive no content":        {"gdrive.upload", `{connection: c, name: f.txt}`},
		"drive both contents":     {"gdrive.upload", `{connection: c, name: f.txt, content: a, content_base64: YQ==}`},
		"drive unknown field":     {"gdrive.upload", `{connection: c, name: f.txt, content: a, public: true}`},
		"dropbox no path":         {"dropbox.upload", `{connection: c, content: a}`},
		"dropbox overwrite shape": {"dropbox.upload", `{connection: c, path: /a, content: a, overwrite: maybe}`},
		"onedrive no content":     {"onedrive.upload", `{connection: c, path: a.txt}`},
	} {
		if status, _ := owner.publish("apiVersion: workflow/v1\nname: bad-app\nnodes:\n  - id: a\n    type: " + tt.nodeType + "\n    config: " + tt.config + "\n"); status != 400 {
			t.Errorf("%s: publish = %d, want 400", name, status)
		}
	}
}
