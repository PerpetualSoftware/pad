package decision

import (
	"context"
	"fmt"
	"log/slog"
)

// The one-shot effort backfill (TASK-3921, IDEA-3906 B4a).
//
// effort, effort_medium and high_stakes joined the attention set after the
// set had already answered needs_human_decision for every item it covers. Left
// alone, an item would only learn its effort answers on its next write, so the
// staged questions would sit empty for most of the board when their reader
// (TASK-3959) arrives. The backfill owes ONE job per item that has a
// needs_human_decision answer and no effort answer, and lets the tick answer
// them under the re-ask debounce (an item evaluated inside the window waits out
// the rest of it). It makes NO provider call itself.
//
// It is gated by a platform_settings marker written only after every chunk has
// been owed, so an interrupted run owes the rest on the next one. Re-owing an
// item that already has a job coalesces into the same row (a generation bump),
// so a repeat is idempotent rather than additive.

// EffortBackfillSetting is the platform_settings marker: set means every
// candidate was owed.
const EffortBackfillSetting = "effort_decisions_backfilled"

// effortBackfillChunk is how many items one OweDecisionJobs call covers.
const effortBackfillChunk = 500

// EffortBackfillCandidates returns the items the backfill would owe a job
// for, grouped by workspace in workspace order: live, answered for
// needs_human_decision, never answered for effort (any state or hash), and
// eligible for the attention set as it applies NOW (the runner's own rule,
// appliesNow). It owes nothing and calls no provider; it is the dry run.
func (r *Runner) EffortBackfillCandidates(ctx context.Context) (map[string][]string, []string, error) {
	if r == nil {
		return nil, nil, nil
	}
	qs, ok := r.registry.Get(AttentionSetName)
	if !ok {
		return nil, nil, ErrUnknownSet
	}
	cands, err := r.store.ItemsMissingDecisionKey(AttentionSetName, AttentionNeedsHuman, AttentionEffort)
	if err != nil {
		return nil, nil, err
	}
	byWS := map[string][]string{}
	var order []string
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		item, err := r.store.GetItem(c.ItemID)
		if err != nil {
			return nil, nil, fmt.Errorf("decision: read item %s: %w", c.ItemID, err)
		}
		if item == nil || item.DeletedAt != nil {
			continue
		}
		applies, err := r.appliesNow(qs, item)
		if err != nil {
			return nil, nil, err
		}
		if !applies {
			continue
		}
		if _, seen := byWS[c.WorkspaceID]; !seen {
			order = append(order, c.WorkspaceID)
		}
		byWS[c.WorkspaceID] = append(byWS[c.WorkspaceID], c.ItemID)
	}
	return byWS, order, nil
}

// CountEffortBackfill is the dry run: how many jobs BackfillEffort would owe
// now. It ignores the marker and writes nothing.
func (r *Runner) CountEffortBackfill(ctx context.Context) (int, error) {
	byWS, _, err := r.EffortBackfillCandidates(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ids := range byWS {
		n += len(ids)
	}
	return n, nil
}

// BackfillEffort owes the attention set to every candidate, then writes the
// marker. owed is the number of jobs owed by this call (0 when the marker was
// already set). A nil runner does nothing.
//
// ctx is checked before each candidate read and before each chunk. A cancel
// that lands before a chunk returns ctx.Err() with no marker, so the next run
// redoes the work. A cancel that lands after the last chunk may still write
// the marker, which is correct: the marker means every candidate was ENQUEUED,
// not answered (lead ruling on TASK-3921, mutant M5).
func (r *Runner) BackfillEffort(ctx context.Context) (owed int, err error) {
	if r == nil {
		return 0, nil
	}
	done, err := r.store.GetPlatformSetting(EffortBackfillSetting)
	if err != nil {
		return 0, fmt.Errorf("decision: read %s: %w", EffortBackfillSetting, err)
	}
	if done != "" {
		return 0, nil
	}
	byWS, order, err := r.EffortBackfillCandidates(ctx)
	if err != nil {
		return 0, err
	}
	chunk := r.backfillChunk
	if chunk <= 0 {
		chunk = effortBackfillChunk
	}
	n := 0
	for _, ws := range order {
		ids := byWS[ws]
		for start := 0; start < len(ids); start += chunk {
			end := start + chunk
			if end > len(ids) {
				end = len(ids)
			}
			if r.beforeBackfillChunk != nil {
				if err := r.beforeBackfillChunk(ctx, n); err != nil {
					return owed, err
				}
			}
			// The chunk-boundary cancel check (single line on purpose).
			if err := ctx.Err(); err != nil {
				return owed, err
			}
			if err := r.store.OweDecisionJobs(ws, AttentionSetName, ids[start:end]); err != nil {
				return owed, err
			}
			owed += end - start
			n++
		}
	}
	// ONLY NOW: every chunk was owed.
	if err := r.store.SetPlatformSetting(EffortBackfillSetting, "1"); err != nil {
		return owed, fmt.Errorf("decision: persist %s: %w", EffortBackfillSetting, err)
	}
	l := r.logger
	if l == nil {
		l = slog.Default()
	}
	l.Info("effort backfill: owed attention jobs for items with no effort answer", "owed", owed)
	return owed, nil
}
