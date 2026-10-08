package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3119 U2b: a PerComment set asks its questions about each COMMENT on
// the item, one comment per state (BuildCommentState: the item's title and
// collection, and the comment), instead of about the item. Measured before it
// shipped (U2 gate 2): 7 of 7 comment-borne breaks caught, 0 of 9 false, a
// credential at rune 2,786 of a comment (past the old trail clip) caught.
//
// Answers are stored in item_decisions under the item, keyed
// "<question key>@<comment id>", with the comment's state hash.
//
// ASKED vs CURRENT (lead ruling). Only the item's last RecentTrailWindow live
// comments are asked or re-asked, which bounds an evaluation at that many
// comments. But an answer stays CURRENT while its comment is unchanged however
// many newer comments arrive: a pasted credential's chip must not vanish
// because ten more comments were written after it.

// CommentKeySep joins a question key and the comment it is about.
const CommentKeySep = "@"

func commentKey(key, commentID string) string { return key + CommentKeySep + commentID }

// SplitCommentKey returns the question key and the comment id of a PerComment
// row's key. ok is false for a key that names no comment.
func SplitCommentKey(k string) (key, commentID string, ok bool) {
	i := strings.LastIndex(k, CommentKeySep)
	if i <= 0 || i == len(k)-1 {
		return "", "", false
	}
	return k[:i], k[i+1:], true
}

// evaluatePerComment is Evaluate for a PerComment set.
func (r *Runner) evaluatePerComment(ctx context.Context, qs QuestionSet, itemID string) (called bool, err error) {
	item, err := r.store.GetItem(itemID)
	if err != nil {
		return false, fmt.Errorf("decision: read item %s: %w", itemID, err)
	}
	if item == nil || item.DeletedAt != nil {
		return false, ErrItemGone
	}
	if ok, err := r.appliesNow(qs, item); err != nil {
		return false, err
	} else if !ok {
		return false, ErrSetNotApplicable
	}
	questions, err := r.questionsFor(ctx, qs, item.WorkspaceID)
	if err != nil {
		return false, err
	}
	if len(questions) == 0 {
		return false, nil
	}
	keys := make([]string, 0, len(questions))
	qhash := make(map[string]string, len(questions))
	for k, q := range questions {
		keys = append(keys, k)
		qhash[k] = QuestionFingerprint(r.provider.Model(), q)
	}
	sort.Strings(keys)

	// The asked window: the newest live comments. Tombstones are excluded by
	// RecentComments and are never asked.
	comments, err := r.store.RecentComments(itemID, RecentTrailWindow)
	if err != nil {
		return false, err
	}

	var usage Usage
	calls, asked := 0, 0
	truncated := false
	defer func() {
		if calls > 0 {
			r.logSpend(qs.Name, item, asked, calls, usage, truncated, err)
		}
	}()

	for _, c := range comments {
		st, berr := BuildCommentState(item.Title, item.CollectionSlug, c.Body)
		if berr != nil {
			return calls > 0, berr
		}
		want := make(map[string]string, len(keys))
		for _, k := range keys {
			want[commentKey(k, c.ID)] = qhash[k]
		}
		have, herr := r.store.HasItemDecisionsAtState(item.ID, qs.Name, st.Hash, want)
		if herr != nil {
			return calls > 0, herr
		}
		if have {
			continue
		}

		answers := make(map[string]Answer, len(keys))
		commentTruncated := st.Truncated
		for _, chunk := range chunkKeys(keys, qs.MaxPerCall) {
			cq := make(map[string]Question, len(chunk))
			for _, k := range chunk {
				cq[k] = questions[k]
			}
			if r.beforeAsk != nil {
				r.beforeAsk()
			}
			// Liveness as the last statement before each send, as Evaluate.
			itemLive, wsLive, lerr := r.store.ItemLiveness(item.ID)
			if lerr != nil {
				return calls > 0, lerr
			}
			if !itemLive {
				return calls > 0, ErrItemGone
			}
			if !wsLive {
				return calls > 0, ErrWorkspaceDeleted
			}
			got, u, aerr := r.provider.Ask(ctx, st.Bytes, cq)
			calls++
			asked += len(cq)
			if r.usage != nil {
				r.usage(qs.Name, u)
			}
			usage.InputTokens += u.InputTokens
			usage.OutputTokens += u.OutputTokens
			usage.StateTruncated = usage.StateTruncated || u.StateTruncated
			commentTruncated = commentTruncated || u.StateTruncated
			if aerr != nil {
				return true, aerr
			}
			for k, a := range got {
				answers[k] = a
			}
		}
		truncated = truncated || commentTruncated

		rows := make([]models.ItemDecision, 0, len(keys))
		for _, k := range keys {
			a, ok := answers[k]
			if !ok {
				return true, fmt.Errorf("decision: provider returned no answer for %q", k)
			}
			raw, merr := json.Marshal(storedAnswerOf(a))
			if merr != nil {
				return true, merr
			}
			rows = append(rows, models.ItemDecision{
				ItemID:         item.ID,
				QuestionSet:    qs.Name,
				QuestionKey:    commentKey(k, c.ID),
				Kind:           string(a.Kind),
				Answer:         raw,
				Confidence:     a.Confidence,
				Provider:       r.provider.Name(),
				Model:          r.provider.Model(),
				StateHash:      st.Hash,
				QuestionHash:   qhash[k],
				ItemSeq:        item.Seq,
				StateTruncated: commentTruncated,
			})
		}
		// Per comment, so a later comment's failure does not cost the
		// answers already paid for.
		if ierr := r.store.InsertItemDecisions(item.WorkspaceID, rows); ierr != nil {
			return true, ierr
		}
	}
	return calls > 0, nil
}

// commentStateNow is a comment's present state hash for a PerComment set, or
// "" when the comment is gone (deleted, tombstoned, or moved off the item).
// Any age: currency is not bounded by the asked window.
func (r *Runner) commentStateNow(item *models.Item, commentID string, cache map[string]string) (string, error) {
	if h, ok := cache[commentID]; ok {
		return h, nil
	}
	c, err := r.store.GetComment(commentID)
	if err != nil {
		return "", err
	}
	h := ""
	if c != nil && !c.Deleted && c.ItemID == item.ID {
		st, berr := BuildCommentState(item.Title, item.CollectionSlug, c.Body)
		if berr != nil {
			return "", berr
		}
		h = st.Hash
	}
	cache[commentID] = h
	return h, nil
}
