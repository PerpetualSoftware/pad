package collections

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3462: every convention and playbook Pad ships has a key, one key
// names one text, and a key is a path an item can record and an API can
// carry.
func TestBuiltinRegistry(t *testing.T) {
	m, err := builtinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	keyShape := regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*(?:/[a-z0-9]+(?:-[a-z0-9]+)*){1,2}$`)
	for k, e := range m {
		if !keyShape.MatchString(k) {
			t.Errorf("key %q is not a lowercase path", k)
		}
		if e.Kind != BuiltinConvention && e.Kind != BuiltinPlaybook {
			t.Errorf("%s: kind %q", k, e.Kind)
		}
		if !strings.Contains(k, e.Kind+"/") {
			t.Errorf("%s: a %s key names its kind", k, e.Kind)
		}
		if len(e.Hash()) != 64 {
			t.Errorf("%s: hash %q", k, e.Hash())
		}
	}
	// The libraries and every template's seeds are all present.
	for _, cat := range ConventionLibrary() {
		for _, c := range cat.Conventions {
			if _, ok := m[c.Key]; !ok {
				t.Errorf("library convention %q is missing", c.Title)
			}
		}
	}
	for _, tpl := range ListAllTemplates() {
		for _, p := range tpl.Playbooks {
			if _, ok := m[p.Key]; !ok {
				t.Errorf("template %s playbook %q is missing", tpl.Name, p.Title)
			}
		}
	}
	for _, k := range []string{"playbook/ship", "playbook/plan", "playbook/decompose", "playbook/onboard", "spec/playbook/spec"} {
		if _, ok := LookupBuiltin(k); !ok {
			t.Errorf("%s is not registered", k)
		}
	}
}

// A seed built from a library entry stores what activating that entry
// stores, so the two doors give the same item the same origin hash
// (TASK-3462, the seed/activate inconsistency folded into U1).
func TestLibrarySeedMatchesActivation(t *testing.T) {
	c := ConventionLibrary()[0].Conventions[0]
	seed := seedConventionFromLibrary(c)
	if seed.Fields != LibraryConventionFields(c) || seed.Key != c.Key {
		t.Fatalf("seed %+v does not match activation fields %s", seed, LibraryConventionFields(c))
	}
	if !strings.Contains(seed.Fields, `"convention":`) || !strings.Contains(seed.Fields, `"commands":`) {
		t.Fatalf("seeded convention lacks the typed metadata activation writes: %s", seed.Fields)
	}
}

// The hash moves with the text and the fields an update writes, and not
// with status, which an update never touches.
func TestBuiltinHashTracksTextNotStatus(t *testing.T) {
	e, _ := LookupBuiltin("playbook/ship")
	h := e.Hash()
	edited := e
	edited.Content += "\n"
	if edited.Hash() == h {
		t.Error("a body change did not move the hash")
	}
	slug := e
	slug.Fields = strings.Replace(e.Fields, `"ship"`, `"ship2"`, 1)
	if slug.Hash() == h {
		t.Error("a field change did not move the hash")
	}
	deprecated := e
	deprecated.Fields = strings.Replace(e.Fields, `"status":"active"`, `"status":"deprecated"`, 1)
	if deprecated.Fields == e.Fields {
		t.Fatal("fixture: status not found in fields")
	}
	if deprecated.Hash() != h {
		t.Error("a status change moved the hash")
	}
}

// An item carrying an entry's text hashes to the entry's hash whatever else
// it holds, and stops matching when the text or an update field changes.
func TestItemStateHash(t *testing.T) {
	e, _ := LookupBuiltin("playbook/ship")
	h, err := e.ItemStateHash(e.Content, e.Fields)
	if err != nil || h != e.Hash() {
		t.Fatalf("an item with the entry's own text: %s %v, want %s", h, err, e.Hash())
	}
	extra := strings.Replace(e.Fields, `"status":"active"`, `"status":"draft","role":"x","notes":null`, 1)
	if h, _ := e.ItemStateHash(e.Content, extra); h != e.Hash() {
		t.Error("status, a user's own key, or a null moved the item's hash")
	}
	if h, _ := e.ItemStateHash(e.Content+"x", e.Fields); h == e.Hash() {
		t.Error("an edited body still matched")
	}
	if h, _ := e.ItemStateHash(e.Content, strings.Replace(e.Fields, `"manual"`, `"on-release"`, 1)); h == e.Hash() {
		t.Error("an edited trigger still matched")
	}
}

// Every state, from the one comparison that decides it.
func TestBuiltinStateOf(t *testing.T) {
	e, _ := LookupBuiltin("playbook/ship")
	lib := e.Hash()
	old := BuiltinEntry{Key: e.Key, Content: e.Content + "\nold", Fields: e.Fields}
	oldHash := old.Hash()
	cases := []struct {
		name    string
		o       models.BuiltinOrigin
		content string
		want    string
	}{
		{"seeded at the current text", models.BuiltinOrigin{Key: e.Key, SeedHash: lib}, e.Content, BuiltinCurrent},
		{"edited, library unchanged", models.BuiltinOrigin{Key: e.Key, SeedHash: lib}, "mine", BuiltinCurrent},
		{"unedited, library changed", models.BuiltinOrigin{Key: e.Key, SeedHash: oldHash}, old.Content, BuiltinUpdateAvailable},
		{"edited, library changed", models.BuiltinOrigin{Key: e.Key, SeedHash: oldHash}, "mine", BuiltinDiverged},
		{"already holds the new text", models.BuiltinOrigin{Key: e.Key, SeedHash: oldHash}, e.Content, BuiltinCurrent},
		{"unknown version, differs", models.BuiltinOrigin{Key: e.Key}, "mine", BuiltinUnknownOrigin},
		{"unknown version, matches", models.BuiltinOrigin{Key: e.Key}, e.Content, BuiltinCurrent},
		{"unknown entry", models.BuiltinOrigin{Key: "acme/playbook/deploy", SeedHash: lib}, e.Content, BuiltinUnknownEntry},
	}
	for _, c := range cases {
		got, _, _, err := BuiltinStateOf(c.o, c.content, e.Fields)
		if err != nil || got != c.want {
			t.Errorf("%s: %s %v, want %s", c.name, got, err, c.want)
		}
	}
}

// A library that ADDS a field is a library change: an item that still holds
// what it was given reads update_available, not diverged (TASK-3462 U2: the item was hashed over the library's keys and compared with
// a hash over the seed's).
func TestBuiltinStateOfAcrossAChangedKeySet(t *testing.T) {
	e, _ := LookupBuiltin("playbook/ship")
	var fields map[string]any
	if err := json.Unmarshal([]byte(e.Fields), &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "arguments") // the seed predates the library's arguments
	fields["legacy_note"] = "x" // and had a field the library dropped
	fb, _ := json.Marshal(fields)
	seed := BuiltinEntry{Key: e.Key, Content: "old body", Fields: string(fb)}
	o := models.BuiltinOrigin{Key: e.Key, SeedHash: seed.Hash(), SeedContent: seed.Content, SeedFields: seed.Fields}
	if got, _, _, err := BuiltinStateOf(o, seed.Content, seed.Fields); err != nil || got != BuiltinUpdateAvailable {
		t.Fatalf("an unedited item: %s %v, want update_available", got, err)
	}
	if got, _, _, _ := BuiltinStateOf(o, "my body", seed.Fields); got != BuiltinDiverged {
		t.Fatalf("an edited item: %s, want diverged", got)
	}
}
