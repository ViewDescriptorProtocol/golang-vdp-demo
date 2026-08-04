package vdp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// JSONValue is a parsed JSON document that preserves object member order.
//
// Transforms need it for two reasons the standard map[string]any cannot serve:
// $entries MUST emit members in document order (§3.8.2), and conforming
// clients MUST parse JSON objects order-preservingly. Go maps are unordered,
// so the transform input is held in this tree instead; transform output is
// converted back to plain Go values (Plain) for the template engine, where
// order only survives inside slices — exactly the places the spec makes it
// significant.
type JSONValue struct {
	// Exactly one of the following shapes is populated:
	Keys    []string              // object: member names in document order
	Members map[string]*JSONValue // object: member values
	Items   []*JSONValue          // array: elements in order
	Scalar  any                   // string, float64, bool or nil
	kind    jsonKind
}

type jsonKind int

const (
	kindScalar jsonKind = iota
	kindObject
	kindArray
)

// IsObject reports whether the value is a JSON object.
func (v *JSONValue) IsObject() bool { return v != nil && v.kind == kindObject }

// IsArray reports whether the value is a JSON array.
func (v *JSONValue) IsArray() bool { return v != nil && v.kind == kindArray }

// ParseJSON reads a JSON document into an order-preserving tree.
func ParseJSON(b []byte) (*JSONValue, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	// Reject trailing content after the document.
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("trailing content after JSON document")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (*JSONValue, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return decodeFrom(dec, tok)
}

func decodeFrom(dec *json.Decoder, tok json.Token) (*JSONValue, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := &JSONValue{kind: kindObject, Members: map[string]*JSONValue{}}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key := keyTok.(string)
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				if _, dup := obj.Members[key]; !dup {
					obj.Keys = append(obj.Keys, key)
				}
				obj.Members[key] = val // duplicate keys: last value wins, first position kept
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return nil, err
			}
			return obj, nil
		case '[':
			arr := &JSONValue{kind: kindArray}
			for dec.More() {
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr.Items = append(arr.Items, val)
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		return &JSONValue{kind: kindScalar, Scalar: tok}, nil
	}
}

// Plain converts the tree to plain Go values (map[string]any, []any, scalars),
// the shapes encoding/json produces and html/template consumes. Object member
// order is dropped — by the time a model reaches Plain, everything
// order-sensitive ($entries results) already lives in slices.
func (v *JSONValue) Plain() any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case kindObject:
		out := make(map[string]any, len(v.Keys))
		for _, k := range v.Keys {
			out[k] = v.Members[k].Plain()
		}
		return out
	case kindArray:
		out := make([]any, len(v.Items))
		for i, item := range v.Items {
			out[i] = item.Plain()
		}
		return out
	default:
		return v.Scalar
	}
}

var arrayIndex = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// Pointer resolves an RFC 6901 JSON Pointer against the value. A pointer that
// resolves to nothing returns nil — per §3.8.2 that is the common case, not an
// error. The empty pointer addresses the whole value.
func (v *JSONValue) Pointer(ptr string) *JSONValue {
	if v == nil {
		return nil
	}
	if ptr == "" {
		return v
	}
	cur := v
	for _, raw := range strings.Split(ptr[1:], "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch {
		case cur.IsObject():
			next, ok := cur.Members[token]
			if !ok {
				return nil
			}
			cur = next
		case cur.IsArray():
			if !arrayIndex.MatchString(token) {
				return nil
			}
			var idx int
			fmt.Sscanf(token, "%d", &idx)
			if idx >= len(cur.Items) {
				return nil
			}
			cur = cur.Items[idx]
		default:
			return nil
		}
	}
	return cur
}

// StripDescriptor removes the embedded view descriptor (_view/_views) from an
// inline-transport response body, per §4.2: the transform input is the
// representation with the descriptor removed, so the same descriptor behaves
// identically on every transport.
func StripDescriptor(v *JSONValue) {
	if !v.IsObject() {
		return
	}
	kept := v.Keys[:0]
	for _, k := range v.Keys {
		if k == "_view" || k == "_views" {
			delete(v.Members, k)
			continue
		}
		kept = append(kept, k)
	}
	v.Keys = kept
}

// pointerSyntax matches a valid RFC 6901 pointer: empty, or /-led segments
// where ~ appears only as ~0 or ~1.
var pointerSyntax = regexp.MustCompile(`^(/([^/~]|~[01])*)*$`)

// ValidPointer reports whether s is syntactically a JSON Pointer (§3.8.1).
func ValidPointer(s string) bool {
	return pointerSyntax.MatchString(s)
}
