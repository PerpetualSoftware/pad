package collections

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3539: template seeding writes collections through the store, not the
// HTTP handlers that refuse a repeated or empty field key or option, so every
// schema a template ships is held to the same rule here.
func TestTASK3539_TemplateSchemasHaveUniqueKeysAndOptions(t *testing.T) {
	all := ListAllTemplates()
	if len(all) == 0 {
		t.Fatal("premise: no templates")
	}
	n := 0
	for _, tpl := range all {
		for _, c := range tpl.Collections {
			n++
			if err := models.ValidateSchemaKeysAndOptions(c.Schema); err != nil {
				t.Errorf("template %q, collection %q: %v", tpl.Name, c.Slug, err)
			}
		}
	}
	for _, c := range Defaults() {
		n++
		if err := models.ValidateSchemaKeysAndOptions(c.Schema); err != nil {
			t.Errorf("default collection %q: %v", c.Slug, err)
		}
	}
	if n < 10 {
		t.Fatalf("premise: only %d template collections checked", n)
	}
}
