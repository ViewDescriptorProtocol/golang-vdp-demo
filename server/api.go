package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ViewDescriptorProtocol/golang-vdp-demo/vdp"
)

// API is the VDP-speaking data server. Each endpoint returns ordinary data and
// attaches a view descriptor by one of the §4 transports.
//
// The endpoints are chosen so that between them they exercise every transport
// the spec defines, rather than because the data is interesting.
type API struct{}

// Routes registers the API, the view descriptor resources and discovery.
func (a *API) Routes(mux *http.ServeMux) {
	// Data endpoints, one per transport (§4).
	mux.HandleFunc("GET /api/login", a.login)                  // View-Template header
	mux.HandleFunc("GET /api/dashboard", a.dashboard)          // Link header
	mux.HandleFunc("GET /api/products/42", a.product)          // inline _views
	mux.HandleFunc("GET /api/feed", a.feed)                    // inline _view, slot array
	mux.HandleFunc("GET /api/odata/products", a.odata)         // OData4 annotation
	mux.HandleFunc("GET /api/dashboard/stats", a.statsPartial) // partial update (§14)

	// Standalone view descriptor resources (§5).
	mux.HandleFunc("GET /views/dashboard.json", a.dashboardView)
	mux.HandleFunc("GET /views/product-list.json", a.productListView)

	// Discovery (§13).
	mux.HandleFunc("GET /.well-known/vdp", a.discovery)
	mux.HandleFunc("OPTIONS /api/", a.options)
}

// TrustedTemplateDomains is the allowlist a client should enforce (§10). The
// demo serves templates from its own origin, so the allowlist is scoped to the
// /templates path of the running instance.
//
// The spec requires HTTPS template URLs in production (§10). This demo runs on
// plain http://localhost, which is the one place that carries no eavesdropping
// risk; a real deployment must not relax this.
func TrustedTemplateDomains(base string) []string {
	return []string{base + "/templates"}
}

func templateURL(base, path string) string {
	return base + "/templates/" + path
}

// login demonstrates §7.1 and the View-Template shorthand (§4.1): one template,
// no composition, so a full descriptor would be overkill.
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	base := baseURL(r)
	w.Header().Set(vdp.HeaderViewTemplate, templateURL(base, "components/forms/form.html"))
	writeJSON(w, http.StatusOK, "application/json", loginData())
}

// dashboard demonstrates §7.2: the descriptor is a standalone, independently
// cacheable resource referenced by a Link header (§4.1). The data payload stays
// completely clean — it would look identical to a client that ignores VDP.
func (a *API) dashboard(w http.ResponseWriter, r *http.Request) {
	viewURL := "/views/dashboard.json"
	if q := r.URL.RawQuery; q != "" {
		viewURL += "?" + q // Propagate the ?fail / ?untrusted demo switches.
	}
	w.Header().Set("Link", vdp.FormatLinkHeader(baseURL(r)+viewURL))
	writeJSON(w, http.StatusOK, "application/hal+json", dashboardData("/api/dashboard"))
}

// dashboardView serves the standalone view descriptor resource (§5).
//
// Its template URLs are relative references, resolved against this resource's
// own URL (§5.4 rule 1) — "../templates/layouts/sidebar.html" becomes
// "<base>/templates/layouts/sidebar.html".
func (a *API) dashboardView(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	layout := "../templates/layouts/sidebar.html"
	chart := "../templates/components/charts/chart.html"
	nav := "../templates/components/navigation/nav.html"

	switch q.Get("fail") {
	case "chart":
		// §9.1: a slot template that cannot be fetched. The client must skip the
		// slot and render the rest of the tree.
		chart = "../templates/components/charts/does-not-exist.html"
	case "root":
		// §9.4 rule 2: the root template fails, so nothing can be composed and
		// the client falls back to the raw data.
		layout = "../templates/layouts/does-not-exist.html"
	}
	if q.Has("untrusted") {
		// §10: outside the trusted allowlist. The client must refuse to fetch it.
		nav = "https://evil.example.com/templates/nav.html"
	}

	view := vdp.ViewDescriptor{
		Template: layout,
		Slots: vdp.Slots{
			"sidebarNav": vdp.Single(vdp.ViewDescriptor{Template: nav}),
			"mainContent": vdp.Single(vdp.ViewDescriptor{
				Template: "../templates/demos/dashboard.html",
				Slots: vdp.Slots{
					"statsCards":    vdp.Single(vdp.ViewDescriptor{Template: "../templates/components/data-display/card.html"}),
					"activityTable": vdp.Single(vdp.ViewDescriptor{Template: "../templates/components/data-display/table.html"}),
					"revenueChart": vdp.Single(vdp.ViewDescriptor{
						Template: chart,
						// §3.3: a slot inside a slot inside a slot.
						Slots: vdp.Slots{
							"legend": vdp.Single(vdp.ViewDescriptor{Template: "../templates/components/charts/chart-legend.html"}),
						},
					}),
				},
			}),
		},
	}

	// §5.2: view descriptor resources are independently cacheable.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("ETag", `"v1-dashboard"`)
	writeJSON(w, http.StatusOK, vdp.MediaType, view)
}

// product demonstrates §7.4: one response, several views, carried inline as
// _views (§4.2). Which view is used is the client's choice — try /product/42
// and /product/42?view=compact.
func (a *API) product(w http.ResponseWriter, r *http.Request) {
	base := baseURL(r)
	data := productData()
	data["_views"] = map[string]vdp.ViewDescriptor{
		vdp.DefaultView: {
			Template: templateURL(base, "demos/product-detail.html"),
			Slots: vdp.Slots{
				"gallery": vdp.Single(vdp.ViewDescriptor{Template: templateURL(base, "components/image-carousel.html")}),
				"reviews": vdp.Single(vdp.ViewDescriptor{Template: templateURL(base, "components/review-list.html")}),
			},
		},
		"compact": {
			Template: templateURL(base, "components/data-display/product-card.html"),
		},
	}
	writeJSON(w, http.StatusOK, "application/hal+json", data)
}

// feed demonstrates §3.5: one slot filled by an ordered array of descriptors,
// which the client must render in sequence.
func (a *API) feed(w http.ResponseWriter, r *http.Request) {
	base := baseURL(r)
	data := feedData()
	data["_view"] = vdp.ViewDescriptor{
		Template: templateURL(base, "layouts/sidebar.html"),
		Slots: vdp.Slots{
			"sidebarNav": vdp.Single(vdp.ViewDescriptor{Template: templateURL(base, "components/navigation/nav.html")}),
			"mainContent": vdp.Sequence(
				vdp.ViewDescriptor{Template: templateURL(base, "components/data-display/card.html")},
				vdp.ViewDescriptor{Template: templateURL(base, "components/charts/chart.html")},
				vdp.ViewDescriptor{Template: templateURL(base, "components/data-display/table.html")},
			),
		},
	}
	writeJSON(w, http.StatusOK, "application/hal+json", data)
}

// odata demonstrates §4.3 and §7.3: an OData4 body is too rigid to carry _view,
// so the descriptor travels as an instance annotation, alongside a Link header
// — the mechanism that needs no cooperation from the body at all.
func (a *API) odata(w http.ResponseWriter, r *http.Request) {
	base := baseURL(r)
	viewURL := base + "/views/product-list.json"
	data := odataProducts(base)
	data["@View.descriptor"] = viewURL
	w.Header().Set("Link", vdp.FormatLinkHeader(viewURL))
	writeJSON(w, http.StatusOK, "application/json;odata.metadata=minimal", data)
}

// productListView is the descriptor for the OData product list (§7.3): a single
// template binding to OData's "value" array, no composition needed.
func (a *API) productListView(w http.ResponseWriter, r *http.Request) {
	view := vdp.ViewDescriptor{
		Template: "../templates/components/data-display/odata-table.html",
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, vdp.MediaType, view)
}

// statsPartial demonstrates §14: a partial update is not a special kind of
// response. It is just a smaller response carrying its own view descriptor.
func (a *API) statsPartial(w http.ResponseWriter, r *http.Request) {
	base := baseURL(r)
	w.Header().Set(vdp.HeaderViewTemplate, templateURL(base, "components/data-display/card.html"))
	writeJSON(w, http.StatusOK, "application/json", map[string]any{
		"stats": map[string]any{"revenue": 52400, "users": 1923, "orders": 347},
	})
}

// discovery serves the §13.2 well-known document, letting clients prefetch
// descriptors and learn the trusted template allowlist before making any data
// request.
func (a *API) discovery(w http.ResponseWriter, r *http.Request) {
	base := baseURL(r)
	doc := vdp.DiscoveryDocument{
		Version: vdp.Version,
		Endpoints: map[string]vdp.ViewDescriptor{
			"/api/dashboard": {Template: base + "/views/dashboard.json"},
			"/api/login":     {Template: templateURL(base, "components/forms/form.html")},
		},
		TrustedTemplateDomains: TrustedTemplateDomains(base),
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, vdp.MediaType, doc)
}

// options advertises VDP support (§13.1).
func (a *API) options(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "GET, HEAD, OPTIONS")
	w.Header().Set(vdp.HeaderSupport, "true")
	w.Header().Set(vdp.HeaderVersion, vdp.Version)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, contentType string, v any) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		http.Error(w, "encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	// The demo is read across origins by nothing, but templates and descriptors
	// fetched cross-origin need CORS (§10).
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	w.Write(append(body, '\n'))
}

// baseURL reconstructs this server's public origin so descriptors can carry
// absolute template URLs regardless of the port the demo runs on.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.TrimSpace(strings.Split(proto, ",")[0])
	}
	return scheme + "://" + r.Host
}
