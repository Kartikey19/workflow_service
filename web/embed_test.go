package web

import (
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
	}{{"/", 200, true}, {"/accept-invite?token=test", 200, true}, {"/connections", 200, true}, {"/assets/missing.js", 404, false}, {"/v1/missing", 404, false}, {"/hooks/missing", 404, false}, {"/missing.js", 404, false}} {
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
