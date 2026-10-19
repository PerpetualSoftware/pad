// Package verification judges stored verification rows against an item's
// present acceptance criteria (TASK-3617). It is pure: the store persists
// rows, the server reads the item and its linked PRs, and this package says
// what each row means now.
//
// Freshness keys on the CRITERION, not the item's seq: a row goes stale when
// the text of its own AC-N changes or disappears, and an edit anywhere else in
// the item leaves it current. Decision freshness made the same choice for the
// same reason (internal/decision, a per-comment row is judged against its own
// comment).
package verification

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/acceptance"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// CriterionHash is the sha256 hex of a criterion's text with runs of
// whitespace collapsed, so re-wrapping a criterion does not stale its
// evidence and rewording it does.
func CriterionHash(text string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return hex.EncodeToString(sum[:])
}

// Lookup is what a write needs to know about one criterion id.
type Lookup int

const (
	// LookupFound: exactly one real criterion carries the id.
	LookupFound Lookup = iota
	// LookupMissing: no real criterion carries it. A placeholder (an id still
	// holding skeleton text) is missing too: there is nothing to verify yet.
	LookupMissing
	// LookupAmbiguous: the id appears more than once, so a verdict could not
	// say which text it was about.
	LookupAmbiguous
)

// Criteria indexes a parsed section by id.
type Criteria struct {
	text        map[string]string
	count       map[string]int
	placeholder map[string]bool
	ids         []string
}

// Index builds the lookup for sec. A body with no acceptance-criteria section
// yields an empty index, in which every id is missing.
func Index(sec acceptance.Section) Criteria {
	c := Criteria{text: map[string]string{}, count: map[string]int{}, placeholder: map[string]bool{}}
	for _, cr := range sec.Criteria {
		if c.count[cr.ID] == 0 {
			c.text[cr.ID] = cr.Text
			c.ids = append(c.ids, cr.ID)
		}
		c.count[cr.ID]++
	}
	for _, id := range sec.Placeholders {
		c.placeholder[id] = true
	}
	return c
}

// Find reports how id resolves and, when it is found, its text's hash.
func (c Criteria) Find(id string) (Lookup, string) {
	n := c.count[id]
	switch {
	case n > 1 || (n == 1 && c.placeholder[id]):
		return LookupAmbiguous, ""
	case n == 1:
		return LookupFound, CriterionHash(c.text[id])
	default:
		return LookupMissing, ""
	}
}

// IDs lists the real (non-placeholder) criterion ids, in body order, once each.
func (c Criteria) IDs() []string {
	out := make([]string, 0, len(c.ids))
	for _, id := range c.ids {
		if l, _ := c.Find(id); l == LookupFound {
			out = append(out, id)
		}
	}
	return out
}

// LinkedPR is a merged PR whose merge may have moved the code a verdict was
// about: the item's own github_pr, or the github_pr of the item named as via.
type LinkedPR struct {
	Number   int
	MergeSHA string
	MergedAt string
}

// Judge sets State on every row, and PossiblyStale where a linked PR merged
// after the row was recorded. rows may be any subset of one item's history;
// "superseded" is decided within rows, so pass the whole history (or at least
// every row for a criterion) when it matters.
//
// The order is the design's: removed, then changed, then superseded, then
// current. A row whose criterion was reworded reads criterion_changed even
// when a newer row exists, because that is the more useful thing to know about
// it.
func Judge(rows []models.ItemVerification, c Criteria, prs []LinkedPR) {
	newest := map[string]int{}
	for i, r := range rows {
		j, ok := newest[r.CriterionID]
		if !ok || newer(r, rows[j]) {
			newest[r.CriterionID] = i
		}
	}
	for i := range rows {
		r := &rows[i]
		lookup, hash := c.Find(r.CriterionID)
		switch {
		case lookup == LookupMissing:
			r.State = models.VerificationCriterionRemoved
		case lookup == LookupAmbiguous || hash != r.CriterionHash:
			r.State = models.VerificationCriterionChanged
		case newest[r.CriterionID] != i:
			r.State = models.VerificationSuperseded
		default:
			r.State = models.VerificationCurrent
		}
		r.PossiblyStale = staleHint(*r, prs)
	}
}

// Latest keeps the newest row per criterion, in the order first seen.
func Latest(rows []models.ItemVerification) []models.ItemVerification {
	idx := map[string]int{}
	var out []models.ItemVerification
	for _, r := range rows {
		j, ok := idx[r.CriterionID]
		if !ok {
			idx[r.CriterionID] = len(out)
			out = append(out, r)
			continue
		}
		if newer(r, out[j]) {
			out[j] = r
		}
	}
	return out
}

// SortByCriteria orders rows the way a reader reads the spec: by the
// criterion's position in the body, then oldest first within a criterion.
// Rows whose criterion is no longer in the body come last, by id, then time.
// One batch shares a created_at, so without this its rows would come back in
// id order, which is random.
func SortByCriteria(rows []models.ItemVerification, c Criteria) {
	pos := map[string]int{}
	for i, id := range c.ids {
		pos[id] = i
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		pa, oka := pos[a.CriterionID]
		pb, okb := pos[b.CriterionID]
		switch {
		case oka != okb:
			return oka
		case oka && pa != pb:
			return pa < pb
		case !oka && a.CriterionID != b.CriterionID:
			return a.CriterionID < b.CriterionID
		}
		return newer(b, a)
	})
}

// newer orders rows by created_at, then id, the same tiebreak the store's
// latest-row read uses. created_at is fixed-width, so string order is time
// order.
func newer(a, b models.ItemVerification) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	return a.ID > b.ID
}

// staleHint returns the first linked PR that merged after the row was
// recorded at a different revision. A row with no code revision is not about
// code, and a PR whose merge commit IS the row's revision did not move it.
//
// The server cannot see the repository, so "after" is by time: the merge time
// against the row's created_at. That is why the hint is advisory and names
// its evidence rather than changing State.
func staleHint(r models.ItemVerification, prs []LinkedPR) *models.VerificationStaleHint {
	if r.CodeRevision == "" {
		return nil
	}
	recorded, err := time.Parse(time.RFC3339Nano, r.CreatedAt)
	if err != nil {
		return nil
	}
	for _, pr := range prs {
		if pr.MergeSHA == "" || pr.MergeSHA == r.CodeRevision {
			continue
		}
		merged, err := time.Parse(time.RFC3339, pr.MergedAt)
		if err != nil || !merged.After(recorded) {
			continue
		}
		return &models.VerificationStaleHint{
			Via:       "PR #" + strconv.Itoa(pr.Number),
			MergedSHA: pr.MergeSHA,
			MergedAt:  pr.MergedAt,
		}
	}
	return nil
}
