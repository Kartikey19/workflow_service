package server

import (
	"net/http/httptest"
	"testing"

	"github.com/kartikey/workflow_service/internal/api"
)

func TestBrowserOrigin(t *testing.T) {
	s := &Server{services: &api.Services{PublicURL: "https://work.example.com"}}
	for _, tt := range []struct {
		method, origin, site string
		ok                   bool
	}{{"POST", "https://work.example.com", "same-origin", true}, {"POST", "https://other.example", "cross-site", false}, {"POST", "null", "", false}, {"POST", "https://work.example.com/path", "", false}, {"POST", "", "", true}, {"POST", "", "cross-site", false}, {"GET", "https://other.example", "cross-site", true}, {"POST", "http://work.example.com", "", false}} {
		r := httptest.NewRequest(tt.method, "https://work.example.com/be/v1/runs", nil)
		r.Header.Set("Origin", tt.origin)
		r.Header.Set("Sec-Fetch-Site", tt.site)
		if got := s.validBrowserOrigin(r); got != tt.ok {
			t.Errorf("%s %q %q = %v", tt.method, tt.origin, tt.site, got)
		}
	}
}
