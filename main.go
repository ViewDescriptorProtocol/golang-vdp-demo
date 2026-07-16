// Command golang-vdp-demo runs a working demonstration of the View Descriptor
// Protocol v0.1 in Go.
//
// It starts one HTTP server playing the three roles VDP defines a relationship
// between, all on the same port but talking to each other over real HTTP:
//
//	API             /api/...     data, with a view descriptor attached (§4)
//	                /views/...   standalone view descriptor resources (§5)
//	                /.well-known/vdp  discovery (§13)
//	Template server /templates/  templates, fetched by URL (§5.2)
//	BFF             /            resolves the tree and renders HTML (§7.5, §8)
//
// Spec: https://viewdescriptorprotocol.github.io/specification/
package main

import (
	"embed"
	"errors"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/ViewDescriptorProtocol/golang-vdp-demo/server"
)

// Templates are embedded, but the resolver never touches this FS — it fetches
// them over HTTP from the template server like any other VDP client would.
//
//go:embed templates
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	mux := http.NewServeMux()

	api := &server.API{}
	api.Routes(mux)

	templates := &server.Templates{FS: templatesFS}
	templates.Routes(mux)

	bff := &server.BFF{
		Client: &http.Client{Timeout: 10 * time.Second},
		Static: staticFS,
	}
	bff.Routes(mux)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("VDP demo listening on http://localhost%s", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
