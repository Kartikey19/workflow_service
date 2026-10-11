package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSPAAndUnknownRoutes(t *testing.T) {
	h := Handler()
	for _, tt := range []struct {
		path   string
		status int
		html   bool
	}{
		{"/ui/", 200, true},
		{"/ui/accept-invite?token=test", 200, true},
		{"/ui/connections", 200, true},
		{"/ui/assets/missing.js", 404, false},
		{"/ui/missing.js", 404, false},
		{"/", 404, false},
		{"/connections", 404, false},
		{"/assets/missing.js", 404, false},
		{"/be/v1/missing", 404, false},
		{"/v1/missing", 404, false},
		{"/hooks/missing", 404, false},
		{"/ui-other", 404, false},
	} {
		t.Run(tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", tt.path, nil))
			if w.Code != tt.status {
				t.Fatalf("status=%d", w.Code)
			}
			if strings.Contains(w.Body.String(), "<html") != tt.html {
				t.Fatalf("HTML fallback=%v", tt.html)
			}
			if tt.html && (w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") == "") {
				t.Fatal("SPA headers missing")
			}
		})
	}
}

func TestEmbeddedAssetsUnderUIPrefix(t *testing.T) {
	entries, err := fs.ReadDir(dist, "dist/assets")
	if err != nil {
		t.Fatal(err)
	}
	h := Handler()
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, "/ui/assets/"+entry.Name(), nil))
			if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
				t.Fatalf("%s asset: status=%d headers=%v", method, w.Code, w.Header())
			}
			if method == http.MethodHead && w.Body.Len() != 0 {
				t.Fatal("HEAD returned an asset body")
			}
		}
		return
	}
	t.Fatal("no embedded JavaScript asset found")
}
