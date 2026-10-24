package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/notify"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3939 part B2b: the real-enqueue mention leg and the enrichment endpoint
// GET /me/notifications/{id}.

// getNotif asks for notification id as the holder of session token tok.
func getNotif(srv *Server, tok, id string) *httptest.ResponseRecorder {
	return doRequestWithCookie(srv, "GET", "/api/v1/me/notifications/"+id, nil, tok)
}

// bobSession is a live session for bob; the plaintext is the cookie value.
func (f *pdx) bobSession() string {
	f.t.Helper()
	plain, err := f.srv.store.CreateSession(f.bob.ID, "web", "", "", time.Hour)
	must(f.t, err)
	return plain
}

// sameBytes fails unless a and b are the same response: status, every header,
// and the body byte for byte.
func sameBytes(t *testing.T, what string, a, b *httptest.ResponseRecorder) {
	t.Helper()
	if a.Code != b.Code || !reflect.DeepEqual(a.Header(), b.Header()) || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		t.Fatalf("%s differs from a missing id:\n got  %d %v %q\n want %d %v %q",
			what, a.Code, a.Header(), a.Body.String(), b.Code, b.Header(), b.Body.String())
	}
}

// Every denial of the shared gate answers the bytes a missing id answers, and
// every other row (settings off, no device, the controls) resolves: a setting
// governs the push, never read access.
func TestNotificationEnrichment_GateDenialsAreOneIndistinguishable404(t *testing.T) {
	denied := map[string]bool{
		skipNotMember: true, skipItemGone: true, skipWorkspaceGone: true, skipNotVisible: true,
		skipNoLongerAssign: true, skipCommentGone: true, skipUserDisabled: true,
	}
	needsBackends(t, func(t *testing.T, srv *Server) {
		for _, c := range gateCases() {
			t.Run(c.name, func(t *testing.T) {
				f := newPdx(t, srv)
				tok := f.bobSession()
				id := f.row(c.kind, c.row...)
				if c.setup != nil {
					c.setup(f, id)
				}
				// Every case goes through the real router with bob's real
				// session, so a middleware refusal (a disabled account is
				// refused 403 before the handler) is what the comparison sees.
				missing := getNotif(srv, tok, "no-such-notification")
				wantMissing := http.StatusNotFound
				if c.name == "user_disabled" || c.name == "user_reason_wins_over_membership" {
					wantMissing = http.StatusForbidden
				}
				if c.name == "app_principal" {
					// A user of kind app has no session door: 401 for every id.
					wantMissing = http.StatusUnauthorized
				}
				if missing.Code != wantMissing {
					t.Fatalf("missing id: %d %s, want %d", missing.Code, missing.Body.String(), wantMissing)
				}
				rr := getNotif(srv, tok, id)
				if denied[c.reason] {
					sameBytes(t, c.reason, rr, missing)
					return
				}
				if rr.Code != http.StatusOK {
					t.Fatalf("reason %q must still resolve: %d %s", c.reason, rr.Code, rr.Body.String())
				}
			})
		}
	})
}

// A notification setting that is off governs the push only. The endpoint must
// not consult it: an already delivered push still resolves.
func TestNotificationEnrichment_SettingsNeverGovernRead(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		tok := f.bobSession()
		id := f.row("comment")
		f.settings(func(s *models.NotificationSettings) {
			s.PushEnabled, s.NotifyComments, s.NotifyAssigned, s.NotifyMentions = false, false, false, false
		})
		if rr := getNotif(srv, tok, id); rr.Code != http.StatusOK {
			t.Fatalf("settings off: %d %s, want 200", rr.Code, rr.Body.String())
		}
		agent := f.row("comment", func(o *rowOpt) { o.agent = true })
		if rr := getNotif(srv, tok, agent); rr.Code != http.StatusOK {
			t.Fatalf("agent activity off: %d %s, want 200", rr.Code, rr.Body.String())
		}
	})
}

// Another user's row and a missing id are the same bytes.
func TestNotificationEnrichment_OtherUsersGetTheMissingIdBytes(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		ownerTok, err := srv.store.CreateSession(f.owner.ID, "web", "", "", time.Hour)
		must(t, err)
		missing := getNotif(srv, ownerTok, "no-such-notification")
		if missing.Code != http.StatusNotFound {
			t.Fatalf("missing: %d", missing.Code)
		}
		sameBytes(t, "another user's row", getNotif(srv, ownerTok, id), missing)
		// Control: the recipient reads it.
		if rr := getNotif(srv, f.bobSession(), id); rr.Code != http.StatusOK {
			t.Fatalf("control: %d %s", rr.Code, rr.Body.String())
		}
	})
}

// A tombstoned comment's text never reaches the response, in any form.
func TestNotificationEnrichment_TombstonedCommentTextNeverLeaks(t *testing.T) {
	f := newPdx(t, testServer(t))
	tok := f.bobSession()
	id := f.row("comment")
	if rr := getNotif(f.srv, tok, id); !strings.Contains(rr.Body.String(), "SECRET-COMMENT") {
		t.Fatalf("control: the live comment's excerpt is missing: %d %s", rr.Code, rr.Body.String())
	}
	f.exec(`UPDATE comments SET deleted_at = ? WHERE id = ?`, f.ts(time.Minute), f.comment.ID)
	rr := getNotif(f.srv, tok, id)
	if rr.Code != http.StatusNotFound || strings.Contains(rr.Body.String(), "SECRET") {
		t.Fatalf("tombstoned: %d %s", rr.Code, rr.Body.String())
	}
}

type notifBody struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Workspace map[string]string `json:"workspace"`
	Item      map[string]string `json:"item"`
	ActorName string            `json:"actor_name"`
	Excerpt   string            `json:"excerpt"`
	Path      string            `json:"path"`
}

func TestNotificationEnrichment_ShapeForEachKind(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		for _, kind := range []string{"comment", "reply", "mention", "assigned", "reminder"} {
			t.Run(kind, func(t *testing.T) {
				f := newPdx(t, srv)
				id := f.row(kind)
				rr := getNotif(srv, f.bobSession(), id)
				if rr.Code != http.StatusOK {
					t.Fatalf("%d %s", rr.Code, rr.Body.String())
				}
				var b notifBody
				if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
					t.Fatal(err)
				}
				wantExcerpt := ""
				if kind == "comment" || kind == "reply" || kind == "mention" {
					wantExcerpt = "SECRET-COMMENT"
				}
				if b.ID != id || b.Kind != kind || b.Workspace["slug"] != f.ws.Slug || b.Workspace["name"] != f.ws.Name ||
					b.Item["title"] != "SECRET-ITEM-TITLE" || b.Item["ref"] == "" ||
					b.ActorName != "owner" || b.Excerpt != wantExcerpt {
					t.Fatalf("body = %+v", b)
				}
				want := "/" + f.owner.Username + "/" + f.ws.Slug + "/tasks/" + f.item.Slug
				if b.Path != want {
					t.Fatalf("path = %q, want %q", b.Path, want)
				}
			})
		}
	})
}

// The path is a route of the web app: one leading slash and the four segments
// of web/src/routes/[username]/[workspace]/[collection]/[slug].
func TestNotificationEnrichment_PathMatchesTheWebRouteTree(t *testing.T) {
	if _, err := os.Stat("../../web/src/routes/[username]/[workspace]/[collection]/[slug]/+page.svelte"); err != nil {
		t.Fatalf("the web route the path targets is gone: %v", err)
	}
	f := newPdx(t, testServer(t))
	var b notifBody
	rr := getNotif(f.srv, f.bobSession(), f.row("assigned"))
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.Path, "/") || strings.HasPrefix(b.Path, "//") {
		t.Fatalf("path %q must start with a single slash", b.Path)
	}
	segs := strings.Split(strings.TrimPrefix(b.Path, "/"), "/")
	if len(segs) != 4 {
		t.Fatalf("path %q has %d segments, want [username]/[workspace]/[collection]/[slug]", b.Path, len(segs))
	}
	for _, s := range segs {
		if s == "" {
			t.Fatalf("path %q has an empty segment", b.Path)
		}
	}
}

func TestNotificationEnrichment_ExcerptCollapsesWhitespaceAndKeeps140Runes(t *testing.T) {
	f := newPdx(t, testServer(t))
	body := "  first\n\n\tsecond   third " + strings.Repeat("é", 300)
	cm, err := f.srv.store.CreateComment(f.ws.ID, f.item.ID, f.owner.ID, models.CommentCreate{Body: body, Author: "Owner", CreatedBy: "user", Source: "web"})
	must(t, err)
	id := f.row("comment", func(o *rowOpt) { o.comment = cm.ID })
	var b notifBody
	rr := getNotif(f.srv, f.bobSession(), id)
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	want := "first second third " + strings.Repeat("é", 140-len("first second third "))
	if b.Excerpt != want || utf8.RuneCountInString(b.Excerpt) != 140 {
		t.Fatalf("excerpt = %q (%d runes), want %q", b.Excerpt, utf8.RuneCountInString(b.Excerpt), want)
	}
	// A short body is whole, and exactly 140 runes is not cut.
	if got := notificationExcerpt(strings.Repeat("x", 140)); len(got) != 140 {
		t.Fatalf("140 runes cut to %d", len(got))
	}
}

func TestNotificationEnrichment_Unauthenticated401(t *testing.T) {
	f := newPdx(t, testServer(t))
	rr := doRequest(f.srv, "GET", "/api/v1/me/notifications/"+f.row("comment"), nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("%d %s, want 401", rr.Code, rr.Body.String())
	}
}

// A token pinned to one workspace is refused: this is an account-level route.
func TestNotificationEnrichment_PinnedTokenRefused(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	f.mustBePinDenial(f.do("GET", "/api/v1/me/notifications/anything", f.pinned, nil), "pinned GET /me/notifications/{id}")
	// Control: the unpinned token reaches the handler, which has no such row.
	f.must(f.do("GET", "/api/v1/me/notifications/anything", f.ownerTok, nil), http.StatusNotFound, "unpinned GET /me/notifications/{id}")
}

// An unpinned personal API token resolves its own user's notification through
// the router, as docs/api.md promises (TASK-3939 B2b codex r2).
func TestNotificationEnrichment_UnpinnedBearerTokenReadsItsOwnRow(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		tok, err := srv.store.CreateAPIToken(f.bob.ID, models.APITokenCreate{Name: "phone"}, 0, 0)
		must(t, err)
		req := httptest.NewRequest("GET", "/api/v1/me/notifications/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"id":"`+id+`"`) {
			t.Fatalf("bearer GET own row: %d %s, want 200 carrying id %s", rr.Code, rr.Body.String(), id)
		}
	})
}

// The mention leg runs the REAL enqueue: a comment @-mentioning bob is written
// through the store, and nothing seeds a row by hand. The dispatcher then
// pushes it as a mention.
func TestPushDispatch_RealMentionEnqueueThroughTheDispatcher(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		mention := func(f *pdx, who ...*models.User) *models.Comment {
			t.Helper()
			var tags []string
			for _, u := range who {
				tags = append(tags, "@"+u.Username)
			}
			cm, err := srv.store.CreateComment(f.ws.ID, f.item.ID, f.owner.ID,
				models.CommentCreate{Body: "ping " + strings.Join(tags, " "), Author: "Owner", CreatedBy: "user", Source: "web"})
			must(t, err)
			return cm
		}
		rowsFor := func(f *pdx, u *models.User, kind string) []string {
			t.Helper()
			rows, err := srv.store.DB().Query(srv.store.D().Rebind(
				`SELECT id FROM user_notifications WHERE user_id = ? AND kind = ?`), u.ID, kind)
			must(t, err)
			defer rows.Close()
			var ids []string
			for rows.Next() {
				var id string
				must(t, rows.Scan(&id))
				ids = append(ids, id)
			}
			return ids
		}
		age := func(f *pdx) { f.exec(`UPDATE user_notifications SET created_at = ?`, f.ts(2*time.Minute)) }

		t.Run("pushed_as_a_mention", func(t *testing.T) {
			f := newPdx(t, srv)
			f.device(f.bob, "tok-bob-second-"+fmt.Sprint(f.n), 4*time.Hour)
			mention(f, f.bob)
			ids := rowsFor(f, f.bob, "mention")
			if len(ids) != 1 {
				t.Fatalf("mention rows for bob = %v, want one from the real enqueue", ids)
			}
			if c := rowsFor(f, f.bob, "comment"); len(c) != 0 {
				t.Fatalf("bob also has a comment row %v: a mention supersedes it", c)
			}
			age(f)
			f.tick()
			if st := f.state(ids[0]); st.State != "sent" {
				t.Fatalf("mention row = %s/%q, want sent", st.State, st.Reason)
			}
			msgs := f.fake.all()
			if len(msgs) != 2 {
				t.Fatalf("%d messages, want one per device", len(msgs))
			}
			cids := map[string]bool{}
			for _, m := range msgs {
				if m.Data.Kind != notify.KindMention || m.Data.NID != ids[0] || m.Body != "You were mentioned" {
					t.Fatalf("message = %+v, want a mention with nid %s", m, ids[0])
				}
				cids[m.Data.CID] = true
			}
			if len(cids) != 2 {
				t.Fatalf("connection ids = %v, want one per device", cids)
			}
		})

		t.Run("tombstoned_before_the_tick_is_comment_gone", func(t *testing.T) {
			f := newPdx(t, srv)
			cm := mention(f, f.bob)
			ids := rowsFor(f, f.bob, "mention")
			if len(ids) != 1 {
				t.Fatalf("mention rows = %v", ids)
			}
			age(f)
			f.exec(`UPDATE comments SET deleted_at = ? WHERE id = ?`, f.ts(time.Second), cm.ID)
			f.tick()
			if st := f.state(ids[0]); st.State != "skipped" || st.Reason != skipCommentGone {
				t.Fatalf("mention row = %s/%q, want skipped/comment_gone", st.State, st.Reason)
			}
			if n := f.fake.callCount(); n != 0 {
				t.Fatalf("gateway called %d times for a tombstoned mention", n)
			}
		})

		t.Run("no_row_for_an_invisible_user_or_the_author", func(t *testing.T) {
			f := newPdx(t, srv)
			carol := f.user("carol")
			must(t, srv.store.AddWorkspaceMember(f.ws.ID, carol.ID, "editor"))
			f.device(carol, "tok-carol-"+fmt.Sprint(f.n), 5*time.Hour)
			f.device(f.owner, "tok-owner-"+fmt.Sprint(f.n), 5*time.Hour)
			must(t, srv.store.SetMemberCollectionAccess(f.ws.ID, carol.ID, "specific", []string{}))
			mention(f, f.bob, carol, f.owner)
			if n := len(rowsFor(f, f.bob, "mention")); n != 1 {
				t.Fatalf("control: bob has %d mention rows, want 1", n)
			}
			if n := len(rowsFor(f, carol, "mention")); n != 0 {
				t.Fatalf("carol cannot see the item yet has %d mention rows", n)
			}
			if n := len(rowsFor(f, f.owner, "mention")); n != 0 {
				t.Fatalf("the author mentioned themselves and has %d mention rows", n)
			}
		})
	})
}

// A disabled account is refused by RequireAuth before the handler, for every
// id. Through the real router, with the user's own live session, a REAL
// notification id and an unknown id answer the same bytes (403
// account_disabled): the refusal cannot tell them apart.
func TestNotificationEnrichment_DisabledUserGetsOneRefusalForEveryId(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		tok := f.bobSession()
		id := f.row("comment")
		if rr := getNotif(srv, tok, id); rr.Code != http.StatusOK {
			t.Fatalf("control: %d %s", rr.Code, rr.Body.String())
		}
		f.exec(`UPDATE users SET disabled_at = ? WHERE id = ?`, f.ts(time.Minute), f.bob.ID)
		real, unknown := getNotif(srv, tok, id), getNotif(srv, tok, "no-such-notification")
		if real.Code != http.StatusForbidden || !strings.Contains(real.Body.String(), "account_disabled") {
			t.Fatalf("disabled, real id: %d %s, want 403 account_disabled", real.Code, real.Body.String())
		}
		sameBytes(t, "a disabled user's real id", real, unknown)
		if strings.Contains(real.Body.String(), "SECRET") {
			t.Fatalf("refusal leaks content: %s", real.Body.String())
		}
	})
}

// An installed app authenticates with a padapp_ token, which opens the
// /api/app/v1 router only. Presented to this account-level route the token is
// not a credential at all: whatever the answer, a REAL notification id (a row
// of the app's bot) and an unknown id get identical bytes, and no content.
func TestNotificationEnrichment_AppTokenGetsOneAnswerForEveryId(t *testing.T) {
	f := appAPIFixture(t, "read")
	id := "n-app-real"
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`INSERT INTO user_notifications
		(id, user_id, workspace_id, item_id, kind, actor_user_id, dedupe_key, created_at)
		VALUES (?, ?, ?, ?, 'assigned', ?, ?, ?)`),
		id, f.in.bot.ID, f.ws.ID, f.item.ID, f.in.bot.ID, "k-"+id, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	real := appGet(f.srv, "/api/v1/me/notifications/"+id, f.token)
	unknown := appGet(f.srv, "/api/v1/me/notifications/no-such-notification", f.token)
	if real.Code == http.StatusOK {
		t.Fatalf("an app token resolved a notification: %s", real.Body.String())
	}
	sameBytes(t, "an app token's real id", real, unknown)
	t.Logf("app token on /api/v1/me/notifications/{id}: %d %s", real.Code, real.Body.String())
}

// TASK-4051: an unreadable owner must produce 500 in the name fallback.
// internal/store/workspaces.go:308 uses COALESCE on the owner username,
// so an existing owner with an empty username reaches it. Kills: M4a.
func TestNotificationEnrichment_OwnerGetUserErrorReturns500(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		task4051EnrichmentScanFault(t, srv, true)
	})
}

// TASK-4051: an unreadable actor is an operational error after the recipient
// passes the access gate, so enrichment returns 500. Kills: M4b.
func TestNotificationEnrichment_ActorGetUserErrorReturns500(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		task4051EnrichmentScanFault(t, srv, false)
	})
}

func task4051EnrichmentScanFault(t *testing.T, srv *Server, ownerFault bool) {
	t.Helper()
	if srv.store.D().Driver() == store.DriverPostgres {
		t.Skip("scan fault needs SQLite's loose typing; the 500 branch is dialect-independent")
	}
	f := newPdx(t, srv)
	actor := f.user("actor")
	tok := f.bobSession()
	id := f.row("comment")
	f.exec(`UPDATE user_notifications SET actor_user_id = ? WHERE id = ?`, actor.ID, id)
	if rr := getNotif(srv, tok, id); rr.Code != http.StatusOK {
		t.Fatalf("healthy control = %d %s, want 200", rr.Code, rr.Body.String())
	}
	faulted := actor
	if ownerFault {
		faulted = f.owner
		f.exec(`UPDATE users SET username = '' WHERE id = ?`, faulted.ID)
		ws, err := srv.store.GetWorkspaceByID(f.ws.ID)
		if err != nil || ws == nil || ws.OwnerUsername != "" {
			t.Fatalf("owner fallback premise: workspace=%+v err=%v", ws, err)
		}
	}
	f.exec(`UPDATE users SET credential_epoch = 'not-a-number' WHERE id = ?`, faulted.ID)
	if _, err := srv.store.GetUser(faulted.ID); err == nil {
		t.Fatal("GetUser did not report the injected INTEGER scan fault")
	}
	n, err := srv.store.GetNotificationForUser(f.bob.ID, id)
	must(t, err)
	if _, reason, err := srv.notificationAccess(newPushGateCache(), *n); err != nil || reason != "" {
		t.Fatalf("recipient's access gate must pass before enrichment: reason=%q err=%v", reason, err)
	}
	if rr := getNotif(srv, tok, id); rr.Code != http.StatusInternalServerError {
		t.Fatalf("enrichment = %d %s, want 500 for GetUser failure", rr.Code, rr.Body.String())
	}
}
