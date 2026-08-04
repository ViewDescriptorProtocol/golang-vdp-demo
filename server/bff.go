package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// TrustedTemplateURLs is this client's local allowlist configuration — the
	// first and strongest source in the §10 chain. The demo leaves it empty
	// and defers to what each origin's discovery document advertises.
	TrustedTemplateURLs []string

	// resolvers are keyed by API origin. Each one is configured from that
	// origin's discovery document and caches templates across requests (§5.2).
	resolvers sync.Map
}

// platform is what this client answers when a server negotiates
// platform-specific views (§5.5): the BFF renders HTML for the web.
const platform = "web"

// Routes registers the browser-facing pages. Each maps to one API endpoint.
func (b *BFF) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", b.index)
	mux.Handle("GET /static/", http.FileServerFS(b.Static))

	mux.HandleFunc("GET /dashboard", b.page("/api/dashboard"))
	mux.HandleFunc("GET /login", b.page("/api/login"))
	mux.HandleFunc("GET /product/42", b.page("/api/products/42"))
	mux.HandleFunc("GET /feed", b.page("/api/feed"))
	mux.HandleFunc("GET /summary", b.page("/api/summary"))
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
			log.Printf("vdp: %s: %v", apiURL, err)
			pt.Error = err.Error()
			if errors.Is(err, vdp.ErrUnknownMapper) {
				// §9.4 rule 2 / §9.6: the root declared a transform that
				// failed. The template's contract is the transform output, so
				// untransformed data MUST NOT be shown in its place — that
				// would be silently wrong output. Error shell only.
				b.writePage(w, pt, "")
				return
			}
			// §9.4 rule 2: the root template failed with no transform
			// declared; falling back to the raw API data beats nothing.
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

	// Step 6: render per node (§8): each node's template receives its own
	// model — the transform output, or the representation unchanged. The
	// input is the extraction's §4.2 transform input (the body with any
	// embedded _view/_views removed).
	return render.Render(root, extraction.TransformInput)
}

// resolverFor returns the resolver for an API origin, building it from that
// origin's discovery document on first use (§13.2). The template allowlist
// follows the §10 source chain: local configuration first, then whatever the
// discovery document advertises, and — with neither — the resolver's built-in
// same-origin default.
func (b *BFF) resolverFor(ctx context.Context, base string, pt *pageTrace) (*vdp.Resolver, error) {
	if cached, ok := b.resolvers.Load(base); ok {
		r := cached.(*vdp.Resolver)
		pt.Trusted = trustedForTrace(r)
		return r, nil
	}
	_, body, err := b.get(ctx, base+vdp.WellKnownPath, vdp.DiscoveryMediaType+", application/json")
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	var doc vdp.DiscoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	trusted := b.TrustedTemplateURLs // §10 source 1: local configuration.
	if len(trusted) == 0 {
		trusted = doc.TrustedTemplateURLs // §10 source 2: discovery document.
	}
	// §10 source 3: with neither, the resolver applies its same-origin default.
	resolver := vdp.NewResolver(b.Client, trusted)
	resolver.Platform = platform // §5.5
	// §3.8.3: mapper support is OPTIONAL; this client opts in with one
	// registered mapper. The code lives here, in the client — a descriptor
	// can name it but never supply it (§10).
	resolver.RegisterMapper(SummaryMapperURI, summaryTotals)
	actual, _ := b.resolvers.LoadOrStore(base, resolver)
	r := actual.(*vdp.Resolver)
	pt.Trusted = trustedForTrace(r)
	return r, nil
}

// trustedForTrace names the allowlist for the trace panel, making the §10
// same-origin default visible instead of showing an empty list.
func trustedForTrace(r *vdp.Resolver) []string {
	if len(r.TrustedTemplateURLs) == 0 {
		return []string{"(same-origin default)"}
	}
	return r.TrustedTemplateURLs
}

func (b *BFF) get(ctx context.Context, rawURL, accept string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", accept)
	// §5.5: negotiation applies to whichever request returns the descriptor,
	// which for inline transports is the API request itself.
	req.Header.Set(vdp.HeaderPlatform, platform)
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
	label := shortURL(n.ID)
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

// summaryTotals is the client-registered mapping code behind SummaryMapperURI
// (§3.8.3): it sums the response's per-day figures into the card template's
// {revenue, users, orders} contract. Cross-field derivation like this is
// deliberately outside the inline transform grammar (§3.8.1).
func summaryTotals(in any) any {
	totals := map[string]any{"revenue": 0.0, "users": 0.0, "orders": 0.0}
	data, ok := in.(map[string]any)
	if !ok {
		return totals
	}
	days, _ := data["days"].([]any)
	for _, day := range days {
		row, ok := day.(map[string]any)
		if !ok {
			continue
		}
		for key := range totals {
			if v, ok := row[key].(float64); ok {
				totals[key] = totals[key].(float64) + v
			}
		}
	}
	return totals
}
