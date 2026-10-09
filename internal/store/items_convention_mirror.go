package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A convention keeps its metadata twice (TASK-2257). The ordinary fields
// (`trigger`, `scope` / `surfaces`, `enforcement` / `priority`, `category`,
// `commands`) are what agents obey: bootstrap and `--field trigger=X` read
// them. The reserved `convention` key (BUG-3163) is what the web reads,
// through item.convention, which prefers it. Only the create door wrote the
// reserved copy, so an edit of an ordinary key (`pad item update --field
// trigger=on-commit`, measured) left it stale, and the conventions page went
// on showing the old trigger while agents had moved on.
//
// The store now keeps the copy in step: when a write changes a mirrored
// ordinary key on an item that HAS a reserved copy, the copy is rebuilt from
// the merged fields in the same transaction. It runs after the BUG-3163
// carry guard (the update's precheck), and rewrites only the reserved key
// itself, so a caller still cannot set it directly. An item without a
// reserved copy (a legacy convention, or anything that is not a convention)
// is left exactly as it was.

// mirrorConventionMetadata returns `after` with its reserved `convention`
// copy brought in line with the ordinary keys that changed since `before`.
func mirrorConventionMetadata(before, after string) (string, error) {
	// UseNumber: the blob is re-encoded when the copy changes, and a float64
	// round trip would round any large integer elsewhere in it (BUG-3217).
	next, err := decodeNumbers(after)
	if err != nil {
		return after, nil // not ours to judge; the caller's own decode refuses it
	}
	rawCopy, ok := next[models.ItemFieldConvention].(map[string]any)
	if !ok {
		return after, nil // no reserved copy: nothing to keep in step
	}
	prev, err := decodeNumbers(before)
	if err != nil {
		prev = map[string]any{}
	}
	changed := func(key string) bool {
		_, in := next[key]
		return in && !reflect.DeepEqual(prev[key], next[key])
	}

	b, _ := json.Marshal(rawCopy)
	var meta models.ItemConventionMetadata
	if err := json.Unmarshal(b, &meta); err != nil {
		return after, nil // an undecodable copy is BUG-3163's to refuse, not ours
	}

	touched := false
	if changed("trigger") {
		if s, ok := next["trigger"].(string); ok {
			meta.Trigger, touched = s, true
		}
	}
	if changed("category") {
		if s, ok := next["category"].(string); ok {
			meta.Category, touched = s, true
		}
	}
	if changed("commands") {
		if list, ok := stringList(next["commands"]); ok {
			meta.Commands, touched = list, true
		}
	}
	if changed("surfaces") {
		if list, ok := stringList(next["surfaces"]); ok {
			meta.Surfaces, touched = list, true
		}
	} else if changed("scope") {
		if s, ok := next["scope"].(string); ok && s != "" {
			meta.Surfaces, touched = []string{s}, true
		}
	}
	if changed("enforcement") {
		if s, ok := next["enforcement"].(string); ok {
			meta.Enforcement, touched = s, true
		}
	} else if changed("priority") {
		if s, ok := next["priority"].(string); ok {
			meta.Enforcement, touched = s, true
		}
	}
	if !touched {
		return after, nil
	}

	normalized, err := models.ValidateConventionMetadata(&meta)
	if err != nil {
		// A ValidationError: every door answers it 400 validation_error with
		// the Reason, and nothing is written.
		return "", &ValidationError{Reason: "this write would leave the convention's stored metadata invalid: " + err.Error()}
	}
	next[models.ItemFieldConvention] = normalized
	out, err := json.Marshal(next)
	if err != nil {
		return "", fmt.Errorf("marshal mirrored convention: %w", err)
	}
	return string(out), nil
}

func decodeNumbers(blob string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(blob))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func stringList(v any) ([]string, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}
