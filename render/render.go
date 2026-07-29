// Package render binds a resolved VDP template tree to Go's html/template.
//
// VDP is template-engine agnostic: it declares which templates render a
// response and how they compose, and stops there. Everything in this package —
// how a slot is spelled, how data reaches a template — is the engine's concern,
// not the protocol's (spec §1, §6, Design Decision 3).
//
// The only requirement VDP places on a template language is named insertion
// points that can be filled externally (§6). The spec's §6.1 table maps that
// onto Qute, HTML <slot>, Thymeleaf, SwiftUI and others; for html/template the
// natural spelling is a function:
//
//	<main>{{slot "mainContent"}}</main>
//
// An unfilled slot renders as empty HTML, which lets a template supply its own
// default content for a slot the descriptor never filled, or one whose template
// failed to resolve (§9.1, §9.2):
//
//	{{with slot "revenueChart"}}{{.}}{{else}}<p>Chart unavailable</p>{{end}}
package render

import (
	"fmt"
	"html/template"
	"strconv"
	"strings"

	"github.com/ViewDescriptorProtocol/golang-vdp-demo/vdp"
)

// MaxRenderDepth guards against a template tree that somehow nests deeper than
// the resolver allowed.
const MaxRenderDepth = vdp.DefaultMaxDepth

// Render executes a resolved template tree against data and returns the
// composed HTML (§8 step 6).
//
// Every node in the tree sees the same data. VDP does not describe which fields
// feed which template — templates extract what they need using the engine's own
// expressions (Design Decision 3).
func Render(node *vdp.Node, data any) (template.HTML, error) {
	return renderNode(node, data, 0)
}

func renderNode(node *vdp.Node, data any, depth int) (template.HTML, error) {
	if node == nil {
		return "", fmt.Errorf("nil node")
	}
	if depth >= MaxRenderDepth {
		return "", fmt.Errorf("maximum render depth (%d) exceeded at %s", MaxRenderDepth, node.ID)
	}

	funcs := template.FuncMap{
		// slot renders the sub-templates filling a named insertion point.
		//
		// Sub-template output is spliced in as template.HTML rather than being
		// escaped: templates come only from the trusted allowlist (§10), and
		// their output is already contextually escaped by their own execution.
		// Data reaching a template is still escaped normally.
		"slot": func(name string) (template.HTML, error) {
			children := node.Slots[name]
			if len(children) == 0 {
				// Either the descriptor never filled this slot, or the slot was
				// skipped after a fetch failure (§9.1). Both render as the
				// template's own default content.
				return "", nil
			}
			var b strings.Builder
			for _, child := range children { // §3.5: arrays render in order.
				html, err := renderNode(child, data, depth+1)
				if err != nil {
					// §9.4: one failing sub-template must not sink the tree.
					continue
				}
				b.WriteString(string(html))
			}
			return template.HTML(b.String()), nil
		},
		// hasSlot lets a template branch on whether a slot was filled (§9.2).
		"hasSlot": func(name string) bool {
			return len(node.Slots[name]) > 0
		},
	}
	for name, fn := range helpers {
		funcs[name] = fn
	}

	t, err := template.New(node.ID).Funcs(funcs).Parse(node.Body)
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", node.ID, err)
	}
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template %s: %w", node.ID, err)
	}
	return template.HTML(buf.String()), nil
}

// helpers are ordinary template functions the demo templates use for
// formatting. They are an html/template convenience and have nothing to do with
// VDP — Qute, Thymeleaf or JSX would each bring their own equivalents.
var helpers = template.FuncMap{
	// pct returns value as a percentage of total, for CSS sizing.
	"pct": func(value, total any) int {
		v, okV := toFloat(value)
		t, okT := toFloat(total)
		if !okV || !okT || t == 0 {
			return 0
		}
		return min(max(int(v/t*100), 0), 100)
	},
	// maxOf returns the largest number in a slice decoded from JSON.
	"maxOf": func(values any) float64 {
		items, ok := values.([]any)
		if !ok {
			return 0
		}
		var out float64
		for _, item := range items {
			if v, ok := toFloat(item); ok && v > out {
				out = v
			}
		}
		return out
	},
	// money formats a number as US dollars: to the cent when there are cents,
	// whole otherwise ($9.99, but $48,200 rather than $48,200.00).
	"money": func(value any) string {
		v, ok := toFloat(value)
		if !ok {
			return ""
		}
		sign, whole, frac := splitNumber(strconv.FormatFloat(v, 'f', 2, 64))
		out := sign + "$" + groupDigits(whole)
		if frac != "00" {
			out += "." + frac
		}
		return out
	},
	// number formats a number with thousands separators.
	"number": func(value any) string {
		v, ok := toFloat(value)
		if !ok {
			return ""
		}
		sign, whole, frac := splitNumber(strconv.FormatFloat(v, 'f', -1, 64))
		out := sign + groupDigits(whole)
		if frac != "" {
			out += "." + frac
		}
		return out
	},
	// title upper-cases the first letter of a string.
	"title": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64: // encoding/json decodes all numbers into float64.
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// splitNumber breaks a formatted decimal into its sign, integer digits and
// fractional digits ("-48200.75" -> "-", "48200", "75").
func splitNumber(s string) (sign, whole, frac string) {
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	whole, frac, _ = strings.Cut(s, ".")
	return sign, whole, frac
}

// groupDigits inserts thousands separators into a run of decimal digits.
func groupDigits(digits string) string {
	var b strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}
