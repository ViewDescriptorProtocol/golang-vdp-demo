package vdp

import (
	"encoding/json"
	"testing"
)

// §13.2: the discovery document maps endpoint paths to descriptor URLs and
// carries the trusted template URL allowlist.
func TestDiscoveryDocumentParse(t *testing.T) {
	const doc = `{
	  "version": "0.1",
	  "endpoints": {
	    "/api/dashboard": {"descriptor": "https://e.com/views/dashboard.json"},
	    "/api/products/{id}": {"descriptor": "/views/product-detail.json"}
	  },
	  "trustedTemplateUrls": ["https://templates.e.com/"],
	  "futureMember": {"clients": "must ignore this"}
	}`
	var d DiscoveryDocument
	// §13.2 extensibility: unrecognized members are ignored, not errors.
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Version != "0.1" {
		t.Errorf("Version = %q", d.Version)
	}
	if got := d.Endpoints["/api/dashboard"].Descriptor; got != "https://e.com/views/dashboard.json" {
		t.Errorf("dashboard descriptor = %q", got)
	}
	if len(d.TrustedTemplateURLs) != 1 || d.TrustedTemplateURLs[0] != "https://templates.e.com/" {
		t.Errorf("TrustedTemplateURLs = %v", d.TrustedTemplateURLs)
	}
}

// §13.2: endpoint keys may be Level 1 URI Templates. Each expression matches
// exactly one path segment, and a literal entry beats a templated one.
func TestDiscoveryEndpointMatching(t *testing.T) {
	d := DiscoveryDocument{
		Endpoints: map[string]EndpointEntry{
			"/api/dashboard":         {Descriptor: "/views/dashboard.json"},
			"/api/products/{id}":     {Descriptor: "/views/product-detail.json"},
			"/api/products/featured": {Descriptor: "/views/featured.json"},
			"/api/{a}/{b}":           {Descriptor: "/views/two-segments.json"},
		},
	}
	tests := []struct {
		path string
		want string // descriptor of the matched entry; "" means no match
	}{
		{"/api/dashboard", "/views/dashboard.json"},
		{"/api/products/42", "/views/product-detail.json"},
		// A literal entry takes precedence over a templated one.
		{"/api/products/featured", "/views/featured.json"},
		// An expression matches exactly one segment: not two, not zero.
		{"/api/products/42/reviews", ""},
		{"/api/products/", ""},
		// Segment counts must agree: /api/{a}/{b} needs a third segment.
		{"/api/products", ""},
		{"/other/path", ""},
	}
	for _, tc := range tests {
		entry, ok := d.Endpoint(tc.path)
		if tc.want == "" {
			if ok {
				t.Errorf("Endpoint(%q) matched %+v, want no match", tc.path, entry)
			}
			continue
		}
		if !ok || entry.Descriptor != tc.want {
			t.Errorf("Endpoint(%q) = %+v, %v; want descriptor %q", tc.path, entry, ok, tc.want)
		}
	}
}

// §13.2: descriptor values may be relative references, resolved against the
// URL of the discovery document itself.
func TestEndpointDescriptorURLResolution(t *testing.T) {
	docURL := mustParse(t, "https://e.com/.well-known/vdp")
	tests := []struct{ in, want string }{
		{"/views/product-detail.json", "https://e.com/views/product-detail.json"},
		{"https://cdn.e.com/views/d.json", "https://cdn.e.com/views/d.json"},
	}
	for _, tc := range tests {
		got, err := EndpointEntry{Descriptor: tc.in}.DescriptorURL(docURL)
		if err != nil || got != tc.want {
			t.Errorf("DescriptorURL(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

// §12.3/§13.2: the discovery document has its own media type — it is not a
// view descriptor and must not be served as application/vdp+json.
func TestDiscoveryMediaType(t *testing.T) {
	if DiscoveryMediaType != "application/vdp-discovery+json" {
		t.Errorf("DiscoveryMediaType = %q", DiscoveryMediaType)
	}
	if DiscoveryMediaType == MediaType {
		t.Error("discovery must not share the view descriptor media type")
	}
}
