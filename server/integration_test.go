package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// newDemoServer wires the three roles onto one mux and serves them over real
// HTTP, exactly as main.go does. The BFF then talks to the API and template
// server over the wire, so these tests exercise the full §8 client algorithm
// end to end rather than any single function.
//
// The template and static trees live at the module root; go test runs with the
// package directory as its working directory, so ".." reaches them.
func newDemoServer(t *testing.T) *httptest.Server {
	t.Helper()
	root := os.DirFS("..")

	mux := http.NewServeMux()
	(&API{Templates: root}).Routes(mux)
	(&Templates{FS: root}).Routes(mux)
	(&BFF{Client: http.DefaultClient, Static: root}).Routes(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// get fetches a BFF page and returns the status and body.
func get(t *testing.T, srv *httptest.Server, path string) (int, string) {
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
	return resp.StatusCode, string(body)
}

// mustContain fails unless every fragment is present; mustNotContain is the
// inverse. Kept as helpers so each case reads as a list of expectations.
func mustContain(t *testing.T, path, body string, fragments ...string) {
	t.Helper()
	for _, f := range fragments {
		if !strings.Contains(body, f) {
			t.Errorf("%s: expected body to contain %q", path, f)
		}
	}
}

func mustNotContain(t *testing.T, path, body string, fragments ...string) {
	t.Helper()
	for _, f := range fragments {
		if strings.Contains(body, f) {
			t.Errorf("%s: body should not contain %q", path, f)
		}
	}
}

// The index lists every demo so a user has somewhere to start.
func TestIntegrationIndex(t *testing.T) {
	srv := newDemoServer(t)
	status, body := get(t, srv, "/")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	mustContain(t, "/", body,
		`href="/dashboard"`, `href="/login"`, `href="/product/42"`, `href="/feed"`, `href="/odata"`)
}

// §4.1: the View-Template header transport. One template, no slots — the form
// renders straight from the data.
func TestIntegrationLoginViewTemplateHeader(t *testing.T) {
	srv := newDemoServer(t)
	status, body := get(t, srv, "/login")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	mustContain(t, "/login", body, "Sign in")
	mustNotContain(t, "/login", body, "Fallback: raw API data", "Resolution failed")
}

// §4.1/§5: the Link-header transport pointing at a standalone descriptor, then a
// full multi-level slot tree composed and rendered with the API data.
func TestIntegrationDashboardComposesTree(t *testing.T) {
	srv := newDemoServer(t)
	status, body := get(t, srv, "/dashboard")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	mustContain(t, "/dashboard", body,
		`class="tree"`,  // the trace shows a resolved tree
		"Daily revenue", // chart slot rendered its data
		"alice",         // activity table rendered its data
	)
	mustNotContain(t, "/dashboard", body, "Fallback: raw API data", "Resolution failed")
}

// §10: a template outside the trusted allowlist is refused. The offending slot
// (nav) is dropped and the refusal is recorded as an error in the trace, but the
// rest of the tree still composes — no fallback.
func TestIntegrationUntrustedTemplateSkipped(t *testing.T) {
	srv := newDemoServer(t)
	status, body := get(t, srv, "/dashboard?untrusted")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	mustContain(t, "/dashboard?untrusted", body,
		"Daily revenue",     // a trusted sibling slot still rendered
		`class="event-err"`, // the refusal is surfaced as an error event
		"evil.example.com",  // ...naming the untrusted URL that was denied
	)
	mustNotContain(t, "/dashboard?untrusted", body, "Fallback: raw API data")
}

// §9.1: a slot template that 404s is skipped and the rest of the tree survives.
// The chart is gone but the page is not a fallback.
func TestIntegrationMissingSlotTemplateSkipped(t *testing.T) {
	srv := newDemoServer(t)
	status, body := get(t, srv, "/dashboard?fail=chart")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	mustContain(t, "/dashboard?fail=chart", body, `class="tree"`)
	mustNotContain(t, "/dashboard?fail=chart", body, "Fallback: raw API data", "Daily revenue")
}

// §9.4 rule 2, end to end: when the ROOT template cannot be fetched there is
// nothing to compose, so the BFF falls back to showing the raw API data. This
// drives the rawDataFallback / fallback.html path through the real handler.
func TestIntegrationRootFailureFallsBackToData(t *testing.T) {
	srv := newDemoServer(t)
	status, body := get(t, srv, "/dashboard?fail=root")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	mustContain(t, "/dashboard?fail=root", body,
		"Fallback: raw API data", // the fallback panel from fallback.html
		"Resolution failed",      // the trace records why
		"recentActivity",         // the raw JSON data is shown instead of a blank page
	)
}

// §3.7: the dashboard's nav slot is filled by descriptor reference. The
// referenced resource resolves and its template renders — the nav markup is in
// the page even though the dashboard descriptor never inlines it.
func TestIntegrationDescriptorReference(t *testing.T) {
	srv := newDemoServer(t)
	status, body := get(t, srv, "/dashboard")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	mustContain(t, "/dashboard", body,
		`class="nav"`,    // the nav template, reached only via the reference
		"descriptor-ref", // the trace records the reference fetch
	)
	mustNotContain(t, "/dashboard", body, "Fallback: raw API data")
}

// §3.6: a template whose bytes do not match the published integrity metadata
// is a fetch failure for that slot — the legend disappears, the page survives.
func TestIntegrationIntegrityMismatchSkipsSlot(t *testing.T) {
	srv := newDemoServer(t)
	status, body := get(t, srv, "/dashboard?fail=integrity")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	mustContain(t, "/dashboard?fail=integrity", body,
		"Daily revenue",     // the chart itself still renders
		"integrity",         // the trace names the failure
		`class="event-err"`, // ...as an error event
	)
	mustNotContain(t, "/dashboard?fail=integrity", body,
		"Fallback: raw API data",
		`class="legend"`, // the tampered legend slot is gone
	)
}

// §7.4/§3.4: one payload offers several views; the client asks for one. Both the
// default and the ?view=card selection must resolve and render.
func TestIntegrationProductViewSelection(t *testing.T) {
	srv := newDemoServer(t)
	for _, path := range []string{"/product/42", "/product/42?view=card"} {
		status, body := get(t, srv, path)
		if status != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, status)
		}
		mustContain(t, path, body, "Widget Pro")
		mustNotContain(t, path, body, "Fallback: raw API data")
	}
}

// §3.5: an inline _view whose slot holds an array of descriptors.
func TestIntegrationFeedSlotArray(t *testing.T) {
	srv := newDemoServer(t)
	status, body := get(t, srv, "/feed")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	mustContain(t, "/feed", body, `class="tree"`)
	mustNotContain(t, "/feed", body, "Fallback: raw API data")
}

// §4.3: a rigid OData body that cannot carry _view, so the descriptor travels by
// annotation and Link header. The product list still composes.
func TestIntegrationODataAnnotation(t *testing.T) {
	srv := newDemoServer(t)
	status, body := get(t, srv, "/odata")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	mustContain(t, "/odata", body, "Widget", "Gadget")
	mustNotContain(t, "/odata", body, "Fallback: raw API data")
}
