package collections

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"sync"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Built-in kinds (TASK-3462).
const (
	BuiltinConvention = "convention"
	BuiltinPlaybook   = "playbook"
)

// BuiltinEntry is one convention or playbook Pad ships: a library entry, or
// a template-only seed. Its Key is what an item seeded or activated from it
// records as its origin (store.ItemBuiltinOrigin), and its Hash is the
// version of its text that item was given.
type BuiltinEntry struct {
	Key     string
	Kind    string
	Title   string
	Content string
	// Fields is the stored fields blob an item made from the entry gets,
	// `status` included.
	Fields string
}

// builtinExcludedFields are the stored fields an entry's hash leaves out,
// because an update from the library never writes them: `status` is the
// user's (a deprecated copy stays deprecated). The title is not hashed
// either: it is the item's name, not its text.
var builtinExcludedFields = map[string]bool{"status": true}

// Hash is the entry's version: a sha256 over its content and the fields an
// update would write. It is computed from the text, so a fix to a body or an
// argument spec changes it without anyone remembering to bump a number.
func (e BuiltinEntry) Hash() string {
	h, err := e.HashErr()
	if err != nil {
		// TestBuiltinRegistry hashes every compiled-in entry; text from
		// elsewhere (an imported origin) goes through HashErr.
		panic(fmt.Sprintf("built-in %q: %v", e.Key, err))
	}
	return h
}

// HashErr is Hash for text that did not come from this binary.
func (e BuiltinEntry) HashErr() (string, error) {
	fields, err := e.updateFields()
	if err != nil {
		return "", err
	}
	return BuiltinStateHash(e.Content, fields), nil
}

// UpdateFieldKeys are the field keys an update from this entry writes, sorted.
func (e BuiltinEntry) UpdateFieldKeys() []string {
	fields, err := e.updateFields()
	if err != nil {
		panic(fmt.Sprintf("built-in %q: %v", e.Key, err))
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (e BuiltinEntry) updateFields() (map[string]any, error) {
	var fields map[string]any
	if err := models.DecodeJSONKeepingNumbers([]byte(e.Fields), &fields); err != nil {
		return nil, fmt.Errorf("decode fields: %w", err)
	}
	for k := range builtinExcludedFields {
		delete(fields, k)
	}
	dropNullFields(fields)
	return fields, nil
}

// dropNullFields removes null values, so a key stored as null and a key not
// stored at all hash the same: neither door distinguishes them on read.
func dropNullFields(fields map[string]any) {
	for k, v := range fields {
		if v == nil {
			delete(fields, k)
		}
	}
}

// ItemStateHash hashes an item's body and its values for this entry's update
// fields, the way Hash hashes the entry, so it equals Hash exactly when the
// item still carries the entry's text. The item's other fields (a user's own
// keys, values a schema default injected) are not the entry's and are
// ignored.
func (e BuiltinEntry) ItemStateHash(content, fieldsJSON string) (string, error) {
	item := map[string]any{}
	if fieldsJSON != "" {
		if err := models.DecodeJSONKeepingNumbers([]byte(fieldsJSON), &item); err != nil {
			return "", fmt.Errorf("decode item fields: %w", err)
		}
	}
	dropNullFields(item)
	own := map[string]any{}
	for _, k := range e.UpdateFieldKeys() {
		if v, ok := item[k]; ok {
			own[k] = v
		}
	}
	return BuiltinStateHash(content, own), nil
}

// BuiltinStateHash hashes a body and a set of fields the way BuiltinEntry.Hash
// does, so an item's current state can be compared with an entry's. The
// fields are canonical JSON: encoding/json sorts map keys, and numbers are
// json.Number so a literal hashes as written.
func BuiltinStateHash(content string, fields map[string]any) string {
	if fields == nil {
		fields = map[string]any{}
	}
	fields = canonicalNumbers(fields).(map[string]any)
	b, err := json.Marshal(struct {
		Content string         `json:"content"`
		Fields  map[string]any `json:"fields"`
	}{content, fields})
	if err != nil {
		panic(fmt.Sprintf("hash built-in state: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalNumbers rewrites every json.Number to one exact spelling, so a
// value hashes the same however it was written: Postgres stores fields as
// jsonb and may respell a number the seed text spells differently (codex
// r1). big.Rat is exact, so two values that differ anywhere still differ.
func canonicalNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		// Bounded: every value here came through DecodeJSONKeepingNumbers,
		// which refuses a number outside float64's range, so an exponent
		// cannot ask big.Rat for more than a few hundred digits.
		if r, ok := new(big.Rat).SetString(t.String()); ok {
			return json.Number(exactDecimal(r))
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = canonicalNumbers(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = canonicalNumbers(x)
		}
		return out
	}
	return v
}

// exactDecimal prints r as a plain decimal with no loss. Every JSON number is
// a terminating decimal, so r's reduced denominator is 2^a*5^b and
// max(a, b) fractional digits print it exactly.
func exactDecimal(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	d := new(big.Int).Set(r.Denom())
	two, five, zero := big.NewInt(2), big.NewInt(5), big.NewInt(0)
	a, b := 0, 0
	m := new(big.Int)
	for m.Mod(d, two).Cmp(zero) == 0 {
		d.Div(d, two)
		a++
	}
	for m.Mod(d, five).Cmp(zero) == 0 {
		d.Div(d, five)
		b++
	}
	prec := a
	if b > prec {
		prec = b
	}
	return r.FloatString(prec)
}

var (
	builtinOnce sync.Once
	builtinMap  map[string]BuiltinEntry
	builtinErr  error
)

// LookupBuiltin returns the built-in with this key.
func LookupBuiltin(key string) (BuiltinEntry, bool) {
	m, _ := builtinRegistry()
	e, ok := m[key]
	return e, ok
}

// builtinRegistry gathers every built-in: the convention and playbook
// libraries, every template's seeds, and the onboard seed. One key names one
// text: two sources sharing a key (the library's `ship` and the startup
// template's) must produce the same entry, and a seed with no key is an
// error. TestBuiltinRegistry fails on either.
func builtinRegistry() (map[string]BuiltinEntry, error) {
	builtinOnce.Do(func() {
		m := map[string]BuiltinEntry{}
		var errs []error
		add := func(e BuiltinEntry, from string) {
			if e.Key == "" {
				errs = append(errs, fmt.Errorf("%s %q from %s has no key", e.Kind, e.Title, from))
				return
			}
			if prev, ok := m[e.Key]; ok {
				if prev.Kind != e.Kind || prev.Title != e.Title || prev.Hash() != e.Hash() {
					errs = append(errs, fmt.Errorf("key %q names two different built-ins (second from %s)", e.Key, from))
				}
				return
			}
			m[e.Key] = e
		}
		for _, cat := range ConventionLibrary() {
			for _, c := range cat.Conventions {
				add(BuiltinEntry{Key: c.Key, Kind: BuiltinConvention, Title: c.Title, Content: c.Content, Fields: LibraryConventionFields(c)}, "the convention library")
			}
		}
		for _, cat := range PlaybookLibrary() {
			for _, p := range cat.Playbooks {
				add(BuiltinEntry{Key: p.Key, Kind: BuiltinPlaybook, Title: p.Title, Content: p.Content, Fields: LibraryPlaybookFields(p)}, "the playbook library")
			}
		}
		for _, t := range ListAllTemplates() {
			for _, c := range t.Conventions {
				add(BuiltinEntry{Key: c.Key, Kind: BuiltinConvention, Title: c.Title, Content: c.Content, Fields: c.Fields}, "template "+t.Name)
			}
			for _, p := range t.Playbooks {
				add(BuiltinEntry{Key: p.Key, Kind: BuiltinPlaybook, Title: p.Title, Content: p.Content, Fields: p.Fields}, "template "+t.Name)
			}
		}
		o := OnboardSeedPlaybook()
		add(BuiltinEntry{Key: o.Key, Kind: BuiltinPlaybook, Title: o.Title, Content: o.Content, Fields: o.Fields}, "the onboard seed")
		builtinMap = m
		if len(errs) > 0 {
			builtinErr = fmt.Errorf("built-in registry: %v", errs)
		}
	})
	return builtinMap, builtinErr
}

// Built-in states an item can be in relative to Pad's current text
// (TASK-3462). Derived on read, never stored.
const (
	// BuiltinCurrent: nothing to offer. The library has not changed since
	// the item was given its text, or the item already holds the library's
	// current text (whoever put it there).
	BuiltinCurrent = "current"
	// BuiltinUpdateAvailable: the library changed and the item still holds
	// the text it was given, so taking the update loses nothing of its
	// user's.
	BuiltinUpdateAvailable = "update_available"
	// BuiltinDiverged: the library changed AND the item was edited since it
	// was given its text. Taking the update replaces those edits (they stay
	// in the item's version history).
	BuiltinDiverged = "diverged"
	// BuiltinUnknownOrigin: the item names a built-in but not which version
	// it was given (adopted after the fact), and does not hold the current
	// text. Only a 2-way comparison is possible.
	BuiltinUnknownOrigin = "unknown_origin"
	// BuiltinUnknownEntry: the item names a built-in this Pad does not ship
	// (imported from a newer one, or retired). Nothing to offer.
	BuiltinUnknownEntry = "unknown_entry"
)

// BuiltinStateOf compares an item with the built-in it was made from. It
// returns the state, the item's hash over the LIBRARY's update fields, and
// the entry (zero when unknown).
//
// The item is hashed twice when it has to be: over the library's update
// fields to ask "does it hold the library's text", and over the SEED's to ask
// "does it still hold what it was given". The two key sets differ exactly
// when the library added or dropped a field, which is a library change, not
// an edit; comparing a hash over one set with a hash over the other would
// call every such item edited.
func BuiltinStateOf(o models.BuiltinOrigin, content, fieldsJSON string) (state, itemHash string, entry BuiltinEntry, err error) {
	entry, ok := LookupBuiltin(o.Key)
	if !ok {
		return BuiltinUnknownEntry, "", BuiltinEntry{}, nil
	}
	itemHash, err = entry.ItemStateHash(content, fieldsJSON)
	if err != nil {
		return "", "", entry, err
	}
	library := entry.Hash()
	// The seed's text, when it is known and readable.
	var seed *BuiltinEntry
	if o.SeedFields != "" {
		s := BuiltinEntry{Key: o.Key, Content: o.SeedContent, Fields: o.SeedFields}
		if _, herr := s.HashErr(); herr == nil {
			seed = &s
		}
	}
	// Matching the library over the library's keys is not enough when the
	// library DROPPED a field (codex r2): the projection cannot see it, so an
	// item still carrying it would read current and never be offered the
	// removal. It holds the library text only when the seed's dropped keys
	// are gone from it too.
	holdsLibrary := itemHash == library
	if holdsLibrary && seed != nil {
		dropped, derr := droppedKeysPresent(entry, *seed, fieldsJSON)
		if derr != nil {
			return "", "", entry, derr
		}
		holdsLibrary = !dropped
	}
	switch {
	case holdsLibrary:
		return BuiltinCurrent, itemHash, entry, nil
	case o.SeedHash == "":
		return BuiltinUnknownOrigin, itemHash, entry, nil
	case o.SeedHash == library:
		return BuiltinCurrent, itemHash, entry, nil
	}
	vsSeed := itemHash
	if seed != nil {
		if vsSeed, err = seed.ItemStateHash(content, fieldsJSON); err != nil {
			return "", "", entry, err
		}
	}
	if vsSeed == o.SeedHash {
		return BuiltinUpdateAvailable, itemHash, entry, nil
	}
	return BuiltinDiverged, itemHash, entry, nil
}

// droppedKeysPresent reports whether the item still carries a non-null value
// for an update field the seed had and the library no longer has.
func droppedKeysPresent(library, seed BuiltinEntry, fieldsJSON string) (bool, error) {
	lib := map[string]bool{}
	for _, k := range library.UpdateFieldKeys() {
		lib[k] = true
	}
	item := map[string]any{}
	if fieldsJSON != "" {
		if err := models.DecodeJSONKeepingNumbers([]byte(fieldsJSON), &item); err != nil {
			return false, fmt.Errorf("decode item fields: %w", err)
		}
	}
	for _, k := range seed.UpdateFieldKeys() {
		if v, ok := item[k]; !lib[k] && ok && v != nil {
			return true, nil
		}
	}
	return false, nil
}

// UpdateFields is the entry's update fields as a map: what an update from the
// library writes into the item, status excluded.
func (e BuiltinEntry) UpdateFields() map[string]any {
	fields, err := e.updateFields()
	if err != nil {
		panic(fmt.Sprintf("built-in %q: %v", e.Key, err))
	}
	return fields
}
