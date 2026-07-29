package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ViewDescriptorProtocol/golang-vdp-demo/vdp"
)

// newAPIServer serves just the API role, with the real template tree so
// integrity metadata can be computed.
func newAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	(&API{Templates: os.DirFS("..")}).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func getBody(t *testing.T, srv *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp, body
}

// §13.2: the discovery document uses its own media type, the trustedTemplateUrls
// member, and endpoint entries holding descriptor URLs — including a Level 1
// URI-Template key and a relative descriptor URL.
func TestDiscoveryDocument(t *testing.T) {
	srv := newAPIServer(t)
	resp, body := getBody(t, srv, "/.well-known/vdp")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, vdp.DiscoveryMediaType) {
		t.Errorf("Content-Type = %q, want %q", ct, vdp.DiscoveryMediaType)
	}
	var doc vdp.DiscoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Version != vdp.Version {
		t.Errorf("version = %q", doc.Version)
	}
	if len(doc.TrustedTemplateURLs) == 0 {
		t.Error("trustedTemplateUrls missing")
	}
	// A relative descriptor URL, resolved against the document's own URL.
	if got := doc.Endpoints["/api/dashboard"].Descriptor; got != "/views/dashboard.json" {
		t.Errorf("dashboard descriptor = %q, want relative /views/dashboard.json", got)
	}
	// A templated key matching any single product id.
	entry, ok := doc.Endpoint("/api/products/42")
	if !ok || entry.Descriptor == "" {
		t.Errorf("templated endpoint for /api/products/42 = %+v, %v", entry, ok)
	}
}

// §3.6/§3.7: the dashboard descriptor carries template metadata and fills its
// nav slot by descriptor reference; §13.1: descriptor resources may carry
// VDP-Version.
func TestDashboardViewMetadata(t *testing.T) {
	srv := newAPIServer(t)
	resp, body := getBody(t, srv, "/views/dashboard.json")
	if got := resp.Header.Get(vdp.HeaderVersion); got != vdp.Version {
		t.Errorf("VDP-Version = %q, want %q", got, vdp.Version)
	}
	s := string(body)
	if !strings.Contains(s, `"integrity": "sha384-`) {
		t.Errorf("descriptor carries no integrity metadata: %s", s)
	}
	if !strings.Contains(s, `"descriptor": "nav.json"`) {
		t.Errorf("sidebarNav should be a descriptor reference: %s", s)
	}
	if !strings.Contains(s, `"type": "`) {
		t.Errorf("descriptor carries no type hint: %s", s)
	}
}

// §3.6: the ?fail=integrity switch publishes a digest that cannot match, so a
// client must treat the slot as failed.
func TestDashboardViewIntegritySwitch(t *testing.T) {
	srv := newAPIServer(t)
	_, body := getBody(t, srv, "/views/dashboard.json?fail=integrity")
	if !strings.Contains(string(body), badIntegrity) {
		t.Errorf("?fail=integrity should publish the bogus digest, got: %s", body)
	}
}

// §3.7: the referenced nav descriptor is a standalone resource; its ?untrusted
// switch swaps in a template outside the allowlist.
func TestNavView(t *testing.T) {
	srv := newAPIServer(t)
	resp, body := getBody(t, srv, "/views/nav.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	views, err := vdp.Parse(body)
	if err != nil {
		t.Fatalf("nav.json is not a valid descriptor: %v", err)
	}
	if vd := views[vdp.DefaultView]; !strings.Contains(vd.Template, "nav.html") {
		t.Errorf("nav template = %q", vd.Template)
	}

	_, body = getBody(t, srv, "/views/nav.json?untrusted")
	if !strings.Contains(string(body), "evil.example.com") {
		t.Errorf("?untrusted should swap in an untrusted template, got: %s", body)
	}
}

// §13.2: the templated /api/products/{id} entry points at a standalone
// product-detail descriptor for prefetching.
func TestProductDetailView(t *testing.T) {
	srv := newAPIServer(t)
	resp, body := getBody(t, srv, "/views/product-detail.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if _, err := vdp.Parse(body); err != nil {
		t.Fatalf("product-detail.json is not a valid descriptor: %v", err)
	}
}

// baseURL reconstructs the public origin so descriptors can carry absolute
// template URLs whatever port the demo runs on.
func TestBaseURL(t *testing.T) {
	r := httptest.NewRequest("GET", "http://example.test:8099/dashboard", nil)
	if got := baseURL(r); got != "http://example.test:8099" {
		t.Errorf("baseURL = %q, want http://example.test:8099", got)
	}
}

// A TLS request reports an https origin.
func TestBaseURLTLS(t *testing.T) {
	r := httptest.NewRequest("GET", "https://secure.test/dashboard", nil)
	if got := baseURL(r); got != "https://secure.test" {
		t.Errorf("baseURL = %q, want https://secure.test", got)
	}
}

// Behind a proxy the scheme comes from X-Forwarded-Proto, and only the first
// value in a comma-separated list is used.
func TestBaseURLForwardedProto(t *testing.T) {
	r := httptest.NewRequest("GET", "http://proxied.test/dashboard", nil)
	r.Header.Set("X-Forwarded-Proto", "https, http")
	if got := baseURL(r); got != "https://proxied.test" {
		t.Errorf("baseURL = %q, want https://proxied.test", got)
	}
}

// §5.4 form (c): the OData product-list descriptor demonstrates the
// scheme-less opaque template identifier — host-qualified, no scheme, compared
// and cached verbatim by clients — and the discovery allowlist carries a
// matching scheme-less entry, since §13.2 matching never crosses identifier
// forms.
func TestProductListViewUsesOpaqueIdentifier(t *testing.T) {
	srv := newAPIServer(t)
	host := strings.TrimPrefix(srv.URL, "http://")

	_, body := getBody(t, srv, "/views/product-list.json")
	var vd vdp.ViewDescriptor
	if err := json.Unmarshal(body, &vd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := host + "/templates/components/data-display/odata-table.html"
	if vd.Template != want {
		t.Errorf("template = %q, want the opaque identifier %q", vd.Template, want)
	}

	_, body = getBody(t, srv, "/.well-known/vdp")
	var doc vdp.DiscoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !slices.Contains(doc.TrustedTemplateURLs, host+"/templates/") {
		t.Errorf("trustedTemplateUrls = %v, missing scheme-less entry %q", doc.TrustedTemplateURLs, host+"/templates/")
	}
}
