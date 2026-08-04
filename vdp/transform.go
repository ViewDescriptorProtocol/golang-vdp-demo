package vdp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Transform is a parsed §3.8 transform: a declarative mapping that adapts the
// API response representation into the JSON model a node's template expects.
// Parsing validates the grammar (§3.8.1); a parsed Transform always evaluates
// without error — missing pointers and wrong-type targets yield nil, which is
// the spec's null-not-error semantics (§3.8.2, §9.6).
type Transform struct {
	kind transformKind

	pointer string   // kindPointer
	keys    []string // kindMapping: output member order
	members map[string]*Transform
	list    []*Transform // kindList
	mapPtr  string       // kindProjection: $map
	to      *Transform   // kindProjection / kindEntries: $to (nil = bare entries)
	entries string       // kindEntries: $entries
	get     string       // kindDefaulted: $get
	deflt   any          // kindDefaulted: $default (plain Go value)
	count   string       // kindCount: $count
	merge   []*Transform // kindMerge
	mapper  string       // kindMapper: $mapper URI
}

type transformKind int

const (
	kindPointer transformKind = iota
	kindMapping
	kindList
	kindProjection
	kindEntries
	kindDefaulted
	kindCount
	kindMerge
	kindMapper
)

// MapperURI returns the $mapper identifier when the transform is a mapper
// reference (§3.8.3), and "" otherwise.
func (t *Transform) MapperURI() string {
	if t == nil || t.kind != kindMapper {
		return ""
	}
	return t.mapper
}

// ParseTransform reads and validates a transform (§3.8.1). The topLevel form
// additionally admits a MapperRef, which is valid only as the entire
// transform value.
func ParseTransform(raw json.RawMessage) (*Transform, error) {
	v, err := ParseJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("transform: %w", err)
	}
	return parseTransformValue(v, true)
}

func parseTransformValue(v *JSONValue, topLevel bool) (*Transform, error) {
	switch {
	case v.IsObject():
		return parseTransformObject(v, topLevel)
	case v.IsArray():
		list := make([]*Transform, len(v.Items))
		for i, item := range v.Items {
			node, err := parseTransformValue(item, false)
			if err != nil {
				return nil, err
			}
			list[i] = node
		}
		return &Transform{kind: kindList, list: list}, nil
	default:
		s, ok := v.Scalar.(string)
		if !ok {
			return nil, fmt.Errorf("transform: %v is not a pointer, mapping, list or construct", v.Scalar)
		}
		if !ValidPointer(s) {
			return nil, fmt.Errorf("transform: %q is not a valid JSON Pointer", s)
		}
		return &Transform{kind: kindPointer, pointer: s}, nil
	}
}

func parseTransformObject(v *JSONValue, topLevel bool) (*Transform, error) {
	// Constructs are discriminated on key presence (§3.8.1).
	has := func(k string) bool { _, ok := v.Members[k]; return ok }

	pointerAt := func(key string) (string, error) {
		m := v.Members[key]
		s, ok := m.Scalar.(string)
		if m.kind != kindScalar || !ok {
			return "", fmt.Errorf("transform: %s must be a JSON Pointer string", key)
		}
		if !ValidPointer(s) {
			return "", fmt.Errorf("transform: %s %q is not a valid JSON Pointer", key, s)
		}
		return s, nil
	}
	// rejectExtra ensures a construct object carries only its own members
	// (x-* vendor extensions excepted, §3.10).
	rejectExtra := func(allowed ...string) error {
		ok := map[string]bool{}
		for _, a := range allowed {
			ok[a] = true
		}
		for _, k := range v.Keys {
			if !ok[k] && !strings.HasPrefix(k, "x-") {
				return fmt.Errorf("transform: unrecognized member %q in %s construct", k, allowed[0])
			}
		}
		return nil
	}

	switch {
	case has("$mapper"):
		if !topLevel {
			return nil, fmt.Errorf("transform: $mapper is valid only as the entire transform value (§3.8.3)")
		}
		if err := rejectExtra("$mapper"); err != nil {
			return nil, err
		}
		s, ok := v.Members["$mapper"].Scalar.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("transform: $mapper must be a non-empty URI string")
		}
		return &Transform{kind: kindMapper, mapper: s}, nil

	case has("$map"):
		if err := rejectExtra("$map", "$to"); err != nil {
			return nil, err
		}
		if !has("$to") {
			return nil, fmt.Errorf("transform: $map requires $to")
		}
		ptr, err := pointerAt("$map")
		if err != nil {
			return nil, err
		}
		to, err := parseTransformValue(v.Members["$to"], false)
		if err != nil {
			return nil, err
		}
		return &Transform{kind: kindProjection, mapPtr: ptr, to: to}, nil

	case has("$entries"):
		if err := rejectExtra("$entries", "$to"); err != nil {
			return nil, err
		}
		ptr, err := pointerAt("$entries")
		if err != nil {
			return nil, err
		}
		t := &Transform{kind: kindEntries, entries: ptr}
		if has("$to") {
			to, err := parseTransformValue(v.Members["$to"], false)
			if err != nil {
				return nil, err
			}
			t.to = to
		}
		return t, nil

	case has("$get"):
		if err := rejectExtra("$get", "$default"); err != nil {
			return nil, err
		}
		if !has("$default") {
			return nil, fmt.Errorf("transform: $get requires $default")
		}
		ptr, err := pointerAt("$get")
		if err != nil {
			return nil, err
		}
		return &Transform{kind: kindDefaulted, get: ptr, deflt: v.Members["$default"].Plain()}, nil

	case has("$count"):
		if err := rejectExtra("$count"); err != nil {
			return nil, err
		}
		ptr, err := pointerAt("$count")
		if err != nil {
			return nil, err
		}
		return &Transform{kind: kindCount, count: ptr}, nil

	case has("$merge"):
		if err := rejectExtra("$merge"); err != nil {
			return nil, err
		}
		ops := v.Members["$merge"]
		if !ops.IsArray() || len(ops.Items) == 0 {
			return nil, fmt.Errorf("transform: $merge must be a non-empty array")
		}
		merge := make([]*Transform, len(ops.Items))
		for i, op := range ops.Items {
			node, err := parseTransformValue(op, false)
			if err != nil {
				return nil, err
			}
			merge[i] = node
		}
		return &Transform{kind: kindMerge, merge: merge}, nil

	default:
		// A Mapping: keys must not begin with "$" (§3.8.1) — anything
		// $-prefixed here is an unrecognized construct (§9.3).
		t := &Transform{kind: kindMapping, members: map[string]*Transform{}}
		for _, k := range v.Keys {
			if strings.HasPrefix(k, "$") {
				return nil, fmt.Errorf("transform: unrecognized construct %q (§9.3)", k)
			}
			node, err := parseTransformValue(v.Members[k], false)
			if err != nil {
				return nil, fmt.Errorf("member %q: %w", k, err)
			}
			t.keys = append(t.keys, k)
			t.members[k] = node
		}
		return t, nil
	}
}

// Eval evaluates the transform against the original response representation
// (§3.8.2) and returns the template model as plain Go values. Evaluation never
// fails: missing pointers and wrong-type targets yield nil. A mapper-reference
// transform is not evaluated here — the resolver dispatches it to registered
// mapper code (§3.8.3) or treats it as a slot failure.
func (t *Transform) Eval(input *JSONValue) any {
	switch t.kind {
	case kindPointer:
		return input.Pointer(t.pointer).Plain()
	case kindMapping:
		out := make(map[string]any, len(t.keys))
		for _, k := range t.keys {
			out[k] = t.members[k].Eval(input)
		}
		return out
	case kindList:
		out := make([]any, len(t.list))
		for i, node := range t.list {
			out[i] = node.Eval(input)
		}
		return out
	case kindProjection:
		arr := input.Pointer(t.mapPtr)
		if !arr.IsArray() {
			return nil // §3.8.2: non-array target, not an error
		}
		out := make([]any, len(arr.Items))
		for i, el := range arr.Items {
			out[i] = t.to.Eval(el) // $to pointers are element-relative
		}
		return out
	case kindEntries:
		obj := input.Pointer(t.entries)
		if !obj.IsObject() {
			return nil
		}
		out := make([]any, 0, len(obj.Keys))
		for _, k := range obj.Keys { // §3.8.2: document order
			pair := &JSONValue{
				kind:    kindObject,
				Keys:    []string{"key", "value"},
				Members: map[string]*JSONValue{"key": {kind: kindScalar, Scalar: k}, "value": obj.Members[k]},
			}
			if t.to != nil {
				out = append(out, t.to.Eval(pair))
			} else {
				out = append(out, pair.Plain())
			}
		}
		return out
	case kindDefaulted:
		v := input.Pointer(t.get)
		// Explicit null and absent member are indistinguishable (§3.8.2):
		// both take the default.
		if v == nil || (v.kind == kindScalar && v.Scalar == nil) {
			return t.deflt
		}
		return v.Plain()
	case kindCount:
		v := input.Pointer(t.count)
		switch {
		case v.IsArray():
			return float64(len(v.Items)) // float64: the type JSON numbers decode to
		case v.IsObject():
			return float64(len(v.Keys))
		default:
			return nil
		}
	case kindMerge:
		out := map[string]any{}
		for _, op := range t.merge {
			if m, ok := op.Eval(input).(map[string]any); ok {
				for k, val := range m { // last operand wins (§3.8.2)
					out[k] = val
				}
			}
		}
		return out
	case kindMapper:
		return nil // dispatched by the resolver, never evaluated inline
	}
	return nil
}

// MarshalJSON writes the transform back in its grammar form, so descriptors
// built in Go round-trip (the demo server emits transforms it constructs from
// parsed JSON; see server/api.go).
func (t *Transform) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.toJSON())
}

func (t *Transform) toJSON() any {
	switch t.kind {
	case kindPointer:
		return t.pointer
	case kindMapping:
		// Order is not significant when emitting (order matters for parsing
		// $entries input, not descriptor JSON), so a plain map is fine.
		out := make(map[string]any, len(t.keys))
		for _, k := range t.keys {
			out[k] = t.members[k].toJSON()
		}
		return out
	case kindList:
		out := make([]any, len(t.list))
		for i, node := range t.list {
			out[i] = node.toJSON()
		}
		return out
	case kindProjection:
		return map[string]any{"$map": t.mapPtr, "$to": t.to.toJSON()}
	case kindEntries:
		out := map[string]any{"$entries": t.entries}
		if t.to != nil {
			out["$to"] = t.to.toJSON()
		}
		return out
	case kindDefaulted:
		return map[string]any{"$get": t.get, "$default": t.deflt}
	case kindCount:
		return map[string]any{"$count": t.count}
	case kindMerge:
		out := make([]any, len(t.merge))
		for i, op := range t.merge {
			out[i] = op.toJSON()
		}
		return map[string]any{"$merge": out}
	case kindMapper:
		return map[string]any{"$mapper": t.mapper}
	}
	return nil
}

// MustTransform parses a transform literal or panics; a convenience for demo
// server code that builds descriptors from constant JSON.
func MustTransform(src string) *Transform {
	t, err := ParseTransform(json.RawMessage(src))
	if err != nil {
		panic(err)
	}
	return t
}
