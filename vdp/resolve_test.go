package vdp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// templateServer serves the named templates and counts requests, so tests can
// assert on caching (§5.2) and on templates never being fetched at all (§10).
type templateServer struct {
	*httptest.Server
	hits sync.Map // path -> *atomic.Int64
}

func newTemplateServer(t *testing.T, files map[string]string) *templateServer {
	t.Helper()
	ts := &templateServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter, _ := ts.hits.LoadOrStore(r.URL.Path, &atomic.Int64{})
		counter.(*atomic.Int64).Add(1)

		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *templateServer) hitCount(path string) int64 {
	if c, ok := ts.hits.Load(path); ok {
		return c.(*atomic.Int64).Load()
	}
	return 0
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// §8: the resolver walks the descriptor into a tree of fetched templates.
// §5.4: relative template URLs resolve against the base, and nested slots use
// that same base rather than their parent's URL.
func TestResolveTree(t *testing.T) {
	ts := newTemplateServer(t, map[string]string{
		"/templates/layout.html": "layout",
		"/templates/main.html":   "main",
		"/templates/chart.html":  "chart",
		"/templates/legend.html": "legend",
	})
	r := NewResolver(ts.Client(), []string{ts.URL + "/templates"})

	// A descriptor resource served from /views/, using relative template URLs.
	base := mustParse(t, ts.URL+"/views/dashboard.json")
	vd := ViewDescriptor{
		Template: "../templates/layout.html",
		Slots: Slots{
			"main": Single(ViewDescriptor{
				Template: "../templates/main.html",
				Slots: Slots{
					"chart": Single(ViewDescriptor{
						Template: "../templates/chart.html",
						Slots:    Slots{"legend": Single(ViewDescriptor{Template: "../templates/legend.html"})},
					}),
				},
			}),
		},
	}

	root, err := r.Resolve(context.Background(), vd, base, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if root.URL != ts.URL+"/templates/layout.html" || root.Body != "layout" {
		t.Fatalf("root = %+v", root)
	}
	// The deepest node proves the base did not drift at each nesting level.
	legend := root.Slots["main"][0].Slots["chart"][0].Slots["legend"][0]
	if legend.Body != "legend" {
		t.Errorf("legend body = %q", legend.Body)
	}
}

// §3.5: array slots resolve in the order they were declared.
func TestResolveSlotArrayOrder(t *testing.T) {
	ts := newTemplateServer(t, map[string]string{
		"/templates/layout.html": "layout",
		"/templates/a.html":      "a",
		"/templates/b.html":      "b",
		"/templates/c.html":      "c",
	})
	r := NewResolver(ts.Client(), []string{ts.URL + "/templates"})

	vd := ViewDescriptor{
		Template: ts.URL + "/templates/layout.html",
		Slots: Slots{
			"main": Sequence(
				ViewDescriptor{Template: ts.URL + "/templates/a.html"},
				ViewDescriptor{Template: ts.URL + "/templates/b.html"},
				ViewDescriptor{Template: ts.URL + "/templates/c.html"},
			),
		},
	}
	root, err := r.Resolve(context.Background(), vd, nil, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var got []string
	for _, n := range root.Slots["main"] {
		got = append(got, n.Body)
	}
	if strings.Join(got, "") != "abc" {
		t.Errorf("render order = %v, want [a b c]", got)
	}
}

// Slot resolution order is deterministic. VDP does not require this, but Go's
// random map iteration would otherwise make fetch order and traces differ on
// every run, which is miserable to debug against.
func TestResolveSlotOrderIsDeterministic(t *testing.T) {
	files := map[string]string{"/templates/layout.html": "layout"}
	slots := Slots{}
	for _, name := range []string{"zulu", "alpha", "mike", "bravo"} {
		files["/templates/"+name+".html"] = name
		slots[name] = Single(ViewDescriptor{Template: "/templates/" + name + ".html"})
	}
	ts := newTemplateServer(t, files)
	vd := ViewDescriptor{Template: "/templates/layout.html", Slots: slots}
	base := mustParse(t, ts.URL+"/views/d.json")

	var runs []string
	for range 5 {
		// A fresh resolver each time, so the cache cannot mask ordering.
		r := NewResolver(ts.Client(), []string{ts.URL + "/templates"})
		tr := &Trace{}
		if _, err := r.Resolve(context.Background(), vd, base, tr); err != nil {
			t.Fatal(err)
		}
		var order []string
		for _, e := range tr.Events {
			if e.Slot != "" {
				order = append(order, e.Slot)
			}
		}
		runs = append(runs, strings.Join(order, ","))
	}
	const want = "alpha,bravo,mike,zulu"
	for i, got := range runs {
		if got != want {
			t.Errorf("run %d resolved slots in order %q, want %q", i, got, want)
		}
	}
}

// §9.1: a slot whose template 404s is skipped; the rest of the tree survives.
func TestResolveSkipsFailedSlot(t *testing.T) {
	ts := newTemplateServer(t, map[string]string{
		"/templates/layout.html": "layout",
		"/templates/ok.html":     "ok",
	})
	r := NewResolver(ts.Client(), []string{ts.URL + "/templates"})

	vd := ViewDescriptor{
		Template: ts.URL + "/templates/layout.html",
		Slots: Slots{
			"good": Single(ViewDescriptor{Template: ts.URL + "/templates/ok.html"}),
			"bad":  Single(ViewDescriptor{Template: ts.URL + "/templates/missing.html"}),
		},
	}

	tr := &Trace{}
	root, err := r.Resolve(context.Background(), vd, nil, tr)
	if err != nil {
		t.Fatalf("a failed slot must not fail the whole render: %v", err)
	}
	if len(root.Slots["good"]) != 1 {
		t.Error("good slot should still be filled")
	}
	if _, ok := root.Slots["bad"]; ok {
		t.Error("failed slot should be absent, letting the template default")
	}
	if !hasEvent(tr, "slot-skipped") {
		t.Error("failure should be traced for diagnostics (§9.1)")
	}
}

// §9.4 rule 2: a root template failure has no partial result to offer, so the
// caller is told and can fall back.
func TestResolveRootFailure(t *testing.T) {
	ts := newTemplateServer(t, map[string]string{})
	r := NewResolver(ts.Client(), []string{ts.URL + "/templates"})

	_, err := r.Resolve(context.Background(), ViewDescriptor{Template: ts.URL + "/templates/missing.html"}, nil, nil)
	if err == nil {
		t.Fatal("root failure should return an error")
	}
}

// §10: templates outside the allowlist must not be fetched at all — not
// fetched-then-discarded.
func TestResolveRejectsUntrusted(t *testing.T) {
	evil := newTemplateServer(t, map[string]string{"/templates/nav.html": "pwned"})
	ts := newTemplateServer(t, map[string]string{"/templates/layout.html": "layout"})
	r := NewResolver(ts.Client(), []string{ts.URL + "/templates"})

	vd := ViewDescriptor{
		Template: ts.URL + "/templates/layout.html",
		Slots:    Slots{"nav": Single(ViewDescriptor{Template: evil.URL + "/templates/nav.html"})},
	}
	root, err := r.Resolve(context.Background(), vd, nil, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, ok := root.Slots["nav"]; ok {
		t.Error("untrusted slot was rendered")
	}
	if n := evil.hitCount("/templates/nav.html"); n != 0 {
		t.Errorf("untrusted template was fetched %d times, want 0", n)
	}

	// A rejected root surfaces the reason rather than a generic failure.
	_, err = r.Resolve(context.Background(), ViewDescriptor{Template: evil.URL + "/templates/nav.html"}, nil, nil)
	if !errors.Is(err, ErrUntrustedTemplate) {
		t.Errorf("err = %v, want ErrUntrustedTemplate", err)
	}
}

// §10: the allowlist must match on path segment boundaries, or a "/templates"
// entry would also trust "/templates-evil".
func TestCheckTrustedPathBoundary(t *testing.T) {
	r := NewResolver(nil, []string{"https://cdn.example.com/templates"})
	tests := []struct {
		url   string
		allow bool
	}{
		{"https://cdn.example.com/templates/a.html", true},
		{"https://cdn.example.com/templates", true},
		{"https://cdn.example.com/templates-evil/a.html", false},
		{"https://cdn.example.com/other/a.html", false},
		{"https://evil.example.com/templates/a.html", false},
		{"http://cdn.example.com/templates/a.html", false}, // Scheme must match.
	}
	for _, tc := range tests {
		err := r.checkTrusted(mustParse(t, tc.url))
		if (err == nil) != tc.allow {
			t.Errorf("checkTrusted(%s): allowed = %v, want %v", tc.url, err == nil, tc.allow)
		}
	}
}

// §10: with no allowlist, nothing is trusted. Failing open would make the
// allowlist an opt-in defence, which is the wrong default.
func TestEmptyAllowlistTrustsNothing(t *testing.T) {
	r := NewResolver(nil, nil)
	if err := r.checkTrusted(mustParse(t, "https://cdn.example.com/templates/a.html")); err == nil {
		t.Error("empty allowlist trusted a template")
	}
}

// §8: clients must bound recursion depth.
func TestResolveMaxDepth(t *testing.T) {
	ts := newTemplateServer(t, map[string]string{"/templates/a.html": "a"})
	r := NewResolver(ts.Client(), []string{ts.URL + "/templates"})
	r.MaxDepth = 3

	// Build a chain deeper than MaxDepth.
	leaf := ViewDescriptor{Template: ts.URL + "/templates/a.html"}
	for range 5 {
		leaf = ViewDescriptor{
			Template: ts.URL + "/templates/a.html",
			Slots:    Slots{"next": Single(leaf)},
		}
	}

	tr := &Trace{}
	root, err := r.Resolve(context.Background(), leaf, nil, tr)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The tree is truncated at the limit, not rejected outright: the too-deep
	// slot is just another slot that could not be resolved (§9.1).
	depth := 1
	for n := root; len(n.Slots["next"]) > 0; n = n.Slots["next"][0] {
		depth++
	}
	if depth != r.MaxDepth {
		t.Errorf("tree depth = %d, want %d", depth, r.MaxDepth)
	}
	if !hasEvent(tr, "depth-exceeded") {
		t.Error("depth limit should be traced")
	}
}

// §5.2: templates are cached, so a template reused across slots is fetched once.
func TestResolveCachesTemplates(t *testing.T) {
	ts := newTemplateServer(t, map[string]string{
		"/templates/layout.html": "layout",
		"/templates/card.html":   "card",
	})
	r := NewResolver(ts.Client(), []string{ts.URL + "/templates"})

	vd := ViewDescriptor{
		Template: ts.URL + "/templates/layout.html",
		Slots: Slots{
			"a": Single(ViewDescriptor{Template: ts.URL + "/templates/card.html"}),
			"b": Single(ViewDescriptor{Template: ts.URL + "/templates/card.html"}),
		},
	}
	if _, err := r.Resolve(context.Background(), vd, nil, nil); err != nil {
		t.Fatal(err)
	}
	// And again on a second request, to prove the cache outlives one resolution.
	if _, err := r.Resolve(context.Background(), vd, nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := ts.hitCount("/templates/card.html"); n != 1 {
		t.Errorf("card.html fetched %d times, want 1", n)
	}
}

// §4.1 + §5: a Link-transported descriptor is fetched from its own URL.
func TestFetchDescriptor(t *testing.T) {
	var descriptorURL string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", MediaType)
		fmt.Fprint(w, `{"template":"../templates/layout.html","slots":{"main":{"template":"../templates/main.html"}}}`)
	}))
	defer ts.Close()
	descriptorURL = ts.URL + "/views/dashboard.json"

	r := NewResolver(ts.Client(), []string{ts.URL + "/templates"})
	e := &Extraction{
		Transport:     TransportLinkHeader,
		DescriptorURL: descriptorURL,
		Base:          mustParse(t, descriptorURL),
	}
	if err := r.FetchDescriptor(context.Background(), e); err != nil {
		t.Fatalf("FetchDescriptor: %v", err)
	}
	if e.DescriptorURL != "" {
		t.Error("DescriptorURL should be cleared once fetched")
	}
	vd, err := e.View("")
	if err != nil {
		t.Fatal(err)
	}
	if vd.Template != "../templates/layout.html" {
		t.Errorf("template = %q", vd.Template)
	}
}

// §9.3: a descriptor resource that is malformed must be rejected.
func TestFetchDescriptorInvalid(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", MediaType)
		fmt.Fprint(w, `{"slots":{}}`) // No "template".
	}))
	defer ts.Close()

	r := NewResolver(ts.Client(), nil)
	e := &Extraction{DescriptorURL: ts.URL + "/views/bad.json"}
	if err := r.FetchDescriptor(context.Background(), e); err == nil {
		t.Error("invalid descriptor resource was accepted")
	}
}

func hasEvent(tr *Trace, kind string) bool {
	for _, e := range tr.Events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
