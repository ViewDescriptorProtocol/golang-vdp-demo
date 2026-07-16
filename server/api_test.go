package server

import (
	"net/http/httptest"
	"testing"
)

// baseURL reconstructs the public origin so descriptors can carry absolute
// template URLs whatever port the demo runs on.
func TestBaseURL(t *testing.T) {
	r := httptest.NewRequest("GET", "http://example.test:8099/dashboard", nil)
	if got := baseURL(r); got != "http://example.test:8099" {
		t.Errorf("baseURL = %q, want http://example.test:8099", got)
	}
}

// A TLS request reports an https origin.
func TestBaseURLTLS(t *testing.T) {
	r := httptest.NewRequest("GET", "https://secure.test/dashboard", nil)
	if got := baseURL(r); got != "https://secure.test" {
		t.Errorf("baseURL = %q, want https://secure.test", got)
	}
}

// Behind a proxy the scheme comes from X-Forwarded-Proto, and only the first
// value in a comma-separated list is used.
func TestBaseURLForwardedProto(t *testing.T) {
	r := httptest.NewRequest("GET", "http://proxied.test/dashboard", nil)
	r.Header.Set("X-Forwarded-Proto", "https, http")
	if got := baseURL(r); got != "https://proxied.test" {
		t.Errorf("baseURL = %q, want https://proxied.test", got)
	}
}
