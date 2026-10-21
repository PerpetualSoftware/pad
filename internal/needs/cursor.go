package needs

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"time"
)

// The sorts. SortAge is oldest-waiting first; SortImportance and
// SortSimplicity order by the scorers' values, highest first (TASK-3959).
const (
	SortAge        = "age"
	SortImportance = "importance"
	SortSimplicity = "simplicity"
)

// ValidSort reports whether s names a sort.
func ValidSort(s string) bool {
	return s == SortAge || s == SortImportance || s == SortSimplicity
}

// Key is a row's place in the total order of one sort. Age orders by (waiting
// since, created, item id) ascending. The score sorts order by the primary
// score then the secondary, both descending (importance: importance then
// simplicity; simplicity: the mirror), then item id ascending. A row a scorer
// left without a score keys at NoScore and so sorts after every scored row.
// The item id makes every order total.
type Key struct {
	Sort      string
	Waiting   time.Time
	Created   time.Time
	ItemID    string
	Primary   int
	Secondary int
}

// NoScore is the key value of a row with no score.
const NoScore = -1

// KeyOf is the row's age key.
func KeyOf(r *Row) Key { return KeyFor(SortAge, r) }

// The instants a cursor can carry: Unix nanoseconds in int64, about 1677 to
// 2262. A key time outside them (a zero time from an unparseable stored date,
// or a far-future one) is clamped to the nearer end, so the sort and the cursor
// see the same instant and the cursor round-trips exactly (TASK-3959 part 2 r2).
// Clamped times tie and the tie falls through to created, then the item id.
var (
	keyTimeMin = time.Unix(0, math.MinInt64).UTC()
	keyTimeMax = time.Unix(0, math.MaxInt64).UTC()
)

func clampKeyTime(t time.Time) time.Time {
	if t.Before(keyTimeMin) {
		return keyTimeMin
	}
	if t.After(keyTimeMax) {
		return keyTimeMax
	}
	return t
}

// KeyFor is the row's key under a sort.
func KeyFor(sort string, r *Row) Key {
	k := Key{Sort: sort, Waiting: clampKeyTime(r.WaitingSince), Created: clampKeyTime(r.Item.CreatedAt), ItemID: r.ItemID}
	val := func(s *Score) int {
		if s == nil {
			return NoScore
		}
		return s.Value
	}
	switch sort {
	case SortImportance:
		k.Primary, k.Secondary = val(r.Importance), val(r.Simplicity)
	case SortSimplicity:
		k.Primary, k.Secondary = val(r.Simplicity), val(r.Importance)
	}
	return k
}

// Less reports whether a sorts before b. Both keys come from one sort.
func (a Key) Less(b Key) bool {
	if a.Sort == SortImportance || a.Sort == SortSimplicity {
		if a.Primary != b.Primary {
			return a.Primary > b.Primary
		}
		if a.Secondary != b.Secondary {
			return a.Secondary > b.Secondary
		}
		return a.ItemID < b.ItemID
	}
	if !a.Waiting.Equal(b.Waiting) {
		return a.Waiting.Before(b.Waiting)
	}
	if !a.Created.Equal(b.Created) {
		return a.Created.Before(b.Created)
	}
	return a.ItemID < b.ItemID
}

const cursorVersion = 1

// validCursorItemID accepts the store's ids (UUIDs) and nothing that could not
// be one: 1 to 64 characters of letters, digits and hyphens.
func validCursorItemID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// ErrInvalidCursor is any cursor the server will not follow: malformed, from
// another version, or minted for a different sort, view or workspace filter.
var ErrInvalidCursor = errors.New("invalid cursor")

// Cursor is a position in a result, and nothing more: it carries no
// authority. Every page re-runs the whole filtered pipeline and takes the rows
// strictly after this key, so a cursor naming an item the caller cannot see
// reveals nothing. AsOf is the time page 1 was computed, so a deferral that
// expires between pages does not reshuffle them.
type Cursor struct {
	V         int    `json:"v"`
	Sort      string `json:"sort"`
	View      string `json:"view"`
	WS        string `json:"ws"`
	AsOf      int64  `json:"as_of"`
	Waiting   int64  `json:"key"`
	CreatedAt int64  `json:"created_at"`
	ItemID    string `json:"item_id"`
	// Score and Score2 are the key's primary and secondary score under the
	// score sorts, and absent under age, so an age cursor is unchanged.
	Score  *int `json:"score,omitempty"`
	Score2 *int `json:"score2,omitempty"`
}

// NewCursor mints the cursor after row r.
func NewCursor(sort, view, ws string, asOf time.Time, r *Row) Cursor {
	k := KeyFor(sort, r)
	c := Cursor{V: cursorVersion, Sort: sort, View: view, WS: ws, AsOf: asOf.UnixNano(),
		Waiting: k.Waiting.UnixNano(), CreatedAt: k.Created.UnixNano(), ItemID: k.ItemID}
	if sort != SortAge {
		c.Score, c.Score2 = &k.Primary, &k.Secondary
	}
	return c
}

// Encode is the opaque token form.
func (c Cursor) Encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Key is the position the cursor names.
func (c Cursor) Key() Key {
	k := Key{Sort: c.Sort, Waiting: time.Unix(0, c.Waiting).UTC(), Created: time.Unix(0, c.CreatedAt).UTC(), ItemID: c.ItemID}
	if c.Score != nil && c.Score2 != nil {
		k.Primary, k.Secondary = *c.Score, *c.Score2
	}
	return k
}

// At is the as_of time.
func (c Cursor) At() time.Time { return time.Unix(0, c.AsOf).UTC() }

// DecodeCursor parses a token and checks it was minted for this sort, view and
// workspace filter. Anything else is ErrInvalidCursor.
func DecodeCursor(token, sort, view, ws string) (Cursor, error) {
	var c Cursor
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || json.Unmarshal(b, &c) != nil {
		return Cursor{}, ErrInvalidCursor
	}
	// A check may refuse only what this server could never mint. v, sort, view
	// and ws must equal this request's; as_of must be positive, since it is the
	// server's own clock (a future one is clamped by the handler, not refused);
	// item_id must look like a store id. key and created_at are NOT checked:
	// every int64 is an instant a key can hold, a pre-1970 date included
	// (TASK-3959 part 2 r2).
	if c.V != cursorVersion || c.Sort != sort || c.View != view || c.WS != ws || c.AsOf <= 0 ||
		!validCursorItemID(c.ItemID) {
		return Cursor{}, ErrInvalidCursor
	}
	// A score sort's cursor must carry both scores in range; an age cursor
	// must carry neither. Anything else was not minted by this server.
	if sort == SortAge {
		if c.Score != nil || c.Score2 != nil {
			return Cursor{}, ErrInvalidCursor
		}
	} else if c.Score == nil || c.Score2 == nil || *c.Score < NoScore || *c.Score > 100 || *c.Score2 < NoScore || *c.Score2 > 100 {
		return Cursor{}, ErrInvalidCursor
	}
	return c, nil
}
