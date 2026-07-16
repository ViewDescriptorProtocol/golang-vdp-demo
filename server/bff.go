package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/ViewDescriptorProtocol/golang-vdp-demo/render"
	"github.com/ViewDescriptorProtocol/golang-vdp-demo/vdp"
)

// BFF is a Backend for Frontend (§7.5). It is the VDP *client*: it calls the
// API, extracts the view descriptor, resolves the template tree and renders the
// HTML the browser receives. The browser itself knows nothing about VDP.
//
// The same code would run in an Android or iOS client; only the renderer would
// differ, because the protocol carries template URLs and slot names and nothing
// engine-specific (§1).
type BFF struct {
	// Client makes the API, descriptor and template requests.
	Client *http.Client
	// Static serves the demo's own CSS (BFF chrome, not part of VDP).
	Static fs.FS

	// resolvers are keyed by API origin. Each one is configured from that
	// origin's discovery document and caches templates across requests (§5.2).
	resolvers sync.Map
}

// Routes registers the browser-facing pages. Each maps to one API endpoint.
func (b *BFF) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", b.index)
	mux.Handle("GET /static/", http.FileServerFS(b.Static))

	mux.HandleFunc("GET /dashboard", b.page("/api/dashboard"))
	mux.HandleFunc("GET /login", b.page("/api/login"))
	mux.HandleFunc("GET /product/42", b.page("/api/products/42"))
	mux.HandleFunc("GET /feed", b.page("/api/feed"))
	mux.HandleFunc("GET /odata", b.page("/api/odata/products"))
}

// page returns a handler that runs the full §8 client resolution algorithm
// against one API endpoint and returns rendered HTML (§7.5).
func (b *BFF) page(apiPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tr := &vdp.Trace{}
		pt := &pageTrace{}
		base := baseURL(r)

		// Forward the demo switches (?fail, ?untrusted, ?view) to the API.
		apiURL := base + apiPath
		if q := r.URL.RawQuery; q != "" {
			apiURL += "?" + q
		}

		html, err := b.resolveAndRender(r.Context(), apiURL, r.URL.Query().Get("view"), base, tr, pt)
		pt.Events = tr.Events
		if err != nil {
			// §9.4 rule 2: the root template failed, so there is nothing to
			// compose. Fall back to showing the raw API data rather than nothing.
			log.Printf("vdp: %s: %v", apiURL, err)
			pt.Error = err.Error()
			b.writePage(w, pt, rawDataFallback(pt.Data))
			return
		}
		b.writePage(w, pt, html)
	}
}

// resolveAndRender is the §8 algorithm end to end.
func (b *BFF) resolveAndRender(ctx context.Context, apiURL, viewName, base string, tr *vdp.Trace, pt *pageTrace) (template.HTML, error) {
	pt.APIURL = apiURL
	pt.ViewName = viewName

	// The resolver is configured from the API's discovery document, so the
	// trusted template allowlist comes from the server rather than being
	// hardcoded in the client (§13.2 feeding §10).
	resolver, err := b.resolverFor(ctx, base, pt)
	if err != nil {
		return "", err
	}

	// Step 1: fetch the data and extract the descriptor.
	resp, body, err := b.get(ctx, apiURL, "application/hal+json, application/json")
	if err != nil {
		return "", err
	}
	pt.Status = resp.Status
	pt.ContentType = resp.Header.Get("Content-Type")
	pt.LinkHeader = resp.Header.Get("Link")
	pt.ViewTemplateHeader = resp.Header.Get(vdp.HeaderViewTemplate)
	pt.Data = body

	extraction, err := vdp.Extract(resp, body)
	if err != nil {
		return "", fmt.Errorf("extract view descriptor: %w", err)
	}
	if extraction == nil {
		return "", fmt.Errorf("response carries no view descriptor")
	}
	pt.Transport = string(extraction.Transport)
	pt.DescriptorURL = extraction.DescriptorURL

	// Step 2: fetch the descriptor if it is a standalone resource (§4.1).
	if err := resolver.FetchDescriptor(ctx, extraction); err != nil {
		return "", err
	}
	pt.ViewNames = viewNames(extraction.Views)

	view, err := extraction.View(viewName) // §3.4: fall back to "default".
	if err != nil {
		return "", err
	}
	pt.Descriptor = prettyJSON(view)
	if extraction.Base != nil {
		pt.BaseURL = extraction.Base.String()
	}

	// Steps 3-5: walk the tree, fetching templates and recursing into slots.
	root, err := resolver.Resolve(ctx, view, extraction.Base, tr)
	if err != nil {
		return "", err
	}
	pt.Tree = treeHTML(root, "")

	// Step 6: render the composed tree with the API data.
	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("decode API data: %w", err)
	}
	return render.Render(root, data)
}

// resolverFor returns the resolver for an API origin, building it from that
// origin's discovery document on first use (§13.2).
func (b *BFF) resolverFor(ctx context.Context, base string, pt *pageTrace) (*vdp.Resolver, error) {
	if cached, ok := b.resolvers.Load(base); ok {
		r := cached.(*vdp.Resolver)
		pt.Trusted = r.TrustedTemplateDomains
		return r, nil
	}
	_, body, err := b.get(ctx, base+"/.well-known/vdp", vdp.MediaType)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	var doc vdp.DiscoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	if len(doc.TrustedTemplateDomains) == 0 {
		// §10: an empty allowlist means no template may be rendered. Better to
		// say so than to quietly trust everything.
		return nil, fmt.Errorf("discovery: no trustedTemplateDomains advertised")
	}
	resolver := vdp.NewResolver(b.Client, doc.TrustedTemplateDomains)
	actual, _ := b.resolvers.LoadOrStore(base, resolver)
	r := actual.(*vdp.Resolver)
	pt.Trusted = r.TrustedTemplateDomains
	return r, nil
}

func (b *BFF) get(ctx context.Context, rawURL, accept string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", accept)
	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s: %w", rawURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("GET %s: unexpected status %s", rawURL, resp.Status)
	}
	return resp, body, nil
}

// pageTrace is everything the demo shows in its "what just happened" panel. It
// exists purely to make the protocol visible; a real BFF would not collect it.
type pageTrace struct {
	APIURL             string
	Status             string
	ContentType        string
	LinkHeader         string
	ViewTemplateHeader string
	Transport          string
	DescriptorURL      string
	BaseURL            string
	Descriptor         string
	ViewName           string
	ViewNames          []string
	Trusted            []string
	Tree               template.HTML
	Events             []vdp.Event
	Data               []byte
	Error              string
}

func viewNames(views map[string]vdp.ViewDescriptor) []string {
	return slices.Sorted(maps.Keys(views))
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// treeHTML renders the resolved template tree as a nested list.
func treeHTML(n *vdp.Node, slot string) template.HTML {
	if n == nil {
		return ""
	}
	var b strings.Builder
	label := shortURL(n.URL)
	b.WriteString("<li>")
	if slot != "" {
		b.WriteString(`<span class="tree-slot">` + template.HTMLEscapeString(slot) + `</span> `)
	}
	b.WriteString(`<code>` + template.HTMLEscapeString(label) + `</code>`)
	if len(n.Slots) > 0 {
		b.WriteString("<ul>")
		for _, name := range slices.Sorted(maps.Keys(n.Slots)) { // Stable display.
			for _, child := range n.Slots[name] {
				b.WriteString(string(treeHTML(child, name)))
			}
		}
		b.WriteString("</ul>")
	}
	b.WriteString("</li>")
	return template.HTML(b.String())
}

func shortURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Path
}

// rawDataFallback is the §9.3/§9.4 last resort: when no template tree can be
// composed, show the data rather than an empty page. The markup lives in
// fallback.html so no HTML is baked into the Go code.
func rawDataFallback(data []byte) template.HTML {
	var buf bytes.Buffer
	if err := chrome.ExecuteTemplate(&buf, "fallback.html", struct{ Data string }{string(data)}); err != nil {
		log.Printf("fallback: %v", err)
		return ""
	}
	return template.HTML(buf.String())
}
