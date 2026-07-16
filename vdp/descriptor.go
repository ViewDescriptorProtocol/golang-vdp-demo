// Package vdp implements the View Descriptor Protocol v0.1.
//
// The package is split along the same lines as the specification:
//
//	descriptor.go — §3 view descriptor format
//	transport.go  — §4 transport mechanisms, §13 discovery
//	resolve.go    — §8 client resolution algorithm, §9 error handling, §10 security
//
// It is deliberately free of any template-engine knowledge. VDP declares which
// templates render a response; binding data to those templates belongs to the
// engine (see the render package for the html/template binding).
//
// Spec: https://viewdescriptorprotocol.github.io/specification/
package vdp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// MediaType is the media type for view descriptor resources (§5.1).
const MediaType = "application/vdp+json"

// Version is the specification version implemented here.
const Version = "0.1"

// ViewDescriptor identifies a root template and its dynamic slot assignments
// (§3.1, §3.2). Slots are recursive: each slot value is itself one or more
// ViewDescriptors, so composition nests to arbitrary depth (§3.3).
type ViewDescriptor struct {
	Template string `json:"template"`
	Slots    Slots  `json:"slots,omitempty"`
}

// Slots maps slot names to their values. A slot name matches a named insertion
// point in the parent template (§2).
type Slots map[string]SlotValue

// MultiViewDescriptor holds several named views for one response (§3.4).
// Clients should use DefaultView when no specific view is requested.
type MultiViewDescriptor struct {
	Views map[string]ViewDescriptor `json:"views"`
}

// DefaultView is the view name clients fall back to (§3.4).
const DefaultView = "default"

// SlotValue is either a single ViewDescriptor or an ordered array of them
// (§3.5). The grammar admits both forms, so this type carries the distinction
// through a round trip: a slot written as an object marshals back as an object,
// and one written as an array marshals back as an array.
//
// Rendering treats the two uniformly — Descriptors is always the render order.
type SlotValue struct {
	Descriptors []ViewDescriptor
	// Array reports whether this value was expressed in JSON as an array.
	Array bool
}

// Single builds a SlotValue holding one descriptor, marshalled as an object.
func Single(vd ViewDescriptor) SlotValue {
	return SlotValue{Descriptors: []ViewDescriptor{vd}}
}

// Sequence builds a SlotValue holding descriptors rendered in order (§3.5),
// marshalled as an array.
func Sequence(vds ...ViewDescriptor) SlotValue {
	return SlotValue{Descriptors: vds, Array: true}
}

// UnmarshalJSON accepts either form of the SlotValue production (§3.6):
//
//	SlotValue = ViewDescriptor | ViewDescriptor[]
func (sv *SlotValue) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimLeft(b, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var vds []ViewDescriptor
		if err := json.Unmarshal(b, &vds); err != nil {
			return fmt.Errorf("slot array: %w", err)
		}
		if len(vds) == 0 {
			return fmt.Errorf("slot array: must contain at least one descriptor")
		}
		*sv = SlotValue{Descriptors: vds, Array: true}
		return nil
	}
	var vd ViewDescriptor
	if err := json.Unmarshal(b, &vd); err != nil {
		return fmt.Errorf("slot: %w", err)
	}
	*sv = SlotValue{Descriptors: []ViewDescriptor{vd}}
	return nil
}

// MarshalJSON writes back the form the value was expressed in.
func (sv SlotValue) MarshalJSON() ([]byte, error) {
	if sv.Array {
		return json.Marshal(sv.Descriptors)
	}
	if len(sv.Descriptors) != 1 {
		return nil, fmt.Errorf("non-array slot must hold exactly one descriptor, has %d", len(sv.Descriptors))
	}
	return json.Marshal(sv.Descriptors[0])
}

// Validate reports whether the descriptor satisfies the §3.6 grammar. An
// invalid descriptor must be rejected by clients (§9.3).
func (vd ViewDescriptor) Validate() error {
	return vd.validate(nil)
}

func (vd ViewDescriptor) validate(path []string) error {
	at := func(err error) error {
		if len(path) == 0 {
			return err
		}
		return fmt.Errorf("slot %q: %w", strings.Join(path, "."), err)
	}
	if vd.Template == "" {
		return at(fmt.Errorf("missing required field \"template\""))
	}
	// §3.6: TemplateURL is a URI reference. Relative references are legal and
	// resolve against a base URL at resolution time (§5.4).
	if _, err := url.Parse(vd.Template); err != nil {
		return at(fmt.Errorf("template %q is not a valid URI: %w", vd.Template, err))
	}
	for name, sv := range vd.Slots {
		if len(sv.Descriptors) == 0 {
			return at(fmt.Errorf("slot %q: no descriptors", name))
		}
		for _, child := range sv.Descriptors {
			if err := child.validate(append(path, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Validate reports whether the multi-view descriptor is well-formed (§3.4).
func (mvd MultiViewDescriptor) Validate() error {
	if len(mvd.Views) == 0 {
		return fmt.Errorf("\"views\" must contain at least one view")
	}
	for name, vd := range mvd.Views {
		if err := vd.Validate(); err != nil {
			return fmt.Errorf("view %q: %w", name, err)
		}
	}
	return nil
}

// Parse reads a standalone VDP document, which is either a ViewDescriptor or a
// MultiViewDescriptor (§3.6), and returns it as a map of named views. A
// single-view document is returned under DefaultView.
func Parse(b []byte) (map[string]ViewDescriptor, error) {
	// The two productions are distinguished by their keys: a MultiViewDescriptor
	// has "views" and no "template".
	var probe struct {
		Template *string         `json:"template"`
		Views    json.RawMessage `json:"views"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("invalid view descriptor: %w", err)
	}
	switch {
	case probe.Views != nil && probe.Template != nil:
		return nil, fmt.Errorf("invalid view descriptor: has both \"template\" and \"views\"")
	case probe.Views != nil:
		var mvd MultiViewDescriptor
		if err := json.Unmarshal(b, &mvd); err != nil {
			return nil, fmt.Errorf("invalid multi-view descriptor: %w", err)
		}
		if err := mvd.Validate(); err != nil {
			return nil, err
		}
		return mvd.Views, nil
	default:
		var vd ViewDescriptor
		if err := json.Unmarshal(b, &vd); err != nil {
			return nil, fmt.Errorf("invalid view descriptor: %w", err)
		}
		if err := vd.Validate(); err != nil {
			return nil, err
		}
		return map[string]ViewDescriptor{DefaultView: vd}, nil
	}
}

// DiscoveryDocument is served at /.well-known/vdp (§13.2). It lets clients
// prefetch view descriptors and learn the trusted template allowlist (§10).
type DiscoveryDocument struct {
	Version                string                    `json:"version"`
	Endpoints              map[string]ViewDescriptor `json:"endpoints,omitempty"`
	TrustedTemplateDomains []string                  `json:"trustedTemplateDomains,omitempty"`
}
