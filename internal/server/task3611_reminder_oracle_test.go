package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// reminderWriteAs sends a session write with its CSRF pair, the shape a
// browser sends (doAuthedDeleteWithCSRF, with a body).
func reminderWriteAs(srv *Server, method, path string, body any, sessionToken string) *httptest.ResponseRecorder {
	const testCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: csrfCookieName(srv.secureCookies), Value: testCSRF})
	req.Header.Set("X-CSRF-Token", testCSRF)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// TASK-3611 codex r3: a reminder on an item the caller cannot see answers
// exactly what an unknown reminder id answers, on every by-id door (re-arm,
// ack, delete), so a guest cannot learn that an id exists. The control is a
// reminder on the guest's own granted item, which the same door serves.
func TestReminderByIDDoesNotRevealHiddenItems(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	owner := mustUser(t, srv, "rem-oracle-owner@example.com", "remoracleowner", "")
	ws := mustWorkspace(t, srv, "Reminder oracle", owner.ID)
	coll := mustCollection(t, srv, ws.ID, "Tasks")
	granted := mustItem(t, srv, ws.ID, coll.ID, "Granted item")
	secret := mustItem(t, srv, ws.ID, coll.ID, "Not for the guest")

	guest := mustUser(t, srv, "rem-oracle-guest@example.com", "remoracleguest", "")
	if _, err := srv.store.CreateItemGrant(ws.ID, granted.ID, guest.ID, "edit", owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	future := time.Now().Add(48 * time.Hour).UTC()
	mine, err := srv.store.CreateReminder(ws.ID, granted.ID, future.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := srv.store.CreateReminder(ws.ID, secret.ID, future.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := srv.store.CreateSession(guest.ID, testSessionUA, "192.0.2.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/workspaces/" + ws.Slug + "/reminders/"
	later := map[string]any{"remind_at": future.Add(time.Hour).Format(time.RFC3339)}

	for _, door := range []struct {
		name, method, suffix string
		body                 any
	}{
		{"rearm", "PATCH", "", later},
		{"ack", "POST", "/ack", nil},
		{"delete", "DELETE", "", nil},
	} {
		unknown := reminderWriteAs(srv, door.method, base+"00000000-0000-0000-0000-000000000000"+door.suffix, door.body, tok)
		onHidden := reminderWriteAs(srv, door.method, base+hidden.ID+door.suffix, door.body, tok)
		if onHidden.Code != http.StatusNotFound || onHidden.Code != unknown.Code || onHidden.Body.String() != unknown.Body.String() {
			t.Errorf("%s: hidden item answered %d %s; unknown id answered %d %s; they must be identical",
				door.name, onHidden.Code, onHidden.Body.String(), unknown.Code, unknown.Body.String())
		}
	}
	// Control: the guest's own reminder is served (delete last, so it exists for the others).
	if rr := reminderWriteAs(srv, "DELETE", base+mine.ID, nil, tok); rr.Code != http.StatusOK {
		t.Fatalf("control: the guest's own reminder must be deletable: %d %s", rr.Code, rr.Body.String())
	}
}
