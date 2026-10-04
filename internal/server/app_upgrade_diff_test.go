package server

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/appmanifest"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3397 (U8b2): the upgrade diff's classes and the additive-field rule.

func diffBase() *appmanifest.Manifest {
	return &appmanifest.Manifest{
		ID: "acme/portal", Version: "1.0.0", Title: "Portal", Publisher: "Acme", Origin: "https://portal.example",
		RedirectURIs: []string{"https://portal.example/cb"},
		Scopes:       appmanifest.Scopes{Service: appmanifest.Access{Access: "write"}, Delegated: appmanifest.Access{Access: "read"}},
		Events:       []appmanifest.Event{{Name: "item.created", Collections: []string{"tickets"}}},
		ItemActions:  []appmanifest.ItemAction{{Key: "open", Label: "Open", Collections: []string{"tickets"}, Path: "/t"}},
		CompanionPack: appmanifest.CompanionPack{
			Collections: []appmanifest.Collection{{Key: "tickets", Slug: "portal-tickets", Name: "Tickets",
				Parsed: models.CollectionSchema{Fields: []models.FieldDef{{Key: "status", Type: "select", Options: []string{"open", "solved"}}}}}},
			Artifacts: []appmanifest.Artifact{{Key: "ship"}},
		},
	}
}

func clone(m *appmanifest.Manifest) *appmanifest.Manifest {
	c := *m
	c.Events = append([]appmanifest.Event(nil), m.Events...)
	c.ItemActions = append([]appmanifest.ItemAction(nil), m.ItemActions...)
	c.RedirectURIs = append([]string(nil), m.RedirectURIs...)
	c.CompanionPack.Collections = nil
	for _, col := range m.CompanionPack.Collections {
		col.Parsed.Fields = append([]models.FieldDef(nil), col.Parsed.Fields...)
		c.CompanionPack.Collections = append(c.CompanionPack.Collections, col)
	}
	c.CompanionPack.Artifacts = append([]appmanifest.Artifact(nil), m.CompanionPack.Artifacts...)
	return &c
}

var diffDigests = map[string]storedArtifactDigest{"ship": {Raw: "r1", Normalized: "n1"}}

func diffFresh(raw, norm string) *appPreview {
	return &appPreview{Artifacts: []appPreviewArtifact{{Key: "ship", RawSHA256: raw, NormalizedSHA256: norm}}}
}

func entryOf(t *testing.T, d *upgradeDiff, kind, key string) upgradeDiffEntry {
	t.Helper()
	for _, e := range d.Entries {
		if e.Kind == kind && e.Key == key {
			return e
		}
	}
	t.Fatalf("no %s %q entry in %+v", kind, key, d.Entries)
	return upgradeDiffEntry{}
}

func TestUpgradeDiff_Classes(t *testing.T) {
	old := diffBase()

	same, err := diffUpgrade(old, clone(old), diffDigests, diffFresh("r1", "n1"))
	if err != nil || len(same.Entries) != 0 || same.ReviewRequired {
		t.Fatalf("an identical manifest diffed: %+v %v", same, err)
	}

	// Removals and narrowing only: auto, no review.
	n := clone(old)
	n.Events = nil
	n.ItemActions = nil
	n.Scopes.Service.Access = "read"
	n.CompanionPack.Collections = nil
	d, err := diffUpgrade(old, n, diffDigests, diffFresh("r1", "n1"))
	if err != nil {
		t.Fatal(err)
	}
	if d.ReviewRequired {
		t.Errorf("removals and narrowing required review: %+v", d.Entries)
	}
	if e := entryOf(t, d, "collection", "tickets"); e.Change != "released" || !strings.Contains(e.Detail, "data kept") {
		t.Errorf("removed companion: %+v", e)
	}
	if len(d.Released) != 1 || d.Released[0] != "portal-tickets" {
		t.Errorf("released %v", d.Released)
	}
	if e := entryOf(t, d, "access", "service"); e.Change != "narrowed" || e.Class != upgradeAuto {
		t.Errorf("narrowing: %+v", e)
	}

	// Each of these alone needs review.
	for name, mut := range map[string]func(m *appmanifest.Manifest){
		"widen service":   func(m *appmanifest.Manifest) { m.Scopes.Service.Access = "write"; old.Scopes.Service.Access = "read" },
		"widen delegated": func(m *appmanifest.Manifest) { m.Scopes.Delegated.Access = "write" },
		"add event":       func(m *appmanifest.Manifest) { m.Events = append(m.Events, appmanifest.Event{Name: "comment.created"}) },
		"widen event":     func(m *appmanifest.Manifest) { m.Events[0].Collections = []string{"tickets", "other"} },
		"change action":   func(m *appmanifest.Manifest) { m.ItemActions[0].Path = "/elsewhere" },
		"redirect uris":   func(m *appmanifest.Manifest) { m.RedirectURIs = []string{"https://portal.example/cb2"} },
		"webhook url":     func(m *appmanifest.Manifest) { m.WebhookURL = "https://portal.example/hooks2" },
		"prose":           func(m *appmanifest.Manifest) { m.Description = "now with more" },
		"add collection": func(m *appmanifest.Manifest) {
			m.CompanionPack.Collections = append(m.CompanionPack.Collections, appmanifest.Collection{Key: "faq", Slug: "portal-faq"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := diffBase()
			old = base
			n := clone(base)
			mut(n)
			d, err := diffUpgrade(old, n, diffDigests, diffFresh("r1", "n1"))
			if err != nil {
				t.Fatal(err)
			}
			if !d.ReviewRequired {
				t.Fatalf("%s did not require review: %+v", name, d.Entries)
			}
		})
	}

	// Artifacts: a raw digest change is a new draft needing review; a
	// removed artifact keeps its item.
	old = diffBase()
	d, err = diffUpgrade(old, clone(old), diffDigests, diffFresh("r2", "n2"))
	if err != nil {
		t.Fatal(err)
	}
	if !d.ChangedArtifacts["ship"] || !d.ReviewRequired {
		t.Errorf("a raw digest change: %+v", d)
	}
	// A normalized-only change is the artifact's own installed item moving
	// its normalization (invocation_slug "ship" to "ship-2"), not the app
	// changing it: no draft, no entry.
	d, err = diffUpgrade(old, clone(old), diffDigests, diffFresh("r1", "n2"))
	if err != nil {
		t.Fatal(err)
	}
	if d.ChangedArtifacts["ship"] || len(d.Entries) != 0 {
		t.Errorf("a normalized-only change was treated as an app change: %+v", d)
	}
	n = clone(old)
	n.CompanionPack.Artifacts = nil
	d, err = diffUpgrade(old, n, diffDigests, &appPreview{})
	if err != nil || d.ReviewRequired || entryOf(t, d, "artifact", "ship").Change != "removed" {
		t.Errorf("removed artifact: %+v %v", d, err)
	}
}

func TestUpgradeDiff_Refusals(t *testing.T) {
	old := diffBase()
	for name, mut := range map[string]func(m *appmanifest.Manifest){
		"app id": func(m *appmanifest.Manifest) { m.ID = "acme/other" },
		"origin": func(m *appmanifest.Manifest) { m.Origin = "https://evil.example" },
		"slug":   func(m *appmanifest.Manifest) { m.CompanionPack.Collections[0].Slug = "portal-tix" },
		"field changed": func(m *appmanifest.Manifest) {
			m.CompanionPack.Collections[0].Parsed.Fields[0].Options = []string{"open"}
		},
		"field removed": func(m *appmanifest.Manifest) { m.CompanionPack.Collections[0].Parsed.Fields = nil },
		"required":      addField(models.FieldDef{Key: "x", Type: "text", Required: true}),
		"default":       addField(models.FieldDef{Key: "x", Type: "text", Default: "d"}),
		"computed":      addField(models.FieldDef{Key: "x", Type: "text", Computed: true}),
		"terminal":      addField(models.FieldDef{Key: "x", Type: "select", Options: []string{"a", "b"}, TerminalOptions: []string{"b"}}),
		"abandoned":     addField(models.FieldDef{Key: "x", Type: "select", Options: []string{"a", "b"}, AbandonedOptions: []string{"b"}}),
	} {
		t.Run(name, func(t *testing.T) {
			n := clone(old)
			mut(n)
			if _, err := diffUpgrade(old, n, diffDigests, diffFresh("r1", "n1")); err == nil {
				t.Fatalf("%s was not refused", name)
			}
		})
	}
	// Allowed additive optional fields.
	for name, f := range map[string]models.FieldDef{
		"text":         {Key: "note", Type: "text"},
		"unique":       {Key: "code", Type: "text", UniqueScope: "workspace_collection"},
		"relation":     {Key: "owner", Type: "relation", Collection: "people"},
		"multi":        {Key: "owners", Type: "multi_relation", Collection: "people"},
		"pattern":      {Key: "sku", Type: "text", Pattern: "^[A-Z]+$"},
		"plain select": {Key: "tier", Type: "select", Options: []string{"a", "b"}},
	} {
		t.Run("allows "+name, func(t *testing.T) {
			n := clone(old)
			addField(f)(n)
			d, err := diffUpgrade(old, n, diffDigests, diffFresh("r1", "n1"))
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if len(d.SchemaAdds) != 1 || d.SchemaAdds[0].Field.Key != f.Key || !d.ReviewRequired {
				t.Fatalf("schema adds %+v", d.SchemaAdds)
			}
		})
	}
}

func addField(f models.FieldDef) func(m *appmanifest.Manifest) {
	return func(m *appmanifest.Manifest) {
		m.CompanionPack.Collections[0].Parsed.Fields = append(m.CompanionPack.Collections[0].Parsed.Fields, f)
	}
}
