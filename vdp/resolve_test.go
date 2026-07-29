package vdp

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
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
		Template: "/templates/layout.html",
		Slots: Slots{
			"main": Single(ViewDescriptor{
				Template: "/templates/main.html",
				Slots: Slots{
					"chart": Single(ViewDescriptor{
						Template: "/templates/chart.html",
						Slots:    Slots{"legend": Single(ViewDescriptor{Template: "/templates/legend.html"})},
					}),
				},
			}),
		},
	}

	root, err := r.Resolve(context.Background(), vd, base, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if root.ID != ts.URL+"/templates/layout.html" || root.Body != "layout" {
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
		err := r.checkTrusted(tc.url, nil)
		if (err == nil) != tc.allow {
			t.Errorf("checkTrusted(%s): allowed = %v, want %v", tc.url, err == nil, tc.allow)
		}
	}
}

// §10: when no allowlist is configured or advertised, the last step of the
// source chain applies — only template URLs sharing an origin with the view
// descriptor's base URL are trusted.
func TestEmptyAllowlistSameOriginDefault(t *testing.T) {
	r := NewResolver(nil, nil)
	base := mustParse(t, "https://e.com/views/dashboard.json")
	tests := []struct {
		url   string
		base  *url.URL
		allow bool
	}{
		{"https://e.com/templates/a.html", base, true},
		{"https://cdn.example.com/templates/a.html", base, false}, // Cross-origin.
		{"http://e.com/templates/a.html", base, false},            // Scheme is part of the origin.
		// With no base either, nothing can be established as trusted.
		{"https://e.com/templates/a.html", nil, false},
	}
	for _, tc := range tests {
		err := r.checkTrusted(tc.url, tc.base)
		if (err == nil) != tc.allow {
			t.Errorf("checkTrusted(%s, base=%v): allowed = %v, want %v", tc.url, tc.base, err == nil, tc.allow)
		}
	}
}

// §10 chain end to end: a resolver with an empty allowlist resolves a tree
// whose templates share the descriptor's origin, and rejects one that strays.
func TestResolveSameOriginDefault(t *testing.T) {
	ts := newTemplateServer(t, map[string]string{"/templates/layout.html": "layout"})
	evil := newTemplateServer(t, map[string]string{"/templates/nav.html": "pwned"})
	r := NewResolver(ts.Client(), nil)

	base := mustParse(t, ts.URL+"/views/dashboard.json")
	vd := ViewDescriptor{
		Template: "/templates/layout.html",
		Slots:    Slots{"nav": Single(ViewDescriptor{Template: evil.URL + "/templates/nav.html"})},
	}
	root, err := r.Resolve(context.Background(), vd, base, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if root.Body != "layout" {
		t.Errorf("same-origin root should resolve, got %+v", root)
	}
	if _, ok := root.Slots["nav"]; ok {
		t.Error("cross-origin slot was rendered under the same-origin default")
	}
	if n := evil.hitCount("/templates/nav.html"); n != 0 {
		t.Errorf("cross-origin template was fetched %d times, want 0", n)
	}
}

// §3.6: template bytes are verified against integrity metadata; a match renders
// and a mismatch is a template fetch failure for that slot (§9.1).
func TestResolveIntegrity(t *testing.T) {
	ts := newTemplateServer(t, map[string]string{
		"/templates/layout.html": "layout",
		"/templates/card.html":   "card",
	})
	good := sriDigest("card")
	bad := sriDigest("tampered")

	t.Run("match renders", func(t *testing.T) {
		r := NewResolver(ts.Client(), []string{ts.URL + "/templates"})
		vd := ViewDescriptor{
			Template: ts.URL + "/templates/layout.html",
			Slots: Slots{
				"main": Single(ViewDescriptor{Template: ts.URL + "/templates/card.html", Integrity: good}),
			},
		}
		root, err := r.Resolve(context.Background(), vd, nil, nil)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(root.Slots["main"]) != 1 {
			t.Error("verified slot should render")
		}
	})

	t.Run("mismatch skips the slot", func(t *testing.T) {
		r := NewResolver(ts.Client(), []string{ts.URL + "/templates"})
		vd := ViewDescriptor{
			Template: ts.URL + "/templates/layout.html",
			Slots: Slots{
				"main": Single(ViewDescriptor{Template: ts.URL + "/templates/card.html", Integrity: bad}),
			},
		}
		tr := &Trace{}
		root, err := r.Resolve(context.Background(), vd, nil, tr)
		if err != nil {
			t.Fatalf("an integrity failure in a slot must not fail the render: %v", err)
		}
		if _, ok := root.Slots["main"]; ok {
			t.Error("slot with mismatched integrity was rendered")
		}
		if !hasEvent(tr, "integrity-failed") {
			t.Error("integrity failure should be traced")
		}
	})

	t.Run("mismatch at the root fails the render", func(t *testing.T) {
		r := NewResolver(ts.Client(), []string{ts.URL + "/templates"})
		vd := ViewDescriptor{Template: ts.URL + "/templates/layout.html", Integrity: bad}
		if _, err := r.Resolve(context.Background(), vd, nil, nil); err == nil {
			t.Fatal("root integrity mismatch should return an error")
		}
	})
}

// W3C SRI semantics: multiple tokens, strongest algorithm wins, unknown
// algorithms and unparseable metadata are ignored.
func TestVerifyIntegrity(t *testing.T) {
	body := []byte("card")
	sha256Good := sri256Digest("card")
	tests := []struct {
		name     string
		metadata string
		ok       bool
	}{
		{"sha384 match", sriDigest("card"), true},
		{"sha384 mismatch", sriDigest("other"), false},
		{"sha256 match", sha256Good, true},
		{"any of several digests of one algorithm", sriDigest("other") + " " + sriDigest("card"), true},
		// The strongest algorithm present is the one that must match.
		{"strongest wins", sha256Good + " " + sriDigest("other"), false},
		{"unknown algorithm ignored", "sha1-AAAA " + sriDigest("card"), true},
		{"only unknown algorithms means no verification", "md5-AAAA whatever", true},
		{"empty means no verification", "", true},
		// SRI options after ? are ignored.
		{"options ignored", sriDigest("card") + "?foo=bar", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyIntegrity(tc.metadata, body)
			if (err == nil) != tc.ok {
				t.Errorf("verifyIntegrity(%q) = %v, want ok=%v", tc.metadata, err, tc.ok)
			}
		})
	}
}

// §3.7: a slot may hold a descriptor reference. The referenced resource is
// fetched and used as the slot's descriptor, and relative template URLs inside
// it resolve against its own URL — not the referrer's base.
func TestResolveDescriptorReference(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/templates/layout.html", serveText("layout"))
	mux.HandleFunc("/templates/nav.html", serveText("nav"))
	mux.HandleFunc("/views/nav.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", MediaType)
		fmt.Fprint(w, `{"template":"/templates/nav.html"}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	r := NewResolver(ts.Client(), []string{ts.URL + "/"})
	base := mustParse(t, ts.URL+"/views/dashboard.json")
	vd := ViewDescriptor{
		Template: "/templates/layout.html",
		Slots:    Slots{"sidebarNav": Reference("nav.json")},
	}
	tr := &Trace{}
	root, err := r.Resolve(context.Background(), vd, base, tr)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	navNodes := root.Slots["sidebarNav"]
	if len(navNodes) != 1 || navNodes[0].Body != "nav" {
		t.Fatalf("referenced slot = %+v", navNodes)
	}
	if !hasEvent(tr, "descriptor-ref") {
		t.Error("reference fetch should be traced")
	}
}

// §3.7: a reference chain that revisits a descriptor URL is a cycle; the slot
// is abandoned (§9.1) and the rest of the tree still renders.
func TestResolveReferenceCycle(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/templates/layout.html", serveText("layout"))
	mux.HandleFunc("/templates/a.html", serveText("a"))
	mux.HandleFunc("/views/a.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", MediaType)
		fmt.Fprint(w, `{"template":"/templates/a.html","slots":{"next":{"descriptor":"b.json"}}}`)
	})
	mux.HandleFunc("/views/b.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", MediaType)
		fmt.Fprint(w, `{"template":"/templates/a.html","slots":{"next":{"descriptor":"a.json"}}}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	r := NewResolver(ts.Client(), []string{ts.URL + "/"})
	base := mustParse(t, ts.URL+"/views/dashboard.json")
	vd := ViewDescriptor{
		Template: "/templates/layout.html",
		Slots:    Slots{"nav": Reference("a.json")},
	}
	tr := &Trace{}
	root, err := r.Resolve(context.Background(), vd, base, tr)
	if err != nil {
		t.Fatalf("a cyclic reference must not fail the whole render: %v", err)
	}
	// a.json itself resolves; the cycle is detected when b.json points back.
	if len(root.Slots["nav"]) != 1 {
		t.Fatal("first hop of the chain should still render")
	}
	if !hasEvent(tr, "ref-cycle") {
		t.Error("cycle should be traced")
	}
}

// §3.7: the referenced resource must be a single ViewDescriptor; a multi-view
// document in slot context is invalid (§9.3) and fails only that slot.
func TestResolveReferenceMultiViewRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/templates/layout.html", serveText("layout"))
	mux.HandleFunc("/views/multi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", MediaType)
		fmt.Fprint(w, `{"views":{"default":{"template":"/templates/layout.html"}}}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	r := NewResolver(ts.Client(), []string{ts.URL + "/"})
	base := mustParse(t, ts.URL+"/views/dashboard.json")
	vd := ViewDescriptor{
		Template: "/templates/layout.html",
		Slots:    Slots{"nav": Reference("multi.json")},
	}
	root, err := r.Resolve(context.Background(), vd, base, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, ok := root.Slots["nav"]; ok {
		t.Error("multi-view reference should fail its slot")
	}
}

// §3.7/§8: references count toward the recursion depth limit.
func TestResolveReferenceCountsTowardDepth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/templates/a.html", serveText("a"))
	// Each ref-N.json nests one template and one further reference.
	for i := range 6 {
		mux.HandleFunc(fmt.Sprintf("/views/ref-%d.json", i), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", MediaType)
			fmt.Fprintf(w, `{"template":"/templates/a.html","slots":{"next":{"descriptor":"ref-%d.json"}}}`, i+1)
		})
	}
	mux.HandleFunc("/views/ref-6.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", MediaType)
		fmt.Fprint(w, `{"template":"/templates/a.html"}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	r := NewResolver(ts.Client(), []string{ts.URL + "/"})
	r.MaxDepth = 4
	base := mustParse(t, ts.URL+"/views/dashboard.json")
	vd := ViewDescriptor{
		Template: "/templates/a.html",
		Slots:    Slots{"next": Reference("ref-0.json")},
	}
	tr := &Trace{}
	root, err := r.Resolve(context.Background(), vd, base, tr)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if root == nil {
		t.Fatal("root should resolve")
	}
	if !hasEvent(tr, "depth-exceeded") {
		t.Error("a reference chain longer than MaxDepth should hit the depth limit")
	}
}

// §10: descriptor reference URLs are validated with the same allowlist chain as
// template URLs — an untrusted reference is never fetched.
func TestResolveReferenceUntrusted(t *testing.T) {
	evil := newTemplateServer(t, map[string]string{"/views/nav.json": `{"template":"nav.html"}`})
	ts := newTemplateServer(t, map[string]string{"/templates/layout.html": "layout"})

	r := NewResolver(ts.Client(), []string{ts.URL + "/"})
	base := mustParse(t, ts.URL+"/views/dashboard.json")
	vd := ViewDescriptor{
		Template: "/templates/layout.html",
		Slots:    Slots{"nav": Reference(evil.URL + "/views/nav.json")},
	}
	root, err := r.Resolve(context.Background(), vd, base, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, ok := root.Slots["nav"]; ok {
		t.Error("untrusted reference was resolved")
	}
	if n := evil.hitCount("/views/nav.json"); n != 0 {
		t.Errorf("untrusted reference was fetched %d times, want 0", n)
	}
}

// §5.5: when the resolver has a platform, descriptor fetches carry the
// VDP-Platform header so the server can negotiate platform-specific views.
func TestFetchDescriptorSendsPlatform(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(HeaderPlatform)
		w.Header().Set("Content-Type", MediaType)
		fmt.Fprint(w, `{"template":"/templates/layout.html"}`)
	}))
	t.Cleanup(ts.Close)

	r := NewResolver(ts.Client(), nil)
	r.Platform = "web"
	e := &Extraction{DescriptorURL: ts.URL + "/views/dashboard.json"}
	if err := r.FetchDescriptor(context.Background(), e); err != nil {
		t.Fatalf("FetchDescriptor: %v", err)
	}
	if got != "web" {
		t.Errorf("VDP-Platform = %q, want %q", got, "web")
	}
}

// serveText returns a handler serving a fixed template body.
func serveText(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, body)
	}
}

// sriDigest computes sha384 SRI metadata for a template body, as a server
// publishing integrity metadata would (§3.6).
func sriDigest(body string) string {
	sum := sha512.Sum384([]byte(body))
	return "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
}

func sri256Digest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
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
		fmt.Fprint(w, `{"template":"/templates/layout.html","slots":{"main":{"template":"/templates/main.html"}}}`)
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
	if vd.Template != "/templates/layout.html" {
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

// §5.4 form (c): a scheme-less identifier not beginning with "/" is opaque —
// never resolved against the base, kept verbatim as the node's identity; the
// client supplies a scheme only to fetch it (§6.3), http being permitted here
// because the test server is loopback (§10).
func TestResolveOpaqueIdentifier(t *testing.T) {
	ts := newTemplateServer(t, map[string]string{"/templates/card.html": "card"})
	host := mustParse(t, ts.URL).Host
	id := host + "/templates/card.html"

	r := NewResolver(ts.Client(), []string{host + "/templates/"})
	base := mustParse(t, ts.URL+"/views/dashboard.json")
	root, err := r.Resolve(context.Background(), ViewDescriptor{Template: id}, base, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if root.ID != id {
		t.Errorf("identity = %q, want the opaque id verbatim %q", root.ID, id)
	}
	if root.Body != "card" {
		t.Errorf("body = %q, want %q", root.Body, "card")
	}
	if n := ts.hitCount("/views/" + id); n != 0 {
		t.Errorf("opaque id was resolved against the base and fetched %d times, want 0", n)
	}
}

// §5.4: a scheme-less value with dot-segments fits no resolvable form — it is
// an opaque identifier like any other non-/-prefixed value, so it can never
// match a URL allowlist and must be rejected, not silently resolved against
// the base as the pre-2026-07-24 spec allowed.
func TestDotRelativeTreatedAsOpaque(t *testing.T) {
	ts := newTemplateServer(t, map[string]string{"/templates/card.html": "card"})
	r := NewResolver(ts.Client(), []string{ts.URL + "/templates/"})
	base := mustParse(t, ts.URL+"/views/dashboard.json")

	_, err := r.Resolve(context.Background(), ViewDescriptor{Template: "../templates/card.html"}, base, nil)
	if !errors.Is(err, ErrUntrustedTemplate) {
		t.Errorf("err = %v, want ErrUntrustedTemplate", err)
	}
	if n := ts.hitCount("/templates/card.html"); n != 0 {
		t.Errorf("dot-relative value was resolved against the base and fetched %d times, want 0", n)
	}
}

// §13.2: scheme-less allowlist entries (first-class since the schemas moved to
// format: uri-reference) match opaque identifiers by verbatim prefix on
// segment boundaries, with the host part comparing case-insensitively.
// Matching never crosses §5.4 forms: an absolute URI does not match a
// scheme-less entry, nor an opaque id an absolute entry — deployments list
// each form they actually serve.
func TestCheckTrustedSchemelessEntries(t *testing.T) {
	r := NewResolver(nil, []string{"cdn.example.com/templates/"})
	tests := []struct {
		id    string
		allow bool
	}{
		{"cdn.example.com/templates/card", true},
		{"cdn.example.com/templates", true},
		{"CDN.Example.COM/templates/card", true},
		{"cdn.example.com/templates-evil/card", false},
		{"cdn.example.com/other/card", false},
		{"evil.example.com/templates/card", false},
		{"https://cdn.example.com/templates/card", false},
	}
	for _, tc := range tests {
		err := r.checkTrusted(tc.id, nil)
		if (err == nil) != tc.allow {
			t.Errorf("checkTrusted(%s): allowed = %v, want %v", tc.id, err == nil, tc.allow)
		}
	}

	abs := NewResolver(nil, []string{"https://cdn.example.com/templates/"})
	if err := abs.checkTrusted("cdn.example.com/templates/card", nil); err == nil {
		t.Error("opaque id matched an absolute allowlist entry")
	}
}

// §10: under the same-origin default, an opaque identifier — which has a host
// but no scheme — is trusted iff its host part equals the base URL's host.
func TestOpaqueSameOriginDefault(t *testing.T) {
	r := NewResolver(nil, nil)
	base := mustParse(t, "https://e.com/views/dashboard.json")
	tests := []struct {
		id    string
		base  *url.URL
		allow bool
	}{
		{"e.com/templates/card", base, true},
		{"E.COM/templates/card", base, true},
		{"cdn.example.com/templates/card", base, false},
		{"e.com/templates/card", nil, false},
	}
	for _, tc := range tests {
		err := r.checkTrusted(tc.id, tc.base)
		if (err == nil) != tc.allow {
			t.Errorf("checkTrusted(%s, base=%v): allowed = %v, want %v", tc.id, tc.base, err == nil, tc.allow)
		}
	}
}

// §5.2/§6.3: an opaque identifier caches under the identifier as written, so
// resolving it twice fetches once.
func TestResolveCachesOpaqueByIdentity(t *testing.T) {
	ts := newTemplateServer(t, map[string]string{"/templates/card.html": "card"})
	host := mustParse(t, ts.URL).Host
	id := host + "/templates/card.html"
	r := NewResolver(ts.Client(), []string{host + "/templates/"})

	for range 2 {
		if _, err := r.Resolve(context.Background(), ViewDescriptor{Template: id}, nil, nil); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if n := ts.hitCount("/templates/card.html"); n != 1 {
		t.Errorf("template fetched %d times, want 1", n)
	}
}

// §10: network retrieval must use HTTPS; plain HTTP passes only for loopback
// hosts (the local-development exception).
func TestRequireHTTPS(t *testing.T) {
	tests := []struct {
		url string
		ok  bool
	}{
		{"https://example.com/templates/card", true},
		{"http://example.com/templates/card", false},
		{"http://127.0.0.1:8080/templates/card", true},
		{"http://localhost:8080/templates/card", true},
		{"http://[::1]:8080/templates/card", true},
		{"http://192.168.1.5/templates/card", false},
	}
	for _, tc := range tests {
		err := requireHTTPS(mustParse(t, tc.url))
		if (err == nil) != tc.ok {
			t.Errorf("requireHTTPS(%s) = %v, want ok=%v", tc.url, err, tc.ok)
		}
	}
}

// §10 end to end: an http:// template URI on a non-loopback host is rejected
// before any request is made, even when the allowlist trusts it.
func TestResolveRejectsPlainHTTP(t *testing.T) {
	r := NewResolver(http.DefaultClient, []string{"http://example.invalid/templates/"})
	_, err := r.Resolve(context.Background(), ViewDescriptor{Template: "http://example.invalid/templates/card"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Errorf("err = %v, want an HTTPS requirement failure", err)
	}
}
