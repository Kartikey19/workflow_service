// Package connectors implements the nodes that act on third-party services
// through a saved connection: gmail.send, gdrive.upload, dropbox.upload and
// onedrive.upload (02 §4.12). Each takes its credentials from
// executor.Connections; none imports the store.
package connectors

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Endpoints are the provider URLs the integrations call. The defaults are the
// public APIs; an operator can override any of them for a proxy or a test server.
// Tenants cannot.
type Endpoints map[string]string

var defaults = map[string]string{
	"google_auth":     "https://accounts.google.com/o/oauth2/v2/auth",
	"google_token":    "https://oauth2.googleapis.com/token",
	"google_userinfo": "https://openidconnect.googleapis.com/v1/userinfo",
	"gmail_api":       "https://gmail.googleapis.com",
	"gdrive_api":      "https://www.googleapis.com",
	"gdrive_upload":   "https://www.googleapis.com",
	"dropbox_auth":    "https://www.dropbox.com/oauth2/authorize",
	"dropbox_token":   "https://api.dropboxapi.com/oauth2/token",
	"dropbox_api":     "https://api.dropboxapi.com",
	"dropbox_content": "https://content.dropboxapi.com",
	"microsoft_auth":  "https://login.microsoftonline.com/{tenant}/oauth2/v2.0/authorize",
	"microsoft_token": "https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token",
	"graph_api":       "https://graph.microsoft.com",
}

// ParseEndpoints reads the INTEGRATION_ENDPOINTS JSON object, rejecting unknown
// keys so a typo does not silently leave the public endpoint in place.
func ParseEndpoints(text string) (Endpoints, error) {
	overrides := map[string]string{}
	if strings.TrimSpace(text) != "" {
		if err := json.Unmarshal([]byte(text), &overrides); err != nil {
			return nil, fmt.Errorf("INTEGRATION_ENDPOINTS must be a JSON object of strings: %w", err)
		}
	}
	out := make(Endpoints, len(defaults))
	for key, value := range defaults {
		out[key] = value
	}
	for key, value := range overrides {
		if _, known := defaults[key]; !known {
			return nil, fmt.Errorf("INTEGRATION_ENDPOINTS: unknown key %q", key)
		}
		out[key] = strings.TrimRight(value, "/")
	}
	return out, nil
}

// URL returns the endpoint for key, with {tenant} replaced.
func (e Endpoints) URL(key, tenant string) string {
	if e == nil {
		e = Endpoints(defaults)
	}
	value, ok := e[key]
	if !ok {
		value = defaults[key]
	}
	return strings.ReplaceAll(strings.TrimRight(value, "/"), "{tenant}", tenant)
}
