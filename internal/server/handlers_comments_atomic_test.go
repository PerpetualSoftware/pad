package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-2716: the two "commented" call sites — handleCreateComment and
// handleCreateReply — wrote the activity row FIRST, in its own transaction,
// and then the comment. The order is forced (comments.activity_id carries a
// foreign key), so a comment failure left an orphan "commented" entry on the
// item timeline with no comment behind it, and the request answered 500.
//
// Both sites now write the pair through CreateCommentWithActivity, one
// transaction, so a failed comment takes its activity with it. The third site
// the trail enumerates — an item update carrying a comment — is deliberately
// unchanged: its "updated" activity records a write that already committed,
// and must survive a comment failure.
//
// THE CONTROL. These tests were run against the pre-fix handlers with the same
// seam: the comment INSERT fails, the request answers 500, and the "commented"
// activity row is STILL THERE — red on the activity-row line, both sites.
//
// Not parallel: the seam is per-store but the tests share package-level
// fixtures the way the other seam-driven suites do.

var errSimCommentInsert = errors.New("simulated comment insert failure")

// commentFixture is a workspace + collection + item on the chosen dialect,
// created through the store so the same code runs on both backends, with
// requests made in the fresh-install window (no users, so no auth needed).
type commentFixture struct {
	srv  *Server
	ws   *models.Workspace
	item *models.Item
}

func newCommentFixture(t *testing.T, driver store.DriverType) commentFixture {
	t.Helper()
	var s *store.Store
	if driver == store.DriverPostgres {
		s = storetest.NewPostgres(t) // skips when PAD_TEST_POSTGRES_URL is unset
	} else {
		s = storetest.NewSQLite(t)
	}
	if got := s.D().Driver(); got != driver {
		t.Fatalf("wanted a %s store, got %s", driver, got)
	}
	srv := New(s)
	t.Cleanup(func() { srv.Stop() })

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Atomic"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Needs a comment"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return commentFixture{srv: srv, ws: ws, item: item}
}

func (f commentFixture) commentedActivities(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.srv.store.DB().QueryRow(f.srv.store.D().Rebind(
		`SELECT COUNT(*) FROM activities WHERE document_id = ? AND action = 'commented'`), f.item.ID).Scan(&n); err != nil {
		t.Fatalf("count commented activities: %v", err)
	}
	return n
}

func (f commentFixture) comments(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.srv.store.DB().QueryRow(f.srv.store.D().Rebind(
		`SELECT COUNT(*) FROM comments WHERE item_id = ?`), f.item.ID).Scan(&n); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	return n
}

func (f commentFixture) postComment(body string) *httptest.ResponseRecorder {
	return doRequest(f.srv, "POST", "/api/v1/workspaces/"+f.ws.Slug+"/items/"+f.item.Slug+"/comments", map[string]string{"body": body})
}

// --- site 1: handleCreateComment -------------------------------------------

func TestCreateComment_FailedCommentLeavesNoActivity_SQLite(t *testing.T) {
	createCommentFailedLeavesNoActivity(t, store.DriverSQLite)
}

func TestCreateComment_FailedCommentLeavesNoActivity_Postgres(t *testing.T) {
	createCommentFailedLeavesNoActivity(t, store.DriverPostgres)
}

func createCommentFailedLeavesNoActivity(t *testing.T, driver store.DriverType) {
	f := newCommentFixture(t, driver)

	// Control within the test: a comment that succeeds writes exactly one
	// linked "commented" activity, so the count below measures the pair.
	if rr := f.postComment("first, fine"); rr.Code != http.StatusCreated {
		t.Fatalf("baseline comment: %d %s", rr.Code, rr.Body.String())
	}
	if got := f.commentedActivities(t); got != 1 {
		t.Fatalf("baseline: want 1 commented activity, got %d", got)
	}

	restore := f.srv.store.SetCommentInsertFailureHookForTesting(func(models.CommentCreate) error {
		return errSimCommentInsert
	})
	t.Cleanup(restore)

	rr := f.postComment("second, fails")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("a failed comment write must answer 500, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := f.comments(t); got != 1 {
		t.Errorf("comment count %d, want 1 (the failed comment must not exist)", got)
	}
	if got := f.commentedActivities(t); got != 1 {
		t.Errorf("commented activities %d, want 1: the failed comment left an ORPHAN 'commented' activity on the timeline", got)
	}
}

// --- site 2: handleCreateReply ---------------------------------------------

func TestCreateReply_FailedCommentLeavesNoActivity_SQLite(t *testing.T) {
	createReplyFailedLeavesNoActivity(t, store.DriverSQLite)
}

func TestCreateReply_FailedCommentLeavesNoActivity_Postgres(t *testing.T) {
	createReplyFailedLeavesNoActivity(t, store.DriverPostgres)
}

func createReplyFailedLeavesNoActivity(t *testing.T, driver store.DriverType) {
	f := newCommentFixture(t, driver)

	rr := f.postComment("parent")
	if rr.Code != http.StatusCreated {
		t.Fatalf("parent comment: %d %s", rr.Code, rr.Body.String())
	}
	var parent struct {
		ID string `json:"id"`
	}
	parseJSON(t, rr, &parent)
	if got := f.commentedActivities(t); got != 1 {
		t.Fatalf("baseline: want 1 commented activity, got %d", got)
	}

	restore := f.srv.store.SetCommentInsertFailureHookForTesting(func(models.CommentCreate) error {
		return errSimCommentInsert
	})
	t.Cleanup(restore)

	rr = doRequest(f.srv, "POST", "/api/v1/workspaces/"+f.ws.Slug+"/comments/"+parent.ID+"/replies", map[string]string{"body": "reply, fails"})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("a failed reply write must answer 500, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := f.comments(t); got != 1 {
		t.Errorf("comment count %d, want 1 (the failed reply must not exist)", got)
	}
	if got := f.commentedActivities(t); got != 1 {
		t.Errorf("commented activities %d, want 1: the failed reply left an ORPHAN 'commented' activity on the timeline", got)
	}
}
