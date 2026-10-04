package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3399 (SPEC-6 U5b-2): the app API for a person signed in through an
// installed app.

type delegatedAPIFix struct {
	appAPIFix
	person *models.User
	reqID  string
}

// delegatedAPIFixture is the U6 app world (a companion, a system and a
// private collection, an app item, a person's comment) with a PERSON signed
// in through the app's real consent flow, consenting to access; the
// fixture's token is that person's delegated token.
func delegatedAPIFixture(t *testing.T, offered, access, role string) delegatedAPIFix {
	t.Helper()
	f := appAPIFixture(t, "write")
	if _, err := f.srv.store.DB().Exec(`UPDATE app_installs SET delegated_access = ? WHERE id = ?`, offered, f.in.id); err != nil {
		t.Fatal(err)
	}
	person, sessionToken := loginTestUserAs(t, f.srv, "dana-"+f.in.id+"@example.com", "Dana", "password123")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, person.ID, role); err != nil {
		t.Fatal(err)
	}
	df := delegatedFix{srv: f.srv, in: f.in, person: person, sessionToken: sessionToken}
	df.csrf = df.csrfFromAppConsent(t)
	tok, reqID := df.signIn(t, access)
	f.token = tok
	return delegatedAPIFix{appAPIFix: f, person: person, reqID: reqID}
}

func TestTask3399b_TheAppActsAsThePerson(t *testing.T) {
	f := delegatedAPIFixture(t, "write", "write", "editor")
	rr := appGet(f.srv, f.path("/me"), f.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("/me: %d %s", rr.Code, rr.Body.String())
	}
	var me AppMe
	_ = json.Unmarshal(rr.Body.Bytes(), &me)
	if me.UserID != f.person.ID || me.IsApp || me.Role != "editor" {
		t.Errorf("/me = %+v, want the person as an editor", me)
	}
	// The ceiling: companions and system collections, never the private one.
	if rr := appGet(f.srv, f.path("/items/"+f.item.ID), f.token); rr.Code != http.StatusOK {
		t.Errorf("a companion item: %d", rr.Code)
	}
	if rr := appGet(f.srv, f.path("/items/"+f.privateItem.ID), f.token); rr.Code != http.StatusNotFound {
		t.Errorf("a private item: %d, want 404", rr.Code)
	}
}

// Lead ruling R3 (TASK-3401 U6b), the delegated half: a write is the
// PERSON's, with via_app = the install, never the bot's.
func TestTask3401_DelegatedWriteAuthorIsThePerson(t *testing.T) {
	f := delegatedAPIFixture(t, "write", "write", "editor")
	bus := events.New()
	f.srv.SetEventBus(bus)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sse, _, _ := bus.Subscribe(ctx, f.ws.ID)

	rr := appDo(f.srv, "POST", f.path("/collections/requests/items"), f.token, map[string]any{"title": "Filed by Dana via the portal"},
		"X-Pad-Agent", "spoofed")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created AppItem
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	var createdBy, createdByUser, viaApp string
	if err := f.srv.store.DB().QueryRow(`SELECT created_by, COALESCE(created_by_user_id, ''), COALESCE(via_app, '') FROM items WHERE id = ?`, created.ID).
		Scan(&createdBy, &createdByUser, &viaApp); err != nil {
		t.Fatal(err)
	}
	if createdBy != "user" || createdByUser != f.person.ID || viaApp != f.in.id {
		t.Errorf("item attribution = %s/%s via %s, want user/%s via %s", createdBy, createdByUser, viaApp, f.person.ID, f.in.id)
	}
	if created.CreatedByDisplay != "Dana" {
		t.Errorf("created_by_display = %q, want the person", created.CreatedByDisplay)
	}

	rr = appDo(f.srv, "POST", f.path("/items/"+f.item.ID+"/comments"), f.token, map[string]any{"body": "following up"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment: %d %s", rr.Code, rr.Body.String())
	}
	var c AppComment
	_ = json.Unmarshal(rr.Body.Bytes(), &c)
	if c.AuthorKind != "user" || c.AuthorDisplay != "Dana" {
		t.Errorf("comment author = %s/%s, want user/Dana", c.AuthorKind, c.AuthorDisplay)
	}
	var cBy, cUser, cVia string
	if err := f.srv.store.DB().QueryRow(`SELECT created_by, COALESCE(user_id, ''), COALESCE(via_app, '') FROM comments WHERE id = ?`, c.ID).
		Scan(&cBy, &cUser, &cVia); err != nil {
		t.Fatal(err)
	}
	if cBy != "user" || cUser != f.person.ID || cVia != f.in.id {
		t.Errorf("comment row = %s/%s via %s", cBy, cUser, cVia)
	}
	deadline := time.After(3 * time.Second)
	for seen := 0; seen < 2; {
		select {
		case e := <-sse:
			seen++
			if e.Actor != "user" {
				t.Errorf("SSE %s actor = %q, want user", e.Type, e.Actor)
			}
		case <-deadline:
			t.Fatalf("saw %d SSE events, want 2", seen)
		}
	}
}

// §4 step 4: the token's access is the LESSER of the person's consent and
// what the manifest offers now; and the person's role still applies.
func TestTask3399b_AccessIsTheLesser(t *testing.T) {
	create := func(f delegatedAPIFix) int {
		return appDo(f.srv, "POST", f.path("/collections/requests/items"), f.token, map[string]any{"title": "x"}).Code
	}
	// Consented read on a write manifest.
	if code := create(delegatedAPIFixture(t, "write", "read", "editor")); code != http.StatusForbidden {
		t.Errorf("a read consent writing: %d, want 403", code)
	}
	// Consented write, then the manifest narrowed to read (an upgrade).
	f := delegatedAPIFixture(t, "write", "write", "editor")
	if code := create(f); code != http.StatusCreated {
		t.Fatalf("control: a write consent writing: %d", code)
	}
	if _, err := f.srv.store.DB().Exec(`UPDATE app_installs SET delegated_access = 'read' WHERE id = ?`, f.in.id); err != nil {
		t.Fatal(err)
	}
	if code := create(f); code != http.StatusForbidden {
		t.Errorf("writing after the manifest narrowed to read: %d, want 403", code)
	}
	// A viewer's write consent cannot write either.
	if code := create(delegatedAPIFixture(t, "write", "write", "viewer")); code < 400 {
		t.Errorf("a viewer writing: %d, want a refusal", code)
	}
}

// §8: removing a delegated user's membership (or disabling them, or moving
// their credentials) stops their token at the NEXT request, even where no
// revocation reached the token itself.
func TestTask3399b_ThePersonIsCheckedOnEveryRequest(t *testing.T) {
	cases := map[string]func(t *testing.T, f delegatedAPIFix){
		"membership removed": func(t *testing.T, f delegatedAPIFix) {
			if _, err := f.srv.store.DB().Exec(`DELETE FROM workspace_members WHERE user_id = ? AND workspace_id = ?`, f.person.ID, f.ws.ID); err != nil {
				t.Fatal(err)
			}
		},
		"account disabled": func(t *testing.T, f delegatedAPIFix) {
			if _, err := f.srv.store.DB().Exec(`UPDATE users SET disabled_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), f.person.ID); err != nil {
				t.Fatal(err)
			}
		},
		"credentials moved": func(t *testing.T, f delegatedAPIFix) {
			if _, err := f.srv.store.DB().Exec(`UPDATE users SET credential_epoch = credential_epoch + 1 WHERE id = ?`, f.person.ID); err != nil {
				t.Fatal(err)
			}
		},
		"membership re-created": func(t *testing.T, f delegatedAPIFix) {
			if _, err := f.srv.store.DB().Exec(`UPDATE workspace_members SET created_at = '2099-01-01T00:00:00Z' WHERE user_id = ? AND workspace_id = ?`, f.person.ID, f.ws.ID); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := delegatedAPIFixture(t, "write", "write", "editor")
			if rr := appGet(f.srv, f.path("/me"), f.token); rr.Code != http.StatusOK {
				t.Fatalf("control: %d", rr.Code)
			}
			change(t, f)
			if rr := appGet(f.srv, f.path("/me"), f.token); rr.Code != http.StatusUnauthorized {
				t.Errorf("after %s: %d, want 401", name, rr.Code)
			}
		})
	}
}

// The person's own visibility bounds what the app sees as them: the request
// ceiling (U6a), not only the companion set.
func TestTask3399b_ThePersonsVisibilityBoundsTheApp(t *testing.T) {
	f := delegatedAPIFixture(t, "write", "read", "editor")
	if _, err := f.srv.store.DB().Exec(`UPDATE workspace_members SET collection_access = 'specific' WHERE user_id = ? AND workspace_id = ?`, f.person.ID, f.ws.ID); err != nil {
		t.Fatal(err)
	}
	if rr := appGet(f.srv, f.path("/items/"+f.item.ID), f.token); rr.Code != http.StatusNotFound {
		t.Errorf("a companion item the person cannot see: %d, want 404", rr.Code)
	}
}

// U6a's Q7 note made load-bearing: re-admission replays every authorization
// under the FRESH context, so a companion released while a delegated read is
// in flight withholds the response (the store's bot ceiling does not cover a
// person).
func TestTask3399b_ACompanionReleasedMidReadWithholdsIt(t *testing.T) {
	f := delegatedAPIFixture(t, "write", "read", "editor")
	f.srv.appAfterHandler = func() {
		if _, err := f.srv.store.DB().Exec(`UPDATE collections SET via_app = NULL WHERE id = ?`, f.companion.ID); err != nil {
			t.Error(err)
		}
	}
	if rr := appGet(f.srv, f.path("/items/"+f.item.ID), f.token); rr.Code != http.StatusUnauthorized {
		t.Errorf("a companion released mid-read: %d, want 401 and nothing", rr.Code)
	}
}

// Lead ruling (U6c): a delegated upload re-admits the PERSON (their
// visibility of the item and their edit right) immediately before its row.
func TestTask3399b_APersonNarrowedMidUploadCommitsNoRow(t *testing.T) {
	for name, change := range map[string]string{
		"collection access narrowed": `UPDATE workspace_members SET collection_access = 'specific' WHERE user_id = ?`,
		"role dropped to viewer":     `UPDATE workspace_members SET role = 'viewer' WHERE user_id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			f := delegatedAPIFixture(t, "write", "write", "editor")
			body := append(testPNG(t), 8, 8, 8)
			before := f.count(t, `SELECT COUNT(*) FROM attachments`)
			changed := false
			lazy := &lazyBody{build: func() []byte {
				if _, err := f.srv.store.DB().Exec(change, f.person.ID); err != nil {
					t.Error(err)
				}
				changed = true
				return body
			}}
			req := httptest.NewRequest("POST", f.path("/items/"+f.item.ID+"/attachments?filename=a.png"), lazy)
			req.ContentLength = int64(len(body))
			req.Header.Set("Authorization", "Bearer "+f.token)
			req.RemoteAddr = "192.0.2.1:1234"
			rr := httptest.NewRecorder()
			f.srv.ServeHTTP(rr, req)
			if !changed {
				t.Fatal("control: the body was never read")
			}
			if rr.Code < 400 {
				t.Errorf("%s mid-upload: %d %s, want a refusal", name, rr.Code, rr.Body.String())
			}
			if after := f.count(t, `SELECT COUNT(*) FROM attachments`); after != before {
				t.Errorf("a row committed after the person's %s", name)
			}
		})
	}
	// Control: the same upload with nothing changed lands, as the person.
	f := delegatedAPIFixture(t, "write", "write", "editor")
	body := append(testPNG(t), 7, 7)
	rr := appUpload(f.appAPIFix, "/items/"+f.item.ID+"/attachments?filename=a.png", body, int64(len(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("control upload: %d %s", rr.Code, rr.Body.String())
	}
	var up struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &up)
	var uploader, via string
	if err := f.srv.store.DB().QueryRow(`SELECT uploaded_by, COALESCE(via_app, '') FROM attachments WHERE id = ?`, up.ID).Scan(&uploader, &via); err != nil {
		t.Fatal(err)
	}
	if uploader != f.person.ID || via != f.in.id {
		t.Errorf("uploaded_by %s via %s, want the person via the install", uploader, via)
	}
}

// Codex U5b-2 r1: every delegated JSON write re-admits the person after its
// body is read; one who lost access while the body arrived writes nothing.
func TestTask3399b_APersonLosingAccessMidBodyWritesNothing(t *testing.T) {
	type call struct {
		method, path string
		body         map[string]any
		table        string
	}
	for _, removeBy := range []string{"removal", "role dropped to viewer"} {
		for name, build := range map[string]func(f delegatedAPIFix) call{
			"item create": func(f delegatedAPIFix) call {
				return call{"POST", "/collections/requests/items", map[string]any{"title": "late"}, "items"}
			},
			"item update": func(f delegatedAPIFix) call {
				return call{"PATCH", "/items/" + f.item.ID, map[string]any{"fields_patch": map[string]any{"size": "XL"}, "expected_etag": f.etag(t, f.item.ID)}, "items"}
			},
			"comment create": func(f delegatedAPIFix) call {
				return call{"POST", "/items/" + f.item.ID + "/comments", map[string]any{"body": "late"}, "comments"}
			},
		} {
			t.Run(removeBy+"/"+name, func(t *testing.T) {
				f := delegatedAPIFixture(t, "write", "write", "editor")
				c := build(f)
				var fieldsBefore string
				_ = f.srv.store.DB().QueryRow(`SELECT fields FROM items WHERE id = ?`, f.item.ID).Scan(&fieldsBefore)
				before := f.count(t, `SELECT COUNT(*) FROM `+c.table)
				changed := false
				rr := appDoLazy(f.appAPIFix, c.method, f.path(c.path), &lazyBody{build: func() []byte {
					q := `DELETE FROM workspace_members WHERE user_id = ? AND workspace_id = ?`
					args := []any{f.person.ID, f.ws.ID}
					if removeBy != "removal" {
						q = `UPDATE workspace_members SET role = 'viewer' WHERE user_id = ? AND workspace_id = ?`
					}
					if _, err := f.srv.store.DB().Exec(q, args...); err != nil {
						t.Error(err)
					}
					changed = true
					b, _ := json.Marshal(c.body)
					return b
				}})
				if !changed {
					t.Fatal("control: the body was never read")
				}
				if rr.Code < 400 {
					t.Errorf("%s after %s mid-body: %d %s, want a refusal", name, removeBy, rr.Code, rr.Body.String())
				}
				if after := f.count(t, `SELECT COUNT(*) FROM `+c.table); after != before {
					t.Errorf("%s after %s: %d rows written", name, removeBy, after-before)
				}
				var fieldsAfter string
				_ = f.srv.store.DB().QueryRow(`SELECT fields FROM items WHERE id = ?`, f.item.ID).Scan(&fieldsAfter)
				if fieldsAfter != fieldsBefore {
					t.Errorf("%s after %s: the item changed", name, removeBy)
				}
			})
		}
	}
}

// Codex U5b-2 r2: a person who sees only granted items in a companion lists
// only those, and the window (has_more, next_offset) counts nothing hidden.
func TestTask3399b_AListCountsNothingHidden(t *testing.T) {
	f := delegatedAPIFixture(t, "write", "read", "editor")
	// A second companion item the person will not be granted.
	hidden, err := f.srv.store.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "Hidden", ActorUserID: f.person.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.DB().Exec(`UPDATE workspace_members SET collection_access = 'specific' WHERE user_id = ? AND workspace_id = ?`, f.person.ID, f.ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.DB().Exec(`INSERT INTO item_grants (id, workspace_id, item_id, user_id, permission, granted_by, created_at) VALUES (?, ?, ?, ?, 'view', ?, ?)`,
		"grant-"+f.item.ID[:8], f.ws.ID, f.item.ID, f.person.ID, f.person.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	rr := appGet(f.srv, f.path("/collections/requests/items?limit=1"), f.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var page struct {
		Items   []AppItem `json:"items"`
		HasMore bool      `json:"has_more"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &page)
	if len(page.Items) != 1 || page.Items[0].ID != f.item.ID {
		t.Fatalf("items = %+v, want only the granted item", page.Items)
	}
	if page.HasMore {
		t.Error("has_more counted an item the person cannot see")
	}
	_ = hidden
}

// Codex U5b-2 r3: the response's final re-admission step re-checks the
// workspace and the person's role, not only the credential.
func TestTask3399b_ARoleDroppedDuringTheReChecksWithholdsTheResponse(t *testing.T) {
	for name, change := range map[string]string{
		"role dropped":      `UPDATE workspace_members SET role = 'viewer' WHERE user_id = ?`,
		"workspace deleted": `UPDATE workspaces SET deleted_at = '2026-01-01T00:00:00Z' WHERE id = (SELECT workspace_id FROM workspace_members WHERE user_id = ? LIMIT 1)`,
	} {
		t.Run(name, func(t *testing.T) {
			f := delegatedAPIFixture(t, "write", "write", "editor")
			ran := false
			f.srv.appAfterRechecks = func() {
				if ran {
					return
				}
				ran = true
				if _, err := f.srv.store.DB().Exec(change, f.person.ID); err != nil {
					t.Error(err)
				}
			}
			rr := appGet(f.srv, f.path("/items/"+f.item.ID), f.token)
			if !ran {
				t.Fatal("control: the seam never ran")
			}
			if rr.Code == http.StatusOK {
				t.Errorf("%s during the re-checks: 200, want the response withheld", name)
			}
		})
	}
}
