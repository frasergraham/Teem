package main

import (
	"bytes"
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
	serveSPAWithBase(w, r, trimPath, "")
}

// serveSPAWithBase is like serveSPA but injects a <base href="..."> tag
// into the index.html so the SPA's relative ./assets/... refs resolve
// against basePrefix instead of the current URL's directory. Needed for
// SPA pages served at deeper paths like /teams/<id>/transcripts/<a>/<j>
// where the directory inferred by the browser would otherwise pull
// assets from /teams/<id>/transcripts/<a>/assets/... and miss the
// /assets/ router branch.
func serveSPAWithBase(w http.ResponseWriter, r *http.Request, trimPath, basePrefix string) {
	dist, err := spaDistFS()
	if err != nil {
		http.Error(w, "spa bundle missing: "+err.Error(), http.StatusInternalServerError)
		return
	}

	clean := strings.TrimPrefix(trimPath, "/")
	if clean == "" {
		serveSPAIndexWithBase(w, r, dist, basePrefix)
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
			serveSPAIndexWithBase(w, r, dist, basePrefix)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = f.Close()

	http.FileServer(http.FS(dist)).ServeHTTP(w, withPath(r, "/"+clean))
}

func serveSPAIndex(w http.ResponseWriter, r *http.Request, dist fs.FS) {
	serveSPAIndexWithBase(w, r, dist, "")
}

func serveSPAIndexWithBase(w http.ResponseWriter, _ *http.Request, dist fs.FS, basePrefix string) {
	data, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		http.Error(w, "spa index.html missing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if basePrefix != "" {
		data = injectBaseHref(data, basePrefix)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// injectBaseHref inserts <base href="basePrefix"> right after the
// opening <head> tag. Idempotent enough for our use — the embedded
// dist/index.html ships without a base element, so this only runs
// once per response. We avoid pulling in an HTML parser for a tag we
// fully control at build time.
func injectBaseHref(data []byte, basePrefix string) []byte {
	headIdx := bytes.Index(data, []byte("<head>"))
	if headIdx < 0 {
		return data
	}
	insertAt := headIdx + len("<head>")
	tag := []byte(`<base href="` + basePrefix + `">`)
	out := make([]byte, 0, len(data)+len(tag))
	out = append(out, data[:insertAt]...)
	out = append(out, tag...)
	out = append(out, data[insertAt:]...)
	return out
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
