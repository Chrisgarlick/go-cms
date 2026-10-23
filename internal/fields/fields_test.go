package fields

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	min := 1.0
	defs := []Field{
		{Name: "heading", Type: Text, Required: true, MaxLength: 10},
		{Name: "count", Type: Number, Min: &min},
		{Name: "style", Type: Select, Options: []Option{{Value: "light"}, {Value: "dark"}}, Default: "light"},
		{Name: "image", Type: Image},
		{Name: "cta", Type: Link},
		{Name: "items", Type: Repeater, Fields: []Field{{Name: "q", Type: Text, Required: true}}},
		{Name: "raw", Type: HTML},
		{Name: "dark_note", Type: Text, Condition: &Condition{Field: "style", Equals: "dark"}},
	}
	if err := Check(defs, ""); err != nil {
		t.Fatal(err)
	}
	in := `{"heading":" Hi ","count":"3","image":"0190f1b6-7a0b-7c00-8000-000000000001",
		"cta":{"url":"javascript:x","label":"go"},"items":[{"q":""}],"raw":"<b>x</b>","dark_note":"x","junk":1}`
	_, _, err := Validate(defs, json.RawMessage(in), Context{CanUse: func(string) bool { return false }})
	errs, ok := err.(Errors)
	if !ok {
		t.Fatalf("expected Errors, got %v", err)
	}
	if errs["cta"] == "" || errs["items.0.q"] == "" {
		t.Fatalf("missing errors: %v", errs)
	}
	in = `{"heading":" Hi ","count":3,"image":"0190f1b6-7a0b-7c00-8000-000000000001","items":[{"q":"a"}],"raw":"<b>x</b>","dark_note":"x","junk":1}`
	out, refs, err := Validate(defs, json.RawMessage(in), Context{CanUse: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, bad := range []string{"junk", "raw", "dark_note"} {
		if strings.Contains(s, bad) {
			t.Errorf("output kept %q: %s", bad, s)
		}
	}
	if !strings.Contains(s, `"heading":"Hi"`) || !strings.Contains(s, `"style":"light"`) {
		t.Errorf("trim/default not applied: %s", s)
	}
	if len(refs.Media) != 1 {
		t.Errorf("media ref not collected: %v", refs.Media)
	}
	// Partial (autosave) skips required checks.
	if _, _, err := Validate(defs, json.RawMessage(`{}`), Context{Partial: true}); err != nil {
		t.Fatalf("partial: %v", err)
	}
	if _, _, err := Validate(defs, json.RawMessage(`{}`), Context{}); err == nil {
		t.Fatal("required not enforced")
	}
}

func TestCheckRejects(t *testing.T) {
	bad := [][]Field{
		{{Name: "Title"}},
		{{Name: "a", Type: "nope"}},
		{{Name: "a", Type: Select}},
		{{Name: "a"}, {Name: "a"}},
		{{Name: "slug"}},
		{{Name: "a", Pattern: "("}},
	}
	for i, b := range bad {
		if err := Check(b, ""); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}
