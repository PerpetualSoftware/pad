package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2760. A comment's agent name lives on the `commented` activity its
// activity_id points at, and nowhere else; the list queries LEFT JOIN it on so
// every comment list carries it. testStore runs these against SQLite by
// default and Postgres under `make test-pg` — the join and the jsonb scan
// must hold on both.

func agentNameFixture(t *testing.T) (*Store, *models.Item) {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "AgentName")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Subject", "")
	return s, item
}

// commentWithActivity writes the pair the create handler writes: a
// `commented` activity carrying the given metadata, then the comment linked
// to it. An empty metadata leaves the comment unlinked (no activity at all).
func commentWithActivity(t *testing.T, s *Store, item *models.Item, metadata, body, parentID string) *models.Comment {
	t.Helper()
	input := models.CommentCreate{Body: body, Author: "Dave", CreatedBy: "agent", Source: "cli", ParentID: parentID}
	if metadata != "" {
		id, err := s.CreateActivity(models.Activity{
			WorkspaceID: item.WorkspaceID, DocumentID: item.ID,
			Action: "commented", Actor: "agent", Source: "cli", Metadata: metadata,
		})
		if err != nil {
			t.Fatalf("create activity: %v", err)
		}
		input.ActivityID = id
	}
	c, err := s.CreateComment(item.WorkspaceID, item.ID, "", input)
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}
	return c
}

func agentNamesByID(comments []models.Comment) map[string]string {
	out := make(map[string]string, len(comments))
	for _, c := range comments {
		out[c.ID] = c.AgentName
	}
	return out
}

func TestListComments_AgentNameFromLinkedActivity(t *testing.T) {
	s, item := agentNameFixture(t)

	named := commentWithActivity(t, s, item, `{"agent":"wren"}`, "named", "")
	// A value the retired display filter would have swallowed — it must come
	// back verbatim, not blanked.
	generic := commentWithActivity(t, s, item, `{"agent":"claude-code"}`, "generic", "")
	stampless := commentWithActivity(t, s, item, `{"changes":"x"}`, "no stamp", "")
	emptyName := commentWithActivity(t, s, item, `{"agent":""}`, "empty", "")
	nonString := commentWithActivity(t, s, item, `{"agent":123}`, "non-string", "")
	unlinked := commentWithActivity(t, s, item, "", "unlinked", "")
	reply := commentWithActivity(t, s, item, `{"agent":"rook"}`, "reply", named.ID)

	want := map[string]string{
		named.ID: "wren", generic.ID: "claude-code", stampless.ID: "",
		emptyName.ID: "", nonString.ID: "", unlinked.ID: "", reply.ID: "rook",
	}

	all, err := s.ListComments(item.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(all) != len(want) {
		t.Fatalf("ListComments returned %d comments, want %d — the join must not drop or duplicate rows", len(all), len(want))
	}
	if got := agentNamesByID(all); !equalStringMaps(got, want) {
		t.Errorf("ListComments agent names = %v, want %v", got, want)
	}
	// The join must not disturb the query's own contract: chronological.
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.Before(all[i-1].CreatedAt) {
			t.Errorf("ListComments order broken at %d: %v before %v", i, all[i].CreatedAt, all[i-1].CreatedAt)
		}
	}

	page, err := s.ListCommentsBeforeTime(item.ID, time.Now().Add(time.Hour), "", 50)
	if err != nil {
		t.Fatalf("ListCommentsBeforeTime: %v", err)
	}
	if len(page) != len(want) {
		t.Fatalf("ListCommentsBeforeTime returned %d comments, want %d", len(page), len(want))
	}
	if got := agentNamesByID(page); !equalStringMaps(got, want) {
		t.Errorf("ListCommentsBeforeTime agent names = %v, want %v", got, want)
	}
	// Newest-first, and the LIMIT still binds with the join in place.
	for i := 1; i < len(page); i++ {
		if page[i].CreatedAt.After(page[i-1].CreatedAt) {
			t.Errorf("ListCommentsBeforeTime order broken at %d: %v after %v", i, page[i].CreatedAt, page[i-1].CreatedAt)
		}
	}
	limited, err := s.ListCommentsBeforeTime(item.ID, time.Now().Add(time.Hour), "", 3)
	if err != nil {
		t.Fatalf("ListCommentsBeforeTime (limit 3): %v", err)
	}
	if len(limited) != 3 {
		t.Errorf("ListCommentsBeforeTime limit 3 returned %d rows", len(limited))
	}
	for _, c := range limited {
		if c.AgentName != want[c.ID] {
			t.Errorf("limited page: %s agent name = %q, want %q", c.ID, c.AgentName, want[c.ID])
		}
	}

	// The cursor branch is a separate query string; drive it too.
	cursor, err := s.ListCommentsBeforeTime(item.ID, time.Now().Add(time.Hour), "g", 50)
	if err != nil {
		t.Fatalf("ListCommentsBeforeTime (cursor): %v", err)
	}
	if got := agentNamesByID(cursor); !equalStringMaps(got, want) {
		t.Errorf("ListCommentsBeforeTime (cursor) agent names = %v, want %v", got, want)
	}

	// The single-row read is NOT enriched — Comment.AgentName documents the
	// list join as its only writer. This pins that so a future "why is it
	// empty here" is answered by the test, and so enriching it is a
	// deliberate change to this assertion rather than an accident.
	one, err := s.GetComment(named.ID)
	if err != nil || one == nil {
		t.Fatalf("GetComment: %v (%v)", err, one)
	}
	if one.AgentName != "" {
		t.Errorf("GetComment.AgentName = %q, want empty — only the list join populates it", one.AgentName)
	}
}

// The design's load-bearing claim: the name reaches the comment regardless of
// which activities a paginated activity read would return. Six later
// activities push the `commented` row out of a three-row activity window —
// the shape the timeline handler reads with perSource = limit*3 — while the
// comment query still carries the name, because it does not depend on that
// window at all. A handler-side join of the two lists would have lost it here.
func TestListCommentsBeforeTime_AgentNameSurvivesActivityWindow(t *testing.T) {
	s, item := agentNameFixture(t)

	c := commentWithActivity(t, s, item, `{"agent":"wren"}`, "early", "")
	// Timestamps are RFC3339 (second precision), so everything this test
	// writes lands in ONE second and the window's ORDER BY breaks the tie on
	// random ids — the commented row would then sit inside the window by
	// luck (it did, under Postgres, on the first full run). Backdate it so
	// the order is strict and the premise below is a fact, not a coin flip.
	if _, err := s.db.Exec(s.q(`UPDATE activities SET created_at = ? WHERE id = ?`),
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), c.ActivityID); err != nil {
		t.Fatalf("backdate activity: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := s.CreateActivity(models.Activity{
			WorkspaceID: item.WorkspaceID, DocumentID: item.ID,
			Action: "updated", Actor: "user", Source: "web", Metadata: `{"changes":"status"}`,
		}); err != nil {
			t.Fatalf("create activity %d: %v", i, err)
		}
	}

	future := time.Now().Add(time.Hour)
	window, err := s.ListDocumentActivityBeforeTime(item.ID, future, "", 3)
	if err != nil {
		t.Fatalf("ListDocumentActivityBeforeTime: %v", err)
	}
	for _, a := range window {
		if a.ID == c.ActivityID {
			t.Fatalf("premise broken: the commented activity is still inside the 3-row window; the test proves nothing")
		}
	}

	comments, err := s.ListCommentsBeforeTime(item.ID, future, "", 3)
	if err != nil {
		t.Fatalf("ListCommentsBeforeTime: %v", err)
	}
	if len(comments) != 1 || comments[0].AgentName != "wren" {
		t.Errorf("comments = %+v, want the one comment with AgentName %q", comments, "wren")
	}
}

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Codex round 2 on TASK-2760: the timeline handler suppressed a comment's
// linked activity only when the comment was in the SAME page — comments and
// activities being paginated separately, an activity could be fetched while
// its comment was not, and render as a standalone "commented" card. The
// activity query now excludes comment-linked rows itself, so the outcome does
// not depend on which comments happened to be fetched alongside.
func TestListDocumentActivityBeforeTime_ExcludesCommentLinkedRows(t *testing.T) {
	s, item := agentNameFixture(t)

	linked := commentWithActivity(t, s, item, `{"agent":"wren"}`, "linked", "")
	loose, err := s.CreateActivity(models.Activity{
		WorkspaceID: item.WorkspaceID, DocumentID: item.ID,
		Action: "updated", Actor: "user", Source: "web", Metadata: `{"changes":"status"}`,
	})
	if err != nil {
		t.Fatalf("create activity: %v", err)
	}

	future := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name     string
		beforeID string
	}{{"no cursor", ""}, {"cursor", "g"}} {
		got, err := s.ListDocumentActivityBeforeTime(item.ID, future, tc.beforeID, 50)
		if err != nil {
			t.Fatalf("%s: ListDocumentActivityBeforeTime: %v", tc.name, err)
		}
		ids := map[string]bool{}
		for _, a := range got {
			ids[a.ID] = true
		}
		if ids[linked.ActivityID] {
			t.Errorf("%s: comment-linked activity %s returned; it must be excluded at the query", tc.name, linked.ActivityID)
		}
		if !ids[loose] {
			t.Errorf("%s: unlinked activity %s missing — the exclusion must not over-reach", tc.name, loose)
		}
	}
}

// Codex round 3 on TASK-2760, two findings with one root: the join keyed on
// activity id alone, and nothing in the schema says a comment's activity
// belongs to the comment's item.
func TestListComments_AgentNameNeverReadAcrossItems(t *testing.T) {
	s, item := agentNameFixture(t)
	other := createTestItem(t, s, item.WorkspaceID, item.CollectionID, "Other", "")

	foreignActivity, err := s.CreateActivity(models.Activity{
		WorkspaceID: other.WorkspaceID, DocumentID: other.ID,
		Action: "commented", Actor: "agent", Source: "cli", Metadata: `{"agent":"wren"}`,
	})
	if err != nil {
		t.Fatalf("create activity: %v", err)
	}
	crossLinked, err := s.CreateComment(item.WorkspaceID, item.ID, "", models.CommentCreate{
		Body: "points at another item's activity", CreatedBy: "agent", Source: "cli", ActivityID: foreignActivity,
	})
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}

	got, err := s.ListComments(item.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(got) != 1 || got[0].ID != crossLinked.ID {
		t.Fatalf("comments = %+v, want just the cross-linked one", got)
	}
	if got[0].AgentName != "" {
		t.Errorf("agent name %q read from ANOTHER item's activity — the join must be item-scoped", got[0].AgentName)
	}

	// And the other item's timeline still sees its own activity: the
	// exclusion is item-scoped the same way, so a foreign link cannot hide it.
	acts, err := s.ListDocumentActivityBeforeTime(other.ID, time.Now().Add(time.Hour), "", 50)
	if err != nil {
		t.Fatalf("ListDocumentActivityBeforeTime: %v", err)
	}
	found := false
	for _, a := range acts {
		found = found || a.ID == foreignActivity
	}
	if !found {
		t.Errorf("other item's activity %s hidden by a comment on a different item", foreignActivity)
	}
}

// Codex round 3 on TASK-2760: an item update that carries a comment links the
// comment to its `updated` activity, and `updated` activities are debounced —
// a later update by the same user merges INTO the most recent row, overlaying
// its `agent` and bumping created_at, silently re-attributing the earlier
// comment. A comment-linked row must therefore never be a merge target; the
// control leg pins that unlinked rows still are.
//
// EVERY WRITE HERE DECLARES THE SAME AGENT NAME, and that is the point since
// BUG-2763. The original wrote as two different agents on one account, which
// can now no longer merge for a SECOND reason — the debounce refuses to
// coalesce across writer identities — so the second assertion below
// (`second != first`) was left passing on the identity guard alone. Measured
// rather than assumed: with the comment-link predicate deleted and the two
// names restored, that assertion still passed and only the final leg caught
// the deletion. One name puts the refusal back as the only mechanism that can
// split those two rows, so each leg fails for its own reason. The cross-agent
// shape this test used to carry is now the BUG-2763 identity matrix's, in
// activities_test.go.
func TestCreateActivityDebounced_NeverMergesIntoCommentLinkedRow(t *testing.T) {
	s, item := agentNameFixture(t)
	user, err := s.CreateUser(models.UserCreate{Email: "shared@test.com", Name: "Dave", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	update := func(agent string) string {
		id, err := s.CreateActivityDebounced(models.Activity{
			WorkspaceID: item.WorkspaceID, DocumentID: item.ID, UserID: user.ID,
			Action: "updated", Actor: "agent", Source: "cli",
			Metadata: `{"agent":"` + agent + `","changes":"status: open → done"}`,
		})
		if err != nil {
			t.Fatalf("debounced update (%s): %v", agent, err)
		}
		return id
	}

	first := update("wren")
	if _, err := s.CreateComment(item.WorkspaceID, item.ID, user.ID, models.CommentCreate{
		Body: "why I closed it", Author: "Dave", CreatedBy: "agent", Source: "cli", ActivityID: first,
	}); err != nil {
		t.Fatalf("create comment: %v", err)
	}

	second := update("wren")
	if second == first {
		t.Fatalf("second update merged into the comment-linked activity %s", first)
	}

	comments, err := s.ListComments(item.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(comments) != 1 || comments[0].AgentName != "wren" {
		t.Errorf("comment attribution after a later update = %+v, want still %q", comments, "wren")
	}

	// Control: with no comment on the newest row, the next update still
	// coalesces — the refusal is scoped to linked rows, not a debounce switch.
	third := update("wren")
	if third != second {
		t.Errorf("unlinked recent update did not merge: got %s, want %s", third, second)
	}

	// A linked row ENDS the run rather than being looked past: with an older
	// unlinked row still inside the window (`third`) and a newer linked one,
	// the next update starts a fresh row — it does not reach back and merge
	// into the older one, which would bump that row's time forward over the
	// comment and fold a later change into an earlier entry.
	if _, err := s.CreateComment(item.WorkspaceID, item.ID, user.ID, models.CommentCreate{
		Body: "and this", Author: "Dave", CreatedBy: "agent", Source: "cli", ActivityID: third,
	}); err != nil {
		t.Fatalf("create second comment: %v", err)
	}
	fourth := update("wren")
	if fourth == third || fourth == first {
		t.Errorf("update after a linked newest row reused %s; want a fresh row (linked=%s, older=%s)", fourth, third, first)
	}
}

// Codex round 6 on TASK-2760: the debounce's read-then-write is two
// statements, and a comment can link the chosen row between them. The write
// itself must refuse a linked row — this drives the write directly with the
// row already linked, the state the race produces. The control leg pins that
// an unlinked row is still written, so the predicate is a freeze and not a
// disabled merge.
//
// (This comment used to say the interleaving could not be scheduled from a
// test. BUG-2770 added the afterDebounceRead seam, so it now can — see
// TestCreateActivityDebounced_FrozenRowIsNotRetried, which schedules exactly
// this ordering through CreateActivityDebounced. Driving the write directly
// is still the right shape for THIS test: it pins the predicate itself,
// independently of any caller.)
func TestMergeIntoUnlinkedActivity_RefusesLinkedRow(t *testing.T) {
	s, item := agentNameFixture(t)
	mk := func(meta string) string {
		id, err := s.CreateActivity(models.Activity{
			WorkspaceID: item.WorkspaceID, DocumentID: item.ID,
			Action: "updated", Actor: "agent", Source: "cli", Metadata: meta,
		})
		if err != nil {
			t.Fatalf("create activity: %v", err)
		}
		return id
	}
	metaOf := func(id string) string {
		var m string
		if err := s.db.QueryRow(s.q(`SELECT metadata FROM activities WHERE id = ?`), id).Scan(&m); err != nil {
			t.Fatalf("read metadata: %v", err)
		}
		return m
	}

	linked := mk(`{"agent":"wren"}`)
	if _, err := s.CreateComment(item.WorkspaceID, item.ID, "", models.CommentCreate{
		Body: "linked", CreatedBy: "agent", Source: "cli", ActivityID: linked,
	}); err != nil {
		t.Fatalf("create comment: %v", err)
	}
	// The compare-and-set arm (BUG-2770) is given the row's CURRENT blob, so
	// it holds and the link arm is the only thing that can refuse this call.
	// A mismatched expectation here would refuse for two reasons at once and
	// this leg would stop discriminating.
	written, err := s.mergeIntoUnlinkedActivity(linked, metaOf(linked), `{"agent":"rook"}`, now())
	if err != nil {
		t.Fatalf("merge into linked: %v", err)
	}
	if written {
		t.Error("merge reported written against a comment-linked row")
	}
	if got := models.AgentNameFromMetadata(metaOf(linked)); got != "wren" {
		t.Errorf("linked row's agent after refused merge = %q, want %q", got, "wren")
	}

	unlinked := mk(`{"agent":"wren"}`)
	written, err = s.mergeIntoUnlinkedActivity(unlinked, metaOf(unlinked), `{"agent":"rook"}`, now())
	if err != nil {
		t.Fatalf("merge into unlinked: %v", err)
	}
	if !written {
		t.Error("control: merge into an unlinked row reported not written")
	}
	if got := models.AgentNameFromMetadata(metaOf(unlinked)); got != "rook" {
		t.Errorf("control: unlinked row's agent after merge = %q, want %q", got, "rook")
	}
}
