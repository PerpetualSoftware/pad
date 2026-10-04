package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/appmanifest"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// The upgrade diff (SPEC-6 U8b2, DOC-3371 §2 "Upgrade"; TASK-3397).
//
// Classes (the spec, plus the lead's rulings of day 86):
//   - "auto": applies without review: removing an event, an item action, an
//     artifact, an access level (narrowing), or a companion collection (which
//     is RELEASED to the workspace with its data, not deleted);
//   - "review": anything else that changed: an addition, a widening, any URL,
//     any artifact raw or normalized digest, prose, and an additive optional
//     field on an existing companion. Prose is never classed as narrowing.
//
// Every upgrade, removals-only included, is applied only by the owner's
// confirm (ruling 3); "auto" means there is no diff to read, not no consent.
// Refused outright: a changed app id or origin, a changed companion slug, and
// any companion schema change other than additive optional fields (ruling 2).

const (
	upgradeAuto   = "auto"
	upgradeReview = "review"
)

type upgradeDiffEntry struct {
	Kind   string `json:"kind"`   // app, access, url, event, item_action, collection, field, artifact, config
	Key    string `json:"key"`    // what changed: an event name, a key, a field "coll.key"
	Change string `json:"change"` // added, removed, changed, widened, narrowed, released
	Class  string `json:"class"`  // auto or review
	Detail string `json:"detail,omitempty"`
}

// upgradeSchemaAdd is one additive optional field for an existing companion.
type upgradeSchemaAdd struct {
	CollectionKey  string
	CollectionSlug string
	Field          models.FieldDef
}

// upgradeDiff is the full comparison.
type upgradeDiff struct {
	Entries          []upgradeDiffEntry
	SchemaAdds       []upgradeSchemaAdd
	Released         []string        // companion collection slugs released to the workspace
	ChangedArtifacts map[string]bool // artifact keys that land as new drafts
	ReviewRequired   bool
}

func (d *upgradeDiff) add(kind, key, change, class, detail string) {
	d.Entries = append(d.Entries, upgradeDiffEntry{Kind: kind, Key: key, Change: change, Class: class, Detail: detail})
	if class == upgradeReview {
		d.ReviewRequired = true
	}
}

// storedArtifactDigest is one artifact's digests as the install recorded them.
type storedArtifactDigest struct {
	Raw        string `json:"raw"`
	Normalized string `json:"normalized"`
}

func refuseUpgrade(path, format string, args ...any) *appInstallError {
	e := installErr(http.StatusUnprocessableEntity, "upgrade_not_supported", format, args...)
	e.path = path
	return e
}

func accessRank(a string) int {
	switch a {
	case "write":
		return 2
	case "read":
		return 1
	}
	return 0
}

// diffUpgrade compares the installed manifest with the new one, using the
// fresh preview for the new artifacts' digests.
func diffUpgrade(old, next *appmanifest.Manifest, oldDigests map[string]storedArtifactDigest, fresh *appPreview) (*upgradeDiff, error) {
	d := &upgradeDiff{ChangedArtifacts: map[string]bool{}}
	if old.ID != next.ID {
		return nil, refuseUpgrade("id", "the app id changed (%q to %q); an upgrade is the same app", old.ID, next.ID)
	}
	if old.Origin != next.Origin {
		return nil, refuseUpgrade("base_url", "the app origin changed (%q to %q); the origin is the app's identity", old.Origin, next.Origin)
	}

	// Prose and URLs: always review when changed.
	prose := []struct{ key, a, b string }{
		{"title", old.Title, next.Title}, {"description", old.Description, next.Description},
		{"publisher", old.Publisher, next.Publisher},
	}
	for _, p := range prose {
		if p.a != p.b {
			d.add("app", p.key, "changed", upgradeReview, fmt.Sprintf("%q to %q", p.a, p.b))
		}
	}
	urls := []struct{ key, a, b string }{
		{"homepage", old.Homepage, next.Homepage}, {"webhook_url", old.WebhookURL, next.WebhookURL}, {"docs", old.Docs, next.Docs},
	}
	for _, u := range urls {
		if u.a != u.b {
			d.add("url", u.key, "changed", upgradeReview, fmt.Sprintf("%q to %q", u.a, u.b))
		}
	}
	if !sameStringSet(old.RedirectURIs, next.RedirectURIs) {
		d.add("url", "redirect_uris", "changed", upgradeReview, fmt.Sprintf("%v to %v", old.RedirectURIs, next.RedirectURIs))
	}
	if !reflect.DeepEqual(old.MinContract, next.MinContract) {
		d.add("app", "min_contract", "changed", upgradeReview, "")
	}
	if !jsonEqual(old.ConfigSchema, next.ConfigSchema) {
		d.add("config", "config_schema", "changed", upgradeReview, "")
	}

	// Access levels: widening needs review, narrowing applies.
	for _, a := range []struct{ key, from, to string }{
		{"service", old.Scopes.Service.Access, next.Scopes.Service.Access},
		{"delegated", old.Scopes.Delegated.Access, next.Scopes.Delegated.Access},
	} {
		switch {
		case accessRank(a.to) > accessRank(a.from):
			detail := fmt.Sprintf("%q to %q", a.from, a.to)
			if a.key == "delegated" {
				detail += "; recorded, and granted nothing until delegated access is enabled (TASK-3399)"
			}
			d.add("access", a.key, "widened", upgradeReview, detail)
		case accessRank(a.to) < accessRank(a.from):
			d.add("access", a.key, "narrowed", upgradeAuto, fmt.Sprintf("%q to %q", a.from, a.to))
		}
	}

	// Events, by name.
	// A name may be declared more than once (the manifest allows it): its
	// collections are the union of every declaration, or a widening could hide
	// behind a duplicate (codex r1 on U8b2).
	unionEvents := func(evs []appmanifest.Event) map[string]appmanifest.Event {
		out := map[string]appmanifest.Event{}
		for _, e := range evs {
			u := out[e.Name]
			u.Name = e.Name
			for _, c := range e.Collections {
				if !subsetOf([]string{c}, u.Collections) {
					u.Collections = append(u.Collections, c)
				}
			}
			out[e.Name] = u
		}
		return out
	}
	oldEv, newEv := unionEvents(old.Events), unionEvents(next.Events)
	for _, name := range upgradeSortedKeys(oldEv) {
		if _, ok := newEv[name]; !ok {
			d.add("event", name, "removed", upgradeAuto, "")
		}
	}
	for _, name := range upgradeSortedKeys(newEv) {
		o, ok := oldEv[name]
		switch {
		case !ok:
			d.add("event", name, "added", upgradeReview, "")
		case !sameStringSet(o.Collections, newEv[name].Collections):
			if subsetOf(newEv[name].Collections, o.Collections) {
				d.add("event", name, "narrowed", upgradeAuto, "")
			} else {
				d.add("event", name, "widened", upgradeReview, "")
			}
		}
	}

	// Item actions, by key.
	oldAct, newAct := map[string]appmanifest.ItemAction{}, map[string]appmanifest.ItemAction{}
	for _, a := range old.ItemActions {
		oldAct[a.Key] = a
	}
	for _, a := range next.ItemActions {
		newAct[a.Key] = a
	}
	for _, k := range upgradeSortedKeys(oldAct) {
		if _, ok := newAct[k]; !ok {
			d.add("item_action", k, "removed", upgradeAuto, "")
		}
	}
	for _, k := range upgradeSortedKeys(newAct) {
		o, ok := oldAct[k]
		switch {
		case !ok:
			d.add("item_action", k, "added", upgradeReview, "")
		case !reflect.DeepEqual(o, newAct[k]):
			d.add("item_action", k, "changed", upgradeReview, "")
		}
	}

	// Companion collections, by key.
	oldColl, newColl := map[string]appmanifest.Collection{}, map[string]appmanifest.Collection{}
	for _, c := range old.CompanionPack.Collections {
		oldColl[c.Key] = c
	}
	for i, c := range next.CompanionPack.Collections {
		newColl[c.Key] = c
		o, ok := oldColl[c.Key]
		if !ok {
			d.add("collection", c.Key, "added", upgradeReview, fmt.Sprintf("a new companion collection %q", c.Slug))
			continue
		}
		path := fmt.Sprintf("companion_pack.collections[%d]", i)
		if o.Slug != c.Slug {
			return nil, refuseUpgrade(path+".slug", "companion collection %q changed its slug (%q to %q); the slug is how the app addresses it", c.Key, o.Slug, c.Slug)
		}
		if o.Name != c.Name {
			d.add("collection", c.Key, "changed", upgradeReview, fmt.Sprintf("name %q to %q", o.Name, c.Name))
		}
		adds, err := additiveFields(o.Parsed, c.Parsed, path+".schema", c.Key)
		if err != nil {
			return nil, err
		}
		for _, f := range adds {
			d.SchemaAdds = append(d.SchemaAdds, upgradeSchemaAdd{CollectionKey: c.Key, CollectionSlug: c.Slug, Field: f})
			d.add("field", c.Key+"."+f.Key, "added", upgradeReview, fmt.Sprintf("an optional %s field %q", f.Type, f.Key))
		}
	}
	for _, k := range upgradeSortedKeys(oldColl) {
		if _, ok := newColl[k]; !ok {
			d.Released = append(d.Released, oldColl[k].Slug)
			d.add("collection", k, "released", upgradeAuto, fmt.Sprintf("%q is released to the workspace (data kept); the app can no longer read or write it", oldColl[k].Slug))
		}
	}

	// Artifacts, by key.
	newArt := map[string]appPreviewArtifact{}
	for _, a := range fresh.Artifacts {
		newArt[a.Key] = a
	}
	oldArt := map[string]bool{}
	for _, a := range old.CompanionPack.Artifacts {
		oldArt[a.Key] = true
	}
	for _, k := range upgradeSortedKeys(oldArt) {
		if _, ok := newArt[k]; !ok {
			d.add("artifact", k, "removed", upgradeAuto, "the installed item is kept")
		}
	}
	for _, a := range fresh.Artifacts {
		was, had := oldDigests[a.Key]
		switch {
		case !had || !oldArt[a.Key]:
			d.ChangedArtifacts[a.Key] = true
			d.add("artifact", a.Key, "added", upgradeReview, "lands as a new draft")
		case was.Raw != a.RawSHA256:
			// Changed is decided by the RAW digest. The normalized digest of
			// an unchanged artifact moves on every upgrade (its fresh
			// normalization de-collides against the very item the install
			// created, e.g. invocation_slug "ship" becomes "ship-2"), and an
			// unchanged artifact stores nothing, so there is nothing of it to
			// review. A changed one lands as a new draft whose normalized
			// form is the reviewed one (TASK-3397 U8b2, flagged to the lead).
			d.ChangedArtifacts[a.Key] = true
			d.add("artifact", a.Key, "changed", upgradeReview, "lands as a new draft; the installed item is not changed")
		}
	}
	return d, nil
}

// additiveFields returns the fields next adds to old, refusing every other
// schema change (lead ruling 2, day 86). An added field must be optional
// with no default, not computed, and declare no terminal or abandoned
// options: each of those would change existing items, which hold no value
// for it (a backfill, or a reclassification of their done state).
func additiveFields(old, next models.CollectionSchema, path, collKey string) ([]models.FieldDef, error) {
	oldByKey := map[string]models.FieldDef{}
	for _, f := range old.Fields {
		oldByKey[f.Key] = f
	}
	newByKey := map[string]bool{}
	var adds []models.FieldDef
	for _, f := range next.Fields {
		newByKey[f.Key] = true
		o, ok := oldByKey[f.Key]
		if ok {
			if !reflect.DeepEqual(o, f) {
				return nil, refuseUpgrade(path, "companion collection %q changes its existing field %q; v1 upgrades may only ADD optional fields (reinstall to change a field)", collKey, f.Key)
			}
			continue
		}
		switch {
		case f.Required:
			return nil, refuseUpgrade(path, "companion collection %q adds a required field %q; existing items have no value for it", collKey, f.Key)
		case f.Default != nil:
			return nil, refuseUpgrade(path, "companion collection %q adds field %q with a default; existing items would need a backfill", collKey, f.Key)
		case f.Computed:
			return nil, refuseUpgrade(path, "companion collection %q adds a computed field %q; existing items would need a backfill", collKey, f.Key)
		case len(f.TerminalOptions) > 0 || len(f.AbandonedOptions) > 0:
			return nil, refuseUpgrade(path, "companion collection %q adds field %q with terminal or abandoned options, which would reclassify existing items", collKey, f.Key)
		}
		adds = append(adds, f)
	}
	for _, f := range old.Fields {
		if !newByKey[f.Key] {
			return nil, refuseUpgrade(path, "companion collection %q removes its field %q; v1 upgrades may only ADD optional fields", collKey, f.Key)
		}
	}
	// Anything in the schema besides fields must be unchanged.
	oc, nc := old, next
	oc.Fields, nc.Fields = nil, nil
	if !reflect.DeepEqual(oc, nc) {
		return nil, refuseUpgrade(path, "companion collection %q changes its schema beyond adding optional fields", collKey)
	}
	return adds, nil
}

func upgradeSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameStringSet(a, b []string) bool {
	return subsetOf(a, b) && subsetOf(b, a)
}

func subsetOf(a, b []string) bool {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	for _, x := range a {
		if !in[x] {
			return false
		}
	}
	return true
}

func jsonEqual(a, b json.RawMessage) bool {
	// Absent and null are the same declaration: the stored manifest is
	// re-marshalled from the struct, which writes an unset RawMessage as
	// null, while a freshly parsed manifest leaves it empty.
	isNone := func(r json.RawMessage) bool {
		t := strings.TrimSpace(string(r))
		return t == "" || t == "null"
	}
	if isNone(a) && isNone(b) {
		return true
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return string(a) == string(b)
	}
	return reflect.DeepEqual(x, y)
}
