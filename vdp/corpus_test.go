package vdp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The VDP test corpus (vendored under testdata/corpus, canonical in the VDP
// repository's tests/ directory) is the cross-implementation contract: this
// evaluator must produce byte-for-byte the same models as the JS reference
// runner. See testdata/corpus/README.md for the sync rule.

func readJSONFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// plainJSON decodes through encoding/json so expected values carry the same
// types (float64, map[string]any) as evaluator output.
func plainJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func TestCorpusTransforms(t *testing.T) {
	cases, err := os.ReadDir("testdata/corpus/transforms")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name(), func(t *testing.T) {
			dir := filepath.Join("testdata/corpus/transforms", c.Name())
			input, err := ParseJSON(readJSONFile(t, filepath.Join(dir, "input.json")))
			if err != nil {
				t.Fatal(err)
			}
			expected := plainJSON(t, readJSONFile(t, filepath.Join(dir, "expected.json")))

			var actual any
			trPath := filepath.Join(dir, "transform.json")
			if _, err := os.Stat(trPath); os.IsNotExist(err) {
				actual = input.Plain() // absent transform = identity (§3.8.2)
			} else {
				tr, err := ParseTransform(readJSONFile(t, trPath))
				if err != nil {
					t.Fatalf("parse transform: %v", err)
				}
				actual = tr.Eval(input)
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Errorf("mismatch\n expected: %#v\n actual:   %#v", expected, actual)
			}
		})
	}
}

func TestCorpusRendering(t *testing.T) {
	cases, err := os.ReadDir("testdata/corpus/rendering")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name(), func(t *testing.T) {
			dir := filepath.Join("testdata/corpus/rendering", c.Name())
			response, err := ParseJSON(readJSONFile(t, filepath.Join(dir, "response.json")))
			if err != nil {
				t.Fatal(err)
			}
			var descriptor ViewDescriptor
			if err := json.Unmarshal(readJSONFile(t, filepath.Join(dir, "descriptor.json")), &descriptor); err != nil {
				t.Fatal(err)
			}
			expected := plainJSON(t, readJSONFile(t, filepath.Join(dir, "expected.json")))

			StripDescriptor(response) // §4.2: _view/_views removed from the input

			model := func(tr *Transform) any {
				if tr == nil {
					return response.Plain()
				}
				return tr.Eval(response)
			}
			actual := map[string]any{"root": model(descriptor.Transform)}
			if len(descriptor.Slots) > 0 {
				slots := map[string]any{}
				for name, sv := range descriptor.Slots {
					// Corpus rendering fixtures use single-descriptor slots.
					slots[name] = model(sv.Descriptors[0].Transform)
				}
				actual["slots"] = slots
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Errorf("mismatch\n expected: %#v\n actual:   %#v", expected, actual)
			}
		})
	}
}

func TestCorpusDescriptors(t *testing.T) {
	run := func(t *testing.T, dir string, wantValid bool) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			t.Run(e.Name(), func(t *testing.T) {
				b := readJSONFile(t, filepath.Join(dir, e.Name()))
				_, err := Parse(b)
				if wantValid && err != nil {
					t.Errorf("expected accept, got: %v", err)
				}
				if !wantValid && err == nil {
					t.Errorf("expected reject, got accept")
				}
			})
		}
	}
	t.Run("valid", func(t *testing.T) { run(t, "testdata/corpus/descriptors/valid", true) })
	t.Run("invalid", func(t *testing.T) { run(t, "testdata/corpus/descriptors/invalid", false) })
}
