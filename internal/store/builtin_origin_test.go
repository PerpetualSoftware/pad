package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3462: an imported origin is stored only when it is well formed AND its
// seed text is the text its hash names; anything else imports the item
// without an origin.
func TestValidImportedBuiltinOrigin(t *testing.T) {
	e, ok := collections.LookupBuiltin("playbook/ship")
	if !ok {
		t.Fatal("playbook/ship not registered")
	}
	good := models.BuiltinOrigin{Key: e.Key, SeedHash: e.Hash(), SeedContent: e.Content, SeedFields: e.Fields}
	if err := validImportedBuiltinOrigin(good); err != nil {
		t.Fatalf("a true origin was refused: %v", err)
	}
	for name, o := range map[string]models.BuiltinOrigin{
		"key only":         {Key: "playbook/ship"},
		"key and hash":     {Key: "playbook/ship", SeedHash: e.Hash()},
		"unknown but sane": {Key: "acme/playbook/deploy", SeedHash: strings.Repeat("a", 64)},
	} {
		if err := validImportedBuiltinOrigin(o); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	for name, o := range map[string]models.BuiltinOrigin{
		"empty key":            {},
		"uppercase key":        {Key: "Playbook/Ship"},
		"one-segment key":      {Key: "ship"},
		"short hash":           {Key: "playbook/ship", SeedHash: "abc"},
		"text without hash":    {Key: "playbook/ship", SeedContent: e.Content},
		"fields not an object": {Key: "playbook/ship", SeedHash: e.Hash(), SeedContent: e.Content, SeedFields: "[]"},
		"text not its hash":    {Key: "playbook/ship", SeedHash: e.Hash(), SeedContent: e.Content + "!", SeedFields: e.Fields},
		"oversized text":       {Key: "playbook/ship", SeedHash: e.Hash(), SeedContent: strings.Repeat("x", models.MaxBuiltinSeedBytes+1)},
		// A NUL would be refused by the database and poison the import's
		// transaction, so it is refused here and the origin is skipped
		// (codex r1): raw in the body, raw in the fields, escaped in the fields.
		"raw NUL in content":    {Key: "playbook/ship", SeedHash: e.Hash(), SeedContent: "a\x00b", SeedFields: e.Fields},
		"raw NUL in fields":     {Key: "playbook/ship", SeedHash: e.Hash(), SeedContent: e.Content, SeedFields: `{"trigger":"a` + "\x00" + `"}`},
		"escaped NUL in fields": {Key: "playbook/ship", SeedHash: e.Hash(), SeedContent: e.Content, SeedFields: `{"trigger":"a\u0000b"}`},
		"escaped NUL in a key":  {Key: "playbook/ship", SeedHash: e.Hash(), SeedContent: e.Content, SeedFields: `{"a\u0000":1}`},
	} {
		if err := validImportedBuiltinOrigin(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
