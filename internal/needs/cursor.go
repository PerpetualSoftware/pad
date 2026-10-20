package needs

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// SortAge is the only sort so far: oldest-waiting first.
const SortAge = "age"

// Key is a row's place in the total order (waiting since, created, item id),
// ascending. The item id makes it total.
type Key struct {
	Waiting time.Time
	Created time.Time
	ItemID  string
}

// KeyOf is the row's sort key.
func KeyOf(r *Row) Key { return Key{r.WaitingSince, r.Item.CreatedAt, r.ItemID} }

// Less reports whether a sorts before b.
func (a Key) Less(b Key) bool {
	if !a.Waiting.Equal(b.Waiting) {
		return a.Waiting.Before(b.Waiting)
	}
	if !a.Created.Equal(b.Created) {
		return a.Created.Before(b.Created)
	}
	return a.ItemID < b.ItemID
}

const cursorVersion = 1

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
}

// NewCursor mints the cursor after row r.
func NewCursor(sort, view, ws string, asOf time.Time, r *Row) Cursor {
	k := KeyOf(r)
	return Cursor{V: cursorVersion, Sort: sort, View: view, WS: ws, AsOf: asOf.UnixNano(),
		Waiting: k.Waiting.UnixNano(), CreatedAt: k.Created.UnixNano(), ItemID: k.ItemID}
}

// Encode is the opaque token form.
func (c Cursor) Encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Key is the position the cursor names.
func (c Cursor) Key() Key {
	return Key{time.Unix(0, c.Waiting).UTC(), time.Unix(0, c.CreatedAt).UTC(), c.ItemID}
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
	if c.V != cursorVersion || c.Sort != sort || c.View != view || c.WS != ws || c.ItemID == "" || c.AsOf <= 0 {
		return Cursor{}, ErrInvalidCursor
	}
	return c, nil
}
