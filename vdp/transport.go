package vdp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Transport identifies how a view descriptor reached the client (§4).
type Transport string

const (
	// TransportInlineView is the "_view" body key (§4.2).
	TransportInlineView Transport = "_view (inline body)"
	// TransportInlineViews is the "_views" body key (§4.2).
	TransportInlineViews Transport = "_views (inline body)"
	// TransportLinkHeader is a Link header with rel="view-descriptor" (§4.1).
	TransportLinkHeader Transport = "Link header"
	// TransportViewTemplate is the View-Template shorthand header (§4.1).
	TransportViewTemplate Transport = "View-Template header"
)

// LinkRel is the link relation type VDP registers for the Link header (§12.1).
const LinkRel = "view-descriptor"

// HeaderViewTemplate is the shorthand header for the single-template case (§4.1).
const HeaderViewTemplate = "View-Template"

// Headers advertising VDP support (§13.1). VDP-Version alone suffices to
// signal support, and may appear on any response carrying a view descriptor
// and on view descriptor resources themselves — not only on OPTIONS.
const (
	HeaderSupport = "VDP-Support"
	HeaderVersion = "VDP-Version"
)

// HeaderPlatform carries the client's rendering platform so servers can
// negotiate platform-specific view descriptors (§5.5). The name has no X-
// prefix; that convention is deprecated by RFC 6648.
const HeaderPlatform = "VDP-Platform"

// Envelope is the inline body transport (§4.2). It follows HAL's underscore
// convention for protocol metadata and coexists with _links and _embedded.
type Envelope struct {
	View  *ViewDescriptor           `json:"_view,omitempty"`
	Views map[string]ViewDescriptor `json:"_views,omitempty"`
}

// Extraction is the outcome of pulling a view descriptor out of a response.
//
// Either Views is populated (the descriptor travelled with the response) or
// DescriptorURL is set (the descriptor is a standalone resource that must be
// fetched — call FetchDescriptor).
type Extraction struct {
	// Transport records which mechanism supplied the descriptor (§4).
	Transport Transport
	// Views holds the descriptor(s). Single-view transports use DefaultView.
	Views map[string]ViewDescriptor
	// DescriptorURL is the standalone view descriptor resource to fetch (§4.1),
	// empty once Views is populated.
	DescriptorURL string
	// Base is the base URL for resolving relative template URLs (§5.4).
	Base *url.URL
}

// View selects a named view, falling back to DefaultView when name is empty or
// absent (§3.4).
func (e *Extraction) View(name string) (ViewDescriptor, error) {
	if name != "" {
		if vd, ok := e.Views[name]; ok {
			return vd, nil
		}
	}
	if vd, ok := e.Views[DefaultView]; ok {
		return vd, nil
	}
	if name != "" {
		return ViewDescriptor{}, fmt.Errorf("no view %q and no %q view", name, DefaultView)
	}
	return ViewDescriptor{}, fmt.Errorf("no %q view", DefaultView)
}

// Extract pulls the view descriptor out of an HTTP response, applying the §4.4
// precedence order:
//
//  1. inline body (_view / _views) — most specific
//  2. Link header with rel="view-descriptor"
//  3. View-Template header
//
// body is the already-read response body; it may be nil for non-JSON responses.
// A nil Extraction with a nil error means the response carries no view
// descriptor at all.
func Extract(resp *http.Response, body []byte) (*Extraction, error) {
	base := responseURL(resp)

	// 1. Inline body (§4.2). Only attempt this for JSON-ish bodies; a body that
	// isn't JSON simply carries no inline descriptor, which is not an error.
	if len(body) > 0 && isJSON(resp.Header.Get("Content-Type")) {
		var env Envelope
		if err := json.Unmarshal(body, &env); err == nil {
			switch {
			case env.View != nil && env.Views != nil:
				return nil, fmt.Errorf("response has both _view and _views")
			case env.Views != nil:
				mvd := MultiViewDescriptor{Views: env.Views}
				if err := mvd.Validate(); err != nil {
					return nil, fmt.Errorf("_views: %w", err)
				}
				return &Extraction{
					Transport: TransportInlineViews,
					Views:     env.Views,
					Base:      base,
				}, nil
			case env.View != nil:
				if err := env.View.Validate(); err != nil {
					return nil, fmt.Errorf("_view: %w", err)
				}
				return &Extraction{
					Transport: TransportInlineView,
					Views:     map[string]ViewDescriptor{DefaultView: *env.View},
					Base:      base,
				}, nil
			}
		}
	}

	// 2. Link header (§4.1). The descriptor is a standalone resource; relative
	// template URLs inside it resolve against the descriptor's own URL (§5.4).
	for _, l := range parseLinks(resp.Header.Values("Link")) {
		if !l.hasRel(LinkRel) {
			continue
		}
		ref, err := url.Parse(l.URL)
		if err != nil {
			return nil, fmt.Errorf("Link header: bad URL %q: %w", l.URL, err)
		}
		if base != nil {
			ref = base.ResolveReference(ref)
		}
		return &Extraction{
			Transport:     TransportLinkHeader,
			DescriptorURL: ref.String(),
			Base:          ref,
		}, nil
	}

	// 3. View-Template shorthand (§4.1): equivalent to {"template": "<URL>"}.
	if tmpl := strings.TrimSpace(resp.Header.Get(HeaderViewTemplate)); tmpl != "" {
		vd := ViewDescriptor{Template: tmpl}
		if err := vd.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", HeaderViewTemplate, err)
		}
		return &Extraction{
			Transport: TransportViewTemplate,
			Views:     map[string]ViewDescriptor{DefaultView: vd},
			Base:      base,
		}, nil
	}

	return nil, nil
}

func responseURL(resp *http.Response) *url.URL {
	if resp.Request == nil {
		return nil
	}
	return resp.Request.URL
}

func isJSON(contentType string) bool {
	mt, _, _ := strings.Cut(contentType, ";")
	mt = strings.TrimSpace(strings.ToLower(mt))
	// Covers application/json, application/hal+json, application/vdp+json and
	// OData's application/json;odata.metadata=minimal (§4.3).
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// link is one parsed entry of an RFC 8288 Link header.
type link struct {
	URL    string
	Params map[string]string
}

// hasRel reports whether the link carries the given relation type. Per RFC 8288
// the rel parameter is a space-separated list of relation types.
func (l link) hasRel(rel string) bool {
	for _, r := range strings.Fields(l.Params["rel"]) {
		if strings.EqualFold(r, rel) {
			return true
		}
	}
	return false
}

// parseLinks parses RFC 8288 Link header values of the form
//
//	<https://example.com/views/dashboard.json>; rel="view-descriptor"
func parseLinks(values []string) []link {
	var links []link
	for _, value := range values {
		for _, field := range splitLinkFields(value) {
			l, ok := parseLink(field)
			if ok {
				links = append(links, l)
			}
		}
	}
	return links
}

// splitLinkFields splits a Link header on commas that separate entries, leaving
// commas inside <...> URI references and "..." quoted strings alone.
func splitLinkFields(value string) []string {
	var (
		fields   []string
		start    int
		inURI    bool
		inQuotes bool
	)
	for i, r := range value {
		switch {
		case inQuotes:
			if r == '"' {
				inQuotes = false
			}
		case inURI:
			if r == '>' {
				inURI = false
			}
		case r == '"':
			inQuotes = true
		case r == '<':
			inURI = true
		case r == ',':
			fields = append(fields, value[start:i])
			start = i + 1
		}
	}
	return append(fields, value[start:])
}

// parseLink parses a single "<uri>; key=value; key="value"" entry.
func parseLink(field string) (link, bool) {
	field = strings.TrimSpace(field)
	if !strings.HasPrefix(field, "<") {
		return link{}, false
	}
	end := strings.Index(field, ">")
	if end < 0 {
		return link{}, false
	}
	l := link{URL: strings.TrimSpace(field[1:end]), Params: map[string]string{}}
	for _, param := range splitParams(field[end+1:]) {
		key, val, ok := strings.Cut(param, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		if len(val) >= 2 && strings.HasPrefix(val, `"`) && strings.HasSuffix(val, `"`) {
			val = val[1 : len(val)-1]
		}
		if _, seen := l.Params[key]; !seen { // RFC 8288: first occurrence wins.
			l.Params[key] = val
		}
	}
	return l, true
}

// splitParams splits ";"-separated parameters, ignoring semicolons inside
// quoted strings.
func splitParams(s string) []string {
	var (
		params   []string
		start    int
		inQuotes bool
	)
	for i, r := range s {
		switch {
		case inQuotes:
			if r == '"' {
				inQuotes = false
			}
		case r == '"':
			inQuotes = true
		case r == ';':
			params = append(params, s[start:i])
			start = i + 1
		}
	}
	params = append(params, s[start:])
	out := params[:0]
	for _, p := range params {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// FormatLinkHeader builds a Link header value pointing at a standalone view
// descriptor resource (§4.1).
func FormatLinkHeader(descriptorURL string) string {
	return fmt.Sprintf("<%s>; rel=%q", descriptorURL, LinkRel)
}
