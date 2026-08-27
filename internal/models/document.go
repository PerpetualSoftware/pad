package models

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Valid document types
var ValidDocTypes = []string{
	"roadmap", "plan", "architecture", "ideation",
	"feature-spec", "notes", "prompt-library", "reference",
}

// Valid document statuses
var ValidStatuses = []string{
	"draft", "active", "completed", "archived",
}

// Valid actors
var ValidActors = []string{"user", "agent"}

// Valid sources
var ValidSources = []string{"cli", "web", "skill"}

type Document struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	Title          string     `json:"title"`
	Slug           string     `json:"slug"`
	Content        string     `json:"content"`
	DocType        string     `json:"doc_type"`
	Status         string     `json:"status"`
	Tags           string     `json:"tags"` // JSON array
	Pinned         bool       `json:"pinned"`
	SortOrder      int        `json:"sort_order"`
	CreatedBy      string     `json:"created_by"`
	LastModifiedBy string     `json:"last_modified_by"`
	Source         string     `json:"source"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
}

type DocumentCreate struct {
	Title     string `json:"title"`
	Content   string `json:"content,omitempty"`
	DocType   string `json:"doc_type,omitempty"`
	Status    string `json:"status,omitempty"`
	Tags      string `json:"tags,omitempty"`
	Pinned    bool   `json:"pinned,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	Source    string `json:"source,omitempty"`
}

type DocumentUpdate struct {
	Title          *string `json:"title,omitempty"`
	Content        *string `json:"content,omitempty"`
	DocType        *string `json:"doc_type,omitempty"`
	Status         *string `json:"status,omitempty"`
	Tags           *string `json:"tags,omitempty"`
	Pinned         *bool   `json:"pinned,omitempty"`
	SortOrder      *int    `json:"sort_order,omitempty"`
	LastModifiedBy string  `json:"last_modified_by,omitempty"`
	Source         string  `json:"source,omitempty"`
	ChangeSummary  string  `json:"change_summary,omitempty"`
}

type DocumentListParams struct {
	Type   string
	Status string
	Tag    string
	Pinned *bool
	Query  string
	Sort   string
	Order  string
}

// MaxDocumentTitleRunes bounds a document title at write time.
//
// Two reasons, and the second is the load-bearing one:
//
//   - A title is emitted into every linking document by the rename cascade
//     (`[[newTitle]]` per occurrence), so its length is an amplification
//     factor on a body the renamer does not supply. Unbounded, one rename
//     could project 10 GB from a 500 KB input — measured, 20,000x (BUG-2798).
//   - 255 is the conventional identifier-ish bound and comfortably above any
//     real title; the longest document title this codebase seeds is far short
//     of it.
//
// RUNES, not bytes, because "255 characters" is what a user and a UI counter
// mean. That makes the byte-level residual up to 4x this number, which is
// precisely why the title bound is the cheap door and NOT the wall: the
// cascade's own guard (store.MaxRenameCascadeRetainedBytes) is byte-accurate
// and is what actually bounds the work.
//
// Enforced at WRITE time only. Titles already over the bound stay valid and
// keep working until their next rename — no retro-breakage of stored data
// (Dave's ruling, day-63).
const MaxDocumentTitleRunes = 255

// wikiTitleRoundTripFailure reports why emitting `[[title]]` the way
// links.ReplaceTitle does — plain concatenation, no escaping — would produce a
// bracket that does not read back as this title. Returns "" when the title
// survives the round trip.
//
// This is BUG-2796 stated as a property rather than as a character blacklist,
// and the distinction is not cosmetic: the first version of this function
// banned `]`, `\` and `|` on the reasoning that all three "look like
// wiki-link syntax", and the test below refuted two thirds of that. The rule
// is therefore derived from the two mechanisms that actually consume a stored
// bracket, both in web/src/lib/utils/markdown.ts:
//
//  1. THE GRAMMAR (markdown.ts:327, shared by renderMarkdown and
//     wikiLinksToMarkdown since BUG-1744): a bracket body is
//     `(?:\\.|[^\]\\])+` — escape pairs, or anything that is neither `]` nor
//     `\`. A raw `]` ends the bracket early, which IS BUG-2796's defect:
//     renaming to `A]] [[A` emitted `[[A]] [[A]]`, two brackets, neither
//     resolving to the renamed document, and the rename reported success. A
//     title ending in `\` fails the same way — the trailing backslash pairs
//     with the first `]` of the terminator and the bracket never closes.
//
//  2. THE UNESCAPER (markdown.ts:753, `\\(\\|\]|\|)` → `$1`): resolution
//     unescapes the body before comparing it to a title. A title containing
//     `\\` or `\|` is emitted raw, unescaped on the way back in, and the
//     result no longer equals the title — so the link resolves to nothing,
//     or to a different document.
//
// Deliberately NOT rejected, because the code these titles pass through
// handles them and refusing them would be a validator inventing a defect:
//
//   - `[` — the grammar excludes only `]` and `\`, so `[[A[B]]` carries the
//     body `A[B` intact. BUG-2796's filing proposed rejecting "`[[` or `]]`";
//     measured against the grammar, the `[[` half of that is overreach.
//   - `|` — resolveWikiBody tries a FULL-BODY title match before the pipe
//     split, a branch whose comment says it exists precisely to handle
//     "stored legacy titles that contain a literal `|`". Banning it would
//     refuse what that branch was written to support.
//   - a lone `\` not followed by `\`, `]` or `|` — passes the grammar as an
//     escape pair and survives the unescaper unchanged. Note this one depends
//     on store.escapeLikePattern: the rename cascade finds linking documents
//     with `content LIKE`, where Postgres reads `\` as an escape character, so
//     before that escaping landed a backslash title rendered fine and then
//     silently failed to cascade on one dialect. Allowing it here is only
//     correct while the cascade's pattern stays escaped.
//
// Boundary, stated rather than papered over: this is derived from the SHARED
// stored-syntax path in markdown.ts. The legacy documents surface has no
// renderer of its own that I could locate — every wiki-link consumer found
// routes through these two functions — so the rule is pinned to them.
func wikiTitleRoundTripFailure(title string) string {
	if strings.Contains(title, "]") {
		return `Title may not contain "]" — it would end the [[wiki-links]] that point at this document early, ` +
			`turning them into broken links`
	}
	if strings.HasSuffix(title, `\`) {
		return `Title may not end with "\" — it would escape the closing bracket of the [[wiki-links]] that point ` +
			`at this document`
	}
	if strings.Contains(title, `\\`) || strings.Contains(title, `\|`) {
		return `Title may not contain "\\" or "\|" — those are escape sequences in [[wiki-link]] syntax, so the ` +
			`links that point at this document would resolve to a different title`
	}
	return ""
}

// ValidateDocumentTitle checks a document title at write time. Returns a
// message suitable for a 400 response, or "" when the title is acceptable.
//
// Covers BUG-2798 (length, which is an amplification factor on OTHER
// documents' bodies, not merely a field-size preference) and BUG-2796
// (wiki-link syntax the cascade emits raw). Both are write-time doors on the
// same field, which is why they share one validator and one insertion point.
func ValidateDocumentTitle(title string) string {
	if title == "" {
		return "Title is required"
	}
	if n := utf8.RuneCountInString(title); n > MaxDocumentTitleRunes {
		return fmt.Sprintf("Title is too long: %d characters, maximum %d", n, MaxDocumentTitleRunes)
	}
	return wikiTitleRoundTripFailure(title)
}

func IsValidDocType(t string) bool {
	for _, v := range ValidDocTypes {
		if v == t {
			return true
		}
	}
	return false
}

func IsValidStatus(s string) bool {
	for _, v := range ValidStatuses {
		if v == s {
			return true
		}
	}
	return false
}

func IsValidActor(a string) bool {
	return a == "user" || a == "agent"
}

func IsValidSource(s string) bool {
	return s == "cli" || s == "web" || s == "skill"
}
