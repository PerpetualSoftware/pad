package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Typed-decision storage and its job queue (PLAN-3114 unit 2, TASK-3117).
//
// See migration 090 for why the queue is a private table rather than the
// event outbox, and why answers are keyed on a state hash rather than
// items.seq.
//
// THE WRITE PATH NEVER WAITS ON THE NETWORK (the day-73 async-only ruling).
// A mutation that makes an evaluation owed inserts a decision_jobs row in its
// own transaction and returns; the runner's tick does the provider call later.

// DecisionSetResolver names the question sets that apply to an item in the
// collection with this slug. The server installs one when a provider is
// configured; with no resolver installed, or one that returns nothing, the
// write doors enqueue nothing — which is the nil-provider case: no job row, no
// error.
//
// It is called INSIDE the write transaction, so it must be pure: a resolver
// that reads the database through the pool would wait on a connection the
// transaction may be holding (the MaxOpenConns(1) deadlock class). That is
// why it takes the slug — read in-tx by the store — rather than an id it
// would have to look up.
type DecisionSetResolver func(collectionSlug string) []string

// SetDecisionSetResolver installs the resolver, or removes it with nil.
//
// Atomic, unlike the test seams on Store, because this is production wiring:
// every write door reads it, and nothing guarantees it is installed before
// the first request.
func (s *Store) SetDecisionSetResolver(r DecisionSetResolver) {
	if r == nil {
		s.decisionSets.Store(nil)
		return
	}
	s.decisionSets.Store(&r)
}

func (s *Store) decisionSetResolver() DecisionSetResolver {
	p := s.decisionSets.Load()
	if p == nil {
		return nil
	}
	return *p
}

// decisionSetsPtr is the Store field's type, named so store.go's struct can
// declare it without importing sync/atomic for one line.
type decisionSetsPtr = atomic.Pointer[DecisionSetResolver]

// decisionTimeFormat is FIXED-WIDTH, unlike RFC3339Nano, which drops trailing
// zeros. These columns are compared and ordered as TEXT, and a variable-width
// fractional part sorts `…05.5Z` after `…05.123Z` but `…05Z` after both —
// the same hazard BUG-3037 measured on updated_at. Nanosecond width because
// two evaluations of one item inside a second are ordinary (an edit, then a
// comment), and "latest" must still mean latest.
const decisionTimeFormat = "2006-01-02T15:04:05.000000000Z"

func decisionTime(t time.Time) string { return t.UTC().Format(decisionTimeFormat) }

// enqueueDecisionJobsTx makes an evaluation owed for every question set that
// applies to the item, on the caller's transaction.
//
// COALESCING: one row per (item, question_set). A conflicting enqueue bumps
// generation instead of adding a row, and clears the failure history, because
// the new generation is new work. enqueued_at is NOT moved: an item edited
// continuously would otherwise sort to the back of the claim scan forever.
func (s *Store) enqueueDecisionJobsTx(tx *sql.Tx, workspaceID, itemID, collectionID string) error {
	resolve := s.decisionSetResolver()
	if resolve == nil {
		return nil
	}
	slug, err := s.getCollectionSlugTx(tx, collectionID)
	if err != nil {
		return fmt.Errorf("enqueue decision job: read collection %s: %w", collectionID, err)
	}
	sets := resolve(slug)
	if len(sets) == 0 {
		return nil
	}
	ts := decisionTime(time.Now())
	for _, set := range sets {
		if _, err := tx.Exec(s.q(`
			INSERT INTO decision_jobs (item_id, workspace_id, question_set, generation, enqueued_at, attempts)
			VALUES (?, ?, ?, 1, ?, 0)
			ON CONFLICT (item_id, question_set) DO UPDATE SET
				generation = decision_jobs.generation + 1,
				attempts   = 0,
				last_error = NULL`),
			itemID, workspaceID, set, ts,
		); err != nil {
			return fmt.Errorf("enqueue decision job %s/%s: %w", itemID, set, err)
		}
	}
	return nil
}

// enqueueDecisionJobsForItemTx is the comment door's form: a comment knows its
// item but not the item's collection.
func (s *Store) enqueueDecisionJobsForItemTx(tx *sql.Tx, itemID string) error {
	if s.decisionSetResolver() == nil {
		return nil
	}
	var workspaceID, collectionID string
	if err := tx.QueryRow(s.q(`SELECT workspace_id, collection_id FROM items WHERE id = ?`), itemID).
		Scan(&workspaceID, &collectionID); err != nil {
		return fmt.Errorf("enqueue decision job: read item %s: %w", itemID, err)
	}
	return s.enqueueDecisionJobsTx(tx, workspaceID, itemID, collectionID)
}

// DecisionJob is one claimed unit of owed work.
type DecisionJob struct {
	ItemID      string
	WorkspaceID string
	QuestionSet string
	// Generation is the generation this claim covers. Completing the job
	// deletes it only if the row still carries this generation.
	Generation int64
	Attempts   int
	ClaimedBy  string
}

// decisionJobClaimable is the single definition of "this job may be taken",
// used by BOTH the candidate scan and the claim UPDATE's arbiter so the two
// cannot drift (the reminderFireable lesson). Its one parameter is the current
// time: an unclaimed row, or one whose lease has expired.
const decisionJobClaimable = `(claimed_by IS NULL OR lease_expires_at IS NULL OR lease_expires_at < ?)`

// ClaimDecisionJobs claims up to limit owed jobs for runner, oldest first,
// each leased until now+lease.
//
// The scan and the claim are separate statements; the claim's WHERE re-checks
// claimability AND the scanned generation, so a job another runner took, or
// one re-enqueued in between, is skipped rather than double-claimed.
func (s *Store) ClaimDecisionJobs(runner string, limit int, lease time.Duration) ([]DecisionJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	nowT := time.Now()
	nowS := decisionTime(nowT)
	until := decisionTime(nowT.Add(lease))

	rows, err := s.db.Query(s.q(`
		SELECT item_id, workspace_id, question_set, generation, attempts
		FROM decision_jobs
		WHERE `+decisionJobClaimable+`
		ORDER BY enqueued_at, item_id, question_set
		LIMIT ?`), nowS, limit)
	if err != nil {
		return nil, fmt.Errorf("scan decision jobs: %w", err)
	}
	var candidates []DecisionJob
	for rows.Next() {
		var j DecisionJob
		if err := rows.Scan(&j.ItemID, &j.WorkspaceID, &j.QuestionSet, &j.Generation, &j.Attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan decision job: %w", err)
		}
		candidates = append(candidates, j)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	claimed := make([]DecisionJob, 0, len(candidates))
	for _, j := range candidates {
		res, err := s.db.Exec(s.q(`
			UPDATE decision_jobs SET claimed_by = ?, lease_expires_at = ?
			WHERE item_id = ? AND question_set = ? AND generation = ?
			  AND `+decisionJobClaimable),
			runner, until, j.ItemID, j.QuestionSet, j.Generation, nowS)
		if err != nil {
			return claimed, fmt.Errorf("claim decision job %s/%s: %w", j.ItemID, j.QuestionSet, err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			j.ClaimedBy = runner
			claimed = append(claimed, j)
		}
	}
	return claimed, nil
}

// CompleteDecisionJob retires a claimed job.
//
// The job is deleted only if its generation is still the one claimed. If a
// write re-enqueued it mid-evaluation, the row stays and the claim is
// released, so the next tick evaluates the newer state — the finished run
// must not delete work it never saw.
func (s *Store) CompleteDecisionJob(j DecisionJob) error {
	res, err := s.db.Exec(s.q(`
		DELETE FROM decision_jobs
		WHERE item_id = ? AND question_set = ? AND generation = ? AND claimed_by = ?`),
		j.ItemID, j.QuestionSet, j.Generation, j.ClaimedBy)
	if err != nil {
		return fmt.Errorf("complete decision job %s/%s: %w", j.ItemID, j.QuestionSet, err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	return s.releaseDecisionJob(j, "", 0)
}

// FailDecisionJob records a failed attempt and holds the job until retryAfter
// has passed. The lease doubles as the backoff: the row stays claimed by this
// runner, with an expiry at the retry time, and becomes claimable again when
// it lapses. A job re-enqueued in the meantime is simply released — its new
// generation is new work, and its failure history was cleared by the enqueue.
func (s *Store) FailDecisionJob(j DecisionJob, cause error, retryAfter time.Duration) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	return s.releaseDecisionJob(j, msg, retryAfter)
}

// DropDecisionJob deletes a job outright, whatever its generation: the item
// is gone, or the job has exhausted its attempts. A write that lands after
// this re-creates it.
func (s *Store) DropDecisionJob(j DecisionJob) error {
	_, err := s.db.Exec(s.q(`DELETE FROM decision_jobs WHERE item_id = ? AND question_set = ? AND claimed_by = ?`),
		j.ItemID, j.QuestionSet, j.ClaimedBy)
	if err != nil {
		return fmt.Errorf("drop decision job %s/%s: %w", j.ItemID, j.QuestionSet, err)
	}
	return nil
}

// ReleaseDecisionJob gives a claimed job back without recording a failure:
// the runner stopped before finishing it (shutdown), which says nothing about
// the job. It is claimable again at once.
func (s *Store) ReleaseDecisionJob(j DecisionJob) error {
	return s.releaseDecisionJob(j, "", 0)
}

func (s *Store) releaseDecisionJob(j DecisionJob, failure string, retryAfter time.Duration) error {
	if failure == "" {
		// Superseded: release the claim so the newer generation is claimable
		// at once.
		_, err := s.db.Exec(s.q(`
			UPDATE decision_jobs SET claimed_by = NULL, lease_expires_at = NULL
			WHERE item_id = ? AND question_set = ? AND claimed_by = ?`),
			j.ItemID, j.QuestionSet, j.ClaimedBy)
		if err != nil {
			return fmt.Errorf("release decision job %s/%s: %w", j.ItemID, j.QuestionSet, err)
		}
		return nil
	}
	_, err := s.db.Exec(s.q(`
		UPDATE decision_jobs
		SET attempts = attempts + 1, last_error = ?, lease_expires_at = ?
		WHERE item_id = ? AND question_set = ? AND generation = ? AND claimed_by = ?`),
		truncateDecisionError(failure), decisionTime(time.Now().Add(retryAfter)),
		j.ItemID, j.QuestionSet, j.Generation, j.ClaimedBy)
	if err != nil {
		return fmt.Errorf("fail decision job %s/%s: %w", j.ItemID, j.QuestionSet, err)
	}
	// Zero rows means the job was re-enqueued; the enqueue already reset its
	// failure history, so release the claim for the new generation.
	return s.releaseSupersededDecisionJob(j)
}

func (s *Store) releaseSupersededDecisionJob(j DecisionJob) error {
	_, err := s.db.Exec(s.q(`
		UPDATE decision_jobs SET claimed_by = NULL, lease_expires_at = NULL
		WHERE item_id = ? AND question_set = ? AND claimed_by = ? AND generation <> ?`),
		j.ItemID, j.QuestionSet, j.ClaimedBy, j.Generation)
	if err != nil {
		return fmt.Errorf("release decision job %s/%s: %w", j.ItemID, j.QuestionSet, err)
	}
	return nil
}

// maxDecisionErrorBytes bounds last_error: a provider error can carry a
// response body, and this column is diagnostics, not a log.
const maxDecisionErrorBytes = 1024

func truncateDecisionError(s string) string {
	if len(s) <= maxDecisionErrorBytes {
		return s
	}
	return s[:maxDecisionErrorBytes]
}

// DecisionJobState is a test/diagnostic read of one job row.
type DecisionJobState struct {
	Generation int64
	Attempts   int
	ClaimedBy  string
	LastError  string
}

// GetDecisionJob returns a job's row, or nil when no evaluation is owed.
func (s *Store) GetDecisionJob(itemID, questionSet string) (*DecisionJobState, error) {
	var st DecisionJobState
	var claimedBy, lastError sql.NullString
	err := s.db.QueryRow(s.q(`
		SELECT generation, attempts, claimed_by, last_error FROM decision_jobs
		WHERE item_id = ? AND question_set = ?`), itemID, questionSet).
		Scan(&st.Generation, &st.Attempts, &claimedBy, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get decision job: %w", err)
	}
	st.ClaimedBy, st.LastError = claimedBy.String, lastError.String
	return &st, nil
}

// HasItemDecisionsAtState reports whether every one of keys already has an
// answer for this item, set and state hash — the runner's idempotency check.
func (s *Store) HasItemDecisionsAtState(itemID, questionSet, stateHash string, keys []string) (bool, error) {
	if len(keys) == 0 {
		return true, nil
	}
	var n int
	if err := s.db.QueryRow(s.q(`
		SELECT COUNT(DISTINCT question_key) FROM item_decisions
		WHERE item_id = ? AND question_set = ? AND state_hash = ?`),
		itemID, questionSet, stateHash).Scan(&n); err != nil {
		return false, fmt.Errorf("check item decisions: %w", err)
	}
	// COUNT rather than per-key lookups: keys come from the registered set,
	// and a stored key no longer in the set can only over-count if the set
	// SHRANK — the runner then re-asks, which is the safe direction.
	return n >= len(keys), nil
}

// InsertItemDecisions stores one evaluation's answers in one transaction.
// A row already present for the same (item, set, key, state) is kept, not
// replaced: two runners that evaluated the same state concurrently stored
// equivalent answers, and the first one is the record.
func (s *Store) InsertItemDecisions(workspaceID string, rows []models.ItemDecision) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	for _, r := range rows {
		truncated := 0
		if r.StateTruncated {
			truncated = 1
		}
		evaluated := r.EvaluatedAt
		if evaluated == "" {
			evaluated = decisionTime(time.Now())
		}
		if _, err := tx.Exec(s.q(`
			INSERT INTO item_decisions
				(id, workspace_id, item_id, question_set, question_key, kind, answer, confidence,
				 provider, model, state_hash, item_seq, state_truncated, evaluated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (item_id, question_set, question_key, state_hash) DO NOTHING`),
			newID(), workspaceID, r.ItemID, r.QuestionSet, r.QuestionKey, r.Kind, string(r.Answer), r.Confidence,
			r.Provider, r.Model, r.StateHash, r.ItemSeq, truncated, evaluated,
		); err != nil {
			return fmt.Errorf("insert item decision %s/%s: %w", r.QuestionSet, r.QuestionKey, err)
		}
	}
	return tx.Commit()
}

// LatestItemDecisions returns the newest answer per (question_set,
// question_key) for an item, ordered by set then key. Current is left false;
// only the caller can compute the item's present state hash.
//
// Older rows stay in the table for the audit; this read hides them.
func (s *Store) LatestItemDecisions(itemID string) ([]models.ItemDecision, error) {
	rows, err := s.db.Query(s.q(`
		SELECT d.id, d.item_id, d.question_set, d.question_key, d.kind, d.answer, d.confidence,
		       d.provider, d.model, d.state_hash, d.item_seq, d.state_truncated, d.evaluated_at
		FROM item_decisions d
		WHERE d.item_id = ?
		  AND NOT EXISTS (
			SELECT 1 FROM item_decisions n
			WHERE n.item_id = d.item_id AND n.question_set = d.question_set AND n.question_key = d.question_key
			  AND (n.evaluated_at > d.evaluated_at OR (n.evaluated_at = d.evaluated_at AND n.id > d.id))
		  )
		ORDER BY d.question_set, d.question_key`), itemID)
	if err != nil {
		return nil, fmt.Errorf("list item decisions: %w", err)
	}
	defer rows.Close()
	var out []models.ItemDecision
	for rows.Next() {
		var d models.ItemDecision
		var answer string
		var conf sql.NullFloat64
		var truncated int
		if err := rows.Scan(&d.ID, &d.ItemID, &d.QuestionSet, &d.QuestionKey, &d.Kind, &answer, &conf,
			&d.Provider, &d.Model, &d.StateHash, &d.ItemSeq, &truncated, &d.EvaluatedAt); err != nil {
			return nil, fmt.Errorf("scan item decision: %w", err)
		}
		d.Answer = []byte(answer)
		if conf.Valid {
			c := conf.Float64
			d.Confidence = &c
		}
		d.StateTruncated = truncated != 0
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecentComments returns an item's newest n comments, oldest first — the
// "recent trail" a decision's state carries. Ordered by (created_at, id) so a
// burst of comments inside one second still has one order, and therefore one
// state hash.
func (s *Store) RecentComments(itemID string, n int) ([]models.Comment, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(s.q(`
		SELECT `+commentListCols+`
		FROM comments c
		`+commentAgentJoin+`
		WHERE c.item_id = ?
		ORDER BY c.created_at DESC, c.id DESC
		LIMIT ?`), itemID, n)
	if err != nil {
		return nil, fmt.Errorf("recent comments: %w", err)
	}
	defer rows.Close()
	out, err := scanComments(rows)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ItemEvaluable reports whether an item may be sent to a decision provider:
// the item, its collection and its workspace are all live.
//
// GetItem filters only the ITEM's soft delete. An item in a soft-deleted
// workspace or collection still reads — and evaluating it would send content
// its owner deleted to a third-party provider during the restore window,
// which the deletion was supposed to stop. Same three-way liveness the
// reminder tick's reminderFireable enforces, for the same reason.
func (s *Store) ItemEvaluable(itemID string) (bool, error) {
	var n int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM items i
		JOIN collections c ON c.id = i.collection_id
		JOIN workspaces w ON w.id = i.workspace_id
		WHERE i.id = ? AND i.deleted_at IS NULL AND c.deleted_at IS NULL AND w.deleted_at IS NULL`), itemID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("item evaluable: %w", err)
	}
	return n == 1, nil
}
