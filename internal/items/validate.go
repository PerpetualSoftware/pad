package items

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// patternCache memoizes compiled regexes so repeat validations don't pay the
// re-compile cost. Schemas change rarely; the entries are tiny.
var (
	patternCache   = make(map[string]*regexp.Regexp)
	patternCacheMu sync.RWMutex
)

func compilePattern(pat string) (*regexp.Regexp, error) {
	patternCacheMu.RLock()
	re, ok := patternCache[pat]
	patternCacheMu.RUnlock()
	if ok {
		return re, nil
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, err
	}
	patternCacheMu.Lock()
	patternCache[pat] = re
	patternCacheMu.Unlock()
	return re, nil
}

// FieldIssueKind classifies a single field-level validation failure.
//
// The two kinds want different treatment from a caller that is building a
// user-facing form rather than just rejecting a request: a IssueRequired
// field needs a value supplied, while a IssueInvalid field HAS a value and
// it is wrong. PLAN-2357's copy preflight buckets them separately, and the
// distinction is impossible to recover from ValidateFields' joined error
// string without parsing English.
type FieldIssueKind string

const (
	// IssueRequired — the schema declares the field required, it is absent
	// (or nil) in the map, and the schema supplies no default to fill it.
	IssueRequired FieldIssueKind = "required"

	// IssueInvalid — the field HAS a value and that value fails the
	// schema's type / options / pattern / range rules.
	IssueInvalid FieldIssueKind = "invalid"
)

// FieldIssue is one field-level validation failure, attributable to a key.
type FieldIssue struct {
	// Key is the schema field key the failure belongs to. Always set —
	// that is the point of this type.
	Key string
	// Kind is IssueRequired or IssueInvalid.
	Kind FieldIssueKind
	// Message is the human-readable explanation. For IssueInvalid it is
	// validateFieldType's error text verbatim, so it stays in step with
	// the single-error path.
	Message string
}

// ValidateFields checks field values against the collection schema.
// It validates required fields are present, types are correct, and select
// values are within the allowed options. It applies defaults for missing
// optional fields, mutating the fields map in place.
//
// It is a thin wrapper over ValidateFieldsDetailed that joins the issues
// into the historical single-error string. Both share one traversal so the
// two surfaces can never disagree about what is valid.
func ValidateFields(fields map[string]any, schema models.CollectionSchema) error {
	_, err := ValidateFieldsWithDrops(fields, schema)
	return err
}

// ValidateFieldsWithDrops is ValidateFields plus the discarded-default list
// (BUG-3079). For a door that has a `warnings.dropped_fields` channel; a door
// without one keeps calling ValidateFields, which discards the list rather than
// storing the value.
func ValidateFieldsWithDrops(
	fields map[string]any,
	schema models.CollectionSchema,
) ([]string, error) {
	issues, dropped := ValidateFieldsDetailedWithDrops(fields, schema)
	if len(issues) == 0 {
		return dropped, nil
	}
	errs := make([]string, 0, len(issues))
	for _, iss := range issues {
		errs = append(errs, iss.Message)
	}
	return dropped, fmt.Errorf("field validation failed: %s", strings.Join(errs, "; "))
}

// ValidateFieldsDetailed is ValidateFields with per-field attribution:
// it returns one FieldIssue per failure instead of a joined error, and
// applies schema defaults to `fields` in place exactly as ValidateFields
// does (that mutation is the same traversal, not a second pass).
//
// It runs ONE mutation before the traversal: `normalizeEmptyRelationLists`,
// which turns an empty `multi_relation` array into an absent key so that the
// required-field branch below refuses it without needing to know the type. It
// is stated here because a reader who has just been told the mutation is the
// traversal would otherwise be surprised by it.
//
// Issues come back in SCHEMA ORDER, which makes the result deterministic
// for a given (fields, schema) pair — repeated calls with equal input
// produce byte-identical output. PLAN-2357's dry-run preflight leans on
// that: it is specified to be safe to call repeatedly and to return
// identical results, and a map-iteration-ordered issue list would break
// that promise on every other call.
//
// A nil return means valid. Callers that only need the boolean should
// keep using ValidateFields.
func ValidateFieldsDetailed(fields map[string]any, schema models.CollectionSchema) []FieldIssue {
	issues, _ := ValidateFieldsDetailedWithDrops(fields, schema)
	return issues
}

// ValidateFieldsDetailedWithDrops is ValidateFieldsDetailed plus the list of
// schema-declared keys whose INJECTED DEFAULT was discarded (BUG-3079).
//
// THE DEFECT THIS CLOSES. A default used to be assigned and `continue`d past
// `validateFieldType`, so the same bytes were refused through one door and
// stored through the other, decided only by who put them there:
//
//	POST .../items {"fields":"{\"status\":\"open\"}"} -> 400 must be an array of strings
//	POST .../items {"fields":"{}"}                    -> 201 stored {"status":"open"}
//
// on a `status` retyped to `multi_select` whose scalar default survived the
// retype. The second stored a scalar in a list field, and defending against
// exactly that is what BUG-3016, BUG-3057, BUG-3067, BUG-3068 and BUG-3074 have
// each been doing one surface at a time.
//
// DROP RATHER THAN REFUSE, ruled on the BUG-3079 trail. Nobody in the request
// typed the value, and refusing would make a whole collection uncreatable —
// through surfaces that never mention the field — over a schema defect its
// author has to fix elsewhere. It is the disposition TASK-2878 already chose
// for the one case that had its own pass, now applied to every type.
//
// A REQUIRED FIELD IS THE EXCEPTION, and it is not a softening of the rule but
// the same rule finishing its sentence: dropping leaves the field absent, and
// an absent required field is precisely what IssueRequired reports. The message
// names the default as the cause, because "field is required" pointing at a
// request that never mentioned the field sends its reader to the wrong place.
//
// WHY A SECOND FUNCTION rather than a wider return on the existing one: eight
// call sites take the current shape and only the doors with a warnings channel
// can do anything with a drop list. The rest keep calling the wrapper above,
// which is honest for them — a preflight and a probe have nowhere to report it
// — and none of them stores the discarded value either way.
func ValidateFieldsDetailedWithDrops(
	fields map[string]any,
	schema models.CollectionSchema,
) (issues []FieldIssue, droppedDefaults []string) {
	normalizeEmptyRelationLists(fields, schema, false)

	for _, def := range schema.Fields {
		val, exists := fields[def.Key]

		// Apply default if field is missing and a default is defined
		if !exists || val == nil {
			// BUG-3028: a blank default on a scalar relation IS "no default" —
			// the key absent is the only stored form of "no target", so there is
			// nothing to inject. Injecting it would satisfy `required` here and
			// then be normalised away after validation, landing a required
			// relation with no target (codex round 1).
			blankRelationDefault := def.Type == "relation" && IsBlankRelationValue(def.Default)
			if def.Default == nil || blankRelationDefault {
				if def.Required {
					msg := fmt.Sprintf("field %q is required", def.Key)
					if blankRelationDefault {
						msg = fmt.Sprintf("field %q is required and its schema default is blank, which names no target", def.Key)
					}
					issues = append(issues, FieldIssue{
						Key:     def.Key,
						Kind:    IssueRequired,
						Message: msg,
					})
				}
				continue
			}

			// The injected default takes the SAME check a supplied value
			// takes. Deliberately the same call, not a parallel one: a
			// second implementation is how the two came to disagree.
			if err := validateFieldType(def, def.Default); err != nil {
				// Leave the key ABSENT rather than storing a value the
				// schema's own rules reject. `delete` rather than skipping
				// the assignment because the key can be present-and-nil
				// here, which downstream reads as a stored null.
				delete(fields, def.Key)
				droppedDefaults = append(droppedDefaults, def.Key)
				if def.Required {
					issues = append(issues, FieldIssue{
						Key:  def.Key,
						Kind: IssueRequired,
						Message: fmt.Sprintf(
							"field %q is required and its schema default was discarded: %s",
							def.Key, err.Error()),
					})
				}
				continue
			}
			fields[def.Key] = def.Default
			continue
		}

		// Validate by type
		if err := validateFieldType(def, val); err != nil {
			issues = append(issues, FieldIssue{
				Key:     def.Key,
				Kind:    IssueInvalid,
				Message: err.Error(),
			})
		}
	}

	return issues, droppedDefaults
}

// normalizeEmptyRelationLists rewrites an EMPTY `multi_relation` array into
// whichever spelling of "no targets" the caller's write shape already has
// (PLAN-2857 U4, lead ruling day 64 on codex round 1's P17).
//
// ONE SITE, deliberately, and it is the reason this lives here rather than in
// the doors: the defect codex round 1 found was a whole CLASS of per-door type
// dispatch, and a normalisation implemented per door would be the same class
// again. Every write reaches one of these two validators.
//
// TWO SPELLINGS, because the two shapes mean different things by an absent key:
//
//   - FULL write (partial=false): absent means "this field has no value", so
//     `[]` becomes an ABSENT KEY and the ordinary required-field machinery
//     below refuses it for a required field with no default. Nothing here
//     needs to know what `required` means.
//   - PARTIAL write (partial=true): absent means "leave this key ALONE", so
//     deleting would turn a clear into a no-op — the caller asked to empty the
//     field and the field would keep its contents. `[]` becomes the patch
//     path's explicit deletion sentinel (nil), which `ValidatePartialFields`
//     already refuses for a required key.
//
// Only the EMPTY array is touched. A non-empty one is a value, and its elements
// are the resolver's business.
// IsEmptyRelationList reports whether val is the EMPTY-LIST spelling of "no
// targets" for def — the value `normalizeEmptyRelationLists` rewrites.
//
// Exported because the copy PREFLIGHT has to ask the same question before this
// package runs: it records where each value came from, and an empty list is
// about to stop being the caller's value, so a preflight that records it as an
// override labels the injected default "your value" (codex round 6). One rule,
// asked in two places, rather than two spellings of it.
func IsEmptyRelationList(def models.FieldDef, val any) bool {
	if !def.IsMultiRelation() || val == nil {
		return false
	}
	switch v := val.(type) {
	case []any:
		return len(v) == 0
	case []string:
		return len(v) == 0
	}
	return false
}

func normalizeEmptyRelationLists(fields map[string]any, schema models.CollectionSchema, partial bool) {
	if len(fields) == 0 {
		return
	}
	for _, def := range schema.Fields {
		if !def.IsMultiRelation() {
			continue
		}
		val, exists := fields[def.Key]
		if !exists {
			continue
		}
		// NIL IS THE OTHER SPELLING (codex round 7). On a FULL write the
		// traversal below treats a present-but-nil key as absent for the
		// purposes of required-ness and defaults — but it does not REMOVE it,
		// so `{"members": null}` was stored: a third representation of "no
		// targets" beside the absent key and the empty list, which is the exact
		// thing this rule exists to prevent. On a PARTIAL write nil already
		// means "delete this key" and is the spelling being normalised TO, so
		// it is left alone.
		if val == nil {
			if !partial && def.IsMultiRelation() {
				delete(fields, def.Key)
			}
			continue
		}
		if !IsEmptyRelationList(def, val) {
			continue
		}
		if partial {
			fields[def.Key] = nil
			continue
		}
		delete(fields, def.Key)
	}
}

// ValidatePartialFields validates ONLY the keys present in `patch` against
// the schema (TASK-2022, field-level PATCH / IDEA-1480). Unlike
// ValidateFields it does NOT enforce required-field presence and does NOT
// inject schema defaults — a partial patch is "change exactly these keys,
// leave everything else alone," so absent keys are neither missing nor
// candidates for default population. Keys the schema doesn't declare (orphan
// keys) are accepted and persist unchanged, matching the full-blob path.
//
// A nil value marks a key for DELETION (see store.mergeFieldsPatch); those
// are skipped here since there's no value to type-check.
func ValidatePartialFields(patch map[string]any, schema models.CollectionSchema) error {
	normalizeEmptyRelationLists(patch, schema, true)
	// Index declared fields by key for O(1) lookup.
	defByKey := make(map[string]models.FieldDef, len(schema.Fields))
	for _, def := range schema.Fields {
		defByKey[def.Key] = def
	}

	var errs []string
	for key, val := range patch {
		def, hasDef := defByKey[key]
		if val == nil {
			// Deletion sentinel. Refuse to delete a schema-declared REQUIRED
			// field — the full-update path (ValidateFields) would reject the
			// resulting blob (or re-default it), so allowing a patch to strip
			// it would persist a state the schema considers invalid. Orphan
			// keys and optional fields delete freely.
			if hasDef && def.Required {
				errs = append(errs, fmt.Sprintf("field %q is required and cannot be deleted", key))
			}
			continue
		}
		if !hasDef {
			// Orphan key (not in schema) — allowed, persists as-is.
			continue
		}
		if err := validateFieldType(def, val); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("field validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

func validateFieldType(def models.FieldDef, val any) error {
	switch def.Type {
	case "text", "url":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("field %q must be a string", def.Key)
		}
	case "number":
		switch n := val.(type) {
		case float64, int, int64, float32:
			// ok
		case json.Number:
			// The write doors decode with UseNumber so a number keeps its
			// literal (BUG-3202). A json.Number is not guaranteed to be a
			// number when it did not come from the decoder, so it is parsed
			// here with the same rule the decoder applies.
			if f, err := strconv.ParseFloat(string(n), 64); err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return fmt.Errorf("field %q must be a number", def.Key)
			}
		default:
			return fmt.Errorf("field %q must be a number", def.Key)
		}
	case "checkbox":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("field %q must be a boolean", def.Key)
		}
	case "date":
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("field %q must be a date string (ISO 8601)", def.Key)
		}
		if s != "" {
			// Accept YYYY-MM-DD or full RFC3339
			if _, err := time.Parse("2006-01-02", s); err != nil {
				if _, err := time.Parse(time.RFC3339, s); err != nil {
					return fmt.Errorf("field %q has invalid date format (expected YYYY-MM-DD or RFC3339)", def.Key)
				}
			}
		}
	case "select":
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("field %q must be a string", def.Key)
		}
		if s != "" && len(def.Options) > 0 {
			found := false
			for _, opt := range def.Options {
				if opt == s {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("field %q value %q is not in allowed options %v", def.Key, s, def.Options)
			}
		}
	case "multi_select":
		// Accept a slice of strings
		switch v := val.(type) {
		case []any:
			for i, item := range v {
				s, ok := item.(string)
				if !ok {
					return fmt.Errorf("field %q item %d must be a string", def.Key, i)
				}
				if len(def.Options) > 0 {
					found := false
					for _, opt := range def.Options {
						if opt == s {
							found = true
							break
						}
					}
					if !found {
						return fmt.Errorf("field %q value %q is not in allowed options %v", def.Key, s, def.Options)
					}
				}
			}
		case []string:
			for _, s := range v {
				if len(def.Options) > 0 {
					found := false
					for _, opt := range def.Options {
						if opt == s {
							found = true
							break
						}
					}
					if !found {
						return fmt.Errorf("field %q value %q is not in allowed options %v", def.Key, s, def.Options)
					}
				}
			}
		default:
			return fmt.Errorf("field %q must be an array of strings", def.Key)
		}
	case "relation":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("field %q must be a string (item ID)", def.Key)
		}
	case "multi_relation":
		// PLAN-2857 U4. SHAPE ONLY — this package is DB-free, so whether an
		// element names a live item is `ResolveRelationReferentsQ`'s question,
		// exactly as it is for a scalar `relation`.
		//
		// Two rules that differ from the scalar case, both ruled day 64 as NEW
		// rules rather than inherited ones (see BUG-3028 for why there was
		// nothing coherent to inherit):
		//
		//   * an EMPTY or WHITESPACE-ONLY element is REFUSED here, where the
		//     scalar resolver merely skips such a value. Skipping is how a
		//     scalar relation ended up with three stored spellings for "no
		//     target"; inside an array it would also silently change the
		//     element count, and an ORDERED list whose length depends on which
		//     elements were blank is not a list anyone can reason about.
		//   * `[]` is ACCEPTED as a shape and means "no targets". It never
		//     reaches this arm from either validator, because
		//     `normalizeEmptyRelationLists` has already rewritten it into the
		//     write shape's own spelling of none — an absent key on a full
		//     write, the nil deletion sentinel on a partial one. The rule used
		//     to say this was "the write door's job"; no door did it, which is
		//     what codex round 1 found, and the lead's ruling put it at the one
		//     entry every door passes through instead. The arm still accepts
		//     the shape: a caller reaching `validateFieldType` directly (the
		//     tests do) gets the honest answer that an empty list is legal.
		//     Refusing it here would leave a caller no way to clear the field.
		//
		// `[]string` is accepted alongside `[]any` for the same reason
		// multi_select accepts both: a Go caller that never round-tripped
		// through JSON has the typed slice.
		switch v := val.(type) {
		case []any:
			for i, entry := range v {
				sv, ok := entry.(string)
				if !ok {
					return fmt.Errorf("field %q element %d must be a string (item ID, ref, or exact title)", def.Key, i)
				}
				if strings.TrimSpace(sv) == "" {
					return fmt.Errorf("field %q element %d is empty; remove it rather than sending a blank reference", def.Key, i)
				}
			}
		case []string:
			for i, sv := range v {
				if strings.TrimSpace(sv) == "" {
					return fmt.Errorf("field %q element %d is empty; remove it rather than sending a blank reference", def.Key, i)
				}
			}
		default:
			return fmt.Errorf("field %q must be an array of strings (item IDs, refs, or exact titles)", def.Key)
		}
	case "json":
		// Accept only structured JSON values (object, array, null). Raw
		// strings / numbers / bools are rejected so a generic text input in
		// the UI can't silently corrupt a structured field (e.g. emitting
		// the string "[]" instead of an actual array). Callers that want a
		// scalar field should use "text", "number", or "checkbox".
		switch val.(type) {
		case map[string]any, []any, nil:
			// ok
		default:
			return fmt.Errorf("field %q must be a JSON object, array, or null", def.Key)
		}
	}

	// Pattern check applies to string-typed values (text, url, and JSON strings).
	if def.Pattern != "" {
		s, ok := val.(string)
		if ok && s != "" {
			re, err := compilePattern(def.Pattern)
			if err != nil {
				return fmt.Errorf("field %q has an invalid pattern in its schema: %v", def.Key, err)
			}
			if !re.MatchString(s) {
				return fmt.Errorf("field %q value %q does not match required pattern %q", def.Key, s, def.Pattern)
			}
		}
	}
	return nil
}
