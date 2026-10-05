// Package webui serves the built SvelteKit client.
package webui

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Handler serves the built client, falling back to index.html so that client
// -side routes survive a refresh.
func Handler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	index := filepath.Join(dir, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(r.URL.Path, "/")))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			// Go resolves MIME types from the Windows registry, which maps .apk to
			// application/zip - so Chrome saves the download as "name.apk.zip" and
			// Android then refuses to install it. Set the real type first;
			// ServeContent leaves an existing Content-Type alone.
			if ct := overrideContentType(path); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".apk", ".exe", ".ps1":
				// Content-Type alone is not enough: a cached response, or a browser
				// that sniffs, still renames the download. Naming the file outright
				// removes the guesswork, and no-store stops a bad earlier response
				// from being reused.
				w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("X-Content-Type-Options", "nosniff")
			}
			files.ServeHTTP(w, r)
			return
		}
		// A missing build asset is a 404, never the app shell: a page opened
		// before a deploy asks for chunks by their old names, and handing it
		// HTML in place of JavaScript fails as an opaque "failed to fetch
		// dynamically imported module" instead of a clean miss.
		if strings.HasPrefix(r.URL.Path, "/_app/") {
			http.NotFound(w, r)
			return
		}
		// The shell must never be served stale: it names the hashed bundles.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, index)
	})
}

// Resolve picks the first directory that actually contains a built client.
func Resolve(candidates ...string) string {
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
			return dir
		}
	}
	return ""
}

// overrideContentType returns the correct MIME type for extensions the host's
// registry gets wrong, or "" to leave the default alone.
func overrideContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".apk":
		return "application/vnd.android.package-archive"
	case ".webmanifest":
		return "application/manifest+json"
	case ".exe":
		return "application/vnd.microsoft.portable-executable"
	case ".ps1":
		return "text/plain; charset=utf-8"
	default:
		return ""
	}
}
