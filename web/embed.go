// Package web serves the React application embedded in the API binary.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Dist is committed so a Go-only checkout can build. Run make ui-build after
// editing the frontend; the Docker and CI builds regenerate it from source.
//
//go:embed dist
var dist embed.FS

func Handler() http.Handler {
	assets, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/ui", http.FileServer(http.FS(assets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only frontend paths may fall back to the SPA document.
		if !strings.HasPrefix(r.URL.Path, "/ui/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		name := strings.TrimPrefix(path.Clean(strings.TrimPrefix(r.URL.Path, "/ui/")), "/")
		if strings.HasPrefix(name, "assets/") {
			if _, err := fs.Stat(assets, name); err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			files.ServeHTTP(w, r)
			return
		}
		if name != "." && name != "index.html" && strings.Contains(name, ".") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		index, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			http.Error(w, "UI is unavailable", http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(index)
	})
}
