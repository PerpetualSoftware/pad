package store

import (
	"encoding/json"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Activity metadata keys that link an item's writes into a chain
// (PLAN-3578). An entry stamped {seq: S, prev_seq: P} records a write that
// moved the item from P to S. ActivitySeqChainBroken marks a merged entry
// whose runs were not contiguous, so the chain cannot pass through it.
const (
	ActivityMetaSeq            = "seq"
	ActivityMetaPrevSeq        = "prev_seq"
	ActivityMetaVersionID      = "version_id"
	ActivitySeqChainBroken     = "seq_chain_broken"
	itemSeqChainActivityWindow = 500
)

// ItemSeqChain walks the item's stamped activity entries from toSeq back to
// fromSeq and returns them newest first, at most max. Complete is true only
// when the walk reaches fromSeq exactly without a gap.
func (s *Store) ItemSeqChain(itemID string, fromSeq, toSeq int64, max int) (*models.PatchIntervening, error) {
	out := &models.PatchIntervening{FromSeq: fromSeq, ToSeq: toSeq, Changes: []models.PatchInterveningChange{}}
	if toSeq <= fromSeq {
		out.Complete = toSeq == fromSeq
		return out, nil
	}
	acts, err := s.ListDocumentActivity(itemID, models.ActivityListParams{Limit: itemSeqChainActivityWindow})
	if err != nil {
		return nil, err
	}
	bySeq := map[int64]models.PatchInterveningChange{}
	broken := map[int64]bool{}
	for _, a := range acts {
		seq, prev, vid, isBroken, ok := seqStamp(a.Metadata)
		if !ok {
			continue
		}
		if _, seen := bySeq[seq]; seen {
			continue // newest first: the first one listed wins
		}
		bySeq[seq] = models.PatchInterveningChange{
			ActivityID: a.ID, Seq: seq, PrevSeq: prev, Action: a.Action,
			Actor: a.Actor, Source: a.Source, At: a.CreatedAt, VersionID: vid,
		}
		broken[seq] = isBroken
	}
	for cursor := toSeq; ; {
		if cursor == fromSeq {
			out.Complete = true
			return out, nil
		}
		c, ok := bySeq[cursor]
		// PrevSeq strictly below cursor keeps the walk finite on a malformed
		// entry. A walk that steps past fromSeq needs no check of its own:
		// seqs only fall, so it never meets fromSeq and ends at a missing
		// entry, incomplete.
		if !ok || broken[cursor] || c.PrevSeq >= cursor {
			return out, nil
		}
		if len(out.Changes) < max {
			out.Changes = append(out.Changes, c)
		} else {
			out.Truncated = true
		}
		cursor = c.PrevSeq
	}
}

// seqStamp reads an entry's chain stamp. Values are strings (the handlers'
// metadata maps are string-valued) or numbers; ok is false without both.
func seqStamp(meta string) (seq, prev int64, versionID string, broken, ok bool) {
	var m map[string]interface{}
	if json.Unmarshal([]byte(meta), &m) != nil || m == nil {
		return 0, 0, "", false, false
	}
	seq, ok1 := metaInt(m[ActivityMetaSeq])
	prev, ok2 := metaInt(m[ActivityMetaPrevSeq])
	if !ok1 || !ok2 {
		return 0, 0, "", false, false
	}
	versionID, _ = m[ActivityMetaVersionID].(string)
	b, _ := m[ActivitySeqChainBroken].(string)
	return seq, prev, versionID, b == "true", true
}

func metaInt(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	case float64:
		return int64(x), true
	}
	return 0, false
}

// mergeSeqChain fixes the chain keys of a debounced merge: the merged entry
// spans the first run's prev_seq to the latest seq, which is true only when
// the runs were contiguous (the existing seq is the incoming prev_seq).
// Otherwise a write between them recorded no stamp here, and the merged
// entry is marked broken, so the chain can never read through the gap.
func mergeSeqChain(existMap, newMap map[string]interface{}) {
	es, eok := metaInt(existMap[ActivityMetaSeq])
	ep, epok := metaInt(existMap[ActivityMetaPrevSeq])
	_, nok := metaInt(newMap[ActivityMetaSeq])
	np, npok := metaInt(newMap[ActivityMetaPrevSeq])
	if !nok || !npok {
		// The incoming write carried no stamp: the merged entry cannot
		// vouch for the span it now covers.
		if eok {
			existMap[ActivitySeqChainBroken] = "true"
		}
		return
	}
	if !eok || !epok || es != np {
		existMap[ActivitySeqChainBroken] = "true"
		return
	}
	// Contiguous: keep the first run's prev_seq (the overlay set the
	// incoming one).
	existMap[ActivityMetaPrevSeq] = strconv.FormatInt(ep, 10)
}
