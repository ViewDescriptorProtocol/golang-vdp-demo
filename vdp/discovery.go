package vdp

import (
	"maps"
	"net/url"
	"slices"
	"strings"
)

// DiscoveryMediaType is the media type of the discovery document (§12.3). The
// document is not a view descriptor and must not be served as
// application/vdp+json (§13.2).
const DiscoveryMediaType = "application/vdp-discovery+json"

// WellKnownPath is where APIs may expose the discovery document (§13.2).
const WellKnownPath = "/.well-known/vdp"

// DiscoveryDocument is served at /.well-known/vdp (§13.2). It lets clients
// prefetch view descriptors and learn the trusted template URL allowlist
// (§10).
type DiscoveryDocument struct {
	Version string `json:"version"`
	// Endpoints maps API paths — absolute, origin-relative, optionally Level 1
	// URI Templates — to their view descriptor resources.
	Endpoints map[string]EndpointEntry `json:"endpoints,omitempty"`
	// TrustedTemplateURLs is the §10 allowlist: URL prefixes template URLs
	// must fall under. Entries should end with a trailing slash.
	TrustedTemplateURLs []string `json:"trustedTemplateUrls,omitempty"`
	// Mappers lists the $mapper URIs descriptors from this API may reference
	// (§3.8.3, §13.2). Identifiers matched verbatim against the client's
	// registered mappers — listing one does not make it fetchable.
	Mappers []string `json:"mappers,omitempty"`
}

// EndpointEntry is one endpoints member (§13.2).
type EndpointEntry struct {
	// Descriptor is the URL of the endpoint's view descriptor resource. It may
	// be relative, resolved against the discovery document's own URL.
	Descriptor string `json:"descriptor"`
}

// DescriptorURL resolves the possibly-relative descriptor URL against the URL
// of the discovery document itself (§13.2).
func (e EndpointEntry) DescriptorURL(docURL *url.URL) (string, error) {
	u, err := resolveRefURL(e.Descriptor, docURL)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// Endpoint returns the entry matching a request path (§13.2). A literal key
// beats a templated one; templated keys are Level 1 URI Templates (RFC 6570)
// whose every expression matches exactly one path segment. Templated keys are
// tried in sorted order so a path overlapping several — which servers should
// not publish — still matches deterministically.
func (d DiscoveryDocument) Endpoint(path string) (EndpointEntry, bool) {
	if e, ok := d.Endpoints[path]; ok {
		return e, true
	}
	for _, key := range slices.Sorted(maps.Keys(d.Endpoints)) {
		if strings.Contains(key, "{") && matchTemplateKey(key, path) {
			return d.Endpoints[key], true
		}
	}
	return EndpointEntry{}, false
}

// matchTemplateKey matches a request path against a Level 1 URI-Template key.
// Each {expression} matches one path segment: one or more characters, none of
// which is "/" (§13.2).
func matchTemplateKey(key, path string) bool {
	keySegs := strings.Split(key, "/")
	pathSegs := strings.Split(path, "/")
	if len(keySegs) != len(pathSegs) {
		return false
	}
	for i, ks := range keySegs {
		if strings.HasPrefix(ks, "{") && strings.HasSuffix(ks, "}") && len(ks) > 2 {
			if pathSegs[i] == "" {
				return false
			}
			continue
		}
		if ks != pathSegs[i] {
			return false
		}
	}
	return true
}
