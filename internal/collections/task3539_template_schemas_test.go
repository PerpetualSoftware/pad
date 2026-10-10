package collections

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// repeatsOrBlanks is an independent check (not the validator under test):
// a field key that is empty or repeated, or a select / multi_select option
// that is empty or repeated.
func repeatsOrBlanks(schema models.CollectionSchema) []string {
	var out []string
	keys := map[string]int{}
	for _, f := range schema.Fields {
		keys[f.Key]++
		if f.Key == "" || keys[f.Key] == 2 {
			out = append(out, "field key "+f.Key)
		}
		if f.Type != "select" && f.Type != "multi_select" {
			continue
		}
		opts := map[string]int{}
		for _, o := range f.Options {
			opts[o]++
			if o == "" || opts[o] == 2 {
				out = append(out, f.Key+" option "+o)
			}
		}
	}
	return out
}

// TASK-3539: template seeding writes collections through the store, not the
// HTTP handlers that refuse a repeated or empty field key or option, so every
// schema a template ships is held to the same rule here, by an independent
// check (codex round 5: calling the validator under test would be circular).
func TestTASK3539_TemplateSchemasHaveUniqueKeysAndOptions(t *testing.T) {
	all := ListAllTemplates()
	if len(all) == 0 {
		t.Fatal("premise: no templates")
	}
	n := 0
	for _, tpl := range all {
		for _, c := range tpl.Collections {
			n++
			if bad := repeatsOrBlanks(c.Schema); len(bad) > 0 {
				t.Errorf("template %q, collection %q repeats or blanks: %v", tpl.Name, c.Slug, bad)
			}
		}
	}
	for _, c := range Defaults() {
		n++
		if bad := repeatsOrBlanks(c.Schema); len(bad) > 0 {
			t.Errorf("default collection %q repeats or blanks: %v", c.Slug, bad)
		}
	}
	if n < 10 {
		t.Fatalf("premise: only %d template collections checked", n)
	}
}
