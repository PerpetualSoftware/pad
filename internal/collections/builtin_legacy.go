package collections

import (
	"encoding/json"
	"sort"
)

// BuiltinEntries lists every built-in Pad ships, in key order (TASK-3462 U4).
// An error building the registry yields what was built; the registry's own
// test fails on that error.
func BuiltinEntries() []BuiltinEntry {
	m, _ := builtinRegistry()
	out := make([]BuiltinEntry, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// MatchLegacyBuiltin names the built-in an item made before origins were
// recorded came from, or "" (TASK-3462 U4). The rule is exact, never fuzzy:
//   - one entry of the item's kind with exactly its title;
//   - several such entries (templates can share a title): the one whose body
//     is exactly the item's, or none;
//   - no title match, for a playbook: the one entry whose invocation_slug is
//     the item's (a rename keeps the slug);
//   - a title naming one entry and a slug naming another: nothing.
//
// Nothing else is adopted: a wrong origin would offer someone else's text as
// an "update".
func MatchLegacyBuiltin(entries []BuiltinEntry, kind, title, invocationSlug, content string) string {
	byTitle := ""
	var titled []BuiltinEntry
	for _, e := range entries {
		if e.Kind == kind && e.Title == title {
			titled = append(titled, e)
		}
	}
	switch len(titled) {
	case 0:
	case 1:
		byTitle = titled[0].Key
	default:
		var byBody []BuiltinEntry
		for _, e := range titled {
			if e.Content == content {
				byBody = append(byBody, e)
			}
		}
		if len(byBody) != 1 {
			return ""
		}
		byTitle = byBody[0].Key
	}

	bySlug := ""
	if kind == BuiltinPlaybook && invocationSlug != "" {
		var slugged []BuiltinEntry
		for _, e := range entries {
			if e.Kind != BuiltinPlaybook {
				continue
			}
			var f struct {
				InvocationSlug string `json:"invocation_slug"`
			}
			if json.Unmarshal([]byte(e.Fields), &f) == nil && f.InvocationSlug == invocationSlug {
				slugged = append(slugged, e)
			}
		}
		if len(slugged) == 1 {
			bySlug = slugged[0].Key
		}
	}

	switch {
	case byTitle != "" && bySlug != "" && byTitle != bySlug:
		// The title names one built-in and the slug another (codex r1):
		// either could be the rename, so neither is adopted.
		return ""
	case byTitle != "":
		return byTitle
	default:
		return bySlug
	}
}
