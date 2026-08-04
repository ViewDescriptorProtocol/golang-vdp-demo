package vdp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// DefaultMaxDepth is the recommended maximum template nesting depth (§8).
const DefaultMaxDepth = 10

// maxTemplateBytes caps how much of a template response is read, so a hostile
// or broken template server cannot exhaust memory (§10).
const maxTemplateBytes = 1 << 20

// Node is a resolved template tree node: an obtained template plus the
// resolved sub-trees filling its slots. It is the output of the §8 algorithm
// and the input to a renderer.
type Node struct {
	// ID is the template's §5.4 identity: the URI as written for absolute and
	// scheme-less opaque forms, or the resolved absolute URL for /-prefixed
	// relative references. It is the key the template was cached under (§6.3),
	// which for an opaque identifier is not the URL it was fetched from.
	ID string
	// Body is the template source.
	Body string
	// Slots maps each slot name to the nodes filling it, in render order (§3.5).
	// Slots whose templates could not be resolved are absent (§9.1).
	Slots map[string][]*Node
	// Transform is the node's §3.8 transform, nil when the node declares none
	// (the template then receives the representation unchanged).
	Transform *Transform
	// Mapper is the client-registered mapping function when Transform is a
	// $mapper reference (§3.8.3). Resolution fails the slot if the mapper URI
	// is not registered, so a resolved node's Mapper is always usable.
	Mapper func(any) any
}

// Trace records what happened during a resolution. It exists so the demo can
// show the protocol at work; resolution does not depend on it.
type Trace struct {
	mu     sync.Mutex
	Events []Event
}

// Event is one step of a resolution.
type Event struct {
	Depth int
	// Kind is one of "fetch", "cached", "slot-skipped", "depth-exceeded",
	// "rejected", "integrity-failed", "descriptor-ref", "ref-cycle",
	// "mapper-unknown".
	Kind string
	Slot string // slot being filled, empty for the root
	URL  string
	Err  string
}

func (t *Trace) add(e Event) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Events = append(t.Events, e)
}

// Resolver implements the client resolution algorithm (§8).
//
// The zero value is not usable; construct one with NewResolver.
type Resolver struct {
	// HTTP fetches descriptor and template resources.
	HTTP *http.Client
	// TrustedTemplateURLs is the allowlist of URL prefixes templates may be
	// loaded from (§10, §13.2). A template URL outside it is rejected. When
	// empty, the §10 source chain ends at its same-origin default: only URLs
	// sharing an origin with the descriptor's base URL are trusted.
	TrustedTemplateURLs []string
	// Platform, when set, is sent as the VDP-Platform header on descriptor
	// fetches so the server can negotiate platform-specific views (§5.5).
	Platform string
	// MaxDepth bounds template nesting (§8). Zero means DefaultMaxDepth.
	MaxDepth int
	// Mappers holds the client-registered mapping code $mapper transforms may
	// name (§3.8.3). Keys are mapper URIs, matched verbatim — a descriptor can
	// name a mapper but never supply one (§10). Support is OPTIONAL for
	// clients; a nil map simply means every $mapper is unknown (§9.1).
	Mappers map[string]func(any) any

	cache     sync.Map // absolute template URL -> string body (§5.2)
	descCache sync.Map // absolute descriptor URL -> ViewDescriptor (§3.7, §5.2)
}

// NewResolver returns a Resolver that trusts templates under the given URL
// prefixes and caches fetched resources in memory (§5.2).
func NewResolver(client *http.Client, trustedTemplateURLs []string) *Resolver {
	if client == nil {
		client = http.DefaultClient
	}
	return &Resolver{
		HTTP:                client,
		TrustedTemplateURLs: trustedTemplateURLs,
		MaxDepth:            DefaultMaxDepth,
	}
}

// ErrUntrustedTemplate is returned when a template URL falls outside the
// trusted URL allowlist (§10).
var ErrUntrustedTemplate = errors.New("template URL is not in the trusted URL allowlist")

// ErrUnknownMapper is returned when a $mapper transform names a URI the client
// has not registered (§3.8.3). On a slot node this is a slot failure (§9.1);
// at the root, the caller MUST NOT render the template against untransformed
// input — the shapes do not match — and renders an error template only
// (§9.4 rule 2, §9.6).
var ErrUnknownMapper = errors.New("no registered mapper for URI")

// RegisterMapper registers client-side mapping code under a mapper URI
// (§3.8.3). The URI is an identifier compared verbatim; it is never fetched.
func (r *Resolver) RegisterMapper(uri string, fn func(any) any) {
	if r.Mappers == nil {
		r.Mappers = map[string]func(any) any{}
	}
	r.Mappers[uri] = fn
}

// FetchDescriptor retrieves a standalone view descriptor resource (§4.1, §5).
// It is called when Extract yields a DescriptorURL rather than inline views.
func (r *Resolver) FetchDescriptor(ctx context.Context, e *Extraction) error {
	if e.DescriptorURL == "" {
		return nil // Already inline.
	}
	if u, err := url.Parse(e.DescriptorURL); err == nil { // §10
		if err := requireHTTPS(u); err != nil {
			return fmt.Errorf("fetch view descriptor %s: %w", e.DescriptorURL, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.DescriptorURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", MediaType+", application/json")
	if r.Platform != "" {
		req.Header.Set(HeaderPlatform, r.Platform) // §5.5
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("fetch view descriptor %s: %w", e.DescriptorURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch view descriptor %s: unexpected status %s", e.DescriptorURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTemplateBytes))
	if err != nil {
		return fmt.Errorf("read view descriptor %s: %w", e.DescriptorURL, err)
	}
	// §9.3: a malformed descriptor must be rejected, and Parse validates.
	views, err := Parse(body)
	if err != nil {
		return fmt.Errorf("view descriptor %s: %w", e.DescriptorURL, err)
	}
	e.Views = views
	e.DescriptorURL = ""
	return nil
}

// Resolve walks a view descriptor into a template tree (§8 steps 3-5).
//
// base is the base URL for relative template URLs (§5.4); nested slot templates
// resolve against that same base, not against their parent's URL.
//
// Error handling follows §9.4's progressive failure: a slot whose template
// cannot be fetched is skipped and the rest of the tree still renders, while a
// root template failure returns an error so the caller can fall back.
func (r *Resolver) Resolve(ctx context.Context, vd ViewDescriptor, base *url.URL, tr *Trace) (*Node, error) {
	if err := vd.Validate(); err != nil { // §9.3
		return nil, err
	}
	return r.resolve(ctx, vd, base, 0, "", tr, nil)
}

// slot is the name of the slot this descriptor fills, empty at the root. It is
// carried only so the trace can say which slot each fetch belongs to. seen
// holds the descriptor URLs of the reference chain leading here, for §3.7
// cycle detection.
func (r *Resolver) resolve(ctx context.Context, vd ViewDescriptor, base *url.URL, depth int, slot string, tr *Trace, seen map[string]bool) (*Node, error) {
	maxDepth := r.maxDepth()
	if depth >= maxDepth {
		tr.add(Event{Depth: depth, Kind: "depth-exceeded", Slot: slot, URL: vd.Template})
		return nil, fmt.Errorf("maximum template nesting depth (%d) exceeded at %q", maxDepth, vd.Template)
	}

	tid, err := resolveTemplateURI(vd.Template, base) // §5.4
	if err != nil {
		return nil, err
	}
	if err := r.checkTrusted(tid.id, base); err != nil { // §10
		tr.add(Event{Depth: depth, Kind: "rejected", Slot: slot, URL: tid.id, Err: err.Error()})
		return nil, err
	}

	body, err := r.fetchTemplate(ctx, tid, depth, slot, tr)
	if err != nil {
		return nil, err
	}
	if vd.Integrity != "" {
		// §3.6: a mismatch is a template fetch failure for this slot (§9.1).
		if err := verifyIntegrity(vd.Integrity, []byte(body)); err != nil {
			tr.add(Event{Depth: depth, Kind: "integrity-failed", Slot: slot, URL: tid.id, Err: err.Error()})
			return nil, fmt.Errorf("template %s: %w", tid.id, err)
		}
	}

	node := &Node{ID: tid.id, Body: body, Transform: vd.Transform}
	if uri := vd.Transform.MapperURI(); uri != "" {
		// §3.8.3: dispatch to registered mapper code, matched verbatim. An
		// unknown mapper fails this node — the slot is skipped by the caller
		// (§9.1); at the root the error surfaces to the renderer, which must
		// not fall back to untransformed input (§9.4 rule 2).
		fn, ok := r.Mappers[uri]
		if !ok {
			tr.add(Event{Depth: depth, Kind: "mapper-unknown", Slot: slot, URL: uri, Err: ErrUnknownMapper.Error()})
			return nil, fmt.Errorf("%w: %s", ErrUnknownMapper, uri)
		}
		node.Mapper = fn
	}
	// Slot names are walked in sorted order. VDP fixes the order of descriptors
	// *within* an array slot (§3.5) but says nothing about the order slots
	// themselves are resolved in — it cannot matter, since each slot renders
	// wherever its template puts it. Sorting costs nothing and makes fetch order
	// and traces reproducible instead of varying with Go's map iteration.
	for _, name := range slices.Sorted(maps.Keys(vd.Slots)) {
		for _, child := range vd.Slots[name].Descriptors {
			// §5.4: nested templates resolve against the same base as the root.
			childNode, err := r.resolveSlot(ctx, child, base, depth+1, name, tr, seen)
			if err != nil {
				// §9.1/§9.4: skip the slot, keep rendering the rest of the tree.
				tr.add(Event{Depth: depth + 1, Kind: "slot-skipped", Slot: name, URL: child.slotURL(), Err: err.Error()})
				continue
			}
			if node.Slots == nil {
				node.Slots = map[string][]*Node{}
			}
			node.Slots[name] = append(node.Slots[name], childNode)
		}
	}
	return node, nil
}

// resolveSlot resolves one slot descriptor: inline descriptors recurse
// directly, references (§3.7) fetch the referenced resource first and use the
// result in its place.
func (r *Resolver) resolveSlot(ctx context.Context, sd SlotDescriptor, base *url.URL, depth int, slot string, tr *Trace, seen map[string]bool) (*Node, error) {
	if sd.Ref == "" {
		return r.resolve(ctx, sd.ViewDescriptor, base, depth, slot, tr, seen)
	}

	// §8: references count toward the recursion depth limit.
	if depth >= r.maxDepth() {
		tr.add(Event{Depth: depth, Kind: "depth-exceeded", Slot: slot, URL: sd.Ref})
		return nil, fmt.Errorf("maximum template nesting depth (%d) exceeded at reference %q", r.maxDepth(), sd.Ref)
	}
	// §3.7: the reference resolves against the same base as the containing
	// descriptor's template URIs. Unlike template URIs, a descriptor reference
	// is always a genuine URL — a fetchable resource location — so ordinary
	// RFC 3986 resolution applies and the §5.4 opaque form does not.
	abs, err := resolveRefURL(sd.Ref, base)
	if err != nil {
		return nil, err
	}
	// §10: reference URLs go through the same allowlist chain as templates.
	if err := r.checkTrusted(abs.String(), base); err != nil {
		tr.add(Event{Depth: depth, Kind: "rejected", Slot: slot, URL: abs.String(), Err: err.Error()})
		return nil, err
	}
	// §3.7: a chain that revisits a descriptor URL is a cycle; abort the slot.
	key := abs.String()
	if seen[key] {
		err := fmt.Errorf("descriptor reference cycle at %s", key)
		tr.add(Event{Depth: depth, Kind: "ref-cycle", Slot: slot, URL: key, Err: err.Error()})
		return nil, err
	}

	vd, err := r.fetchReferencedDescriptor(ctx, abs)
	event := Event{Depth: depth, Kind: "descriptor-ref", Slot: slot, URL: key}
	if err != nil {
		// §9.1: an unfetchable or invalid reference fails like a template fetch.
		event.Err = err.Error()
		tr.add(event)
		return nil, err
	}
	tr.add(event)

	chain := maps.Clone(seen)
	if chain == nil {
		chain = map[string]bool{}
	}
	chain[key] = true
	// §3.7: the fetched resource is a standalone descriptor, so relative URLs
	// inside it resolve against its own URL.
	return r.resolve(ctx, vd, abs, depth+1, slot, tr, chain)
}

// fetchReferencedDescriptor retrieves and validates a referenced view
// descriptor resource (§3.7), caching parsed descriptors by URL (§5.2).
func (r *Resolver) fetchReferencedDescriptor(ctx context.Context, u *url.URL) (ViewDescriptor, error) {
	key := u.String()
	if cached, ok := r.descCache.Load(key); ok {
		return cached.(ViewDescriptor), nil
	}
	if err := requireHTTPS(u); err != nil { // §10
		return ViewDescriptor{}, fmt.Errorf("fetch referenced descriptor %s: %w", key, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, key, nil)
	if err != nil {
		return ViewDescriptor{}, err
	}
	req.Header.Set("Accept", MediaType+", application/json")
	if r.Platform != "" {
		req.Header.Set(HeaderPlatform, r.Platform) // §5.5
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return ViewDescriptor{}, fmt.Errorf("fetch referenced descriptor %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ViewDescriptor{}, fmt.Errorf("fetch referenced descriptor %s: unexpected status %s", key, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTemplateBytes))
	if err != nil {
		return ViewDescriptor{}, fmt.Errorf("read referenced descriptor %s: %w", key, err)
	}
	vd, err := parseSingle(body)
	if err != nil {
		return ViewDescriptor{}, fmt.Errorf("referenced descriptor %s: %w", key, err)
	}
	r.descCache.Store(key, vd)
	return vd, nil
}

func (r *Resolver) maxDepth() int {
	if r.MaxDepth <= 0 {
		return DefaultMaxDepth
	}
	return r.MaxDepth
}

// templateID is a template's §5.4 identity together with the network location
// it may be fetched from. The two coincide for absolute URIs and resolved
// /-prefixed references; for a scheme-less opaque identifier the id stays
// verbatim while fetch carries the scheme the client supplied (§6.3).
type templateID struct {
	id     string
	fetch  *url.URL
	opaque bool
}

// resolveTemplateURI classifies a template URI into its §5.4 form and returns
// its identity and fetch location.
func resolveTemplateURI(ref string, base *url.URL) (templateID, error) {
	// Form (a): an absolute URI with a scheme. Identity as written.
	if u, err := url.Parse(ref); err == nil && u.IsAbs() {
		return templateID{id: ref, fetch: u}, nil
	}
	// Form (b): an RFC 3986 relative reference beginning with "/"
	// (path-absolute) or "//" (network-path), resolved against the transport's
	// base URL. Identity is the resolved absolute URL.
	if strings.HasPrefix(ref, "/") {
		u, err := url.Parse(ref)
		if err != nil {
			return templateID{}, fmt.Errorf("template %q: %w", ref, err)
		}
		if base != nil {
			u = base.ResolveReference(u)
		}
		if !u.IsAbs() {
			return templateID{}, fmt.Errorf("template %q is relative and no base URL is available", ref)
		}
		return templateID{id: u.String(), fetch: u}, nil
	}
	// Form (c): a scheme-less, host-qualified opaque identifier. It is NOT
	// resolved against the base — the identifier as written is the identity —
	// and a scheme is supplied only for the network fetch (§6.3).
	fetch, err := opaqueFetchURL(ref)
	if err != nil {
		return templateID{}, err
	}
	return templateID{id: ref, fetch: fetch, opaque: true}, nil
}

// opaqueFetchURL builds the URL a §5.4 form (c) identifier is fetched from
// when no local source supplies the template: the identifier with a
// client-supplied scheme — HTTPS per §10, or plain HTTP for loopback hosts
// (the §10 local-development exception, which is how this demo runs).
func opaqueFetchURL(id string) (*url.URL, error) {
	host, _, _ := strings.Cut(id, "/")
	if host == "" {
		return nil, fmt.Errorf("template %q: opaque identifier has no host part", id)
	}
	scheme := "https"
	if isLoopbackHost(host) {
		scheme = "http"
	}
	u, err := url.Parse(scheme + "://" + id)
	if err != nil {
		return nil, fmt.Errorf("template %q: %w", id, err)
	}
	return u, nil
}

// isLoopbackHost reports whether a host (possibly with port) is loopback.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// resolveRefURL turns a possibly-relative descriptor reference URL into an
// absolute one per RFC 3986 (§3.7).
func resolveRefURL(ref string, base *url.URL) (*url.URL, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("descriptor reference %q: %w", ref, err)
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if !u.IsAbs() {
		return nil, fmt.Errorf("descriptor reference %q is relative and no base URL is available", ref)
	}
	return u, nil
}

// requireHTTPS enforces §10 transport security on a network fetch: HTTPS
// always passes, and plain HTTP only for loopback hosts (the §10 exception
// for local development, which is also how this demo runs).
func requireHTTPS(u *url.URL) error {
	switch {
	case strings.EqualFold(u.Scheme, "https"):
		return nil
	case strings.EqualFold(u.Scheme, "http") && isLoopbackHost(u.Host):
		return nil
	}
	return fmt.Errorf("network retrieval requires HTTPS (loopback excepted): %s", u)
}

// checkTrusted enforces the template URI allowlist (§10) on a §5.4 identity.
// Rendering templates from untrusted origins is a code injection risk.
//
// Matching follows §13.2: the identity must begin with an allowlist entry,
// compared verbatim after normalization (lowercased scheme and host) and on
// path segment boundaries, so a /templates entry does not also match
// /templates-evil. Matching never crosses §5.4 forms — an absolute URI never
// matches a scheme-less entry, nor an opaque identifier an absolute entry — so
// a deployment lists each identifier form it actually serves.
//
// With no allowlist configured or advertised, the §10 source chain falls back
// to its same-origin default: identities sharing an origin with the
// descriptor's base URL are trusted (host-only comparison for opaque
// identifiers, which carry no scheme), and with no base, nothing is.
func (r *Resolver) checkTrusted(id string, base *url.URL) error {
	idN, opaque := normalizeIdentity(id)
	if len(r.TrustedTemplateURLs) == 0 {
		if base != nil {
			if opaque {
				host, _, _ := strings.Cut(idN, "/")
				if strings.EqualFold(host, base.Host) {
					return nil
				}
			} else if u, err := url.Parse(id); err == nil &&
				strings.EqualFold(u.Scheme, base.Scheme) && strings.EqualFold(u.Host, base.Host) {
				return nil
			}
		}
		return fmt.Errorf("%w: %s (same-origin default)", ErrUntrustedTemplate, id)
	}
	for _, trusted := range r.TrustedTemplateURLs {
		tN, tOpaque := normalizeIdentity(trusted)
		if tOpaque != opaque {
			continue // §13.2: no cross-form matching.
		}
		prefix := strings.TrimSuffix(tN, "/")
		if idN == prefix || strings.HasPrefix(idN, prefix+"/") {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrUntrustedTemplate, id)
}

// normalizeIdentity prepares a §5.4 identity (or allowlist entry) for §13.2
// comparison — lowercasing the scheme and host, leaving the path untouched —
// and reports whether the value is the scheme-less opaque form.
func normalizeIdentity(s string) (string, bool) {
	if u, err := url.Parse(s); err == nil && u.IsAbs() {
		return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.EscapedPath(), false
	}
	// Scheme-less: the part before the first "/" is the host.
	host, rest, found := strings.Cut(s, "/")
	n := strings.ToLower(host)
	if found {
		n += "/" + rest
	}
	return n, true
}

// fetchTemplate obtains a template over the network, caching bodies by §5.4
// identity (§5.2, §6.3).
func (r *Resolver) fetchTemplate(ctx context.Context, tid templateID, depth int, slot string, tr *Trace) (string, error) {
	key := tid.id
	event := Event{Depth: depth, Slot: slot, URL: key}

	if cached, ok := r.cache.Load(key); ok {
		event.Kind = "cached"
		tr.add(event)
		return cached.(string), nil
	}
	event.Kind = "fetch"

	fail := func(err error, detail string) (string, error) {
		event.Err = detail
		tr.add(event)
		return "", err
	}
	if err := requireHTTPS(tid.fetch); err != nil { // §10
		return fail(fmt.Errorf("fetch template %s: %w", key, err), err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tid.fetch.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return fail(fmt.Errorf("fetch template %s: %w", key, err), err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("fetch template %s: unexpected status %s", key, resp.Status), resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTemplateBytes))
	if err != nil {
		return fail(fmt.Errorf("read template %s: %w", key, err), err.Error())
	}
	tr.add(event)
	r.cache.Store(key, string(body))
	return string(body), nil
}
