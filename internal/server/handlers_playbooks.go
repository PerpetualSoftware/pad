package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

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
	Ref       string                    `json:"ref"`
	Slug      string                    `json:"slug"`
	Title     string                    `json:"title"`
	Body      string                    `json:"body"`
	Arguments []PlaybookArgumentSpec    `json:"arguments"`
	BoundArgs map[string]any            `json:"bound_args"`
	Unbound   []PlaybookUnboundArgument `json:"unbound,omitempty"`
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

// handleListPlaybooks returns playbook metadata for the workspace.
// Same shape as the bootstrap blob's `playbooks` field, hand-rolled
// here so callers can fetch it without pulling in the full bootstrap.
func (s *Server) handleListPlaybooks(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	// Reuse the same metadata projection as bootstrap. Visibility is
	// applied at the ListItems level: collIDs/itemIDs reflect the
	// caller's filtered view.
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	fullCollIDs, grantedItemIDs, err := s.guestResourceFilter(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	subCollIDs := visibleIDs
	var subItemIDs []string
	if len(grantedItemIDs) > 0 {
		subCollIDs = fullCollIDs
		subItemIDs = grantedItemIDs
	}
	meta, err := s.collectPlaybookMetadata(workspaceID, subCollIDs, subItemIDs)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
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
	item, err := s.resolvePlaybook(workspaceID, identifier)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}
	writeJSON(w, http.StatusOK, item)
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
	item, err := s.resolvePlaybook(workspaceID, identifier)
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
		Ref:       item.Ref,
		Slug:      item.Slug,
		Title:     item.Title,
		Body:      item.Content,
		Arguments: specs,
		BoundArgs: bound,
		Unbound:   unbound,
	})
}

// resolvePlaybook finds a playbook by either its invocation_slug, its
// item slug, or its issue ref. invocation_slug takes precedence — that's
// the user-facing identifier callers will type most often.
func (s *Server) resolvePlaybook(workspaceID, identifier string) (*models.Item, error) {
	// First, try invocation_slug. If we find an exact match, return it.
	bySlug, err := s.store.ListItems(workspaceID, models.ItemListParams{
		CollectionSlug: "playbooks",
		Fields:         map[string]string{"invocation_slug": identifier},
		Limit:          1,
	})
	if err != nil {
		return nil, err
	}
	if len(bySlug) == 1 {
		return &bySlug[0], nil
	}
	// Fall back to standard item resolution (UUID, ref, or item slug),
	// then verify it lives in the playbooks collection so a stray
	// TASK-5 doesn't surface here.
	item, err := s.store.ResolveItem(workspaceID, identifier)
	if err != nil || item == nil {
		return nil, fmt.Errorf("playbook %q not found", identifier)
	}
	if item.CollectionSlug != "playbooks" {
		return nil, fmt.Errorf("item %s is not a playbook", item.Ref)
	}
	return item, nil
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
