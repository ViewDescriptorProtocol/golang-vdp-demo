// Package vdp implements the View Descriptor Protocol v0.1.
//
// The package is split along the same lines as the specification:
//
//	descriptor.go — §3 view descriptor format
//	transport.go  — §4 transport mechanisms
//	resolve.go    — §8 client resolution algorithm, §9 error handling, §10 security
//	integrity.go  — §3.6 template integrity verification (W3C SRI)
//	discovery.go  — §13 discovery
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
const Version = "0.2"

// ViewDescriptor identifies a root template and its dynamic slot assignments
// (§3.1, §3.2). Slots are recursive: each slot value is itself one or more
// descriptors, so composition nests to arbitrary depth (§3.3).
//
// Type and Integrity are optional template metadata (§3.6). Both describe the
// template resource: Type is an advisory media type hint (the fetched
// response's Content-Type stays authoritative), and Integrity is W3C
// Subresource Integrity metadata the resolver verifies fetched template bytes
// against.
// Transform is the optional §3.8 member adapting the response representation
// into the model this node's template expects.
type ViewDescriptor struct {
	Template  string     `json:"template"`
	Type      string     `json:"type,omitempty"`
	Integrity string     `json:"integrity,omitempty"`
	Slots     Slots      `json:"slots,omitempty"`
	Transform *Transform `json:"transform,omitempty"`
}

// viewDescriptorMembers are the members this version understands on a view
// descriptor node. Per §3.10, any other member not prefixed "x-" makes the
// descriptor invalid — a client that ignored an unknown must-understand member
// (like a 0.1 client ignoring "transform") would silently render wrong output.
var viewDescriptorMembers = map[string]bool{
	"template": true, "type": true, "integrity": true, "slots": true, "transform": true,
}

// checkMembers enforces the §3.10 extensibility rule for a descriptor object.
func checkMembers(members map[string]json.RawMessage, known map[string]bool, context string) error {
	for name := range members {
		if !known[name] && !strings.HasPrefix(name, "x-") {
			return fmt.Errorf("%s: unrecognized member %q (§3.10: members not prefixed \"x-\" are must-understand)", context, name)
		}
	}
	return nil
}

// UnmarshalJSON reads a view descriptor node, rejecting unrecognized members
// (§3.10) and validating any transform grammar (§3.8.1, §9.3).
func (vd *ViewDescriptor) UnmarshalJSON(b []byte) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil {
		return err
	}
	if err := checkMembers(members, viewDescriptorMembers, "view descriptor"); err != nil {
		return err
	}
	type plain struct {
		Template  string `json:"template"`
		Type      string `json:"type,omitempty"`
		Integrity string `json:"integrity,omitempty"`
		Slots     Slots  `json:"slots,omitempty"`
	}
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*vd = ViewDescriptor{Template: p.Template, Type: p.Type, Integrity: p.Integrity, Slots: p.Slots}
	if raw, ok := members["transform"]; ok {
		tr, err := ParseTransform(raw)
		if err != nil {
			return err // malformed transform = invalid descriptor (§9.3)
		}
		vd.Transform = tr
	}
	return nil
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

// SlotDescriptor is one element of a slot value (§3.8): either an inline
// ViewDescriptor or a descriptor reference (§3.7) — the URL of a standalone
// view descriptor resource to fetch and use in its place.
type SlotDescriptor struct {
	ViewDescriptor
	// Ref is the descriptor reference URL. When set, the embedded
	// ViewDescriptor is zero: a reference contains exactly the "descriptor"
	// member (§3.7).
	Ref string
}

// UnmarshalJSON distinguishes the two SlotDescriptor forms by the "descriptor"
// member (§3.7).
func (sd *SlotDescriptor) UnmarshalJSON(b []byte) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil {
		return fmt.Errorf("slot: %w", err)
	}
	if raw, ok := members["descriptor"]; ok {
		// §3.7: a reference contains exactly the "descriptor" member — so a
		// reference site can never carry template, slots or a transform.
		// Vendor extensions ("x-", §3.10) are the one exception.
		if err := checkMembers(members, map[string]bool{"descriptor": true}, "descriptor reference"); err != nil {
			return err
		}
		var ref string
		if err := json.Unmarshal(raw, &ref); err != nil {
			return fmt.Errorf("descriptor reference: %w", err)
		}
		if ref == "" {
			return fmt.Errorf("descriptor reference: empty URL")
		}
		*sd = SlotDescriptor{Ref: ref}
		return nil
	}
	var vd ViewDescriptor
	if err := json.Unmarshal(b, &vd); err != nil {
		return fmt.Errorf("slot: %w", err)
	}
	*sd = SlotDescriptor{ViewDescriptor: vd}
	return nil
}

// MarshalJSON writes back whichever form the descriptor holds.
func (sd SlotDescriptor) MarshalJSON() ([]byte, error) {
	if sd.Ref != "" {
		return json.Marshal(struct {
			Descriptor string `json:"descriptor"`
		}{sd.Ref})
	}
	return json.Marshal(sd.ViewDescriptor)
}

// SlotValue is either a single slot descriptor or an ordered array of them
// (§3.5, §3.8). The grammar admits both forms, so this type carries the
// distinction through a round trip: a slot written as an object marshals back
// as an object, and one written as an array marshals back as an array.
//
// Rendering treats the two uniformly — Descriptors is always the render order.
type SlotValue struct {
	Descriptors []SlotDescriptor
	// Array reports whether this value was expressed in JSON as an array.
	Array bool
}

// Single builds a SlotValue holding one inline descriptor, marshalled as an
// object.
func Single(vd ViewDescriptor) SlotValue {
	return SlotValue{Descriptors: []SlotDescriptor{{ViewDescriptor: vd}}}
}

// Sequence builds a SlotValue holding descriptors rendered in order (§3.5),
// marshalled as an array.
func Sequence(vds ...ViewDescriptor) SlotValue {
	sds := make([]SlotDescriptor, len(vds))
	for i, vd := range vds {
		sds[i] = SlotDescriptor{ViewDescriptor: vd}
	}
	return SlotValue{Descriptors: sds, Array: true}
}

// Reference builds a SlotValue holding one descriptor reference (§3.7).
func Reference(url string) SlotValue {
	return SlotValue{Descriptors: []SlotDescriptor{{Ref: url}}}
}

// UnmarshalJSON accepts either form of the SlotValue production (§3.8):
//
//	SlotValue = SlotDescriptor | SlotDescriptor[]
func (sv *SlotValue) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimLeft(b, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var sds []SlotDescriptor
		if err := json.Unmarshal(b, &sds); err != nil {
			return fmt.Errorf("slot array: %w", err)
		}
		if len(sds) == 0 {
			return fmt.Errorf("slot array: must contain at least one descriptor")
		}
		*sv = SlotValue{Descriptors: sds, Array: true}
		return nil
	}
	var sd SlotDescriptor
	if err := json.Unmarshal(b, &sd); err != nil {
		return err
	}
	*sv = SlotValue{Descriptors: []SlotDescriptor{sd}}
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

// Validate reports whether the descriptor satisfies the §3.8 grammar. An
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
	// §3.8/§5.4: a template URI is an absolute URI, a /-prefixed relative
	// reference, or a scheme-less opaque identifier. url.Parse rejects opaque
	// identifiers whose host carries a port (host:port/...), so those are
	// validated by parsing with a scheme supplied, as a fetch would (§6.3).
	if _, err := url.Parse(vd.Template); err != nil {
		valid := false
		if !strings.HasPrefix(vd.Template, "/") {
			_, err2 := url.Parse("https://" + vd.Template)
			valid = err2 == nil
		}
		if !valid {
			return at(fmt.Errorf("template %q is not a valid URI: %w", vd.Template, err))
		}
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

func (sd SlotDescriptor) validate(path []string) error {
	if sd.Ref != "" {
		// §3.7: a reference carries only the descriptor URL.
		if sd.Template != "" || sd.Slots != nil {
			return fmt.Errorf("slot %q: descriptor reference must not also carry template or slots", strings.Join(path, "."))
		}
		if _, err := url.Parse(sd.Ref); err != nil {
			return fmt.Errorf("slot %q: descriptor reference %q is not a valid URI: %w", strings.Join(path, "."), sd.Ref, err)
		}
		return nil
	}
	return sd.ViewDescriptor.validate(path)
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

// slotURL names the resource a slot descriptor points at, for traces: the
// reference URL for references, the template URL otherwise.
func (sd SlotDescriptor) slotURL() string {
	if sd.Ref != "" {
		return sd.Ref
	}
	return sd.Template
}

// parseSingle reads a referenced view descriptor resource (§3.7). It must be a
// single ViewDescriptor: a MultiViewDescriptor is invalid in slot context
// (§9.3), and a reference cannot point at another reference.
func parseSingle(b []byte) (ViewDescriptor, error) {
	var probe struct {
		Views      json.RawMessage `json:"views"`
		Descriptor *string         `json:"descriptor"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return ViewDescriptor{}, fmt.Errorf("invalid view descriptor: %w", err)
	}
	if probe.Views != nil {
		return ViewDescriptor{}, fmt.Errorf("a multi-view descriptor is invalid in slot context")
	}
	if probe.Descriptor != nil {
		return ViewDescriptor{}, fmt.Errorf("a descriptor reference cannot be the document root")
	}
	var vd ViewDescriptor
	if err := json.Unmarshal(b, &vd); err != nil {
		return ViewDescriptor{}, fmt.Errorf("invalid view descriptor: %w", err)
	}
	if err := vd.Validate(); err != nil {
		return ViewDescriptor{}, err
	}
	return vd, nil
}

// Parse reads a standalone VDP document, which is either a ViewDescriptor or a
// MultiViewDescriptor (§3.8), and returns it as a map of named views. A
// single-view document is returned under DefaultView.
func Parse(b []byte) (map[string]ViewDescriptor, error) {
	// The two productions are distinguished by their keys: a MultiViewDescriptor
	// has "views" and no "template".
	var probe struct {
		Template   *string         `json:"template"`
		Views      json.RawMessage `json:"views"`
		Descriptor *string         `json:"descriptor"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("invalid view descriptor: %w", err)
	}
	switch {
	case probe.Descriptor != nil:
		// §3.7: descriptor references are valid only as slot values, never as
		// the root of a descriptor resource or inline view.
		return nil, fmt.Errorf("invalid view descriptor: a descriptor reference cannot be the document root")
	case probe.Views != nil && probe.Template != nil:
		return nil, fmt.Errorf("invalid view descriptor: has both \"template\" and \"views\"")
	case probe.Views != nil:
		var members map[string]json.RawMessage
		if err := json.Unmarshal(b, &members); err != nil {
			return nil, fmt.Errorf("invalid multi-view descriptor: %w", err)
		}
		if err := checkMembers(members, map[string]bool{"views": true}, "multi-view descriptor"); err != nil {
			return nil, err
		}
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
