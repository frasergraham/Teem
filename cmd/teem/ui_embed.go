package main

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:ui/dist
var spaFS embed.FS

// spaDistFS returns the SPA's dist/ directory rooted (so paths inside
// are e.g. "index.html", "assets/...").
func spaDistFS() (fs.FS, error) {
	return fs.Sub(spaFS, "ui/dist")
}

// serveSPA serves the SPA bundle for /teams/<id>/v2[/...].
//
// Layout: trimPath is the path inside the bundle ("" or "assets/...").
// History fallback: any path that doesn't resolve to a real file in
// dist/ returns index.html so the React router can take over.
func serveSPA(w http.ResponseWriter, r *http.Request, trimPath string) {
	dist, err := spaDistFS()
	if err != nil {
		http.Error(w, "spa bundle missing: "+err.Error(), http.StatusInternalServerError)
		return
	}

	clean := strings.TrimPrefix(trimPath, "/")
	if clean == "" {
		serveSPAIndex(w, r, dist)
		return
	}

	f, err := dist.Open(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Real asset paths (anything with an extension) must 404 so
			// missing-image / missing-bundle bugs don't get masked by the
			// history fallback.
			if hasFileExtension(clean) {
				http.NotFound(w, r)
				return
			}
			serveSPAIndex(w, r, dist)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = f.Close()

	http.FileServer(http.FS(dist)).ServeHTTP(w, withPath(r, "/"+clean))
}

func serveSPAIndex(w http.ResponseWriter, _ *http.Request, dist fs.FS) {
	data, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		http.Error(w, "spa index.html missing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// withPath returns a shallow clone of r with URL.Path rewritten so
// http.FileServer reads the correct file from the sub-FS.
func withPath(r *http.Request, p string) *http.Request {
	r2 := r.Clone(r.Context())
	r2.URL.Path = p
	return r2
}

// hasFileExtension reports whether the last path segment carries an
// extension like ".js" / ".css" / ".png". Used to gate the history
// fallback so we don't serve index.html for /assets/missing.png.
func hasFileExtension(p string) bool {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	return strings.Contains(p, ".")
}
