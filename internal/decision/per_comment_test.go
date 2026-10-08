package decision

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3119 U2b: the conventions questions asked about each comment alone.

func newCommentFixture(t *testing.T) *convFixture {
	t.Helper()
	fx := newConvFixture(t)
	if err := fx.r.registry.Register(ConventionsCommentsSet()); err != nil {
		t.Fatal(err)
	}
	return fx
}

func (fx *convFixture) comment(t *testing.T, item *models.Item, body string) *models.Comment {
	t.Helper()
	c, err := fx.s.CreateComment(fx.ws.ID, item.ID, "", models.CommentCreate{Author: "wren", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (fx *convFixture) evalComments(t *testing.T, item *models.Item) bool {
	t.Helper()
	called, err := fx.r.Evaluate(context.Background(), item.ID, ConventionsCommentsSetName)
	if err != nil {
		t.Fatalf("evaluate comments: %v", err)
	}
	return called
}

// commentCurrent reports whether every conventions_comments row for the
// comment is current, and whether there are any.
func (fx *convFixture) commentCurrent(t *testing.T, item *models.Item, commentID string) (rows int, allCurrent bool) {
	t.Helper()
	ds, err := fx.r.Decisions(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	allCurrent = true
	for _, d := range ds {
		if d.QuestionSet != ConventionsCommentsSetName {
			continue
		}
		if _, cid, ok := SplitCommentKey(d.QuestionKey); !ok || cid != commentID {
			continue
		}
		rows++
		allCurrent = allCurrent && d.Current
	}
	return rows, allCurrent
}

func TestPerComment_EachCommentAskedAloneOnce(t *testing.T) {
	fx := newCommentFixture(t)
	fx.convention(t, "No creds", `{"status":"active","trigger":"always"}`, "no plaintext credentials")
	item := fx.task(t, "Staging access")
	c1 := fx.comment(t, item, "password is hunter2")

	if !fx.evalComments(t, item) {
		t.Fatal("a new comment cost no call")
	}
	if len(fx.f.rawBodies) != 1 {
		t.Fatalf("calls = %d, want 1", len(fx.f.rawBodies))
	}
	// The state is the comment subject's: title, collection, the comment.
	want, err := BuildCommentState(item.Title, item.CollectionSlug, c1.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fx.f.rawBodies[0], string(want.Bytes)) {
		t.Fatalf("request does not carry the comment state %s: %s", want.Bytes, fx.f.rawBodies[0])
	}
	if n, cur := fx.commentCurrent(t, item, c1.ID); n != 1 || !cur {
		t.Fatalf("rows for the comment = %d current=%v, want 1 current", n, cur)
	}

	// Nothing new: no call. A second comment: one more call, for it alone.
	if fx.evalComments(t, item) {
		t.Fatal("an unchanged comment was asked again")
	}
	fx.comment(t, item, "second")
	fx.evalComments(t, item)
	if len(fx.f.rawBodies) != 2 {
		t.Fatalf("calls = %d, want 2", len(fx.f.rawBodies))
	}
	if !strings.Contains(fx.f.rawBodies[1], `"comment":"second"`) {
		t.Fatalf("the second call is not about the new comment: %s", fx.f.rawBodies[1])
	}
}

// Lead ruling: the window bounds what is ASKED, not what stays current. A
// breaking comment keeps its answer, and so its chip, after 11 newer ones.
func TestPerComment_AnswerStaysCurrentAfterElevenNewerComments(t *testing.T) {
	fx := newCommentFixture(t)
	fx.convention(t, "No creds", `{"status":"active","trigger":"always"}`, "no plaintext credentials")
	item := fx.task(t, "Staging access")
	breaking := fx.comment(t, item, "password is hunter2")
	fx.evalComments(t, item)
	// Comment timestamps have second resolution, so comments written in one
	// burst tie and the window's (created_at DESC, id DESC) order could keep
	// the breaking one inside it. Backdate it so it is unambiguously older.
	if _, err := fx.s.DB().Exec(`UPDATE comments SET created_at = ? WHERE id = ?`, "2020-01-01T00:00:00Z", breaking.ID); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 11; i++ {
		fx.comment(t, item, "later comment "+strconv.Itoa(i))
	}
	before := len(fx.f.rawBodies)
	fx.evalComments(t, item)
	if got := len(fx.f.rawBodies) - before; got != RecentTrailWindow {
		t.Fatalf("asked %d comments, want the window of %d", got, RecentTrailWindow)
	}
	if n, cur := fx.commentCurrent(t, item, breaking.ID); n != 1 || !cur {
		t.Fatalf("the breaking comment's answer: rows=%d current=%v, want 1 current after 11 newer comments", n, cur)
	}
}

// An edited comment's answer is not current until it is asked again; a
// deleted one's never is.
func TestPerComment_EditAndDeleteEndCurrency(t *testing.T) {
	fx := newCommentFixture(t)
	fx.convention(t, "No creds", `{"status":"active","trigger":"always"}`, "no plaintext credentials")
	item := fx.task(t, "Staging access")
	c := fx.comment(t, item, "password is hunter2")
	gone := fx.comment(t, item, "to be deleted")
	fx.evalComments(t, item)

	if _, err := fx.s.UpdateComment(c.ID, "password is in the vault"); err != nil {
		t.Fatal(err)
	}
	if _, cur := fx.commentCurrent(t, item, c.ID); cur {
		t.Fatal("an edited comment's answer still reads current")
	}
	fx.evalComments(t, item)
	if _, cur := fx.commentCurrent(t, item, c.ID); !cur {
		t.Fatal("the edited comment was not re-asked")
	}

	if err := fx.s.DeleteComment(gone.ID); err != nil {
		t.Fatal(err)
	}
	if n, cur := fx.commentCurrent(t, item, gone.ID); n == 0 || cur {
		t.Fatalf("a deleted comment's answer: rows=%d current=%v, want its old row and not current", n, cur)
	}
}
