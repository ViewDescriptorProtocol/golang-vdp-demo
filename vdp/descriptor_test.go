package vdp

import (
	"encoding/json"
	"testing"
)

// §3.5: a slot value is a descriptor or an array of them, and a round trip must
// preserve which form was used.
func TestSlotValueRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		json  string
		count int
		array bool
	}{
		{"single object", `{"template":"https://e.com/a"}`, 1, false},
		{"array of one", `[{"template":"https://e.com/a"}]`, 1, true},
		{"array of three", `[{"template":"https://e.com/a"},{"template":"https://e.com/b"},{"template":"https://e.com/c"}]`, 3, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sv SlotValue
			if err := json.Unmarshal([]byte(tc.json), &sv); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(sv.Descriptors) != tc.count {
				t.Errorf("got %d descriptors, want %d", len(sv.Descriptors), tc.count)
			}
			if sv.Array != tc.array {
				t.Errorf("Array = %v, want %v", sv.Array, tc.array)
			}
			out, err := json.Marshal(sv)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(out) != tc.json {
				t.Errorf("round trip:\n got %s\nwant %s", out, tc.json)
			}
		})
	}
}

// §3.5: "minItems: 1" — an empty array is not a valid slot value.
func TestSlotValueEmptyArrayRejected(t *testing.T) {
	var sv SlotValue
	if err := json.Unmarshal([]byte(`[]`), &sv); err == nil {
		t.Fatal("empty slot array was accepted, want error")
	}
}

// §3.3: slots nest to arbitrary depth, and each level unmarshals recursively.
func TestNestedSlots(t *testing.T) {
	const doc = `{
	  "template": "https://e.com/layout",
	  "slots": {
	    "main": {
	      "template": "https://e.com/dashboard",
	      "slots": {
	        "chart": {
	          "template": "https://e.com/chart",
	          "slots": {"legend": {"template": "https://e.com/legend"}}
	        }
	      }
	    }
	  }
	}`
	views, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	vd, ok := views[DefaultView]
	if !ok {
		t.Fatalf("single-view document should be keyed %q, got %v", DefaultView, views)
	}
	legend := vd.
		Slots["main"].Descriptors[0].
		Slots["chart"].Descriptors[0].
		Slots["legend"].Descriptors[0]
	if legend.Template != "https://e.com/legend" {
		t.Errorf("depth-3 template = %q", legend.Template)
	}
}

// §3.6: a document is either a ViewDescriptor or a MultiViewDescriptor.
func TestParse(t *testing.T) {
	t.Run("multi view", func(t *testing.T) {
		views, err := Parse([]byte(`{"views":{"default":{"template":"https://e.com/a"},"compact":{"template":"https://e.com/b"}}}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(views) != 2 || views["compact"].Template != "https://e.com/b" {
			t.Errorf("got %+v", views)
		}
	})

	// §9.3: malformed descriptors must be rejected, not guessed at.
	for _, tc := range []struct{ name, doc string }{
		{"missing template", `{"slots":{"a":{"template":"https://e.com/a"}}}`},
		{"nested missing template", `{"template":"https://e.com/a","slots":{"main":{"slots":{}}}}`},
		{"both template and views", `{"template":"https://e.com/a","views":{}}`},
		{"empty views", `{"views":{}}`},
		{"not json", `{nope}`},
		{"template wrong type", `{"template":42}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.doc)); err == nil {
				t.Errorf("Parse(%s) succeeded, want error", tc.doc)
			}
		})
	}
}

// §3.2: slots is omitted, not emitted as null, when a template has none.
func TestMarshalOmitsEmptySlots(t *testing.T) {
	out, err := json.Marshal(ViewDescriptor{Template: "https://e.com/a"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != `{"template":"https://e.com/a"}` {
		t.Errorf("got %s", out)
	}
}
