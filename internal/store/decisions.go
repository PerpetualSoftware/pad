package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/textguard"
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

// DecisionDebounce returns the re-ask window for a question set (TASK-3916):
// how long after the item's latest evaluation for that set a new write's job
// waits before it is claimable. Zero means no wait.
type DecisionDebounce func(questionSet string) time.Duration

// SetDecisionDebounce installs the window function, or removes it with nil
// (no set waits). Atomic for the same reason as the resolver: every write
// door reads it.
func (s *Store) SetDecisionDebounce(d DecisionDebounce) {
	if d == nil {
		s.decisionDebounce.Store(nil)
		return
	}
	s.decisionDebounce.Store(&d)
}

func (s *Store) decisionDebounceFunc() DecisionDebounce {
	p := s.decisionDebounce.Load()
	if p == nil {
		return nil
	}
	return *p
}

// decisionWindowFor is debounce(set), or zero when no debounce is installed.
func decisionWindowFor(debounce DecisionDebounce, questionSet string) time.Duration {
	if debounce == nil {
		return 0
	}
	return debounce(questionSet)
}

// latestDecisionEvaluationSQL reads the item's latest evaluation for a set;
// bind item_id, question_set. Run on the caller's own handle (a tx or the db)
// and passed to decisionNotBefore as the row.
const latestDecisionEvaluationSQL = `
		SELECT MAX(evaluated_at) FROM item_decisions WHERE item_id = ? AND question_set = ?`

// decisionNotBefore is the not_before a job gets, from the row of
// latestDecisionEvaluationSQL: that evaluation plus window, or "" (NULL,
// claimable now) when the window is zero, the item has never been evaluated
// for the set (a first evaluation is never delayed), or the window has
// already passed. The arithmetic is in Go, on decisionTime TEXT, so SQLite
// and Postgres agree. A caller with no window skips the read entirely.
func decisionNotBefore(latest *sql.Row, window time.Duration, now time.Time) (string, error) {
	var last sql.NullString
	if err := latest.Scan(&last); err != nil {
		return "", fmt.Errorf("read latest evaluation: %w", err)
	}
	if !last.Valid || last.String == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339Nano, last.String)
	if err != nil {
		// An unreadable timestamp must not hold a job back: ask now.
		return "", nil
	}
	nb := t.Add(window)
	if !nb.After(now) {
		return "", nil
	}
	return decisionTime(nb), nil
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// decisionSetsPtr is the Store field's type, named so store.go's struct can
// declare it without importing sync/atomic for one line.
type decisionSetsPtr = atomic.Pointer[DecisionSetResolver]

// decisionDebouncePtr is the debounce field's type, named for the same reason.
type decisionDebouncePtr = atomic.Pointer[DecisionDebounce]

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
//
// DEBOUNCE (TASK-3916): a fresh row gets not_before from decisionNotBefore.
// A conflicting enqueue KEEPS the row's existing not_before when it has one
// (COALESCE(old, new)), so a stream of comments cannot keep pushing the
// re-ask back; it only fills one in when the row had none.
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
	debounce := s.decisionDebounceFunc()
	now := time.Now()
	ts := decisionTime(now)
	for _, set := range sets {
		if err := s.owedDecisionJobTx(tx, workspaceID, itemID, set, debounce, now, ts); err != nil {
			return err
		}
	}
	return nil
}

// owedDecisionJobTx is the per-(item, set) body of enqueueDecisionJobsTx,
// shared with OweDecisionJobs so the backfill and the write doors cannot
// drift: same upsert, same coalescing, same debounce.
func (s *Store) owedDecisionJobTx(tx *sql.Tx, workspaceID, itemID, set string, debounce DecisionDebounce, now time.Time, ts string) error {
	notBefore := ""
	if window := decisionWindowFor(debounce, set); window > 0 {
		var err error
		if notBefore, err = decisionNotBefore(tx.QueryRow(s.q(latestDecisionEvaluationSQL), itemID, set), window, now); err != nil {
			return fmt.Errorf("enqueue decision job %s/%s: %w", itemID, set, err)
		}
	}
	if _, err := tx.Exec(s.q(`
		INSERT INTO decision_jobs (item_id, workspace_id, question_set, generation, enqueued_at, attempts, not_before)
		VALUES (?, ?, ?, 1, ?, 0, ?)
		ON CONFLICT (item_id, question_set) DO UPDATE SET
			generation = decision_jobs.generation + 1,
			attempts   = 0,
			last_error = NULL,
			not_before = COALESCE(decision_jobs.not_before, excluded.not_before)`),
		itemID, workspaceID, set, ts, nullableText(notBefore),
	); err != nil {
		return fmt.Errorf("enqueue decision job %s/%s: %w", itemID, set, err)
	}
	return nil
}

// oweDecisionChunk bounds the items one OweDecisionJobs transaction covers.
const oweDecisionChunk = 500

// OweDecisionJobs makes the one question set owed for each item, in chunked
// transactions, under the installed debounce (an item evaluated inside the
// window gets a future not_before, exactly as a write would give it). It does
// not consult the set resolver or any eligibility: the caller has decided.
// Re-owing an item that already has a job bumps its generation and keeps one
// row. Used by the one-shot effort backfill (TASK-3921).
func (s *Store) OweDecisionJobs(workspaceID, set string, itemIDs []string) error {
	debounce := s.decisionDebounceFunc()
	for start := 0; start < len(itemIDs); start += oweDecisionChunk {
		end := start + oweDecisionChunk
		if end > len(itemIDs) {
			end = len(itemIDs)
		}
		if err := s.oweDecisionChunk(workspaceID, set, itemIDs[start:end], debounce); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) oweDecisionChunk(workspaceID, set string, itemIDs []string, debounce DecisionDebounce) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	now := time.Now()
	ts := decisionTime(now)
	for _, id := range itemIDs {
		if err := s.owedDecisionJobTx(tx, workspaceID, id, set, debounce, now, ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DecisionBackfillCandidate is one live item the backfill may owe a job for.
type DecisionBackfillCandidate struct {
	ItemID      string
	WorkspaceID string
}

// ItemsMissingDecisionKey lists the live items that have a stored answer for
// withKey in the set (in any state or hash) and NONE for missingKey: the items
// the set has been asked about before the question missingKey existed. The
// caller filters by the set's current eligibility. Ordered for stable chunks.
func (s *Store) ItemsMissingDecisionKey(set, withKey, missingKey string) ([]DecisionBackfillCandidate, error) {
	rows, err := s.db.Query(s.q(`
		SELECT i.workspace_id, i.id
		FROM items i
		WHERE i.deleted_at IS NULL
		  AND EXISTS (SELECT 1 FROM item_decisions d
		              WHERE d.item_id = i.id AND d.question_set = ? AND d.question_key = ?)
		  AND NOT EXISTS (SELECT 1 FROM item_decisions e
		              WHERE e.item_id = i.id AND e.question_set = ? AND e.question_key = ?)
		ORDER BY i.workspace_id, i.id`), set, withKey, set, missingKey)
	if err != nil {
		return nil, fmt.Errorf("list decision backfill candidates: %w", err)
	}
	defer rows.Close()
	var out []DecisionBackfillCandidate
	for rows.Next() {
		var c DecisionBackfillCandidate
		if err := rows.Scan(&c.WorkspaceID, &c.ItemID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
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
// cannot drift (the reminderFireable lesson). Its TWO parameters are both the
// current time: an unclaimed row, or one whose lease has expired, and whose
// debounce (not_before, TASK-3916) has passed or was never set.
//
// A job whose WORKSPACE is soft-deleted is not claimable: it is held in place
// so a workspace restore finds the work still owed (codex round 5). Nothing
// enqueues on restore — that would be a fan-out over every item — so keeping
// the row is the only thing that makes a restore resume. A purge hard-deletes
// the items, and the composite FK cascades the held rows away.
const decisionJobClaimable = `(claimed_by IS NULL OR lease_expires_at IS NULL OR lease_expires_at < ?)
	AND (not_before IS NULL OR not_before <= ?)
	AND NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = decision_jobs.workspace_id AND w.deleted_at IS NOT NULL)`

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
		LIMIT ?`), nowS, nowS, limit)
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
			runner, until, j.ItemID, j.QuestionSet, j.Generation, nowS, nowS)
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

// CountClaimableDecisionJobs counts the set's jobs for itemIDs that
// ClaimDecisionJobs could take RIGHT NOW at `now`: the very decisionJobClaimable
// predicate, so the two cannot drift. A job that is claimed (lease live), held
// by the re-ask debounce (not_before ahead), held by a soft-deleted workspace,
// answered or deleted is not counted. The effort backfill uses it to tell
// whether its last chunk has drained (TASK-3921).
func (s *Store) CountClaimableDecisionJobs(set string, itemIDs []string, now time.Time) (int, error) {
	nowS := decisionTime(now)
	total := 0
	for start := 0; start < len(itemIDs); start += oweDecisionChunk {
		end := start + oweDecisionChunk
		if end > len(itemIDs) {
			end = len(itemIDs)
		}
		part := itemIDs[start:end]
		args := make([]any, 0, len(part)+3)
		args = append(args, set)
		marks := make([]string, len(part))
		for i, id := range part {
			marks[i] = "?"
			args = append(args, id)
		}
		args = append(args, nowS, nowS)
		var n int
		if err := s.db.QueryRow(s.q(`
			SELECT COUNT(*) FROM decision_jobs
			WHERE question_set = ? AND item_id IN (`+strings.Join(marks, ",")+`)
			  AND `+decisionJobClaimable), args...).Scan(&n); err != nil {
			return 0, fmt.Errorf("count claimable decision jobs: %w", err)
		}
		total += n
	}
	return total, nil
}

// CompleteDecisionJob retires a claimed job.
//
// The job is deleted only if its generation is still the one claimed. If a
// write re-enqueued it mid-evaluation, the row stays and the claim is
// released, so the next tick evaluates the newer state — the finished run
// must not delete work it never saw.
//
// That released row's not_before is RECOMPUTED (TASK-3916): the write that
// bumped it read the latest evaluation BEFORE this run's answers landed, so
// it may have found the window past and left the row claimable at once. The
// recomputation sees them, and the newer state is asked about a window after
// this run, not on the very next tick. A run that stored nothing (its state
// was already answered) moves nothing, so this never invents a delay.
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
	notBefore := ""
	if window := decisionWindowFor(s.decisionDebounceFunc(), j.QuestionSet); window > 0 {
		if notBefore, err = decisionNotBefore(s.db.QueryRow(s.q(latestDecisionEvaluationSQL), j.ItemID, j.QuestionSet), window, time.Now()); err != nil {
			return fmt.Errorf("debounce superseded decision job %s/%s: %w", j.ItemID, j.QuestionSet, err)
		}
	}
	if notBefore != "" {
		if _, err := s.db.Exec(s.q(`
			UPDATE decision_jobs SET not_before = ?
			WHERE item_id = ? AND question_set = ? AND claimed_by = ?
			  AND (not_before IS NULL OR not_before < ?)`),
			notBefore, j.ItemID, j.QuestionSet, j.ClaimedBy, notBefore); err != nil {
			return fmt.Errorf("debounce superseded decision job %s/%s: %w", j.ItemID, j.QuestionSet, err)
		}
	}
	return s.releaseDecisionClaim(j)
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
	if msg == "" {
		msg = noDecisionErrorMessage
	}
	return s.recordDecisionFailure(j, msg, retryAfter)
}

// DropDecisionJob abandons a claimed job: the item is gone, the set no
// longer applies, or the job has exhausted its attempts.
//
// Generation-matched, exactly like CompleteDecisionJob (codex round 2): the
// verdict is about the generation this runner claimed. A write that landed
// mid-evaluation bumped the row to a newer generation, which is new work the
// verdict says nothing about — so the row is released for it, not deleted.
func (s *Store) DropDecisionJob(j DecisionJob) error {
	res, err := s.db.Exec(s.q(`
		DELETE FROM decision_jobs
		WHERE item_id = ? AND question_set = ? AND generation = ? AND claimed_by = ?`),
		j.ItemID, j.QuestionSet, j.Generation, j.ClaimedBy)
	if err != nil {
		return fmt.Errorf("drop decision job %s/%s: %w", j.ItemID, j.QuestionSet, err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	return s.releaseSupersededDecisionJob(j)
}

// ReleaseDecisionJob gives a claimed job back without recording a failure:
// the runner stopped before finishing it (shutdown), which says nothing about
// the job. It is claimable again at once, unless its debounce (not_before)
// is still ahead.
func (s *Store) ReleaseDecisionJob(j DecisionJob) error {
	return s.releaseDecisionClaim(j)
}

// releaseDecisionClaim gives a claim back without recording anything: attempts
// and last_error are untouched, so the job is claimable again as soon as its
// not_before allows. Complete (superseded generation) and Release use it.
func (s *Store) releaseDecisionClaim(j DecisionJob) error {
	_, err := s.db.Exec(s.q(`
		UPDATE decision_jobs SET claimed_by = NULL, lease_expires_at = NULL
		WHERE item_id = ? AND question_set = ? AND claimed_by = ?`),
		j.ItemID, j.QuestionSet, j.ClaimedBy)
	if err != nil {
		return fmt.Errorf("release decision job %s/%s: %w", j.ItemID, j.QuestionSet, err)
	}
	return nil
}

// noDecisionErrorMessage stands in for a failure whose cause carries no text
// (a nil error, or one whose Error() is ""). A failure must always count an
// attempt: reading an empty message as "superseded" released the claim with
// no attempt and no last_error, so the job was re-run on every tick (BUG-4022).
const noDecisionErrorMessage = "decision evaluation failed with no error message"

// unrecordableDecisionError is the last_error written when the store refused
// the real message. It is a constant on purpose: anything read out of the
// error that just failed is driver text, which is what may be unrecordable.
// The SQLSTATE, when the driver gives one in its fixed shape, is appended.
const unrecordableDecisionError = "failure not recordable: the store refused the message"

// decisionSQLState returns the five-character SQLSTATE of a Postgres error,
// or "" for anything else (SQLite has none) or a code of an unexpected shape.
func decisionSQLState(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || !decisionSQLStateRe.MatchString(pgErr.Code) {
		return ""
	}
	return pgErr.Code
}

var decisionSQLStateRe = regexp.MustCompile(`^[0-9A-Z]{5}$`)

// recordDecisionFailure counts a failed attempt: attempts+1, last_error, and
// the lease moved to the retry time. Generation- and claim-matched.
//
// It fails OPEN (BUG-4022): if the UPDATE carrying the message is refused,
// one fallback UPDATE with the same WHERE and lease records the attempt with
// a fixed last_error instead. Without it a refused message left attempts at 0,
// so the backoff never grew, the permanent drop never fired, and the job was
// re-run (a paid provider call) every lease, forever. If the fallback also
// fails, both errors are returned.
//
// The fallback also requires attempts to still be the value read at claim.
// An error from the first UPDATE does not prove it did not commit (a reply
// lost after the commit); if it did, attempts has already moved and the
// fallback matches nothing rather than counting the attempt twice. A
// fallback that matches nothing returns the first error, as before BUG-4022,
// so the runner still logs an outcome it could not confirm.
func (s *Store) recordDecisionFailure(j DecisionJob, failure string, retryAfter time.Duration) error {
	const q = `
		UPDATE decision_jobs
		SET attempts = attempts + 1, last_error = ?, lease_expires_at = ?
		WHERE item_id = ? AND question_set = ? AND generation = ? AND claimed_by = ?`
	lease := decisionTime(time.Now().Add(retryAfter))
	res, err := s.db.Exec(s.q(q), truncateDecisionError(failure), lease,
		j.ItemID, j.QuestionSet, j.Generation, j.ClaimedBy)
	if err == nil {
		if n, _ := res.RowsAffected(); n == 1 {
			return nil
		}
		// Zero rows means the job was re-enqueued; the enqueue already reset
		// its failure history, so release the claim for the new generation.
		return s.releaseSupersededDecisionJob(j)
	}
	firstErr := fmt.Errorf("fail decision job %s/%s: %w", j.ItemID, j.QuestionSet, err)
	fallback := unrecordableDecisionError
	if code := decisionSQLState(err); code != "" {
		fallback += " (SQLSTATE " + code + ")"
	}
	res, ferr := s.db.Exec(s.q(q+` AND attempts = ?`), fallback, lease,
		j.ItemID, j.QuestionSet, j.Generation, j.ClaimedBy, j.Attempts)
	if ferr != nil {
		return errors.Join(firstErr,
			fmt.Errorf("fail decision job %s/%s (fallback): %w", j.ItemID, j.QuestionSet, ferr))
	}
	if n, _ := res.RowsAffected(); n == 1 {
		slog.Warn("decision failure message not stored; recorded a fixed one",
			"item_id", j.ItemID, "question_set", j.QuestionSet)
		return nil
	}
	// Zero rows: either the first UPDATE committed after all (attempts has
	// moved) or the job was re-enqueued. The superseded release covers the
	// second and is a no-op for the first; the first error is returned either
	// way, since this call cannot confirm what was recorded.
	if rerr := s.releaseSupersededDecisionJob(j); rerr != nil {
		return errors.Join(firstErr, rerr)
	}
	return firstErr
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

// truncateDecisionError makes a failure message safe to record: valid UTF-8,
// NUL-free and at most maxDecisionErrorBytes. A provider's response body is
// arbitrary bytes, and a byte cut can land inside a multi-byte rune; Postgres
// TEXT refuses invalid UTF-8 (SQLSTATE 22021) and the NUL guard refuses a NUL,
// and either way the UPDATE fails, so the failure goes unrecorded (BUG-4018).
// Sanitise first, then cut at a rune start. Short clean ASCII is unchanged.
func truncateDecisionError(s string) string {
	s = strings.ToValidUTF8(textguard.Repair(s, false), textguard.Replacement)
	if len(s) <= maxDecisionErrorBytes {
		return s
	}
	cut := maxDecisionErrorBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// DecisionJobState is a test/diagnostic read of one job row.
type DecisionJobState struct {
	Generation int64
	Attempts   int
	ClaimedBy  string
	LastError  string
	// NotBefore is the debounce instant (TASK-3916), "" when claimable now.
	NotBefore string
}

// GetDecisionJob returns a job's row, or nil when no evaluation is owed.
func (s *Store) GetDecisionJob(itemID, questionSet string) (*DecisionJobState, error) {
	var st DecisionJobState
	var claimedBy, lastError, notBefore sql.NullString
	err := s.db.QueryRow(s.q(`
		SELECT generation, attempts, claimed_by, last_error, not_before FROM decision_jobs
		WHERE item_id = ? AND question_set = ?`), itemID, questionSet).
		Scan(&st.Generation, &st.Attempts, &claimedBy, &lastError, &notBefore)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get decision job: %w", err)
	}
	st.ClaimedBy, st.LastError, st.NotBefore = claimedBy.String, lastError.String, notBefore.String
	return &st, nil
}

// HasItemDecisionsAtState reports whether every asked question already has an
// answer for this item, set and state hash — the runner's idempotency check.
// keys maps each asked question key to its current question hash (model plus
// definition): an answer to an older wording or model does not count.
func (s *Store) HasItemDecisionsAtState(itemID, questionSet, stateHash string, keys map[string]string) (bool, error) {
	if len(keys) == 0 {
		return true, nil
	}
	// Counted over the ASKED (key, question_hash) pairs only (codex rounds 3
	// and 6). Counting every stored key was wrong in a way the count cannot
	// see: a RENAMED key keeps the total equal — the old name answered, the
	// new one not — so the set read complete and the new key was never asked.
	// Matching the question hash as well is what makes a reworded question or
	// a new model re-ask at an unchanged item state.
	pairs := make([]string, 0, len(keys))
	args := []any{itemID, questionSet, stateHash}
	for k, qh := range keys {
		pairs = append(pairs, "(question_key = ? AND question_hash = ?)")
		args = append(args, k, qh)
	}
	var n int
	if err := s.db.QueryRow(s.q(`
		SELECT COUNT(DISTINCT question_key) FROM item_decisions
		WHERE item_id = ? AND question_set = ? AND state_hash = ?
		  AND (`+strings.Join(pairs, " OR ")+`)`), args...).Scan(&n); err != nil {
		return false, fmt.Errorf("check item decisions: %w", err)
	}
	return n == len(keys), nil
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
				 provider, model, state_hash, question_hash, item_seq, state_truncated, evaluated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (item_id, question_set, question_key, state_hash, question_hash) DO NOTHING`),
			newID(), workspaceID, r.ItemID, r.QuestionSet, r.QuestionKey, r.Kind, string(r.Answer), r.Confidence,
			r.Provider, r.Model, r.StateHash, r.QuestionHash, r.ItemSeq, truncated, evaluated,
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
		       d.provider, d.model, d.state_hash, d.question_hash, d.item_seq, d.state_truncated, d.evaluated_at
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
	return scanItemDecisions(rows)
}

func scanItemDecisions(rows *sql.Rows) ([]models.ItemDecision, error) {
	var out []models.ItemDecision
	for rows.Next() {
		var d models.ItemDecision
		var answer string
		var conf sql.NullFloat64
		var truncated int
		if err := rows.Scan(&d.ID, &d.ItemID, &d.QuestionSet, &d.QuestionKey, &d.Kind, &answer, &conf,
			&d.Provider, &d.Model, &d.StateHash, &d.QuestionHash, &d.ItemSeq, &truncated, &d.EvaluatedAt); err != nil {
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

// LatestWorkspaceDecisions returns the newest answer per (item, question_key)
// in one question set across a workspace — the dashboard's batch form of
// LatestItemDecisions. Soft-deleted items are excluded HERE as a cost filter,
// not as the correctness guard: the one caller (Runner.WorkspaceFlags) reads
// each flagged item's state and skips a gone one, so without this join a
// deleted item costs a read and is still not shown. Visibility and
// terminal-state filtering are the caller's, which already holds the item
// list it is joining against. Current is left false, as there.
func (s *Store) LatestWorkspaceDecisions(workspaceID, questionSet string) ([]models.ItemDecision, error) {
	rows, err := s.db.Query(s.q(`
		SELECT d.id, d.item_id, d.question_set, d.question_key, d.kind, d.answer, d.confidence,
		       d.provider, d.model, d.state_hash, d.question_hash, d.item_seq, d.state_truncated, d.evaluated_at
		FROM item_decisions d
		JOIN items i ON i.id = d.item_id AND i.deleted_at IS NULL
		WHERE d.workspace_id = ? AND d.question_set = ?
		  AND NOT EXISTS (
			SELECT 1 FROM item_decisions n
			WHERE n.item_id = d.item_id AND n.question_set = d.question_set AND n.question_key = d.question_key
			  AND (n.evaluated_at > d.evaluated_at OR (n.evaluated_at = d.evaluated_at AND n.id > d.id))
		  )
		ORDER BY d.item_id, d.question_key`), workspaceID, questionSet)
	if err != nil {
		return nil, fmt.Errorf("list workspace decisions: %w", err)
	}
	defer rows.Close()
	return scanItemDecisions(rows)
}

// DecisionJobsFailing reports whether any owed job for the set in this
// workspace carries a recorded failure — the read path's only view of a
// provider that is erroring, since the provider is called off the request.
// A failure is cleared when a new write re-enqueues the job, and the row is
// deleted when an evaluation succeeds, so a true answer means the most
// recent attempt at some item's current work failed.
func (s *Store) DecisionJobsFailing(workspaceID, questionSet string) (bool, error) {
	var n int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM decision_jobs
		WHERE workspace_id = ? AND question_set = ? AND last_error IS NOT NULL`),
		workspaceID, questionSet).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("count failing decision jobs: %w", err)
	}
	return n > 0, nil
}

// RecentComments returns an item's newest n comments, oldest first — the
// "recent trail" a decision's state carries. Ordered by (created_at, id) so a
// burst of comments inside one second still has one order, and therefore one
// state hash. Tombstones (BUG-3252) are left out: they carry no words.
func (s *Store) RecentComments(itemID string, n int) ([]models.Comment, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(s.q(`
		SELECT `+commentListCols+`
		FROM comments c
		`+commentAgentJoin+`
		WHERE c.item_id = ? AND c.deleted_at IS NULL
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

// ItemLiveness reports whether an item may be sent to a decision provider,
// in two parts because the runner treats them differently:
//
//   - itemLive: the item and its collection are not soft-deleted. False means
//     the job is dropped — neither has a restore path the queue must survive
//     (an item restore re-enqueues through its own door).
//   - workspaceLive: its workspace is not soft-deleted. False means the job is
//     HELD for a possible workspace restore.
//
// GetItem filters only the ITEM's soft delete, so this exists to stop content
// its owner deleted — by deleting the workspace or collection — from reaching
// a third-party provider. Same liveness the reminder tick's reminderFireable
// enforces, for the same reason.
func (s *Store) ItemLiveness(itemID string) (itemLive, workspaceLive bool, err error) {
	var itemDeleted, collDeleted, wsDeleted sql.NullString
	err = s.db.QueryRow(s.q(`
		SELECT i.deleted_at, c.deleted_at, w.deleted_at FROM items i
		JOIN collections c ON c.id = i.collection_id
		JOIN workspaces w ON w.id = i.workspace_id
		WHERE i.id = ?`), itemID).Scan(&itemDeleted, &collDeleted, &wsDeleted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("item liveness: %w", err)
	}
	return !itemDeleted.Valid && !collDeleted.Valid, !wsDeleted.Valid, nil
}
