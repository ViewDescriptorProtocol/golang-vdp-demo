package server

import (
	"bytes"
	"embed"
	"html/template"
	"log"
	"net/http"
)

// The page shell and index are the BFF's own chrome — they wrap whatever the
// VDP template tree produced. They are ordinary server-side templates and have
// nothing to do with the protocol.
//
//go:embed shell.html index.html fallback.html
var chromeFS embed.FS

var chrome = template.Must(template.ParseFS(chromeFS, "*.html"))

type shellData struct {
	Body template.HTML
	// Home controls the back link, which the index itself has no use for.
	Home  bool
	Trace *pageTrace
}

func (b *BFF) writePage(w http.ResponseWriter, pt *pageTrace, body template.HTML) {
	b.writeShell(w, shellData{Body: body, Home: true, Trace: pt})
}

func (b *BFF) writeShell(w http.ResponseWriter, data shellData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := chrome.ExecuteTemplate(w, "shell.html", data); err != nil {
		log.Printf("shell: %v", err)
	}
}

func (b *BFF) index(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if err := chrome.ExecuteTemplate(&buf, "index.html", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	b.writeShell(w, shellData{Body: template.HTML(buf.String())})
}
