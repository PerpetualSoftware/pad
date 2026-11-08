package store

import (
	"fmt"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4400: an item list sorted on a select field used to order by the
// option TEXT (medium before low before high). A select field now sorts by
// rank, per collection, because collections declare the same field's options
// in different orders (tasks: low..critical, ui-polish: high..low):
//
//   - priority, and severity (the dashboard's fallback for it): the ranks the
//     dashboard and project next already use (models.PriorityRank, or
//     models.ConventionPriorityRank on a must/should/nice-to-have field), so
//     the sorted list cannot disagree with them. asc is lowest first, desc
//     highest first.
//   - any other select field, and a priority field whose options name no
//     ranked word: the declared option order, asc = first option first,
//     desc = reversed.
//   - values outside the options sort after the known ones, and items with no
//     value last, in BOTH directions.
//
// Not covered: a select value compared across collections that declare
// different option sets for a non-priority field. The rank is per collection,
// so two collections' "stage" fields interleave by position, not by name.

// selectSortRank is the rank of one select field key in one workspace: for
// each collection that declares the key as a select, value (lowercased) ->
// position, larger = higher.
type selectSortRank struct {
	byCollection map[string]map[string]int
}

// selectSortKeys are the fields whose select values rank by PriorityRank.
func isRankedPriorityKey(key string) bool { return key == "priority" || key == "severity" }

// sortFieldKeys is the set of field keys a sort names, leaving out the
// built-in columns and any key a field name cannot be.
func sortFieldKeys(sort string) map[string]bool {
	keys := map[string]bool{}
	for _, seg := range strings.Split(sort, ",") {
		// Not trimmed: buildItemSortRanked reads the column as written, and a
		// key it would skip must not load ranks.
		col := strings.SplitN(strings.TrimSpace(seg), ":", 2)[0]
		switch col {
		case "", "title", "created_at", "updated_at", "sort_order", "_num", "_id":
		default:
			if validSortField.MatchString(col) {
				keys[col] = true
			}
		}
	}
	return keys
}

// selectSortRanks returns the rank table of every select field the sort
// names, keyed by field key. It reads the collections only when the sort
// names a non-builtin field.
func (s *Store) selectSortRanks(workspaceID string, params models.ItemListParams) (map[string]selectSortRank, error) {
	keys := sortFieldKeys(params.Sort)
	if len(keys) == 0 {
		return nil, nil
	}
	colls, err := s.ListCollectionsMinimal(workspaceID)
	if err != nil {
		return nil, fmt.Errorf("select sort ranks: %w", err)
	}
	inScope := func(id string) bool {
		if params.ScopeCollectionID != "" && id != params.ScopeCollectionID {
			return false
		}
		// ItemIDs are individual grants that may sit outside CollectionIDs, so
		// with them the list can hold any collection's items.
		if len(params.CollectionIDs) == 0 || len(params.ItemIDs) > 0 {
			return true
		}
		for _, c := range params.CollectionIDs {
			if c == id {
				return true
			}
		}
		return false
	}
	out := map[string]selectSortRank{}
	for _, c := range colls {
		if !inScope(c.ID) || c.Schema == "" {
			continue
		}
		var schema models.CollectionSchema
		if err := models.UnmarshalItemFieldSchema([]byte(c.Schema), &schema); err != nil {
			continue
		}
		for _, f := range schema.Fields {
			if f.Type != "select" || !keys[f.Key] || len(f.Options) == 0 {
				continue
			}
			r, ok := out[f.Key]
			if !ok {
				r = selectSortRank{byCollection: map[string]map[string]int{}}
			}
			r.byCollection[c.ID] = optionRanks(f)
			out[f.Key] = r
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// optionRanks maps each option (lowercased) to its position, larger = higher.
func optionRanks(f models.FieldDef) map[string]int {
	m := map[string]int{}
	if isRankedPriorityKey(f.Key) {
		for _, rank := range []func(string) int{models.PriorityRank, models.ConventionPriorityRank} {
			unknown := rank("\x00unknown")
			known := 0
			for _, o := range f.Options {
				if rank(o) != unknown {
					known++
				}
			}
			if known == 0 {
				continue
			}
			for _, o := range f.Options {
				if r := rank(o); r != unknown {
					m[strings.ToLower(o)] = unknown - 1 - r // lowest rank number = highest
				}
			}
			return m
		}
	}
	for i, o := range f.Options {
		if _, dup := m[strings.ToLower(o)]; !dup {
			m[strings.ToLower(o)] = i
		}
	}
	return m
}

// orderBy is the ORDER BY term for key: group (known 0, unknown 1, no value
// 2) ascending in both directions, then the rank in dir, then the raw value.
func (r selectSortRank) orderBy(d Dialect, key, dir string) (string, []any) {
	text := "LOWER(" + d.JSONFieldText("i.fields", key) + ")"
	var args []any
	var group, rank strings.Builder
	group.WriteString("(CASE WHEN " + text + " IS NULL OR " + text + " = '' THEN 2")
	rank.WriteString("(CASE")
	// Sorted so the SQL text is stable.
	ids := make([]string, 0, len(r.byCollection))
	for id := range r.byCollection {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		vals := make([]string, 0, len(r.byCollection[id]))
		for v := range r.byCollection[id] {
			vals = append(vals, v)
		}
		sort.Strings(vals)
		group.WriteString(" WHEN i.collection_id = ? AND " + text + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",") + ") THEN 0")
		args = append(args, id)
		for _, v := range vals {
			args = append(args, v)
		}
	}
	group.WriteString(" ELSE 1 END)")
	// The rank args follow the group args in the final statement; build them
	// in the same walk order.
	rankArgs := []any{}
	for _, id := range ids {
		vals := make([]string, 0, len(r.byCollection[id]))
		for v := range r.byCollection[id] {
			vals = append(vals, v)
		}
		sort.Strings(vals)
		rank.WriteString(" WHEN i.collection_id = ? THEN (CASE " + text)
		rankArgs = append(rankArgs, id)
		for _, v := range vals {
			rank.WriteString(" WHEN ? THEN " + fmt.Sprint(r.byCollection[id][v]))
			rankArgs = append(rankArgs, v)
		}
		rank.WriteString(" END)")
	}
	rank.WriteString(" END)")
	// The tail is the field's ordinary order (numbers numerically, then
	// strings, NULLs last), so an item in a collection that does not declare
	// the key as a select, or holds a value outside the options, orders as
	// it did before this rank existed.
	return fmt.Sprintf("%s ASC, %s %s, %s", group.String(), rank.String(), dir, d.JSONFieldOrder("i.fields", key, dir)), append(args, rankArgs...)
}
