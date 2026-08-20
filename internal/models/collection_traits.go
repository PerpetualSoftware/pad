package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Collection traits — SPEC-5 §Collection traits (approved v1.1, PLAN-2656
// phase 0 / TASK-2657).
//
// A trait is a DECLARATION that a collection carries kernel behavior. Before
// traits, three kernel behaviors were keyed on the literal collection slugs
// "conventions" and "playbooks": what the agent bootstrap loads, which items
// route by invocation slug, and which items export as portable artifacts. A
// slug is not a stable identifier — Store.UpdateCollection re-slugs on any
// name change — so those behaviors silently detached from a renamed
// collection ([[BUG-2702]]). A trait travels with the collection.
//
// WHY TRAITS ARE THEIR OWN COLUMN and not a key inside the schema JSON:
// the schema column is overwritten wholesale on update, and every client
// rebuilds it fields-only (EditCollectionModal.svelte, the webmcp dispatcher,
// QuickActionsMenu). A traits key stored there is destroyed by any ordinary
// collection edit — measured, not assumed, during TASK-2657. Trait authority
// cannot rest on a value that an unrelated UI save deletes.
//
// The kernel trait set is CLOSED and versioned (SPEC-0: adding one is a kernel
// change). Packs carry kernel traits; they cannot invent them. Open-vocabulary
// LABEL traits (`publisher/name`) are a separate SPEC-5 tier with zero kernel
// semantics and are not implemented in phase 0.

// BootstrapIncludeMode enumerates how a bootstrap_include declaration
// projects its items. Closed set — see SPEC-5 §Collection traits.
const (
	// BootstrapModeBodies ships full item content. Expensive per item; the
	// L4 boot budget is the host-imposed limit.
	BootstrapModeBodies = "bodies"
	// BootstrapModeMetadata ships item metadata with content omitted.
	BootstrapModeMetadata = "metadata"
)

// First-party bootstrap payload keys. These name the three boot surfaces that
// existed before traits and that agents already consume, so each has a FIXED
// projection shape in the server — bodies for the always-on rules, metadata for
// the index and for playbooks.
//
// They live here, in the grammar, rather than only in the server, because
// validation has to know them: a declaration is free to feed any key, but
// feeding one of THESE with the wrong mode is a contradiction the projection
// cannot honour. Declaring `{mode: metadata, key: conventions}` would still
// emit bodies, because the conventions projection always does. Rather than
// letting a declaration mean something different from what it says, that
// combination is refused. Codex round 7.
const (
	BootstrapKeyConventions     = "conventions"
	BootstrapKeyConventionIndex = "convention_index"
	BootstrapKeyPlaybooks       = "playbooks"
)

// firstPartyKeyModes pins each first-party key to the only mode its projection
// implements. A key absent from this map is an ordinary generic payload and may
// declare either mode.
var firstPartyKeyModes = map[string]string{
	BootstrapKeyConventions:     BootstrapModeBodies,
	BootstrapKeyConventionIndex: BootstrapModeMetadata,
	BootstrapKeyPlaybooks:       BootstrapModeMetadata,
}

// InvocationSlugField is the ONLY field name invocation_field may reference
// in v1 (SPEC-5 v1.1 amendment 4).
//
// The trait selects WHICH collections route by invocation; it does not yet
// rename the field. Uniqueness has two guards and only one is generic: the
// application pre-check (checkUniqueFields on FieldDef.UniqueScope) is
// field-name-agnostic, but the actual race guard is a partial unique index on
// this literal name in both dialects (migrations/054, pgmigrations/033). A
// trait naming any other field would drop out of index coverage and degrade to
// the TOCTOU pre-check that 054's own comment calls insufficient. Phase 1 may
// widen this — deliberately, with the index question answered first.
const InvocationSlugField = "invocation_slug"

// BootstrapInclude is one bootstrap_include declaration: a set of this
// collection's items that surfaces in the agent bootstrap payload.
//
// A collection may declare SEVERAL. The conventions collection declares two —
// full bodies of the always-on rules, plus a body-less index of every active
// rule so triggered conventions are discoverable without flooding the payload.
// SPEC-5 v1.0 allowed only one declaration and could not express that at all.
type BootstrapInclude struct {
	// Mode is bodies or metadata.
	Mode string `json:"mode"`
	// Filter selects which items participate, as a field-equality map.
	//
	// v1 is equality-only, NOT the SPEC-2 query/1 where-fragment the v1.0
	// spec named: query/1 is phase 1 and unbuilt, and PLAN-2656 forbids
	// growing phase 0 toward it. An equality map is a strict subset of any
	// future where-fragment, so query/1 is a widening path rather than a
	// rewrite — and it is already the shape ItemListParams.Fields consumes.
	//
	// An empty or nil filter means every item in the collection. That is
	// deliberate for playbooks, which list draft and deprecated entries so
	// an agent can see a half-written playbook exists.
	Filter map[string]string `json:"filter,omitempty"`
	// Key names the bootstrap payload this declaration feeds.
	Key string `json:"key"`
}

// ArtifactKindTrait declares the portable artifact kind items of this
// collection export as. Import maps the kind back to whichever local
// collection declares it; an unknown kind imports as a plain item.
type ArtifactKindTrait struct {
	Kind string `json:"kind"`
}

// CollectionTraits is the closed kernel-trait set a collection may declare.
// The zero value declares nothing, which is correct for ordinary collections:
// traits are opt-in and absence is never an error.
type CollectionTraits struct {
	BootstrapInclude []BootstrapInclude `json:"bootstrap_include,omitempty"`
	InvocationField  string             `json:"invocation_field,omitempty"`
	ArtifactKind     *ArtifactKindTrait `json:"artifact_kind,omitempty"`
}

// IsZero reports whether the collection declares no kernel traits at all.
func (t CollectionTraits) IsZero() bool {
	return len(t.BootstrapInclude) == 0 && t.InvocationField == "" && t.ArtifactKind == nil
}

// BootstrapIncludeForKey returns the declaration feeding the named bootstrap
// payload, or nil when this collection feeds no such payload.
func (t CollectionTraits) BootstrapIncludeForKey(key string) *BootstrapInclude {
	for i := range t.BootstrapInclude {
		if t.BootstrapInclude[i].Key == key {
			return &t.BootstrapInclude[i]
		}
	}
	return nil
}

// ErrInvalidTraitsType is returned when an inbound `traits` value is neither a
// JSON object nor a JSON-encoded string. Mirrors ErrInvalidSettingsType.
var ErrInvalidTraitsType = errors.New(`"traits" must be a JSON object or a JSON-encoded string`)

// isStoreSafeFieldKey mirrors the item store's field-key sanitizer
// (store.isValidFieldKey). Duplicated rather than imported because models must
// not depend on store; the two are pinned together by
// TestFilterKeyRuleMatchesStoreSanitizer.
func isStoreSafeFieldKey(key string) bool {
	if key == "" {
		return false
	}
	for _, c := range key {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// bootstrapKeyPattern constrains a bootstrap payload key to a conservative
// identifier shape. The key becomes a JSON object key in the bootstrap
// response, so it must be predictable for agents reading the payload.
var bootstrapKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ParseCollectionTraits decodes a stored traits JSON blob. An empty or "null"
// blob yields the zero value with no error — a collection that declares no
// traits is the common case, not a failure.
//
// Parsing is STRICT about shape (unknown fields are rejected) so a typo in a
// pack manifest surfaces at declaration time rather than as a kernel behavior
// that silently never fires. That is SPEC-0 L6's fail-loud posture applied to
// the trait grammar.
func ParseCollectionTraits(raw string) (CollectionTraits, error) {
	var t CollectionTraits
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return t, nil
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return CollectionTraits{}, fmt.Errorf("parse collection traits: %w", err)
	}
	// Decode stops at the end of the FIRST JSON value and ignores whatever
	// follows, so `{"artifact_kind":{"kind":"playbook"}} garbage` would parse
	// cleanly and activate behavior from a blob that is not valid JSON. That
	// makes "strict" a false claim, which is worse than being lenient on
	// purpose. Reject trailing content. Codex round 7.
	if _, err := dec.Token(); err != io.EOF {
		return CollectionTraits{}, fmt.Errorf("parse collection traits: unexpected trailing content after the JSON object")
	}
	return t, nil
}

// Validate enforces the kernel trait grammar. Returns a nil error for the zero
// value: declaring no traits is always legal.
func (t CollectionTraits) Validate() error {
	seenKeys := make(map[string]bool, len(t.BootstrapInclude))
	for i, inc := range t.BootstrapInclude {
		switch inc.Mode {
		case BootstrapModeBodies, BootstrapModeMetadata:
		case "":
			return fmt.Errorf("bootstrap_include[%d]: mode is required (%q or %q)", i, BootstrapModeBodies, BootstrapModeMetadata)
		default:
			return fmt.Errorf("bootstrap_include[%d]: unknown mode %q (want %q or %q)", i, inc.Mode, BootstrapModeBodies, BootstrapModeMetadata)
		}
		if inc.Key == "" {
			return fmt.Errorf("bootstrap_include[%d]: key is required", i)
		}
		if !bootstrapKeyPattern.MatchString(inc.Key) {
			return fmt.Errorf("bootstrap_include[%d]: key %q must match %s", i, inc.Key, bootstrapKeyPattern)
		}
		// Two declarations feeding the same payload from one collection is
		// always a mistake — the second would silently overwrite or
		// double-append depending on assembly order. Fail loud (SPEC-0 L6).
		if seenKeys[inc.Key] {
			return fmt.Errorf("bootstrap_include[%d]: duplicate key %q in the same collection", i, inc.Key)
		}
		seenKeys[inc.Key] = true
		// A first-party key's projection shape is fixed, so a declaration
		// naming one with the other mode would be quietly ignored — the
		// payload would come out in the projection's shape regardless. Refuse
		// rather than accept a declaration that does not mean what it says.
		if want, ok := firstPartyKeyModes[inc.Key]; ok && inc.Mode != want {
			return fmt.Errorf("bootstrap_include[%d]: key %q is a first-party boot payload with a fixed %q projection and cannot be declared %q", i, inc.Key, want, inc.Mode)
		}
		for k, v := range inc.Filter {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("bootstrap_include[%d]: filter has an empty field name (value %q)", i, v)
			}
			// The key must match the shape the item store will accept, and
			// this check FAILS OPEN if it is missing, which is why it is here
			// rather than left to the query layer: ListItems sanitizes field
			// keys and silently `continue`s past any it rejects, dropping that
			// predicate from the WHERE clause entirely. A declaration filtering
			// on `"stat us"` would therefore not narrow anything — the
			// conventions bodies include would ship EVERY convention, drafts
			// included, to every agent at boot. Refuse the declaration instead.
			//
			// (A well-shaped key naming a field that doesn't exist is a
			// different and safe case: the predicate is applied and matches
			// nothing, so the payload is empty rather than unfiltered. Not
			// rejected here — a collection may legitimately declare a filter
			// on a field its items don't all carry.)
			if !isStoreSafeFieldKey(k) {
				return fmt.Errorf("bootstrap_include[%d]: filter key %q must be alphanumeric with underscores or hyphens; the item store drops keys it cannot sanitize, which would silently apply NO filter", i, k)
			}
		}
	}

	if t.InvocationField != "" && t.InvocationField != InvocationSlugField {
		return fmt.Errorf("invocation_field: v1 supports only %q, got %q (SPEC-5 v1.1 amendment 4: any other field falls outside the partial unique indexes that guard invocation-slug uniqueness)", InvocationSlugField, t.InvocationField)
	}

	if t.ArtifactKind != nil && strings.TrimSpace(t.ArtifactKind.Kind) == "" {
		return fmt.Errorf("artifact_kind: kind is required when the trait is declared")
	}

	return nil
}

// JSON serializes traits for storage. The zero value serializes to "{}" so the
// column is never NULL and never an empty string.
func (t CollectionTraits) JSON() (string, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("marshal collection traits: %w", err)
	}
	return string(b), nil
}
