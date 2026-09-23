package items

import (
	"sort"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// UndeclaredOverrideKeys returns the keys in an override map that the target
// schema does not declare, sorted so a caller's error message is stable for a
// given input. A nil or empty override map returns nil.
//
// It lives HERE, in the package both consumers already import, because two
// implementations of it is exactly the divergence PLAN-2357's DR-6 exists to
// prevent (Codex round 17). The cross-workspace copy PREFLIGHT refuses an
// undeclared override with a 400, and the COPY refuses it in
// Store.migrateCopyFields; if the two ever disagreed about which keys count as
// undeclared, the preview would promise something the copy refuses, or worse
// permit something the copy silently persists as an orphan key.
//
// The rule is a straight key-set difference with no exemptions. There is no
// reserved-key escape hatch in items.fields — PLAN-2357 DR-2 records why the
// cross-workspace provenance pointer needed its own table rather than a fields
// key — so every key the schema does not declare is an orphan.
//
// Why refusing is the right answer rather than dropping the key silently:
// ValidateFields ignores keys the schema does not declare, so an undeclared
// override that is merged is WRITTEN, invisible to every schema-driven
// surface. Dropping it instead would be no better — a client that asked for a
// value would get an item without it and no way to tell.
func UndeclaredOverrideKeys(overrides map[string]any, targetFields []models.FieldDef) []string {
	if len(overrides) == 0 {
		return nil
	}
	declared := make(map[string]struct{}, len(targetFields))
	for _, f := range targetFields {
		declared[f.Key] = struct{}{}
	}
	var bad []string
	for k := range overrides {
		if _, ok := declared[k]; !ok {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	return bad
}

// ReservedFieldKeysIn returns the keys of a caller-supplied field map that name
// system metadata, sorted.
//
// Two kinds of map consult it, for the same reason:
//
//   - FIELD-OVERRIDE maps, on every path that has one: the same-workspace move,
//     the copy preflight, and the mutating copy. Those paths refuse every key
//     this returns, and all three also run UndeclaredOverrideKeys against a
//     stripped schema (the move since BUG-2379).
//   - `fields_patch`, the partial-update door every USER field-setter lowers
//     into — `pad item update --field`, the MCP `field` param on both
//     transports, and anything else PATCHing an item (BUG-2627 part 2). It
//     refuses every key this returns since BUG-2696 (PatchRefusedFieldKeysIn
//     keeps the record of github_pr's former exemption).
//   - item CREATE's `fields` (BUG-3163). Convention activation, the one
//     system writer that used that door, sends the typed ItemCreate.Convention
//     member instead.
//
// It was named ReservedOverrideKeys until the second caller arrived; the list
// and the semantics are unchanged.
//
// A FULL `fields` blob on update is the one door that may still name these
// keys, and only to CARRY the stored value unchanged: a blob that changes one,
// or omits a stored one (which would delete it), is refused in the update
// transaction against the locked row (BUG-3163; see the server's
// composeReservedCarryGuard). Pad's own writers no longer send a full blob:
// note/decide use typed append members, `pad github link` a typed member, and
// convention activation ItemCreate.Convention.
//
// What the fields_patch gate buys is that the UPDATE door — the one a user or
// an agent actually reaches for, on all three transports — can no longer write
// implementation_notes or decision_log at all. That matters because the append
// helpers (AppendImplementationNote / AppendDecisionLogEntry) refuse rather
// than destroy an undecodable stored value (BUG-2627 part 3): a `--field`
// write that lands a malformed blob does not merely look wrong, it disables
// the legitimate append path for that item until someone repairs the row.
//
// On the copy it is worse than a validation hole: MigrateFields drops a
// referential key like github_pr when the item leaves its workspace (BUG-2674),
// and an override applied afterwards would put it straight back, defeating the
// scope rule by the simplest possible route.
//
// Callers that already run UndeclaredOverrideKeys against a schema stripped by
// SchemaForMigratedFields get this for free — a reserved key is undeclared
// there by construction. This exists for the paths with no schema gate at all.
func ReservedFieldKeysIn(fields map[string]any) []string {
	if len(fields) == 0 {
		return nil
	}
	var bad []string
	for k := range fields {
		if models.IsReservedItemField(k) {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	return bad
}

// PatchRefusedFieldKeysIn returns the keys of a `fields_patch` that the UPDATE
// door refuses (BUG-2627 part 2), sorted. It is ReservedFieldKeysIn minus the
// keys whose only cross-surface writer IS this door.
//
// THE RULE, because the list below is only a snapshot of it: refuse a raw write
// WHERE A REAL WRITER EXISTS. When a key is added to reserved metadata, ask
// whether every audience that can reach this door has another way in — the CLI,
// remote MCP, and stdio MCP each separately. If one does not, exempting it is
// not a softening, it is the rule; refusing would hand that audience a message
// naming a command they cannot run, which is the disease this bug family is
// about rather than a cure for it. Do not pattern-match a new key onto the
// exemption list; evaluate it against the predicate (lead ruling, 2026-08-20).
//
// Today that exemption is `github_pr`, and here is the working of it. The other three reserved keys have a real writer on
// every surface that can reach them: implementation_notes and decision_log have
// `note` / `decide` (CLI and both MCP transports), and `convention` has library
// activation (likewise). `github_pr` does not. `pad github link` needs the
// agent's local git branch and the `gh` CLI, so it is excluded from the remote
// MCP surface by name — and internal/mcp/dispatch_http.go's noRemoteEquivalent
// map tells remote agents, in so many words, to use
// `item update --field github_pr=...` instead. That makes this door the
// SANCTIONED writer for that key, not a bypass of one.
//
// Refusing it would therefore have removed a documented capability with nothing
// to replace it, and — worse — answered with a message naming `pad github link`,
// a command those same agents cannot run. That is the circular-remedy failure
// this refusal exists to avoid, aimed at ourselves (Codex round 3).
//
// Note what is NOT claimed: a raw `github_pr` write is still unvalidated, and
// the move/copy OVERRIDE paths still refuse it (BUG-2674) because there the
// argument is different — an override re-introduces a key MigrateFields
// deliberately dropped. Whether remote agents should get a real PR-link action
// so this key can be closed too is a product question, not a gate question.
//
// SUPERSEDED FOR github_pr BY BUG-2696, and the reasoning above is kept as the
// record of why the exemption existed. The premise was that this door WORKED
// as github_pr's cross-surface writer; it never did — a `--field` value is
// stored as a string on every transport, so the PR data landed double-encoded,
// no link rendered, and `=null` stored the string "null". An exemption for a
// door that only writes unreadable data protects nothing. github_pr now has a
// typed, validated member (models.ItemUpdate.GitHubPR / ClearGitHubPR) that
// `pad github link` / `unlink` / `project reconcile` use, so this door refuses
// all four reserved keys. Remote agents lose nothing that worked; a structured
// remote PR-link action is deferred to the MCP catalog-trim decision.
func PatchRefusedFieldKeysIn(fields map[string]any) []string {
	return ReservedFieldKeysIn(fields)
}
