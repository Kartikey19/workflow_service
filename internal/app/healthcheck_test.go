package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	for _, status := range []int{200, 503, 302} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		got := healthcheck(server.URL)
		server.Close()
		if (got == 0) != (status == 200) {
			t.Errorf("status %d exit %d", status, got)
		}
	}
	if healthcheck("://invalid") == 0 {
		t.Fatal("invalid URL passed")
	}
}
