package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/go-chi/chi/v5"
)

// PlaybookRunResponse is the result of `POST /workspaces/{ws}/playbooks/{ref}/run`.
// The handler does NOT execute the playbook — playbooks are agent
// instructions, not shell scripts. The CLI/MCP/skill consumer parses
// the args, then the agent (the actual executor) follows the body's
// steps with those args bound.
//
// Shape is deliberately denormalized for the agent: the body is
// markdown ready to render, bound_args carries the parsed argument
// values keyed by name, and unbound carries required args the caller
// didn't supply (so the agent can prompt the user instead of failing
// the call outright).
type PlaybookRunResponse struct {
	Ref    string `json:"ref"`
	Slug   string `json:"slug"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Body   string `json:"body"`
	// ContentState marks `Body` above as one the server knows is BEHIND the
	// playbook item's live collaborative document (BUG-3033, the read-door half
	// of BUG-3000). Same values and the same meaning as models.Item.ContentState;
	// omitted whenever the row is current, so a consumer that does not know the
	// field sees byte-identical responses.
	//
	// This response does not serialise models.Item — it hoists four of its
	// values into a hand-built shape — so it inherits nothing, which is why the
	// field is repeated here rather than arriving for free. Its sibling
	// PlaybookShowResponse DOES embed *models.Item and therefore does get it for
	// free; TestPlaybookShowInheritsTheContentStateMarker pins that so the two
	// doors cannot silently diverge.
	//
	// It matters more on this door than on an ordinary read: a playbook body is
	// EXECUTED. A stale one is an agent running superseded steps, which is a
	// different kind of wrong from a stale document somebody reads.
	ContentState string                    `json:"content_state,omitempty"`
	Arguments    []PlaybookArgumentSpec    `json:"arguments"`
	BoundArgs    map[string]any            `json:"bound_args"`
	Unbound      []PlaybookUnboundArgument `json:"unbound,omitempty"`
}

// PlaybookShowResponse is the result of `GET /workspaces/{ws}/playbooks/{ref}`.
// It embeds the full item and hoists the playbook's status (which lives
// inside the item's fields JSON) to a top-level `status` key so callers
// can read the draft/active gate without reaching into fields. Embedding
// keeps the item's existing shape intact — models.Item has no top-level
// `status` field, so there's no collision.
type PlaybookShowResponse struct {
	*models.Item
	Status string `json:"status"`
}

// PlaybookArgumentSpec is one entry from the playbook's `arguments`
// field — the queryable form of the body's `## Arguments` section.
// Five types are supported per the PLAN-1377 design: ref, string,
// flag, enum, number. Default values are passed through opaquely; the
// agent decides what to do with non-literal defaults like "current
// git branch".
type PlaybookArgumentSpec struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Default     any      `json:"default,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// PlaybookUnboundArgument tells the caller which required args the run
// invocation didn't supply, so the agent can fall back to prompting
// the user instead of refusing the call.
type PlaybookUnboundArgument struct {
	Name string               `json:"name"`
	Spec PlaybookArgumentSpec `json:"spec"`
}

// listPlaybookItems gathers the workspace's playbook items, scoped to what
// the caller can see. Shared by handleListPlaybooks and handleMatchPlaybook
// (TASK-3120) so the two can never disagree about which items a caller's
// playbook catalog contains — the same reasoning as the copy/preflight pair
// in CLAUDE.md's relation-field notes: two doors, one function, because they
// live in different handlers and that is how they'd otherwise drift.
//
// needsContent controls whether item bodies are fetched: callers that derive
// a playbook's summary from its markdown (list, match) need it; a caller
// that only wants ref/title/status does not.
func (s *Server) listPlaybookItems(r *http.Request, workspaceID string, needsContent bool) ([]models.Item, error) {
	// Visibility is applied at the ListItems level: collIDs/itemIDs reflect
	// the caller's filtered view.
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		return nil, err
	}
	fullCollIDs, grantedItemIDs, err := s.guestResourceFilter(r, workspaceID)
	if err != nil {
		return nil, err
	}
	subCollIDs := visibleIDs
	var subItemIDs []string
	if len(grantedItemIDs) > 0 {
		subCollIDs = fullCollIDs
		subItemIDs = grantedItemIDs
	}
	// Same trait-driven source as the bootstrap's playbooks payload, so the
	// two can never disagree about which collection holds playbooks — and so
	// `pad playbook list` keeps working when that collection is renamed
	// ([[BUG-2702]]). TASK-2657.
	traited, err := s.store.ListTraitedCollections(workspaceID)
	if err != nil {
		return nil, err
	}
	return s.collectBootstrapSourceItems(workspaceID, traited, bootstrapKeyPlaybooks, needsContent, visibleIDs, subCollIDs, subItemIDs)
}

// handleListPlaybooks returns playbook metadata for the workspace.
// Same shape as the bootstrap blob's `playbooks` field, hand-rolled
// here so callers can fetch it without pulling in the full bootstrap.
func (s *Server) handleListPlaybooks(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	items, err := s.listPlaybookItems(r, workspaceID, true)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectPlaybookMetadata(items))
}

const (
	// maxPlaybookMatchTextRunes bounds the free text this endpoint sends to
	// the provider. This is a short intent string — a user request, an
	// agent action — not a document body, so it is bounded far tighter than
	// decision/runner.go's maxStateBodyRunes (16000), which bounds a full
	// item body.
	maxPlaybookMatchTextRunes = 4000

	// playbookMatchNoneOption is the reserved Choice option meaning "the
	// text does not ask for any of the playbooks offered". Without it a
	// Choice question forces a pick among the playbooks sent even when the
	// text asks for none of them, and (as a side effect) a workspace with
	// exactly one active playbook would have no legal question to ask —
	// decision.Question.Validate refuses a Choice with fewer than 2 options.
	// One active playbook plus "none" is 2, which is legal.
	playbookMatchNoneOption = "none"

	// playbookMatchTimeout bounds the SYNCHRONOUS provider call this
	// endpoint makes — an HTTP caller (viewer, CLI, MCP) blocks on it
	// directly, unlike the async decision tick (decision_tick.go), which
	// can afford the provider's full worst-case retry budget because
	// nothing is waiting on it. That budget is real: typesafe.go's client
	// already times out each HTTP attempt at requestTimeout (60s), but its
	// retry loop can chain up to maxAttempts=4 attempts with up to
	// maxRetryAfter=30s between them — a worst case of 4*60 + 3*30 = 330s
	// (decision_tick.go carries the identical receipt for that path). 30s
	// here is generous next to the eval's measured ~70-500ms latency and
	// covers one slow attempt plus its first backoff, but stops well short
	// of the full retry ladder. No prior synchronous provider call exists
	// to inherit a bound from — this is a fresh judgment call for this
	// endpoint, not an established precedent.
	playbookMatchTimeout = 30 * time.Second
)

// PlaybookMatchResponse is the result of `POST /workspaces/{ws}/playbooks/match`
// (PLAN-3114 unit 5, TASK-3120): a typed-decision Choice over the workspace's
// caller-visible ACTIVE playbooks, plus a reserved "none" option so text that
// doesn't ask for any of them gets an honest answer instead of a forced pick.
// Read-only, synchronous, stores nothing and enqueues nothing — see
// [decision.Runner.Provider]'s doc comment for why this bypasses the
// owed-jobs pipeline [decision.Runner.Evaluate] drives.
type PlaybookMatchResponse struct {
	// Choice is the provider's pick: a playbook ref from Options, or the
	// literal "none". Empty (with Reason set) only when there were zero
	// active playbooks to choose among.
	Choice string `json:"choice,omitempty"`
	// Reason explains an empty Choice. The only value today is
	// "no_active_playbooks": the caller-visible workspace has none, so no
	// provider call was made — a Choice needs at least two options, and
	// "none" alone isn't one.
	Reason string `json:"reason,omitempty"`
	// Confidence is the provider's confidence in Choice. Present whenever
	// Choice is (a Choice answer always carries one — see [decision.Answer]).
	Confidence *float64 `json:"confidence,omitempty"`
	// Probabilities is the full per-option distribution the provider
	// returned — every key in Options plus "none" — exactly as received,
	// unfiltered.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Model is the provider's pinned model identifier.
	Model string `json:"model,omitempty"`
	// Options lists every active playbook the provider was asked to choose
	// among — enough identity to act on Choice without a second lookup.
	// Never includes the reserved "none" entry, which carries no identity.
	Options []PlaybookMatchOption `json:"options"`
}

// PlaybookMatchOption is one playbook's identity as offered to the provider.
type PlaybookMatchOption struct {
	Ref            string `json:"ref"`
	Title          string `json:"title"`
	InvocationSlug string `json:"invocation_slug,omitempty"`
}

// playbookMatchOptions filters items to active playbooks (draft and
// deprecated are excluded — the same gate `pad playbook run` enforces
// server-side, so match never offers a playbook running it would refuse) and
// builds both the decision.Choice options (ref -> description, for the
// provider) and the identity list (for the response).
//
// Option description = title + first-paragraph summary (BUG-3033's
// PlaybookSummary — same derivation `pad playbook list` uses), falling back
// to the title alone when the body yields no summary: decision.Question.
// Validate refuses an empty option description, and an item's title is
// never empty (models.ValidateItemTitle). Trigger and invocation_slug, when
// present, ride in a trailing parenthetical rather than as more sentences —
// the summary's own trailing punctuation is caller-written prose the
// builder doesn't control, and appending ". Trigger: ..." after a summary
// that already ends in "." reads as a stutter ("it.. Trigger:").
func playbookMatchOptions(items []models.Item) (map[string]string, []PlaybookMatchOption) {
	choiceOptions := map[string]string{}
	var options []PlaybookMatchOption
	for i := range items {
		it := &items[i]
		if !strings.EqualFold(playbookStatus(it), "active") {
			continue
		}
		var fields map[string]any
		if it.Fields != "" {
			_ = json.Unmarshal([]byte(it.Fields), &fields)
		}
		invocationSlug, _ := fields["invocation_slug"].(string)
		trigger, _ := fields["trigger"].(string)
		summary := collections.PlaybookSummary(it.Content)

		var b strings.Builder
		b.WriteString(it.Title)
		if summary != "" {
			b.WriteString(": ")
			b.WriteString(summary)
		}
		var meta []string
		if trigger != "" {
			meta = append(meta, "trigger: "+trigger)
		}
		if invocationSlug != "" {
			meta = append(meta, `invoke via slug "`+invocationSlug+`"`)
		}
		if len(meta) > 0 {
			b.WriteString(" (")
			b.WriteString(strings.Join(meta, "; "))
			b.WriteString(")")
		}
		choiceOptions[it.Ref] = b.String()
		options = append(options, PlaybookMatchOption{
			Ref:            it.Ref,
			Title:          it.Title,
			InvocationSlug: invocationSlug,
		})
	}
	return choiceOptions, options
}

// handleMatchPlaybook answers "does this text ask for one of the workspace's
// active playbooks" as a typed-decision Choice (PLAN-3114 unit 5, TASK-3120).
// Read-only and side-effect-free: no job is enqueued, no row is stored, and a
// repeat call is safe.
//
// Draft and deprecated playbooks are excluded from the options, matching the
// skill's own activation rule. Zero active playbooks answers 200 with
// choice="" and a reason — that is a legitimate answer ("nothing to match
// against"), not an error. Over the provider's option ceiling refuses rather
// than silently dropping playbooks, which would answer from an incomplete
// set. A provider outage or an out-of-set answer both refuse with
// decision_provider_error — distinct from decision_provider_unavailable
// (no provider configured at all), so a caller can tell "try slug/trigger
// routing instead" (unavailable) apart from "something went wrong upstream"
// (error) rather than reading one as the other.
func (s *Server) handleMatchPlaybook(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	var input struct {
		Text string `json:"text"`
	}
	// EOF (an empty body) is not a decode failure here — it just means no
	// text was sent, and the empty-text check below reports that with a
	// clearer message than a JSON decode error would.
	if err := decodeJSON(r, &input); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if strings.TrimSpace(input.Text) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "text is required")
		return
	}
	if n := utf8.RuneCountInString(input.Text); n > maxPlaybookMatchTextRunes {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("text is too long: %d characters, maximum %d", n, maxPlaybookMatchTextRunes))
		return
	}

	runner := s.decisionRunner()
	if runner == nil {
		writeError(w, http.StatusNotFound, "decision_provider_unavailable",
			"no typed-decision provider is configured — fall back to slug/trigger routing")
		return
	}
	provider := runner.Provider()

	items, err := s.listPlaybookItems(r, workspaceID, true)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	choiceOptions, options := playbookMatchOptions(items)
	sort.Slice(options, func(i, j int) bool { return options[i].Ref < options[j].Ref })

	if len(options) == 0 {
		writeJSON(w, http.StatusOK, PlaybookMatchResponse{
			Reason:  "no_active_playbooks",
			Options: []PlaybookMatchOption{},
		})
		return
	}
	choiceOptions[playbookMatchNoneOption] = "the text does not ask for any of the other options' procedures"
	// decision.MaxChoiceOptions is the provider's documented ceiling,
	// INCLUDING the reserved "none" entry — reading the exported constant
	// rather than a copied literal means this refusal and
	// decision.Question.Validate's own cannot drift apart (TASK-3120 review).
	if len(choiceOptions) > decision.MaxChoiceOptions {
		writeError(w, http.StatusBadRequest, "too_many_playbooks",
			fmt.Sprintf("%d active playbooks (+1 for %q) exceed the provider's %d-option limit", len(options), playbookMatchNoneOption, decision.MaxChoiceOptions))
		return
	}

	q := decision.Choice(
		"Given the text, does it ask for one of these procedures to be run? Pick the option that best matches what the text is asking for, or \"none\" if it does not ask for any of them.",
		choiceOptions,
	)

	ctx, cancel := context.WithTimeout(r.Context(), playbookMatchTimeout)
	defer cancel()
	answers, _, err := provider.Ask(ctx, input.Text, map[string]decision.Question{"match": q})
	if err != nil {
		// A request refused BEFORE it reached the provider (the question set
		// alone busts the token budget, or the provider's own after-the-fact
		// max_tokens_exceeded) is a caller-fixable input problem, not an
		// outage — distinct 400, never the generic provider-error path below
		// (TASK-3120 review F2).
		if errors.Is(err, decision.ErrRequestTooLarge) || errors.Is(err, decision.ErrMaxTokensExceeded) {
			writeError(w, http.StatusBadRequest, "match_request_too_large",
				"too many active playbooks or too much text for one provider request — try shorter text or fewer active playbooks")
			return
		}
		writeDecisionProviderError(w, workspaceID, err)
		return
	}
	answer, ok := answers["match"]
	if !ok || answer.Kind != decision.KindChoice {
		writeError(w, http.StatusBadGateway, "decision_provider_error", "provider returned no choice answer")
		return
	}
	// Never pass an unknown value through: the provider named an option it
	// was never offered.
	if _, known := choiceOptions[answer.Choice]; !known {
		writeError(w, http.StatusBadGateway, "decision_provider_error",
			fmt.Sprintf("provider returned an unrecognized choice %q", answer.Choice))
		return
	}

	writeJSON(w, http.StatusOK, PlaybookMatchResponse{
		Choice:        answer.Choice,
		Confidence:    answer.Confidence,
		Probabilities: answer.Probabilities,
		Model:         provider.Model(),
		Options:       options,
	})
}

// writeDecisionProviderError answers a genuine provider-side failure — a
// network error, a timeout, or the provider's own non-budget refusal — with
// a FIXED message and a coarse `details.reason`, never the raw error
// (TASK-3120 review F1). typesafe.go's client surfaces the provider's 4xx
// response body VERBATIM in err.Error() (APIError.Error()), and that body is
// the INSTANCE's own account state with the provider — key validity, quota,
// billing wording — not anything about the caller's request. This endpoint
// requires only viewer access, so echoing it would hand any workspace member
// a window into the instance admin's provider account. The full error is
// logged server-side instead, where only someone who can read server logs
// sees it.
//
// reason is "timeout" for a context deadline (this endpoint's own
// playbookMatchTimeout, or the caller's request context being cancelled) and
// "upstream" for everything else — enough for a caller to decide whether
// retrying later is worth it, without any provider-specific detail.
func writeDecisionProviderError(w http.ResponseWriter, workspaceID string, err error) {
	slog.Error("playbook match: provider error", "workspace", workspaceID, "error", err)
	reason := "upstream"
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "timeout"
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{
		"error": map[string]any{
			"code":    "decision_provider_error",
			"message": "the typed-decision provider failed to answer; try again",
			"details": map[string]any{"reason": reason},
		},
	})
}

// handleShowPlaybook returns the full playbook item identified by ref,
// slug, OR invocation_slug. The resolver walks both invocation_slug
// (the user-facing identifier) and item slug/ref (the workspace-wide
// identifier) so `pad playbook show ship` works whether `ship` is the
// invocation slug or the item ref.
func (s *Server) handleShowPlaybook(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	identifier := chi.URLParam(r, "ref")
	if identifier == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "ref required")
		return
	}
	resolveVisibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}
	item, err := s.resolvePlaybook(workspaceID, identifier, resolveVisibleIDs)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}
	writeJSON(w, http.StatusOK, PlaybookShowResponse{
		Item:   item,
		Status: playbookStatus(item),
	})
}

// handleRunPlaybook parses the caller's args against the playbook's
// declared argument spec and returns the rendered body + bound args.
// Side-effect-free: it does NOT execute the playbook — it just primes
// the agent.
func (s *Server) handleRunPlaybook(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	identifier := chi.URLParam(r, "ref")
	if identifier == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "ref required")
		return
	}
	resolveVisibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}
	item, err := s.resolvePlaybook(workspaceID, identifier, resolveVisibleIDs)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}

	// Run accepts EITHER pre-parsed args (from MCP / web clients that
	// already have the key/value map) OR raw CLI tokens (positional,
	// bareword flags, key=value). When raw_args is non-empty the server
	// applies the same strict parsing rules `pad playbook run` uses, so
	// the CLI doesn't need to duplicate the logic and so any drift is
	// impossible.
	var input struct {
		Args    map[string]any `json:"args"`
		RawArgs []string       `json:"raw_args"`
		// AllowDraft is the escape hatch for the draft-playbook gate.
		// When true, a playbook whose status != "active" (e.g. a draft
		// still being authored) may still be run. Left false, the gate
		// below rejects non-active playbooks with a structured error so
		// agents don't silently execute half-written procedures.
		AllowDraft bool `json:"allow_draft"`
	}
	// decodeJSON wraps the underlying error as "invalid JSON: ..." so an
	// EOF check on the wrapped message is unreliable. Use errors.Is on
	// the unwrapped chain so a zero-length body (which is a legitimate
	// "no args" call) is accepted regardless of how decodeJSON labels
	// the wrapper. http.NoBody as well as a closed reader both surface
	// io.EOF; either is fine to treat as "empty request".
	if err := decodeJSON(r, &input); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// The escape hatch can also arrive as a bareword token in raw_args
	// (the shape the ExecDispatcher-backed MCP path forwards). Pull it
	// out BEFORE the strict parser runs so it isn't rejected as an
	// unknown argument, and OR it into the decoded flag.
	input.RawArgs, input.AllowDraft = extractAllowDraft(input.RawArgs, input.AllowDraft)

	// Draft-playbook gate. Playbooks are meant to be executed only once
	// they're marked active; a draft is still being authored. The gate
	// lived only in the agent skill before (unenforced server-side) —
	// enforce it here so any surface (CLI, MCP, direct HTTP) is covered.
	status := playbookStatus(item)
	if !strings.EqualFold(status, "active") && !input.AllowDraft {
		writeError(w, http.StatusConflict, "playbook_not_active",
			fmt.Sprintf("playbook %s has status %q, not \"active\" — refusing to run it. Pass allow_draft (CLI: --allow-draft) to run it anyway.",
				item.Ref, defaultStatus(status)))
		return
	}

	specs, _, err := parsePlaybookArguments(item.Fields)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	supplied := input.Args
	if len(input.RawArgs) > 0 {
		parsed, perr := ParsePlaybookCLIArgs(input.RawArgs, specs)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "bad_request", perr.Error())
			return
		}
		// Merge: explicit Args take precedence over RawArgs so a
		// caller that passes both can override.
		if supplied == nil {
			supplied = parsed
		} else {
			for k, v := range parsed {
				if _, present := supplied[k]; !present {
					supplied[k] = v
				}
			}
		}
	}

	bound, unbound := bindPlaybookArgs(specs, supplied)

	writeJSON(w, http.StatusOK, PlaybookRunResponse{
		Ref:          item.Ref,
		Slug:         item.Slug,
		Title:        item.Title,
		Status:       status,
		Body:         item.Content,
		ContentState: item.ContentState,
		Arguments:    specs,
		BoundArgs:    bound,
		Unbound:      unbound,
	})
}

// playbookStatus reads the `status` value out of a playbook item's
// fields JSON. Status lives in the typed fields blob, not as a
// top-level Item column, so callers that need the draft/active gate go
// through here. Returns "" when the field is absent or unparseable.
func playbookStatus(item *models.Item) string {
	if item == nil || item.Fields == "" {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(item.Fields), &fields); err != nil {
		return ""
	}
	if s, ok := fields["status"].(string); ok {
		return s
	}
	return ""
}

// defaultStatus renders an empty status as a readable placeholder for
// error messages.
func defaultStatus(status string) string {
	if status == "" {
		return "(unset)"
	}
	return status
}

// extractAllowDraft removes any `allow-draft` / `allow_draft` bareword
// token from the raw CLI args and reports whether one was present. The
// returned bool is ORed with the caller's existing flag so an explicit
// allow_draft body field and a bareword token are equivalent. This lets
// the ExecDispatcher-backed MCP path forward the escape hatch as a
// bareword without the strict argument parser rejecting it as unknown.
func extractAllowDraft(rawArgs []string, current bool) ([]string, bool) {
	if len(rawArgs) == 0 {
		return rawArgs, current
	}
	out := rawArgs[:0:0]
	found := false
	for _, tok := range rawArgs {
		if tok == "allow-draft" || tok == "allow_draft" {
			found = true
			continue
		}
		out = append(out, tok)
	}
	return out, current || found
}

// resolvePlaybook finds a playbook by either its invocation_slug, its
// item slug, or its issue ref. invocation_slug takes precedence — that's
// the user-facing identifier callers will type most often.
// Which collections participate is resolved from the invocation_field trait
// (SPEC-5), not from the literal slug "playbooks": renaming the collection
// used to unregister every invokable playbook at once, so `/pad ship` fell
// through to natural-language routing with no sign the playbook still existed
// ([[BUG-2702]]). TASK-2657.
func (s *Server) resolvePlaybook(workspaceID, identifier string, visibleCollIDs []string) (*models.Item, error) {
	traited, err := s.store.ListTraitedCollections(workspaceID)
	if err != nil {
		return nil, err
	}
	routing := collections.FindByInvocationField(traited)
	// Consider only collections the CALLER can see. Before traits this was
	// structurally impossible — resolution named one collection — but any
	// number may now declare invocation_field, and resolving across all of
	// them then rejecting on visibility lets a hidden collection SHADOW a
	// visible one: the resolver returns the hidden item, the caller's
	// visibility check refuses it, and the visible playbook with the same
	// invocation slug becomes unreachable. Filtering first makes the hidden
	// collection invisible to resolution rather than merely unreadable, so
	// shadowing cannot occur and no 404 is contingent on a row the caller was
	// never allowed to know about. Codex round 2.
	//
	// nil visibleCollIDs means an unrestricted caller (full member) — the
	// same convention every other bootstrap/list path in this package uses.
	if visibleCollIDs != nil {
		filtered := routing[:0:0]
		for _, c := range routing {
			if isCollectionVisible(c.ID, visibleCollIDs) {
				filtered = append(filtered, c)
			}
		}
		routing = filtered
	}
	if len(routing) == 0 {
		return nil, fmt.Errorf("playbook %q not found: this workspace has no collection that routes by invocation slug", identifier)
	}

	// First, try the invocation field. If we find an exact match, return it.
	// The FIELD NAME comes from the declaration; v1 constrains it to
	// invocation_slug so it stays covered by the partial unique indexes that
	// guard uniqueness (SPEC-5 v1.1 amendment 4).
	for _, coll := range routing {
		bySlug, err := s.store.ListItems(workspaceID, models.ItemListParams{
			CollectionSlug: coll.Slug,
			Fields:         map[string]string{coll.Traits.InvocationField: identifier},
			Limit:          1,
		})
		if err != nil {
			return nil, err
		}
		if len(bySlug) == 1 {
			return &bySlug[0], nil
		}
	}

	// Fall back to standard item resolution (UUID, ref, or item slug),
	// then verify it lives in a routing collection so a stray
	// TASK-5 doesn't surface here.
	item, err := s.store.ResolveItem(workspaceID, identifier)
	if err != nil || item == nil {
		return nil, fmt.Errorf("playbook %q not found", identifier)
	}
	for _, coll := range routing {
		if item.CollectionSlug == coll.Slug {
			return item, nil
		}
	}
	return nil, fmt.Errorf("item %s is not a playbook", item.Ref)
}

// parsePlaybookArguments pulls the `arguments` field out of the
// playbook item's fields JSON and decodes it into a list of
// PlaybookArgumentSpec entries. Returns (specs, raw, error) where raw
// is the JSON-decoded original so callers can fall back to it for
// unknown shapes. A nil/missing `arguments` field decodes to an empty
// spec list (no arguments declared).
func parsePlaybookArguments(fieldsJSON string) ([]PlaybookArgumentSpec, any, error) {
	if fieldsJSON == "" {
		return nil, nil, nil
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		return nil, nil, fmt.Errorf("parse playbook fields: %w", err)
	}
	raw, ok := fields["arguments"]
	if !ok || raw == nil {
		return nil, nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, raw, fmt.Errorf("playbook arguments field is not an array; got %T", raw)
	}
	specs := make([]PlaybookArgumentSpec, 0, len(arr))
	for i, entry := range arr {
		m, ok := entry.(map[string]any)
		if !ok {
			return nil, raw, fmt.Errorf("playbook argument [%d] is not an object", i)
		}
		spec := PlaybookArgumentSpec{}
		if name, ok := m["name"].(string); ok {
			spec.Name = name
		}
		if t, ok := m["type"].(string); ok {
			spec.Type = t
		}
		if req, ok := m["required"].(bool); ok {
			spec.Required = req
		}
		spec.Default = m["default"]
		if d, ok := m["description"].(string); ok {
			spec.Description = d
		}
		if enum, ok := m["enum"].([]any); ok {
			for _, e := range enum {
				if s, ok := e.(string); ok {
					spec.Enum = append(spec.Enum, s)
				}
			}
		}
		specs = append(specs, spec)
	}
	return specs, raw, nil
}

// bindPlaybookArgs walks the declared arg specs and binds the caller's
// supplied values. Unsupplied required args land in the `unbound`
// list so the agent can prompt the user instead of failing.
// Unsupplied optional args are filled with their declared default
// when present; otherwise omitted.
//
// Flag-typed args are special-cased: presence (true) wins over absence.
// A caller that sent `{stop-after-each: false}` still gets that value.
func bindPlaybookArgs(specs []PlaybookArgumentSpec, supplied map[string]any) (map[string]any, []PlaybookUnboundArgument) {
	bound := make(map[string]any, len(specs))
	var unbound []PlaybookUnboundArgument
	for _, spec := range specs {
		val, present := supplied[spec.Name]
		if !present {
			if spec.Default != nil {
				bound[spec.Name] = spec.Default
				continue
			}
			if spec.Type == "flag" {
				// Flag types default to false when absent.
				bound[spec.Name] = false
				continue
			}
			if spec.Required {
				unbound = append(unbound, PlaybookUnboundArgument{
					Name: spec.Name,
					Spec: spec,
				})
			}
			continue
		}
		bound[spec.Name] = val
	}
	return bound, unbound
}

// ParsePlaybookCLIArgs splits a sequence of positional args + bareword
// flags + key=value pairs into a typed map matching the playbook's
// declared spec. Exposed for the CLI side (`pad playbook run`) and the
// MCP run action — both follow the same strict rules:
//
//   - Required positional args first, in declared order.
//   - `flag` types: bareword presence sets the value to true.
//   - All other types: `key=value` form.
//
// Unknown tokens return an error with a hint about which arg names
// are valid. The CLI surfaces this back to the user.
func ParsePlaybookCLIArgs(args []string, specs []PlaybookArgumentSpec) (map[string]any, error) {
	out := make(map[string]any)
	specByName := make(map[string]PlaybookArgumentSpec, len(specs))
	flagSet := make(map[string]bool, len(specs))
	for _, s := range specs {
		specByName[s.Name] = s
		if s.Type == "flag" {
			flagSet[s.Name] = true
		}
	}

	// Walk required-positional specs in order. They consume args from
	// the front until either they're all filled or the next token
	// looks like a key=value/flag.
	positionalIdx := 0
	for _, tok := range args {
		if strings.Contains(tok, "=") {
			eq := strings.IndexByte(tok, '=')
			key := tok[:eq]
			val := tok[eq+1:]
			spec, known := specByName[key]
			if !known {
				return nil, fmt.Errorf("unknown argument %q", key)
			}
			if spec.Type == "flag" {
				return nil, fmt.Errorf("argument %q is a flag — use bareword presence, not key=value", key)
			}
			coerced, err := coercePlaybookValue(val, spec)
			if err != nil {
				return nil, err
			}
			out[key] = coerced
			continue
		}
		if flagSet[tok] {
			out[tok] = true
			continue
		}
		// Treat as next positional fill. Per the PLAN-1377 contract,
		// ONLY required args are positional — optional args must be
		// supplied via key=value. Skip past flag-typed, optional, and
		// already-bound slots so a bareword token can't accidentally
		// land on an optional spec that happens to sit before a
		// required one in the schema.
		for positionalIdx < len(specs) && (specs[positionalIdx].Type == "flag" || !specs[positionalIdx].Required || hasValue(out, specs[positionalIdx].Name)) {
			positionalIdx++
		}
		if positionalIdx >= len(specs) {
			return nil, fmt.Errorf("unexpected positional token %q (no remaining positional slots)", tok)
		}
		spec := specs[positionalIdx]
		coerced, err := coercePlaybookValue(tok, spec)
		if err != nil {
			return nil, err
		}
		out[spec.Name] = coerced
		positionalIdx++
	}
	return out, nil
}

func hasValue(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

// coercePlaybookValue converts a raw CLI token into the typed Go value
// the spec describes. ref/string pass through; number is parsed as
// float64; enum is validated against the declared options.
func coercePlaybookValue(raw string, spec PlaybookArgumentSpec) (any, error) {
	switch spec.Type {
	case "number":
		// strconv.ParseFloat validates the entire token (Sscanf with %g
		// would accept "1abc" as 1) AND lets us reject NaN/Inf, which
		// would otherwise blow up json.Marshal later in the request.
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("argument %q expects a finite number; got %q", spec.Name, raw)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("argument %q must be a finite number; got %q", spec.Name, raw)
		}
		return f, nil
	case "enum":
		if len(spec.Enum) == 0 {
			return raw, nil
		}
		for _, opt := range spec.Enum {
			if opt == raw {
				return raw, nil
			}
		}
		return nil, fmt.Errorf("argument %q must be one of %v; got %q", spec.Name, spec.Enum, raw)
	default:
		return raw, nil
	}
}
