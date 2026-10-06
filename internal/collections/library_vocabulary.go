package collections

import (
	"sort"

	"github.com/PerpetualSoftware/pad/internal/artifact"
)

// LibraryOptionVocabulary returns, per select field key, the values the
// library writes into a collection of the given artifact kind when one of
// its entries is activated (BUG-3446). It is computed from the library
// itself, never from a hand list, so a new library entry brings its own
// words along.
//
// The `blank` template seeds its system collections with one trigger and
// one scope on purpose (templates_blank.go), so every triggered library
// entry used to be refused there. A write that carries one of these values
// may add it to the collection's options; any other value is still refused,
// so the options grow only by words the library itself uses.
//
// Convention activation stores `scope = surfaces[0]`
// (models.ApplyItemConventionMetadata), so every surface is a scope word.
func LibraryOptionVocabulary(kind string) map[string][]string {
	words := map[string]map[string]bool{"trigger": {}, "scope": {}}
	add := func(key, v string) {
		if v != "" {
			words[key][v] = true
		}
	}
	switch kind {
	case string(artifact.KindConvention):
		for _, cat := range ConventionLibrary() {
			for _, c := range cat.Conventions {
				add("trigger", c.Trigger)
				for _, s := range c.Surfaces {
					add("scope", s)
				}
			}
		}
	case string(artifact.KindPlaybook):
		for _, cat := range PlaybookLibrary() {
			for _, p := range cat.Playbooks {
				add("trigger", p.Trigger)
				add("scope", p.Scope)
			}
		}
	default:
		return nil
	}
	out := make(map[string][]string, len(words))
	for key, set := range words {
		vals := make([]string, 0, len(set))
		for v := range set {
			vals = append(vals, v)
		}
		sort.Strings(vals)
		out[key] = vals
	}
	return out
}
