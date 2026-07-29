package server

import (
	"strings"
	"testing"

	"github.com/ViewDescriptorProtocol/golang-vdp-demo/vdp"
)

// §9.4 rule 2: when the root template cannot be composed, the fallback shows the
// raw API data instead of an empty page.
func TestRawDataFallbackShowsData(t *testing.T) {
	got := string(rawDataFallback([]byte(`{"name":"Widget"}`)))
	if !strings.Contains(got, "Fallback: raw API data") {
		t.Errorf("missing fallback heading: %q", got)
	}
	if !strings.Contains(got, `{"name":"Widget"}`) && !strings.Contains(got, "Widget") {
		t.Errorf("data not shown: %q", got)
	}
	if !strings.Contains(got, "<pre>") {
		t.Errorf("data should be in a <pre> block: %q", got)
	}
}

// §9.3: with no data there is nothing left to render, so the fallback says so
// rather than emitting an empty data panel.
func TestRawDataFallbackEmpty(t *testing.T) {
	got := string(rawDataFallback(nil))
	if !strings.Contains(got, "Nothing to render") {
		t.Errorf("empty data should show the nothing-to-render panel: %q", got)
	}
	if strings.Contains(got, "Fallback: raw API data") {
		t.Errorf("empty data must not show the raw-data panel: %q", got)
	}
}

// §10: templates are trusted, the data they carry is not. The fallback renders
// the data through html/template so markup in it is escaped, never executed.
func TestRawDataFallbackEscapesData(t *testing.T) {
	got := string(rawDataFallback([]byte(`{"x":"<script>alert(1)</script>"}`)))
	if strings.Contains(got, "<script>") {
		t.Errorf("unescaped markup leaked from the data: %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("data should be HTML-escaped: %q", got)
	}
}

// treeHTML renders the resolved tree as a nested list; a nil node is the empty
// string so callers can splice it unconditionally.
func TestTreeHTMLNil(t *testing.T) {
	if got := string(treeHTML(nil, "")); got != "" {
		t.Errorf("nil node should render empty, got %q", got)
	}
}

func TestTreeHTMLSingleNode(t *testing.T) {
	n := &vdp.Node{ID: "https://e.com/templates/layout.html"}
	got := string(treeHTML(n, ""))
	if !strings.Contains(got, "<code>/templates/layout.html</code>") {
		t.Errorf("expected shortened path label, got %q", got)
	}
	if strings.Contains(got, "<ul>") {
		t.Errorf("leaf node should not open a child list: %q", got)
	}
}

// §3.5: child slots render nested, and maps.Keys is sorted so the display is
// stable regardless of Go's map iteration order.
func TestTreeHTMLNestedAndStable(t *testing.T) {
	n := &vdp.Node{
		ID: "https://e.com/layout",
		Slots: map[string][]*vdp.Node{
			"zeta":  {{ID: "https://e.com/z"}},
			"alpha": {{ID: "https://e.com/a"}},
		},
	}
	got := string(treeHTML(n, ""))
	if !strings.Contains(got, "<ul>") {
		t.Fatalf("nested slots should open a child list: %q", got)
	}
	// alpha sorts before zeta no matter the map order.
	if strings.Index(got, "alpha") > strings.Index(got, "zeta") {
		t.Errorf("slots not rendered in sorted order: %q", got)
	}
}

// The slot label comes from the descriptor, so it is escaped like any untrusted
// string before being written into the list.
func TestTreeHTMLEscapesSlotLabel(t *testing.T) {
	n := &vdp.Node{ID: "https://e.com/x"}
	got := string(treeHTML(n, "<b>evil</b>"))
	if strings.Contains(got, "<b>evil</b>") {
		t.Errorf("slot label not escaped: %q", got)
	}
	if !strings.Contains(got, "&lt;b&gt;evil&lt;/b&gt;") {
		t.Errorf("expected escaped slot label: %q", got)
	}
}

// shortURL keeps only the path, so tree labels stay readable regardless of host
// or port. A value that will not parse falls back to itself.
func TestShortURL(t *testing.T) {
	cases := map[string]string{
		"https://e.com/templates/card.html":  "/templates/card.html",
		"http://localhost:8099/views/x.json": "/views/x.json",
		"":                                   "",
	}
	for in, want := range cases {
		if got := shortURL(in); got != want {
			t.Errorf("shortURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// viewNames lists the views a descriptor offers in sorted order for a stable
// trace display.
func TestViewNamesSorted(t *testing.T) {
	got := viewNames(map[string]vdp.ViewDescriptor{
		"table": {},
		"cards": {},
		"admin": {},
	})
	want := []string{"admin", "cards", "table"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("viewNames = %v, want %v", got, want)
	}
}

// prettyJSON indents so the trace's descriptor dump is readable.
func TestPrettyJSON(t *testing.T) {
	got := prettyJSON(map[string]any{"a": 1})
	if !strings.Contains(got, "\n") || !strings.Contains(got, "  ") {
		t.Errorf("expected indented JSON, got %q", got)
	}
}
