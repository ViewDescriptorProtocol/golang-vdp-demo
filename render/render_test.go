package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ViewDescriptorProtocol/golang-vdp-demo/vdp"
)

// jsonData decodes like a real API response would, so tests see the float64s
// encoding/json actually produces.
func jsonData(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// §6: a slot is a named insertion point filled from outside the template.
func TestRenderSlots(t *testing.T) {
	tree := &vdp.Node{
		ID:  "https://e.com/layout",
		Body: `<main>{{slot "content"}}</main>`,
		Slots: map[string][]*vdp.Node{
			"content": {{ID: "https://e.com/card", Body: `<p>{{.name}}</p>`}},
		},
	}
	got, err := Render(tree, jsonData(t, `{"name":"Widget"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "<main><p>Widget</p></main>" {
		t.Errorf("got %q", got)
	}
}

// §3.5: array slots render in sequence.
func TestRenderSlotArrayInOrder(t *testing.T) {
	tree := &vdp.Node{
		ID:  "https://e.com/layout",
		Body: `<main>{{slot "content"}}</main>`,
		Slots: map[string][]*vdp.Node{
			"content": {
				{ID: "https://e.com/a", Body: `<a>`},
				{ID: "https://e.com/b", Body: `<b>`},
				{ID: "https://e.com/c", Body: `<c>`},
			},
		},
	}
	got, err := Render(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "<main><a><b><c></main>" {
		t.Errorf("got %q, want sequence preserved", got)
	}
}

// §9.1/§9.2: an unfilled slot — whether never declared or skipped after a fetch
// failure — renders the template's own default content, not an error.
func TestRenderUnfilledSlotUsesDefault(t *testing.T) {
	tree := &vdp.Node{
		ID:  "https://e.com/layout",
		Body: `<main>{{with slot "chart"}}{{.}}{{else}}<p>no chart</p>{{end}}</main>`,
	}
	got, err := Render(tree, nil)
	if err != nil {
		t.Fatalf("an unfilled slot must not fail the render: %v", err)
	}
	if string(got) != "<main><p>no chart</p></main>" {
		t.Errorf("got %q", got)
	}
}

// §9.2: a descriptor may name a slot the template has no insertion point for.
// The assignment is ignored rather than being an error.
func TestRenderIgnoresUnmatchedSlot(t *testing.T) {
	tree := &vdp.Node{
		ID:  "https://e.com/layout",
		Body: `<main>only static content</main>`,
		Slots: map[string][]*vdp.Node{
			"nonexistent": {{ID: "https://e.com/x", Body: `<x>`}},
		},
	}
	got, err := Render(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "<x>") {
		t.Errorf("unmatched slot leaked into output: %q", got)
	}
}

// §9.4: a sub-template that fails at render time must not sink its parent.
func TestRenderBrokenChildDoesNotSinkTree(t *testing.T) {
	tree := &vdp.Node{
		ID:  "https://e.com/layout",
		Body: `<main>{{slot "a"}}{{slot "b"}}</main>`,
		Slots: map[string][]*vdp.Node{
			"a": {{ID: "https://e.com/broken", Body: `{{.missing.deeper}}`}},
			"b": {{ID: "https://e.com/ok", Body: `<ok>`}},
		},
	}
	got, err := Render(tree, jsonData(t, `{}`))
	if err != nil {
		t.Fatalf("parent render should survive a broken child: %v", err)
	}
	if !strings.Contains(string(got), "<ok>") {
		t.Errorf("healthy sibling lost: %q", got)
	}
}

// Data reaching a template is escaped; sub-template output is spliced in as
// markup. Templates are trusted (§10), the data they render is not.
func TestRenderEscapesDataNotTemplates(t *testing.T) {
	tree := &vdp.Node{
		ID:  "https://e.com/layout",
		Body: `<main>{{slot "content"}}</main>`,
		Slots: map[string][]*vdp.Node{
			"content": {{ID: "https://e.com/card", Body: `<p>{{.name}}</p>`}},
		},
	}
	got, err := Render(tree, jsonData(t, `{"name":"<script>alert(1)</script>"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "<script>") {
		t.Errorf("data was not escaped: %q", got)
	}
	if !strings.Contains(string(got), "<p>") {
		t.Errorf("template markup was escaped, it should not be: %q", got)
	}
}

// §8: rendering is bounded even if a cyclic tree somehow reaches it.
func TestRenderDepthLimit(t *testing.T) {
	node := &vdp.Node{ID: "https://e.com/loop", Body: `<i>{{slot "next"}}`}
	node.Slots = map[string][]*vdp.Node{"next": {node}} // Self-referential.

	got, err := Render(node, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// The cycle is cut at MaxRenderDepth rather than recursing forever.
	if n := strings.Count(string(got), "<i>"); n != MaxRenderDepth {
		t.Errorf("rendered %d levels, want %d", n, MaxRenderDepth)
	}
}

func TestHelpers(t *testing.T) {
	money := helpers["money"].(func(any) string)
	number := helpers["number"].(func(any) string)
	pct := helpers["pct"].(func(any, any) int)

	// Prices must survive to the cent — truncating 9.99 to $9 would be worse
	// than useless — while whole amounts stay free of noisy ".00".
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{9.99, "$9.99"},
		{29.99, "$29.99"},
		{48200, "$48,200"},
		{1234567.5, "$1,234,567.50"},
		{-1847.25, "-$1,847.25"},
		{0, "$0"},
		{0.5, "$0.50"},
		{9.999, "$10"}, // Rounds to the cent, then reads as whole.
	} {
		if got := money(tc.in); got != tc.want {
			t.Errorf("money(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}

	for _, tc := range []struct {
		in   float64
		want string
	}{
		{1847, "1,847"},
		{312, "312"},
		{1000, "1,000"},
		{-14200, "-14,200"},
		{1234.5, "1,234.5"},
	} {
		if got := number(tc.in); got != tc.want {
			t.Errorf("number(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// pct clamps, and never divides by zero.
	for _, tc := range []struct {
		v, total float64
		want     int
	}{
		{50, 100, 50},
		{14200, 14200, 100},
		{0, 100, 0},
		{150, 100, 100}, // Clamped.
		{-5, 100, 0},    // Clamped.
		{5, 0, 0},       // No division by zero.
	} {
		if got := pct(tc.v, tc.total); got != tc.want {
			t.Errorf("pct(%v, %v) = %d, want %d", tc.v, tc.total, got, tc.want)
		}
	}

	// Helpers receive whatever JSON decoding produced, so a non-number must not
	// panic the render.
	if got := money("not a number"); got != "" {
		t.Errorf("money(string) = %q, want empty", got)
	}
	if got := pct("a", "b"); got != 0 {
		t.Errorf("pct(strings) = %d, want 0", got)
	}
}

func TestMaxOf(t *testing.T) {
	maxOf := helpers["maxOf"].(func(any) float64)
	if got := maxOf(jsonData(t, `[12400, 9800, 14200, 6100]`)); got != 14200 {
		t.Errorf("maxOf = %v, want 14200", got)
	}
	if got := maxOf(jsonData(t, `[]`)); got != 0 {
		t.Errorf("maxOf(empty) = %v, want 0", got)
	}
	if got := maxOf("not a slice"); got != 0 {
		t.Errorf("maxOf(string) = %v, want 0", got)
	}
}
