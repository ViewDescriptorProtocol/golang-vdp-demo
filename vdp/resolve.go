package vdp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
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

// Node is a resolved template tree node: a fetched template plus the resolved
// sub-trees filling its slots. It is the output of the §8 algorithm and the
// input to a renderer.
type Node struct {
	// URL is the absolute template URL this node was fetched from.
	URL string
	// Body is the template source.
	Body string
	// Slots maps each slot name to the nodes filling it, in render order (§3.5).
	// Slots whose templates could not be resolved are absent (§9.1).
	Slots map[string][]*Node
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
	// "rejected", "integrity-failed", "descriptor-ref", "ref-cycle".
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

// FetchDescriptor retrieves a standalone view descriptor resource (§4.1, §5).
// It is called when Extract yields a DescriptorURL rather than inline views.
func (r *Resolver) FetchDescriptor(ctx context.Context, e *Extraction) error {
	if e.DescriptorURL == "" {
		return nil // Already inline.
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

	abs, err := resolveURL(vd.Template, base) // §5.4
	if err != nil {
		return nil, err
	}
	if err := r.checkTrusted(abs, base); err != nil { // §10
		tr.add(Event{Depth: depth, Kind: "rejected", Slot: slot, URL: abs.String(), Err: err.Error()})
		return nil, err
	}

	body, err := r.fetchTemplate(ctx, abs, depth, slot, tr)
	if err != nil {
		return nil, err
	}
	if vd.Integrity != "" {
		// §3.6: a mismatch is a template fetch failure for this slot (§9.1).
		if err := verifyIntegrity(vd.Integrity, []byte(body)); err != nil {
			tr.add(Event{Depth: depth, Kind: "integrity-failed", Slot: slot, URL: abs.String(), Err: err.Error()})
			return nil, fmt.Errorf("template %s: %w", abs, err)
		}
	}

	node := &Node{URL: abs.String(), Body: body}
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
	// descriptor's template URLs.
	abs, err := resolveURL(sd.Ref, base)
	if err != nil {
		return nil, err
	}
	// §10: reference URLs go through the same allowlist chain as templates.
	if err := r.checkTrusted(abs, base); err != nil {
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

// resolveURL turns a possibly-relative template URL into an absolute one (§5.4).
func resolveURL(ref string, base *url.URL) (*url.URL, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("template %q: %w", ref, err)
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if !u.IsAbs() {
		return nil, fmt.Errorf("template %q is relative and no base URL is available", ref)
	}
	return u, nil
}

// checkTrusted enforces the template URL allowlist (§10). Rendering templates
// from untrusted origins is a code injection risk. With no allowlist configured
// or advertised, the §10 source chain falls back to its same-origin default:
// only URLs sharing an origin with the descriptor's base URL are trusted, and
// with no base either, nothing is.
func (r *Resolver) checkTrusted(u *url.URL, base *url.URL) error {
	if len(r.TrustedTemplateURLs) == 0 {
		if base != nil && strings.EqualFold(u.Scheme, base.Scheme) && strings.EqualFold(u.Host, base.Host) {
			return nil
		}
		return fmt.Errorf("%w: %s (same-origin default)", ErrUntrustedTemplate, u)
	}
	for _, trusted := range r.TrustedTemplateURLs {
		t, err := url.Parse(trusted)
		if err != nil {
			continue
		}
		if !strings.EqualFold(u.Scheme, t.Scheme) || !strings.EqualFold(u.Host, t.Host) {
			continue
		}
		// The allowlist entry may narrow to a path prefix. Compare on segment
		// boundaries so /templates does not also match /templates-evil.
		prefix := strings.TrimSuffix(t.Path, "/")
		if prefix == "" || u.Path == prefix || strings.HasPrefix(u.Path, prefix+"/") {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrUntrustedTemplate, u)
}

// fetchTemplate retrieves a template, caching bodies by absolute URL (§5.2).
func (r *Resolver) fetchTemplate(ctx context.Context, u *url.URL, depth int, slot string, tr *Trace) (string, error) {
	key := u.String()
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, key, nil)
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
