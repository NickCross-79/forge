package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/nickcross-79/forge/web"
)

// notBuiltPage is served when the binary was built without a dashboard bundle.
// It is deliberately plain: it exists so that `forge serve` in a fresh checkout
// explains itself instead of returning a bare 404.
const notBuiltPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>forge — dashboard not built</title>
<style>
  :root { color-scheme: light dark; }
  body {
    font: 15px/1.6 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
    margin: 0; min-height: 100vh; display: grid; place-items: center;
    background: #0f1115; color: #e6e8ee;
  }
  main { max-width: 34rem; padding: 2rem; }
  h1 { font-size: 1.25rem; margin: 0 0 .75rem; letter-spacing: -0.01em; }
  p { margin: 0 0 1rem; color: #a8b0c0; }
  code { background: #1a1d25; padding: .15em .4em; border-radius: 4px; color: #e6e8ee; }
  pre { background: #1a1d25; padding: .9rem 1rem; border-radius: 8px; overflow-x: auto; }
  a { color: #6ea8fe; }
</style>
</head>
<body>
<main>
  <h1>The dashboard has not been built</h1>
  <p>The API is running, but this binary does not embed a dashboard bundle.</p>
  <pre>make web &amp;&amp; make build</pre>
  <p>Or run the dev server with hot reload:</p>
  <pre>make dev</pre>
  <p>The REST API is available at <a href="/api/v1/health"><code>/api/v1/health</code></a>.</p>
</main>
</body>
</html>
`

// dashboardHandler serves the embedded single-page dashboard.
//
// Hashed asset filenames are cached aggressively while index.html never is, so a
// rebuilt dashboard is picked up on the next reload. Unknown paths fall back to
// index.html because the dashboard does its own client-side routing — except
// under /api/, where a wrong path should be an honest 404 rather than HTML.
func (s *Server) dashboardHandler() http.Handler {
	if !web.Built() {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(notBuiltPage))
		})
	}

	dist, err := web.Dist()
	if err != nil {
		s.logger.Error("failed to open the embedded dashboard", "error", err)
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
			return
		}

		clean := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "/" {
			clean = "index.html"
		}

		if _, err := fs.Stat(dist, clean); err != nil {
			// Unknown path: hand it to the SPA router.
			serveIndex(w, r, dist)
			return
		}

		if strings.HasPrefix(clean, "assets/") {
			// Vite fingerprints these filenames, so they are safe to cache forever.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// serveIndex writes the SPA entry point.
func serveIndex(w http.ResponseWriter, r *http.Request, dist fs.FS) {
	body, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
