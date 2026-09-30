package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3314: every activity read returned the ip_address and user_agent of the
// member who made each change, to anyone who could read the item, a viewer or
// a guest holding a grant included. Only the instance-admin audit surfaces
// (/admin/audit-log, /admin/users/{id}/activity) may carry them.
//
// The probe row is written through the store with values nothing else in the
// fixture produces, and each door's RAW body is searched for them, so the
// assertion holds whatever key a future shape puts them under.

const (
	bug3314IP = "203.0.113.77"
	bug3314UA = "LeakProbe/3314"
)

type bug3314World struct {
	slug, itemSlug, docID, memberID string
	memberTok, guestTok, adminTok   string
}

func bug3314Setup(t *testing.T, srv *Server) bug3314World {
	t.Helper()
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	item := createItem(t, srv, slug, "tasks", map[string]any{"title": "Probed task"})
	doc, err := srv.store.CreateDocument(ws.ID, models.DocumentCreate{Title: "Probed doc"})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	mk := func(name, role string) *models.User {
		u, err := srv.store.CreateUser(models.UserCreate{Email: name + "-3314@example.com", Name: name, Username: name + "-3314", Password: "pw-test-12345", Role: role})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
		return u
	}
	member, guest, admin := mk("member", ""), mk("guest", ""), mk("admin", "admin")
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, item.ID, guest.ID, "view", member.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	session := func(u *models.User) string {
		tok, err := srv.store.CreateSession(u.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return tok
	}

	for _, docID := range []string{item.ID, doc.ID} {
		if _, err := srv.store.CreateActivity(models.Activity{
			WorkspaceID: ws.ID, DocumentID: docID, Action: "updated", Actor: "user", Source: "web",
			Metadata: `{"changes":"status: open → done"}`, UserID: member.ID,
			IPAddress: bug3314IP, UserAgent: bug3314UA,
		}); err != nil {
			t.Fatalf("CreateActivity: %v", err)
		}
	}
	return bug3314World{slug: slug, itemSlug: item.Slug, docID: doc.ID, memberID: member.ID,
		memberTok: session(member), guestTok: session(guest), adminTok: session(admin)}
}

// get returns the body after checking the door answered, so an empty 403 or
// 404 can never pass as "no leak".
func bug3314Get(t *testing.T, srv *Server, path, tok string) string {
	t.Helper()
	rr := doRequestWithCookie(srv, "GET", path, nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

func bug3314AssertClean(t *testing.T, door, body string) {
	t.Helper()
	// Precondition: the probe row IS in this response, by its change text,
	// or an absent row would pass as a stripped one.
	if !strings.Contains(body, "open → done") {
		t.Fatalf("%s: the probe activity is not in the response, so the check means nothing: %s", door, body)
	}
	for _, leak := range []string{bug3314IP, bug3314UA, `"ip_address"`, `"user_agent"`} {
		if strings.Contains(body, leak) {
			t.Errorf("%s: response carries %s", door, leak)
		}
	}
}

func TestBUG3314_MemberDoorsCarryNoIPOrUserAgent(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		w := bug3314Setup(t, srv)
		base := "/api/v1/workspaces/" + w.slug
		for door, path := range map[string]string{
			"item timeline":     base + "/items/" + w.itemSlug + "/timeline",
			"item activity":     base + "/items/" + w.itemSlug + "/activity",
			"document activity": base + "/documents/" + w.docID + "/activity",
			"workspace feed":    base + "/activity",
		} {
			bug3314AssertClean(t, "member "+door, bug3314Get(t, srv, path, w.memberTok))
		}
	})
}

func TestBUG3314_GuestTimelineCarriesNoIPOrUserAgent(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		w := bug3314Setup(t, srv)
		base := "/api/v1/workspaces/" + w.slug + "/items/" + w.itemSlug
		bug3314AssertClean(t, "guest timeline", bug3314Get(t, srv, base+"/timeline", w.guestTok))
		bug3314AssertClean(t, "guest item activity", bug3314Get(t, srv, base+"/activity", w.guestTok))
	})
}

// The audit surfaces keep them: that is what they are for, and the fix must not
// have been made by dropping the columns everywhere.
func TestBUG3314_AdminAuditLogKeepsIPAndUserAgent(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		w := bug3314Setup(t, srv)
		for door, path := range map[string]string{
			"audit log":           "/api/v1/audit-log",
			"admin user activity": "/api/v1/admin/users/" + w.memberID + "/activity",
		} {
			body := bug3314Get(t, srv, path, w.adminTok)
			for _, want := range []string{bug3314IP, bug3314UA} {
				if !strings.Contains(body, want) {
					t.Errorf("%s lost %s: %s", door, want, body)
				}
			}
		}
	})
}
