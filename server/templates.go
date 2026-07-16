package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Templates serves template resources over HTTP (§5.2 "template URLs are also
// cacheable resources").
//
// In a real deployment this would be a CDN, a git host, or the app's own static
// origin. What matters for the demo is that templates are fetched by URL over
// the wire, exactly as a VDP client would fetch them — nothing here shares
// memory with the resolver.
type Templates struct {
	// FS holds the template files under a "templates/" prefix.
	FS fs.FS
}

// Routes registers the template resource server.
func (t *Templates) Routes(mux *http.ServeMux) {
	mux.Handle("GET /templates/", t)
}

func (t *Templates) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/templates/")
	name = path.Clean("/" + name)[1:] // Defuse any ".." before touching the FS.
	if name == "" || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}

	body, err := fs.ReadFile(t.FS, "templates/"+name)
	if err != nil {
		// A 404 here is what §9.1 is about: the resolver must skip the slot and
		// carry on. Try /dashboard?fail=chart to watch it happen.
		http.NotFound(w, r)
		return
	}

	// Served as text/plain so the source is readable straight from the browser —
	// these are template resources, not pages. VDP does not define a media type
	// for templates; that is the template engine's concern (§6).
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Access-Control-Allow-Origin", "*") // §10: templates fetched cross-origin need CORS.

	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(body)
}
