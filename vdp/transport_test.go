package vdp

import (
	"net/http"
	"net/url"
	"testing"
)

// mkResponse builds a response as if fetched from rawURL.
func mkResponse(t *testing.T, rawURL, contentType string, header http.Header) *http.Response {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	if header == nil {
		header = http.Header{}
	}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Request:    &http.Request{Method: http.MethodGet, URL: u},
	}
}

// §4.4: inline body beats Link, which beats View-Template.
func TestExtractPrecedence(t *testing.T) {
	header := http.Header{
		"Link":             {FormatLinkHeader("https://api.example.com/views/dashboard.json")},
		HeaderViewTemplate: {"https://api.example.com/templates/shorthand"},
	}
	body := []byte(`{"_view":{"template":"https://api.example.com/templates/inline"}}`)

	t.Run("inline wins over both headers", func(t *testing.T) {
		resp := mkResponse(t, "https://api.example.com/api/dashboard", "application/hal+json", header.Clone())
		got, err := Extract(resp, body)
		if err != nil {
			t.Fatal(err)
		}
		if got.Transport != TransportInlineView {
			t.Fatalf("transport = %q, want %q", got.Transport, TransportInlineView)
		}
		vd, _ := got.View("")
		if vd.Template != "https://api.example.com/templates/inline" {
			t.Errorf("template = %q", vd.Template)
		}
	})

	t.Run("Link wins over View-Template", func(t *testing.T) {
		resp := mkResponse(t, "https://api.example.com/api/dashboard", "application/json", header.Clone())
		got, err := Extract(resp, []byte(`{"revenue":1}`))
		if err != nil {
			t.Fatal(err)
		}
		if got.Transport != TransportLinkHeader {
			t.Fatalf("transport = %q, want %q", got.Transport, TransportLinkHeader)
		}
		if got.DescriptorURL != "https://api.example.com/views/dashboard.json" {
			t.Errorf("descriptor URL = %q", got.DescriptorURL)
		}
	})

	t.Run("View-Template last", func(t *testing.T) {
		h := http.Header{HeaderViewTemplate: {"https://api.example.com/templates/form"}}
		resp := mkResponse(t, "https://api.example.com/api/login", "application/json", h)
		got, err := Extract(resp, []byte(`{"csrfToken":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		if got.Transport != TransportViewTemplate {
			t.Fatalf("transport = %q, want %q", got.Transport, TransportViewTemplate)
		}
		// §4.1: the shorthand is equivalent to {"template": "<URL>"}.
		vd, _ := got.View("")
		if vd.Template != "https://api.example.com/templates/form" || vd.Slots != nil {
			t.Errorf("got %+v", vd)
		}
	})
}

// §4.2: _views carries several named views inline.
func TestExtractInlineViews(t *testing.T) {
	body := []byte(`{"_views":{"default":{"template":"https://e.com/detail"},"compact":{"template":"https://e.com/card"}},"id":42}`)
	resp := mkResponse(t, "https://e.com/api/products/42", "application/hal+json", nil)
	got, err := Extract(resp, body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Transport != TransportInlineViews {
		t.Fatalf("transport = %q", got.Transport)
	}

	// §3.4: clients use "default" when no view is requested.
	vd, err := got.View("")
	if err != nil || vd.Template != "https://e.com/detail" {
		t.Errorf("default view = %+v, err = %v", vd, err)
	}
	vd, err = got.View("compact")
	if err != nil || vd.Template != "https://e.com/card" {
		t.Errorf("compact view = %+v, err = %v", vd, err)
	}
	// An unknown view name falls back to default rather than failing.
	if vd, err := got.View("nope"); err != nil || vd.Template != "https://e.com/detail" {
		t.Errorf("unknown view = %+v, err = %v", vd, err)
	}
}

// A response with no VDP metadata at all is not an error — it just has no view.
func TestExtractNone(t *testing.T) {
	resp := mkResponse(t, "https://e.com/api/plain", "application/json", nil)
	got, err := Extract(resp, []byte(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil extraction", got)
	}
}

// §4.3: an OData4 body carries the descriptor by Link header, and the odd
// content type must not stop extraction.
func TestExtractODataContentType(t *testing.T) {
	h := http.Header{"Link": {FormatLinkHeader("https://e.com/views/product-list.json")}}
	resp := mkResponse(t, "https://e.com/odata/Products", "application/json;odata.metadata=minimal", h)
	got, err := Extract(resp, []byte(`{"@odata.context":"...","value":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Transport != TransportLinkHeader {
		t.Errorf("transport = %q", got.Transport)
	}
}

// §9.3: a malformed inline descriptor must be rejected rather than ignored.
func TestExtractInvalidInline(t *testing.T) {
	resp := mkResponse(t, "https://e.com/api/x", "application/json", nil)
	if _, err := Extract(resp, []byte(`{"_view":{"slots":{}}}`)); err == nil {
		t.Error("_view without template was accepted, want error")
	}
	if _, err := Extract(resp, []byte(`{"_view":{"template":"a"},"_views":{"default":{"template":"b"}}}`)); err == nil {
		t.Error("both _view and _views was accepted, want error")
	}
}

// §4.1 + §5.4: a relative Link URL resolves against the response URL, and
// becomes the base for the descriptor's own relative template URLs.
func TestExtractLinkRelativeResolution(t *testing.T) {
	h := http.Header{"Link": {`</views/dashboard.json>; rel="view-descriptor"`}}
	resp := mkResponse(t, "https://e.com/api/dashboard", "application/json", h)
	got, err := Extract(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.DescriptorURL != "https://e.com/views/dashboard.json" {
		t.Errorf("descriptor URL = %q", got.DescriptorURL)
	}
	if got.Base.String() != "https://e.com/views/dashboard.json" {
		t.Errorf("base = %q, want the descriptor's own URL", got.Base)
	}
}

// RFC 8288 Link headers: several links, other rels, quoted params, and commas
// inside the URI must all parse correctly.
func TestParseLinks(t *testing.T) {
	values := []string{
		`</api/next?a=1,2>; rel="next", </views/d.json>; rel="view-descriptor"; type="application/vdp+json"`,
		`</style.css>; rel=preload`,
	}
	links := parseLinks(values)
	if len(links) != 3 {
		t.Fatalf("got %d links, want 3: %+v", len(links), links)
	}
	if links[0].URL != "/api/next?a=1,2" {
		t.Errorf("comma inside <> split the field: %q", links[0].URL)
	}
	if !links[1].hasRel(LinkRel) {
		t.Errorf("view-descriptor rel not found: %+v", links[1])
	}
	if links[1].Params["type"] != MediaType {
		t.Errorf("quoted param = %q", links[1].Params["type"])
	}
	if !links[2].hasRel("preload") {
		t.Errorf("unquoted rel not parsed: %+v", links[2])
	}
}

// RFC 8288 allows a space-separated list of relation types.
func TestLinkMultipleRels(t *testing.T) {
	links := parseLinks([]string{`</views/d.json>; rel="alternate view-descriptor"`})
	if len(links) != 1 || !links[0].hasRel(LinkRel) {
		t.Errorf("multi-rel link not matched: %+v", links)
	}
}
