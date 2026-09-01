package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ItemFieldGitHubPR            = "github_pr"
	ItemFieldImplementationNotes = "implementation_notes"
	ItemFieldDecisionLog         = "decision_log"
	ItemFieldConvention          = "convention"
)

// reservedItemFieldKeys is the canonical set of field keys Pad itself writes
// into an item's fields blob. They are deliberately NOT declared by any
// collection schema — each is rendered from its own dedicated surface rather
// than as a generic field — which means every code path that reasons about
// fields by consulting a schema is, by construction, blind to them.
//
// That blindness is not hypothetical: it is the shared root of BUG-2627 (the
// CLI types a --field value by schema lookup, so these keys fall through to a
// raw string and become unreadable) and BUG-2674 (MigrateFields drops any key
// absent from the target schema, so a move destroyed them outright). Both were
// code paths that did not know these keys are special.
//
// The set lives here, once, so a caller can ASK instead of re-listing. Before
// this existed there were four constants and a single inline || chain in a CLI
// display path — a shape where the next reserved field added lands in the
// constants, gets wired into whichever surface prompted it, and silently misses
// every other.
//
// ADDING A KEY HERE IS NOT THE WHOLE JOB, and pretending otherwise would
// recreate the drift this set exists to stop. Membership tests inherit it for
// free — MigrateFields' carry, SchemaForMigratedFields, the collection-schema
// gate, the copy preflight's enumeration, the CLI's display filter. Three
// places still need a hand edit, because each needs something a set cannot
// supply:
//
//   - referentialItemFieldKeys below — does the new key point OUT of the item?
//   - reservedFieldLabel (handlers_items_copy_preflight.go) — a human label
//   - RESERVED_FIELD_KEYS (web/src/lib/components/collections/
//     field-editor-types.ts) — the client-side gate, deliberately a separate
//     list because it lowercases and is therefore stricter than this one
//
// TestReservedItemFieldKeysAreStableAndComplete fails on any change to this
// set, which is the reminder to visit all three.
var reservedItemFieldKeys = map[string]struct{}{
	ItemFieldGitHubPR:            {},
	ItemFieldImplementationNotes: {},
	ItemFieldDecisionLog:         {},
	ItemFieldConvention:          {},
}

// IsReservedItemField reports whether key is system-written metadata rather than
// a user-facing schema field. Callers that filter, migrate, or render an item's
// fields map should consult this rather than enumerating the constants.
func IsReservedItemField(key string) bool {
	_, ok := reservedItemFieldKeys[key]
	return ok
}

// referentialItemFieldKeys are the reserved keys whose VALUE points at
// something outside the item — a resource whose meaning depends on the
// surrounding workspace's context rather than on the item itself.
//
// The distinction decides how far they travel (BUG-2674, lead ruling). The
// carry rule is one sentence: system-minted NON-REFERENTIAL data carries;
// referential system data carries only where its referent's context still
// holds. implementation_notes and decision_log describe the item's own history
// and are true wherever the item is. github_pr names a repository that is a
// property of the SOURCE workspace's context — carried into a different
// workspace it renders as a live PR link on an item whose project may have no
// relationship to that repo, which is a false statement rather than a preserved
// one.
//
// So this is not an exception to the rule; it is the rule's own qualifier doing
// its job. A same-workspace move leaves the referent's context unchanged, so
// these carry there.
var referentialItemFieldKeys = map[string]struct{}{
	ItemFieldGitHubPR: {},
}

// IsReferentialItemField reports whether a reserved key's value depends on the
// workspace context around it. See referentialItemFieldKeys.
func IsReferentialItemField(key string) bool {
	_, ok := referentialItemFieldKeys[key]
	return ok
}

// ReservedItemFieldKeys returns the reserved keys in a stable order, for
// callers that need to enumerate rather than test membership (schema-key
// validation, error messages). Sorted so the output is deterministic — an
// error message that lists these must not reorder between runs.
func ReservedItemFieldKeys() []string {
	keys := make([]string, 0, len(reservedItemFieldKeys))
	for k := range reservedItemFieldKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type Item struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	CollectionID   string     `json:"collection_id"`
	Title          string     `json:"title"`
	Slug           string     `json:"slug"`
	Ref            string     `json:"ref,omitempty"` // computed: e.g. "TASK-5", "BUG-8"
	Content        string     `json:"content"`
	Fields         string     `json:"fields"` // JSON string
	Tags           string     `json:"tags"`   // JSON array string
	Pinned         bool       `json:"pinned"`
	SortOrder      int        `json:"sort_order"`
	ParentID       *string    `json:"parent_id,omitempty"`
	CreatedBy      string     `json:"created_by"`
	LastModifiedBy string     `json:"last_modified_by"`
	Source         string     `json:"source"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`

	// Assignment: (user, role) pair
	AssignedUserID *string `json:"assigned_user_id,omitempty"`
	AgentRoleID    *string `json:"agent_role_id,omitempty"`
	RoleSortOrder  int     `json:"role_sort_order"`

	// Auto-assigned sequential number within collection
	ItemNumber *int `json:"item_number,omitempty"`

	// Seq is a workspace-scoped monotonically-increasing sequence number
	// stamped on every mutation (create / update / soft-delete /
	// restore). It is the cursor mechanic for the local-first read
	// model's delta sync (PLAN-1343, DOC-1342 design decision #1).
	// Clients track the max seq they have seen and request
	// `?since=<seq>` deltas to resume. Robust against clock-skew /
	// same-millisecond-write / NTP-step correctness holes that an
	// `updated_at` watermark would carry.
	Seq int64 `json:"seq,omitempty"`

	// Populated by joins (not stored)
	AssignedUserName  string `json:"assigned_user_name,omitempty"`
	AssignedUserEmail string `json:"assigned_user_email,omitempty"`
	AgentRoleName     string `json:"agent_role_name,omitempty"`
	AgentRoleSlug     string `json:"agent_role_slug,omitempty"`
	AgentRoleIcon     string `json:"agent_role_icon,omitempty"`
	CollectionSlug    string `json:"collection_slug,omitempty"`
	CollectionName    string `json:"collection_name,omitempty"`
	CollectionIcon    string `json:"collection_icon,omitempty"`
	CollectionPrefix  string `json:"collection_prefix,omitempty"`

	// Parent link (populated by enrichItemForResponse / enrichItemsWithParent)
	ParentLinkID         string `json:"parent_link_id,omitempty"`
	ParentRef            string `json:"parent_ref,omitempty"`
	ParentTitle          string `json:"parent_title,omitempty"`
	ParentSlug           string `json:"parent_slug,omitempty"`
	ParentCollectionSlug string `json:"parent_collection_slug,omitempty"`

	// HasChildren is true if this item has child items linked to it.
	// Populated by enrichment, not stored in the DB.
	HasChildren bool `json:"has_children,omitempty"`

	// IsUnparented is populated only on unrestricted local-first index and
	// delta responses. A pointer preserves the distinction between a
	// structurally-parented item (false) and metadata that was deliberately
	// omitted for a restricted caller (nil).
	IsUnparented *bool `json:"is_unparented,omitempty"`

	// MovedTo names the destination(s) an ARCHIVED item was moved to by a
	// cross-workspace move (PLAN-2357 / TASK-2359). Populated on the single-item
	// GET response only, and only for destinations the caller has independently
	// been authorized to READ — see (*server.Server).movedToDestinations.
	//
	// `omitempty` is load-bearing, not cosmetic. A caller who may not read the
	// destination must receive a response byte-identical to one for an archived
	// item that was never moved: no key, no null, no empty array. A
	// structurally distinguishable response is itself the disclosure the ACL
	// gate exists to prevent. Never populate this from a list, search, activity
	// or share-link path, and never emit a placeholder when the gate denies.
	MovedTo []ItemMovedTo `json:"moved_to,omitempty"`

	DerivedClosure      *ItemDerivedClosure      `json:"derived_closure,omitempty"`
	CodeContext         *ItemCodeContext         `json:"code_context,omitempty"`
	Convention          *ItemConventionMetadata  `json:"convention,omitempty"`
	ImplementationNotes []ItemImplementationNote `json:"implementation_notes,omitempty"`
	DecisionLog         []ItemDecisionLogEntry   `json:"decision_log,omitempty"`

	// LastMutation carries the status/assignment delta that THIS specific
	// store call (UpdateItemWithParentLink / MoveItemWithPreCheck) produced,
	// computed synchronously inside the same transaction that already writes
	// the canonical status_transitions row (TASK-2533). It exists so the
	// watch-notification pipeline can classify an update's kind
	// (status-change / assignment) from a race-free, in-tx source of truth
	// instead of a separate before/after snapshot pair racing against
	// concurrent writers of the same item.
	//
	// Nil when the update touched neither the collection's done-field nor
	// assigned_user_id. Never persisted, never populated by GetItem or any
	// list/search path — only the two store functions above set it, and
	// only on the *models.Item they hand back to their caller. `json:"-"`
	// is deliberate: this is an internal signal for one in-process
	// consumer, not a wire contract the API is committing to.
	LastMutation *ItemMutationSignal `json:"-"`

	// PreUpdate is the item as it stood immediately BEFORE this store call
	// wrote it, read inside the same transaction and under the same locks as
	// the write (BUG-2776). It exists so a caller can describe what IT
	// changed rather than what changed since the caller last looked: the
	// handler's own pre-read happens before permission checks and before the
	// store's locks, so any concurrent writer's change lands between the two
	// and, diffed naively, gets attributed to this request's author.
	//
	// Same contract as LastMutation above and for the same reasons: set only
	// by UpdateItemWithParentLink, on the *models.Item it hands back, never
	// persisted, never populated by GetItem or any list/search path, and
	// `json:"-"` because it is an in-process signal rather than a wire
	// contract. It is a COPY taken at re-read time, so it stays the
	// pre-write view even though the store keeps reading `existing` after.
	//
	// Non-nil on every successful UpdateItemWithParentLink return. A consumer
	// that finds it nil is looking at an item from some other path and must
	// NOT silently fall back to its own stale snapshot — that fallback IS the
	// defect this field exists to remove.
	PreUpdate *Item `json:"-"`
}

// ItemMutationSignal is the race-free status/assignment delta attached to
// models.Item.LastMutation. See that field's doc comment for why it exists
// and who populates it.
type ItemMutationSignal struct {
	StatusChanged  bool
	StatusFieldKey string
	FromStatus     string
	ToStatus       string

	AssignmentChanged  bool
	FromAssignedUserID string // "" = was unassigned
	ToAssignedUserID   string // "" = now unassigned
}

// ComputeRef sets the Ref field from CollectionPrefix and ItemNumber.
// Call this after populating the item from a database query.
func (item *Item) ComputeRef() {
	if item.CollectionPrefix != "" && item.ItemNumber != nil {
		item.Ref = fmt.Sprintf("%s-%d", item.CollectionPrefix, *item.ItemNumber)
	}
}

// ItemMovedTo is one destination an archived item was moved to, rendered in
// DISPLAYABLE terms. It deliberately carries no UUIDs: the consumer must be
// able to render (and link to) the destination without a second call, and
// exposing internal IDs of a resource in another workspace buys nothing the
// slug/ref pair does not already give.
//
// Every field describes a resource the caller has already been authorized to
// read, and authorization is all-or-nothing per entry: an entry is never
// partially REDACTED, it is dropped. (Individual fields may still be empty for
// ordinary reasons — a workspace with no resolvable owner username, an item
// whose collection has no prefix — which is what the omitempty tags are for.
// Absence here never means "withheld".)
type ItemMovedTo struct {
	// WorkspaceSlug is the destination workspace's CANONICAL slug — the same
	// value the token consent allow-list was tested against.
	WorkspaceSlug string `json:"workspace_slug"`
	WorkspaceName string `json:"workspace_name,omitempty"`
	// WorkspaceOwnerUsername completes the /{username}/{workspace}/... web
	// route. Empty when the join did not resolve one; the consumer degrades to
	// a non-linked label rather than guessing.
	WorkspaceOwnerUsername string `json:"workspace_owner_username,omitempty"`

	CollectionSlug string `json:"collection_slug,omitempty"`
	// Ref is the destination item's issue ID ("TASK-5"). Empty only for the
	// rare item whose collection has no prefix or number.
	Ref      string `json:"ref,omitempty"`
	ItemSlug string `json:"item_slug"`
	Title    string `json:"title"`

	// MovedAt is when the move was recorded (RFC3339 UTC), matching the
	// provenance row's created_at.
	MovedAt string `json:"moved_at,omitempty"`
}

type ItemRelationRef struct {
	ID             string `json:"id"`
	Slug           string `json:"slug,omitempty"`
	Ref            string `json:"ref,omitempty"`
	Title          string `json:"title"`
	CollectionSlug string `json:"collection_slug,omitempty"`
	Status         string `json:"status,omitempty"`
}

type ItemDerivedClosure struct {
	IsClosed     bool              `json:"is_closed"`
	Kind         string            `json:"kind"`
	Summary      string            `json:"summary"`
	RelatedItems []ItemRelationRef `json:"related_items,omitempty"`
}

type ItemCodeContext struct {
	Provider    string                   `json:"provider"`
	Repo        string                   `json:"repo,omitempty"`
	Branch      string                   `json:"branch,omitempty"`
	PullRequest *ItemPullRequestMetadata `json:"pull_request,omitempty"`
}

type ItemConventionMetadata struct {
	Category    string   `json:"category,omitempty"`
	Trigger     string   `json:"trigger,omitempty"`
	Surfaces    []string `json:"surfaces,omitempty"`
	Enforcement string   `json:"enforcement,omitempty"`
	Commands    []string `json:"commands,omitempty"`
}

type ItemPullRequestMetadata struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type ItemImplementationNote struct {
	ID        string `json:"id,omitempty"`
	Summary   string `json:"summary"`
	Details   string `json:"details,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
}

type ItemDecisionLogEntry struct {
	ID        string `json:"id,omitempty"`
	Decision  string `json:"decision"`
	Rationale string `json:"rationale,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
}

type githubPRFields struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Branch    string `json:"branch"`
	Repo      string `json:"repo"`
	UpdatedAt string `json:"updated_at"`
}

type conventionFields struct {
	Category    string   `json:"category"`
	Trigger     string   `json:"trigger"`
	Surfaces    []string `json:"surfaces"`
	Enforcement string   `json:"enforcement"`
	Commands    []string `json:"commands"`
}

func ExtractItemCodeContext(fieldsJSON string) *ItemCodeContext {
	fieldsMap, ok := parseItemFields(fieldsJSON)
	if !ok {
		return nil
	}

	raw, ok := fieldsMap[ItemFieldGitHubPR]
	if !ok {
		return nil
	}

	payload, err := json.Marshal(raw)
	if err != nil {
		return nil
	}

	var githubPR githubPRFields
	if err := json.Unmarshal(payload, &githubPR); err != nil {
		return nil
	}
	if githubPR.Number == 0 && githubPR.URL == "" && githubPR.Branch == "" && githubPR.Repo == "" {
		return nil
	}

	context := &ItemCodeContext{
		Provider: "github",
		Repo:     githubPR.Repo,
		Branch:   githubPR.Branch,
	}
	if githubPR.Number != 0 || githubPR.URL != "" || githubPR.Title != "" || githubPR.State != "" {
		context.PullRequest = &ItemPullRequestMetadata{
			Number:    githubPR.Number,
			URL:       githubPR.URL,
			Title:     githubPR.Title,
			State:     githubPR.State,
			UpdatedAt: githubPR.UpdatedAt,
		}
	}

	return context
}

func ExtractItemConventionMetadata(fieldsJSON string) *ItemConventionMetadata {
	fieldsMap, ok := parseItemFields(fieldsJSON)
	if !ok {
		return nil
	}

	var metadata ItemConventionMetadata
	hasMetadata := false

	// hasConventionShape tracks whether we've found a Convention-
	// SPECIFIC marker — the structured convention field, or one of
	// trigger / surfaces / scope / commands / direct enforcement.
	// `category` alone is NOT a Convention marker (Ideas, Bugs, Roadmap
	// items also use category). Used to gate the priority→enforcement
	// legacy fallback below; without this gate every Task/Idea with a
	// `priority` field got a phantom `convention.enforcement` surfaced
	// on its response (BUG-987 bug 13).
	hasConventionShape := false

	if raw, ok := fieldsMap[ItemFieldConvention]; ok {
		payload, err := json.Marshal(raw)
		if err == nil {
			var structured conventionFields
			if err := json.Unmarshal(payload, &structured); err == nil {
				metadata = ItemConventionMetadata{
					Category:    structured.Category,
					Trigger:     structured.Trigger,
					Surfaces:    append([]string(nil), structured.Surfaces...),
					Enforcement: structured.Enforcement,
					Commands:    append([]string(nil), structured.Commands...),
				}
				hasMetadata = true
				hasConventionShape = true
			}
		}
	}

	if metadata.Category == "" {
		if category, ok := fieldsMap["category"].(string); ok {
			metadata.Category = category
			hasMetadata = true
			// Note: category alone does NOT flip hasConventionShape —
			// many non-Convention collections legitimately use it.
		}
	}
	if metadata.Trigger == "" {
		if trigger, ok := fieldsMap["trigger"].(string); ok {
			metadata.Trigger = trigger
			hasMetadata = true
			hasConventionShape = true
		}
	}
	// Direct enforcement only — the priority fallback runs at the
	// END so surfaces/scope/commands have a chance to flip
	// hasConventionShape first. Without that ordering, a legacy
	// Convention like `{scope:"all", priority:"must"}` (no trigger)
	// would silently drop enforcement because the fallback ran
	// before scope set hasConventionShape.
	if metadata.Enforcement == "" {
		if value, ok := fieldsMap["enforcement"].(string); ok {
			metadata.Enforcement = value
			hasMetadata = true
			hasConventionShape = true
		}
	}
	if len(metadata.Surfaces) == 0 {
		if surfaces := extractStringList(fieldsMap["surfaces"]); len(surfaces) > 0 {
			metadata.Surfaces = surfaces
			hasMetadata = true
			hasConventionShape = true
		} else if scope, ok := fieldsMap["scope"].(string); ok && scope != "" {
			metadata.Surfaces = []string{scope}
			hasMetadata = true
			hasConventionShape = true
		}
	}
	if len(metadata.Commands) == 0 {
		if commands := extractStringList(fieldsMap["commands"]); len(commands) > 0 {
			metadata.Commands = commands
			hasMetadata = true
			hasConventionShape = true
		}
	}

	// Legacy priority→enforcement fallback. Runs AFTER all other
	// markers because hasConventionShape only flips once we've seen
	// a Convention-specific signal. Without this ordering, a legacy
	// Convention with only `{scope, priority}` would lose its
	// enforcement value because scope hadn't been processed yet
	// (Codex review on PR #361 caught this).
	if metadata.Enforcement == "" && hasConventionShape {
		if priority, ok := fieldsMap["priority"].(string); ok {
			metadata.Enforcement = priority
		}
	}

	if !hasMetadata {
		return nil
	}
	// Final guard: if we ONLY matched on `category` (no Convention-
	// specific markers), the item isn't a Convention. Suppress the
	// metadata entirely — surfacing { category } on a non-Convention
	// item just for category alone produced confusing responses.
	if !hasConventionShape {
		return nil
	}
	return normalizeItemConventionMetadata(&metadata)
}

func ExtractItemImplementationNotes(fieldsJSON string) []ItemImplementationNote {
	fieldsMap, ok := parseItemFields(fieldsJSON)
	if !ok {
		return nil
	}
	raw, ok := fieldsMap[ItemFieldImplementationNotes]
	if !ok {
		return nil
	}

	payload, err := json.Marshal(raw)
	if err != nil {
		return nil
	}

	var notes []ItemImplementationNote
	if err := json.Unmarshal(payload, &notes); err != nil {
		return nil
	}
	if len(notes) == 0 {
		return nil
	}
	return notes
}

func ExtractItemDecisionLog(fieldsJSON string) []ItemDecisionLogEntry {
	fieldsMap, ok := parseItemFields(fieldsJSON)
	if !ok {
		return nil
	}
	raw, ok := fieldsMap[ItemFieldDecisionLog]
	if !ok {
		return nil
	}

	payload, err := json.Marshal(raw)
	if err != nil {
		return nil
	}

	var entries []ItemDecisionLogEntry
	if err := json.Unmarshal(payload, &entries); err != nil {
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	return entries
}

// ErrStructuredFieldUnreadable is returned by the Append* helpers when the item
// already carries a structured-entry field whose stored value cannot be decoded
// into its entry slice. Callers should surface it rather than retry: the append
// is refused precisely because completing it would destroy the stored value.
var ErrStructuredFieldUnreadable = errors.New("structured field is present but unreadable")

// assertStructuredFieldAppendable refuses an append when key holds a value that
// does not decode into []T.
//
// BUG-2627. The Append* helpers below build the new slice from Extract*, then
// assign it over the key unconditionally. Extract* returns nil for FOUR
// different reasons and only one of them is a defect, so the nil itself cannot
// be the refusal condition:
//
//  1. the key is absent          — the first append on an item. Must proceed.
//  2. the key holds an empty array — well-formed, just empty. Must proceed.
//  3. the key holds an explicit JSON null — carries no entries. Must proceed.
//  4. the key holds a value that does not decode — the defect. Must REFUSE,
//     because the unconditional assign would overwrite that value with a
//     one-element slice and report success. Observed live: an item whose
//     implementation_notes was a JSON-ENCODED STRING lost its stored note to a
//     single `pad item note` call, with no warning on any surface.
//
// So this checks decodability directly against the raw value instead of reusing
// Extract*'s nil, which cannot distinguish (4) from (1), (2) or (3).
//
// T must be the entry type the MATCHING Extract* decodes into. A wrong-but-
// compiling instantiation is silently destructive rather than merely wrong:
// encoding/json ignores unknown fields, so ItemImplementationNote ACCEPTS
// `{"decision":{...}}` while ExtractItemDecisionLog rejects it — the guard would
// permit an append that the extractor then reads as empty, which is the exact
// divergence this function exists to prevent. Both directions are pinned by
// tests; see TestAppendDecisionLogRefusesEntriesOnlyItsOwnTypeRejects.
func assertStructuredFieldAppendable[T any](fieldsMap map[string]any, key string) error {
	raw, ok := fieldsMap[key]
	if !ok {
		return nil // case 1 — absent, nothing to lose.
	}
	if raw == nil {
		return nil // an explicit JSON null carries no entries either.
	}

	payload, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("%w: %q could not be re-encoded for inspection: %w", ErrStructuredFieldUnreadable, key, err)
	}

	var entries []T
	if err := json.Unmarshal(payload, &entries); err != nil {
		// Deliberately does NOT name a repair command. The remedy has to be
		// one that works in THIS state, and every append path is exactly what
		// is being refused here; `pad item show --format json` is read-only
		// and does surface the raw value, so it is the one action safe to
		// suggest.
		return fmt.Errorf(
			"%w: %q holds a value that is not a list of entries, so appending would overwrite and destroy it; "+
				"inspect the stored value with `pad item show <ref> --format json` and repair it before appending (BUG-2627)",
			ErrStructuredFieldUnreadable, key)
	}
	return nil
}

// unreadableFieldsBlob tags an append's fields-parse failure as
// ErrStructuredFieldUnreadable (Codex round 5).
//
// The classification, not the failure, is what this changes. An item whose
// whole fields column will not parse is unreadable in exactly the sense
// BUG-2675's code exists for — deterministic, unfixable by the caller, and
// pointless to retry — but the bare parse error carried none of that, so it
// reached agents as `server_error` and invited the retry loop the code was
// added to stop. Same reasoning, one level out from the per-key guard.
func unreadableFieldsBlob(err error) error {
	return fmt.Errorf("%w: the item's fields blob does not parse, so nothing can be appended to it; "+
		"inspect it with `pad item show <ref> --format json` and repair it: %w", ErrStructuredFieldUnreadable, err)
}

// StructuredFieldIsAppendable reports whether an append to key would be
// ACCEPTED on an item carrying fieldsJSON — i.e. whether the Append* helpers
// would proceed rather than refusing with ErrStructuredFieldUnreadable.
//
// It exists so a caller that wants to TALK about appendability asks the same
// question the guard answers, instead of re-deriving it. BUG-2627 part 2's
// refusal message names `pad item note` as the remedy, which is a lie whenever
// that command would itself refuse; the message therefore has to agree with the
// guard EXACTLY, and a second decode written to look equivalent is not exact.
// Codex round 2 caught a first version of it that decoded into
// []json.RawMessage: a stored `[1]` passed there and failed the real guard, so
// the message prescribed a command that refuses.
//
// The entry type per key matches the matching Extract*, for the reason
// assertStructuredFieldAppendable documents at length: json ignores unknown
// fields, so the WRONG-but-compiling instantiation silently permits what the
// extractor rejects.
//
// A fieldsJSON that will not parse AT ALL returns false, because that is the
// honest answer to the question asked: the Append* helpers bail on the same
// parse and return an error, so an append would not be accepted. An earlier
// version returned true on the reasoning that a broken outer blob is "a
// different problem" — which is true of the CAUSE and irrelevant to the
// CALLER, who would have been told to run a command that cannot succeed
// (Codex round 4).
func StructuredFieldIsAppendable(fieldsJSON, key string) bool {
	fieldsMap, err := parseMutableItemFields(fieldsJSON)
	if err != nil {
		return false
	}
	switch key {
	case ItemFieldImplementationNotes:
		return assertStructuredFieldAppendable[ItemImplementationNote](fieldsMap, key) == nil
	case ItemFieldDecisionLog:
		return assertStructuredFieldAppendable[ItemDecisionLogEntry](fieldsMap, key) == nil
	}
	// No append helper owns this key, so nothing can refuse an append to it.
	return true
}

func AppendImplementationNote(fieldsJSON string, note ItemImplementationNote) (string, error) {
	fieldsMap, err := parseMutableItemFields(fieldsJSON)
	if err != nil {
		return "", unreadableFieldsBlob(err)
	}
	if err := assertStructuredFieldAppendable[ItemImplementationNote](fieldsMap, ItemFieldImplementationNotes); err != nil {
		return "", err
	}

	notes := ExtractItemImplementationNotes(fieldsJSON)
	notes = append(notes, note)
	fieldsMap[ItemFieldImplementationNotes] = notes
	return marshalItemFields(fieldsMap)
}

func AppendDecisionLogEntry(fieldsJSON string, entry ItemDecisionLogEntry) (string, error) {
	fieldsMap, err := parseMutableItemFields(fieldsJSON)
	if err != nil {
		return "", unreadableFieldsBlob(err)
	}
	if err := assertStructuredFieldAppendable[ItemDecisionLogEntry](fieldsMap, ItemFieldDecisionLog); err != nil {
		return "", err
	}

	entries := ExtractItemDecisionLog(fieldsJSON)
	entries = append(entries, entry)
	fieldsMap[ItemFieldDecisionLog] = entries
	return marshalItemFields(fieldsMap)
}

func ApplyItemConventionMetadata(fieldsJSON string, metadata *ItemConventionMetadata) (string, error) {
	fieldsMap, err := parseMutableItemFields(fieldsJSON)
	if err != nil {
		return "", err
	}

	normalized := normalizeItemConventionMetadata(metadata)
	if normalized == nil {
		delete(fieldsMap, ItemFieldConvention)
		delete(fieldsMap, "category")
		delete(fieldsMap, "trigger")
		delete(fieldsMap, "scope")
		delete(fieldsMap, "priority")
		delete(fieldsMap, "enforcement")
		delete(fieldsMap, "surfaces")
		delete(fieldsMap, "commands")
		return marshalItemFields(fieldsMap)
	}

	fieldsMap[ItemFieldConvention] = normalized
	fieldsMap["category"] = normalized.Category
	fieldsMap["trigger"] = normalized.Trigger
	fieldsMap["enforcement"] = normalized.Enforcement
	fieldsMap["priority"] = normalized.Enforcement
	fieldsMap["surfaces"] = normalized.Surfaces
	fieldsMap["commands"] = normalized.Commands
	if len(normalized.Surfaces) > 0 {
		fieldsMap["scope"] = normalized.Surfaces[0]
	}

	return marshalItemFields(fieldsMap)
}

func BuildConventionItemFields(status string, metadata *ItemConventionMetadata) (string, error) {
	fieldsJSON, err := ApplyItemConventionMetadata("{}", metadata)
	if err != nil {
		return "", err
	}
	fieldsMap, err := parseMutableItemFields(fieldsJSON)
	if err != nil {
		return "", err
	}
	if status != "" {
		fieldsMap["status"] = status
	}
	return marshalItemFields(fieldsMap)
}

func parseItemFields(fieldsJSON string) (map[string]any, bool) {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return nil, false
	}
	var fieldsMap map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &fieldsMap); err != nil {
		return nil, false
	}
	return fieldsMap, true
}

func parseMutableItemFields(fieldsJSON string) (map[string]any, error) {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return map[string]any{}, nil
	}
	var fieldsMap map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &fieldsMap); err != nil {
		return nil, fmt.Errorf("parse item fields: %w", err)
	}
	if fieldsMap == nil {
		// A literal `null` unmarshals into a NIL map with no error, and every
		// caller here goes on to assign into what it gets back — so `pad item
		// note` against an item whose fields column holds "null" panicked with
		// "assignment to entry in nil map" rather than appending (reproduced;
		// Codex round 3 on BUG-2627). An absent blob and a null blob mean the
		// same thing to every caller, so they get the same empty map.
		return map[string]any{}, nil
	}
	return fieldsMap, nil
}

func marshalItemFields(fieldsMap map[string]any) (string, error) {
	payload, err := json.Marshal(fieldsMap)
	if err != nil {
		return "", fmt.Errorf("marshal item fields: %w", err)
	}
	return string(payload), nil
}

func normalizeItemConventionMetadata(metadata *ItemConventionMetadata) *ItemConventionMetadata {
	if metadata == nil {
		return nil
	}
	normalized := &ItemConventionMetadata{
		Category:    metadata.Category,
		Trigger:     metadata.Trigger,
		Enforcement: metadata.Enforcement,
		Surfaces:    uniqueStrings(metadata.Surfaces),
		Commands:    uniqueStrings(metadata.Commands),
	}
	if normalized.Category == "" && normalized.Trigger == "" && normalized.Enforcement == "" && len(normalized.Surfaces) == 0 && len(normalized.Commands) == 0 {
		return nil
	}
	return normalized
}

func extractStringList(raw any) []string {
	switch value := raw.(type) {
	case []string:
		return uniqueStrings(value)
	case []any:
		var out []string
		for _, entry := range value {
			if str, ok := entry.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		return uniqueStrings(out)
	default:
		return nil
	}
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type ItemCreate struct {
	Title          string  `json:"title"`
	Content        string  `json:"content,omitempty"`
	Fields         string  `json:"fields,omitempty"`
	Tags           string  `json:"tags,omitempty"`
	Pinned         bool    `json:"pinned,omitempty"`
	ParentID       *string `json:"parent_id,omitempty"`
	AssignedUserID *string `json:"assigned_user_id,omitempty"`
	AgentRoleID    *string `json:"agent_role_id,omitempty"`
	CreatedBy      string  `json:"created_by,omitempty"`
	Source         string  `json:"source,omitempty"`
}

type ItemUpdate struct {
	Title   *string `json:"title,omitempty"`
	Content *string `json:"content,omitempty"`
	Fields  *string `json:"fields,omitempty"`
	// FieldsPatch, when non-nil, applies a SHALLOW field-level merge onto
	// the item's CURRENT fields JSON instead of replacing the whole blob
	// (IDEA-1480 / TASK-2022). Each key sets its field; a key mapped to a
	// JSON null DELETES that field; every other stored field is left
	// untouched. The merge runs INSIDE UpdateItem's transaction against the
	// row read under the write lock, so two concurrent single-field patches
	// can no longer clobber each other the way the full-blob
	// read-modify-write does (the lost-write race IDEA-1480 describes).
	//
	// Mutually exclusive with Fields — the HTTP handler rejects a request
	// carrying both. In-process callers should set exactly one.
	FieldsPatch    map[string]interface{} `json:"fields_patch,omitempty"`
	Tags           *string                `json:"tags,omitempty"`
	Pinned         *bool                  `json:"pinned,omitempty"`
	SortOrder      *int                   `json:"sort_order,omitempty"`
	ParentID       *string                `json:"parent_id,omitempty"`
	AssignedUserID *string                `json:"assigned_user_id,omitempty"`
	AgentRoleID    *string                `json:"agent_role_id,omitempty"`
	LastModifiedBy string                 `json:"last_modified_by,omitempty"`
	Source         string                 `json:"source,omitempty"`
	// VersionSource overrides the per-version-row Source attribution
	// without mutating `items.source`. When unset (the common case),
	// the version row inherits the same value as `items.source`
	// (whatever Source ends up being). When set, the version row
	// uses VersionSource and `items.source` is left alone — used by
	// the collab 5s-flush PATCH handler so an auto-flush doesn't
	// re-attribute a CLI/MCP-created item to "collab-snapshot" and
	// silently flip it out of `WorkspaceHasAgentActivity`'s count.
	// Per Codex review round 3 of TASK-1267 [P2].
	VersionSource string `json:"version_source,omitempty"`
	ChangeSummary string `json:"change_summary,omitempty"`
	// ForceVersion bypasses the per-(actor, source) version throttle
	// (VersionThrottleInterval) so a version snapshot is ALWAYS created when
	// content changes. Set by deliberate, infrequent operations that must
	// leave an undo point regardless of recent edit cadence — chiefly a
	// version RESTORE, which changes items.content out from under the existing
	// reverse-patch chain and would corrupt that chain (and lose the restore's
	// own undo point) if a throttled write moved content forward without a
	// bracketing version. Internal-only (`json:"-"`): the store honours it, but
	// no HTTP client can set it. Only consulted when content actually changes.
	ForceVersion bool `json:"-"`
	// MarkRestoreBoundary stamps items.last_restore_seq with this update's newly
	// assigned seq, INSIDE the same transaction as the content write + op-log
	// prune (BUG-2264). Set only by version RESTORE. That durable per-item
	// boundary is what Join reads to force_refresh a client whose ?content_seq
	// seed predates the restore — surviving a server restart, unlike the
	// in-memory fast-path. Internal-only (`json:"-"`); no HTTP client can set it.
	MarkRestoreBoundary bool    `json:"-"`
	Comment             *string `json:"comment,omitempty"`
	// OpLogCursor is the highest item_yjs_updates.id the calling client
	// has applied into its local Y.Doc (TASK-1319). Used by the
	// collab-snapshot flush PATCH to advance the op-log GC watermark
	// (`items.content_flushed_op_log_id`) when, and only when, the
	// cursor matches the current MAX(item_yjs_updates.id) for the item.
	//
	// **Why a pointer.** A nil cursor means "the caller didn't claim
	// to know"; the watermark stays put. *0 means "I have nothing"
	// (e.g. a fresh editor whose op-log is empty); when MAX is also 0
	// the watermark advances to 0 (a no-op stamp) — but practical
	// flushes always have *some* op-log id, so this branch rarely
	// matters.
	//
	// Only honoured when VersionSource == "collab-snapshot". Other
	// content writes (CLI / MCP / version restore / PruneAndApply)
	// already advance the watermark to MAX(op-log.id) at write time
	// because they reconstruct or replace items.content wholesale.
	// Per TASK-1319.
	OpLogCursor *int64 `json:"op_log_cursor,omitempty"`
	// ClearAssignedUser / ClearAgentRole allow explicitly setting to NULL
	// (since nil pointer means "don't change" in partial updates)
	ClearAssignedUser bool `json:"clear_assigned_user,omitempty"`
	ClearAgentRole    bool `json:"clear_agent_role,omitempty"`

	// Force, when true, overrides the server-side open-children guard
	// (IDEA-1494) that otherwise rejects a non-terminal → terminal
	// done-field transition while the item still has non-terminal
	// children. Transport-only: the store layer never sees it (the
	// handler consumes it before calling UpdateItem). Used by `pad item
	// update --force` and MCP `pad_item.action: update` with
	// `force: true`.
	Force bool `json:"force,omitempty"`

	// ExpectedUpdatedAt, when non-empty, enables optimistic-concurrency
	// checking (TASK-2022). UpdateItem parses it as RFC3339 and compares it
	// against the item's current updated_at read under the write lock;
	// on mismatch it returns *store.UpdateConflictError so the handler can
	// surface the pad-structured-error/v1 conflict envelope (HTTP 409,
	// code "update_conflict"). Empty = no check (last-writer-wins, the
	// historical default). Round-trip the `updated_at` you last read.
	ExpectedUpdatedAt string `json:"expected_updated_at,omitempty"`
}

// ErrInvalidFieldsType / ErrInvalidTagsType are returned by
// ItemUpdate.UnmarshalJSON AND ItemCreate.UnmarshalJSON when the
// inbound `fields` / `tags` value is neither a JSON-encoded string
// nor the natural object/array shape. Wire handlers surface the
// sentinel's message verbatim (without the "invalid JSON: ..." wrapper
// from decodeJSON) so callers see a clean domain-level error instead
// of leaked Go internals. See BUG-1144 (Update) and BUG-1432 (Create).
var (
	ErrInvalidFieldsType = errors.New(`"fields" must be a JSON object or a JSON-encoded string`)
	ErrInvalidTagsType   = errors.New(`"tags" must be a JSON array or a JSON-encoded string`)
)

// UnmarshalJSON for ItemCreate mirrors the flexible-shape behaviour
// ItemUpdate gained under BUG-1144: accept `fields` / `tags` either as
// the canonical JSON-encoded string shape (matches models.Item storage)
// OR as the natural nested object/array shape any reasonable HTTP
// client would send.
//
// Pre-BUG-1432 the Create path was brittle in two specific ways agents
// hit on Pad Cloud:
//
//   - `tags: ["foo","bar"]` (natural JSON-array shape) was rejected by
//     the default unmarshaler because the struct field is `string`,
//     yielding `"cannot unmarshal array into Go struct field
//     ItemCreate.tags of type string"` — an HTTP 400 the MCP
//     dispatcher then surfaced as a validation_failed envelope.
//
//   - `fields: {status: "open"}` (natural nested-object shape) hit the
//     same brittleness with `cannot unmarshal object into ... fields of
//     type string`.
//
// The asymmetry with ItemUpdate (which already handled both shapes
// cleanly per BUG-1144) was Codex's tip in the BUG-1432 investigation
// — fixing it here closes the create/update gap and gives agents a
// uniform shape contract across both verbs.
//
// In-process callers that construct ItemCreate{} literals never hit
// this path; only the JSON decode boundary changes.
func (c *ItemCreate) UnmarshalJSON(data []byte) error {
	// Use an alias to inherit every other field's default unmarshal
	// behaviour, while shadowing fields/tags with json.RawMessage so we
	// can inspect the raw shape. The outer fields shadow the embedded
	// alias's same-named fields because they are less deeply nested.
	type alias ItemCreate
	aux := struct {
		Fields json.RawMessage `json:"fields,omitempty"`
		Tags   json.RawMessage `json:"tags,omitempty"`
		*alias
	}{alias: (*alias)(c)}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// flexJSONToString returns *string (nil when absent / null / empty).
	// ItemCreate.Fields/Tags are plain strings, not pointers, so deref
	// when present and leave at zero value otherwise.
	if fieldsStr, err := flexJSONToString(aux.Fields, '{', ErrInvalidFieldsType); err != nil {
		return err
	} else if fieldsStr != nil {
		c.Fields = *fieldsStr
	}

	if tagsStr, err := flexJSONToString(aux.Tags, '[', ErrInvalidTagsType); err != nil {
		return err
	} else if tagsStr != nil {
		c.Tags = *tagsStr
	}

	return nil
}

// UnmarshalJSON accepts `fields` / `tags` either as the canonical
// JSON-encoded string shape (matches models.Item.Fields/Tags storage)
// OR as the natural nested object/array shape any reasonable HTTP
// client would send. The struct fields stay `*string` and the rest
// of the pipeline (validation, store writes, web/CLI consumers) is
// unchanged — we just normalize the wire input here.
//
// In-process callers that construct ItemUpdate{} literals never hit
// this path, so no internal call site needs to change.
//
// See BUG-1144 (input side) and BUG-991 (the symmetric response-side
// dual-emit, fixed at the MCP boundary in PR #364).
func (u *ItemUpdate) UnmarshalJSON(data []byte) error {
	// Use an alias to inherit every other field's default unmarshal
	// behaviour, while shadowing fields/tags with json.RawMessage so we
	// can inspect the raw shape. The outer fields shadow the embedded
	// alias's same-named fields because they are less deeply nested.
	type alias ItemUpdate
	aux := struct {
		Fields json.RawMessage `json:"fields,omitempty"`
		Tags   json.RawMessage `json:"tags,omitempty"`
		*alias
	}{alias: (*alias)(u)}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// The alias decode leaves u.Fields / u.Tags nil (the raw bytes were
	// captured at the outer level). Re-populate them from the flex parser.
	fieldsStr, err := flexJSONToString(aux.Fields, '{', ErrInvalidFieldsType)
	if err != nil {
		return err
	}
	u.Fields = fieldsStr

	tagsStr, err := flexJSONToString(aux.Tags, '[', ErrInvalidTagsType)
	if err != nil {
		return err
	}
	u.Tags = tagsStr

	return nil
}

// flexJSONToString accepts a raw JSON value and returns it as a
// canonical JSON-encoded string. Acceptable inbound shapes:
//
//   - absent / empty / null → nil (caller leaves the field unchanged)
//   - JSON-encoded string whose INNER content is either empty (the
//     legacy empty-string sentinel handled downstream by store-layer
//     coercion) OR a JSON value whose shape matches expectedStart
//     ('{' or '[')
//   - JSON object or array (matching expectedStart '{' or '[') → re-
//     marshal to string
//
// Any other shape returns errInvalid so the handler surfaces a clean
// domain-level error instead of a leaked Go unmarshal message.
//
// IDEA-1488 R1 codex hardening: the `case '"'` branch validates the
// INNER content's shape (not just the JSON-encoded-string envelope).
// Without this, `{"config": "[]"}` or `{"settings": "not json"}` would
// slip past — the outer string-shape check accepted any inner content
// verbatim, which defeated the shape-validation ceiling that IDEA-1488
// is supposed to add. The pre-existing ItemUpdate fields/tags path
// inherits the same tightening because it routes through this helper.
func flexJSONToString(raw json.RawMessage, expectedStart byte, errInvalid error) (*string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	switch trimmed[0] {
	case '"':
		// JSON-encoded string envelope — unmarshal to the underlying Go
		// string and then validate the inner content's shape matches
		// expectedStart. Subsequent code that does
		// json.Unmarshal([]byte(*s), ...) sees the inner JSON, not a
		// re-quoted string.
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, errInvalid
		}
		// Empty inner string is the empty-string sentinel; store-layer
		// coercion handles it (IDEA-1486). Don't reject here so callers
		// retain the legacy "" → default normalization shape.
		innerTrimmed := bytes.TrimSpace([]byte(s))
		if len(innerTrimmed) == 0 {
			return &s, nil
		}
		if innerTrimmed[0] != expectedStart {
			return nil, errInvalid
		}
		// Confirm the inner content actually parses as JSON of the
		// expected shape. Catches strings that start with the right
		// brace but are otherwise garbage (e.g. `"{not valid"`).
		var inner any
		if err := json.Unmarshal(innerTrimmed, &inner); err != nil {
			return nil, errInvalid
		}
		return &s, nil
	case expectedStart:
		// Object or array — re-marshal to a canonical JSON string so
		// the downstream string-typed pipeline can json.Unmarshal it
		// back to a map/slice exactly as if the caller had stringified.
		var v any
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return nil, errInvalid
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, errInvalid
		}
		s := string(b)
		return &s, nil
	default:
		return nil, errInvalid
	}
}

type ItemListParams struct {
	CollectionSlug string
	CollectionIDs  []string          // permission filter: restrict to these collection IDs (nil = no filter)
	ItemIDs        []string          // permission filter: additionally restrict to these item IDs (for item-level grants)
	Fields         map[string]string // field filters: key=value
	Sort           string            // e.g. "priority:desc,created_at:asc"
	GroupBy        string
	Search         string // FTS query
	ParentID       string
	Tag            string
	AssignedUserID string // filter by assigned user
	AgentRoleID    string // filter by agent role (ID or slug)
	ParentLinkID   string // filter by parent link (item ID of the parent)
	// Unparented keeps only items with neither the legacy parent_id column nor
	// an outgoing parent/implements item_link. Incoming links do not count.
	Unparented      bool
	IncludeArchived bool
	// NonTerminal, when true, restricts results to items whose resolved
	// done-field value is NOT one of their collection's terminal options.
	// Each collection is evaluated against its OWN terminal_options (falling
	// back to DefaultTerminalStatuses when the schema declares none), so
	// collections with custom status vocabularies (e.g. todo/drafting/
	// scheduled) are handled correctly rather than against a hardcoded
	// global status allowlist (BUG-2001).
	NonTerminal bool
	// NoContent omits the (potentially large) rich-text body column from
	// the projection — callers that only count/summarize/scan structured
	// fields (e.g. the dashboard builder) don't pay to load every item's
	// full markdown. item.Content comes back empty when set. See BUG-2002.
	NoContent bool
	Limit     int
	Offset    int
}

// TagCount is a distinct tag used within a workspace and the number of items
// carrying it. Returned by Store.ListWorkspaceTags and the
// GET /workspaces/{ws}/tags endpoint, ordered by Count desc then Tag asc.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

type ItemLink struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	SourceID    string    `json:"source_id"`
	TargetID    string    `json:"target_id"`
	LinkType    string    `json:"link_type"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`

	// Populated by joins
	SourceTitle          string `json:"source_title,omitempty"`
	TargetTitle          string `json:"target_title,omitempty"`
	SourceSlug           string `json:"source_slug,omitempty"`
	TargetSlug           string `json:"target_slug,omitempty"`
	SourceRef            string `json:"source_ref,omitempty"`
	TargetRef            string `json:"target_ref,omitempty"`
	SourceCollectionSlug string `json:"source_collection_slug,omitempty"`
	TargetCollectionSlug string `json:"target_collection_slug,omitempty"`
	SourceStatus         string `json:"source_status,omitempty"`
	TargetStatus         string `json:"target_status,omitempty"`
}

type ItemLinkCreate struct {
	TargetID  string `json:"target_id"`
	LinkType  string `json:"link_type,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
}

// MaxItemTitleRunes bounds an item title at write time.
//
// 255 matches MaxDocumentTitleRunes, but for items the number is not borrowed
// — it is mechanically sufficient, which is why BUG-2831's "there is no single
// safe number, it depends on compressibility" does not apply once the bound
// exists:
//
//   - items carries UNIQUE(workspace_id, slug), which Postgres implements as a
//     btree, and a btree index tuple has a size cap. That cap, not the title
//     column, is what refuses a long title on Postgres while SQLite takes it
//     (BUG-2831).
//   - store.slugify emits ONLY [a-z0-9-] — one output BYTE per input rune at
//     most, and it truncates nothing. So an N-rune title yields at most N bytes
//     of slug.
//
// THE CAP IS 2704 BYTES, NOT 8191, and this is measured rather than inherited.
// BUG-2831 quoted 8191 — the absolute maximum — and the first version of this
// comment repeated it. Driving high-entropy slugs of 2000 / 4000 / 9000 bytes
// through ImportWorkspace against Postgres 17 (the container the PG gate uses)
// with the coercion disabled gives:
//
//	2000 -> accepted
//	4000 -> ERROR: index row size 4056 exceeds btree version 4 maximum 2704
//	        for index "items_workspace_id_slug_key" (SQLSTATE 54000)
//	9000 -> ERROR: index row requires 9056 bytes, maximum size is 8191 (SQLSTATE 54000)
//
// So 2704 is what a regular index tuple is held to (a third of a page) and
// 8191 is the hard ceiling reached only past it. Both are post-compression:
// a REPETITIVE 9000-byte slug is accepted, which is why the filing's threshold
// was unquotable as a single number and why the test fixture uses a
// deterministic high-entropy slug rather than strings.Repeat.
//
// 255 runes therefore bounds the slug at ~255 bytes against the 2704 that
// actually fires: ~10x headroom, so compressibility stops mattering. The
// uniqueness suffix (-2, -3, ...) adds a handful of bytes and does not change
// that.
//
// RUNES, not bytes, because "255 characters" is what a user and a UI counter
// mean. The byte-level residual on the TITLE column is up to 4x this number,
// which the title column (TEXT) carries without complaint — the slug is the
// constrained derivative, and it is ASCII by construction.
//
// Enforced at WRITE time only, and deliberately NOT retroactive: an item whose
// stored title already exceeds the bound keeps working, and an update that does
// not set a title never validates one. Same grandfathering rule as documents
// (Dave's ruling, day-63).
//
// TWO PATHS DO NOT VALIDATE, both by ruling and both carrying legacy data
// rather than caller input (codex round 5 — an earlier version of this comment
// said every door normalizes then validates, which is not true of either):
//
//   - ImportWorkspace COERCES rather than refuses, so restoring an archive
//     cannot die on a row this product itself once accepted.
//   - Cross-workspace copy PROPAGATES the source row's title verbatim, for the
//     same reason; it takes no title from the caller, so it cannot mint one.
//
// The guarantee that holds across every path is therefore narrower than "every
// stored title satisfies this bound": no CALLER-SUPPLIED title is stored
// without being validated against it. Legacy titles at rest are out of scope
// here; making the bound global would be a count-then-repair sweep over
// existing rows, not a change to any door.
const MaxItemTitleRunes = 255

// NormalizeItemTitle is the canonical normalization for an item title:
// leading/trailing whitespace is not part of a title.
//
// It exists as its own function because normalize-then-validate must not drift
// between doors — the whole of BUG-2833 is create and update disagreeing about
// one field, and BUG-2831 is create and Postgres disagreeing about the same
// field.
//
// Every door that takes a title FROM A CALLER pairs this with
// ValidateItemTitle and stores what it validated, never the raw input. The two
// legacy-data paths do not, by ruling: ImportWorkspace coerces (it normalizes,
// then truncates or substitutes rather than refusing) and cross-workspace copy
// carries the source row's title through untouched. See MaxItemTitleRunes for
// both. An earlier version of this comment said "every door", which reads as
// covering those two and does not (codex round 6).
func NormalizeItemTitle(title string) string {
	return strings.TrimSpace(title)
}

// ValidateItemTitle checks an ALREADY-NORMALIZED item title at write time.
// Returns a message suitable for a 400 response, or "" when acceptable.
//
// Callers pass NormalizeItemTitle's output. Validating the normalized value is
// the point rather than an implementation detail: a title of "   " is untitled
// wearing a costume, and before this function existed it was legal on create
// (which tested title == "" exactly) and refused on artifact import (which
// trimmed first) — two doors, two rules, and the import door's comment claimed
// to mirror the create gate it was in fact stricter than.
//
// Deliberately NOT covering wikiTitleRoundTripFailure, which ValidateDocumentTitle
// does: the item rename cascade escapes its emission (BUG-2805), so an item
// title containing wiki-link syntax round-trips. Documents' cascade does not
// escape, which is why the check lives there and not here.
func ValidateItemTitle(title string) string {
	if title == "" {
		return "Title is required"
	}
	if n := utf8.RuneCountInString(title); n > MaxItemTitleRunes {
		return fmt.Sprintf("Title is too long: %d characters, maximum %d", n, MaxItemTitleRunes)
	}
	return ""
}
