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
	Kind  string // "fetch", "cached", "slot-skipped", "depth-exceeded", "rejected"
	Slot  string // slot being filled, empty for the root
	URL   string
	Err   string
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
	// TrustedTemplateDomains is the allowlist of base URLs templates may be
	// loaded from (§10, §13.2). A template URL outside it is rejected.
	TrustedTemplateDomains []string
	// MaxDepth bounds template nesting (§8). Zero means DefaultMaxDepth.
	MaxDepth int

	cache sync.Map // absolute template URL -> string body (§5.2)
}

// NewResolver returns a Resolver that trusts templates under the given base
// URLs and caches fetched templates in memory (§5.2).
func NewResolver(client *http.Client, trustedTemplateDomains []string) *Resolver {
	if client == nil {
		client = http.DefaultClient
	}
	return &Resolver{
		HTTP:                   client,
		TrustedTemplateDomains: trustedTemplateDomains,
		MaxDepth:               DefaultMaxDepth,
	}
}

// ErrUntrustedTemplate is returned when a template URL falls outside the
// trusted domain allowlist (§10).
var ErrUntrustedTemplate = errors.New("template URL is not in the trusted domain allowlist")

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
	return r.resolve(ctx, vd, base, 0, "", tr)
}

// slot is the name of the slot this descriptor fills, empty at the root. It is
// carried only so the trace can say which slot each fetch belongs to.
func (r *Resolver) resolve(ctx context.Context, vd ViewDescriptor, base *url.URL, depth int, slot string, tr *Trace) (*Node, error) {
	maxDepth := r.MaxDepth
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}
	if depth >= maxDepth {
		tr.add(Event{Depth: depth, Kind: "depth-exceeded", Slot: slot, URL: vd.Template})
		return nil, fmt.Errorf("maximum template nesting depth (%d) exceeded at %q", maxDepth, vd.Template)
	}

	abs, err := resolveURL(vd.Template, base) // §5.4
	if err != nil {
		return nil, err
	}
	if err := r.checkTrusted(abs); err != nil { // §10
		tr.add(Event{Depth: depth, Kind: "rejected", Slot: slot, URL: abs.String(), Err: err.Error()})
		return nil, err
	}

	body, err := r.fetchTemplate(ctx, abs, depth, slot, tr)
	if err != nil {
		return nil, err
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
			childNode, err := r.resolve(ctx, child, base, depth+1, name, tr)
			if err != nil {
				// §9.1/§9.4: skip the slot, keep rendering the rest of the tree.
				tr.add(Event{Depth: depth + 1, Kind: "slot-skipped", Slot: name, URL: child.Template, Err: err.Error()})
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
// from untrusted origins is a code injection risk, so an empty allowlist denies
// everything rather than permitting everything.
func (r *Resolver) checkTrusted(u *url.URL) error {
	for _, trusted := range r.TrustedTemplateDomains {
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
