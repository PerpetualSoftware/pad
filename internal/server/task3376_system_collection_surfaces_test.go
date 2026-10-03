package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestTask3376_SystemCollectionSurfaces drives the surfaces that each carried
// their own "system collections are always visible to restricted members"
// union until TASK-3376: item read, search, and the /me permission payload
// (visible and full-access sets). A restricted member who is not given the
// system collection must get nothing from it on any of them; once it is
// listed, every one of them answers. The listed leg is the control: the same
// requests, the same item, only the listing differs.
func TestTask3376_SystemCollectionSurfaces(t *testing.T) {
	f := newRefResolverFixture(t)
	st := f.srv.store

	alpha, err := st.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Alpha", Slug: "alpha", Prefix: "ALPHA"})
	if err != nil {
		t.Fatalf("CreateCollection alpha: %v", err)
	}
	sysColl, err := st.CreateCollection(f.ws.ID, models.CollectionCreate{
		Name: "Conventions", Slug: "conventions", Prefix: "CONV", IsSystem: true,
	})
	if err != nil {
		t.Fatalf("CreateCollection (system): %v", err)
	}
	sysItem, err := st.CreateItem(f.ws.ID, sysColl.ID, models.ItemCreate{Title: "Zebracorn rule"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	member, err := st.CreateUser(models.UserCreate{
		Email: "m3376@example.com", Name: "M", Username: "m3376", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := st.AddWorkspaceMember(f.ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tok, err := st.CreateAPIToken(member.ID, models.APITokenCreate{Name: "m-tok", WorkspaceID: f.ws.ID}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	get := func(path string) (int, string) {
		t.Helper()
		rr := f.doAuth(tok.Token, "GET", path, nil)
		return rr.Code, rr.Body.String()
	}
	itemPath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + sysItem.Ref
	searchPath := "/api/v1/search?q=Zebracorn&workspace=" + f.ws.Slug
	mePath := "/api/v1/workspaces/" + f.ws.Slug + "/me"

	type meBody struct {
		Visible []string `json:"visible_collection_ids"`
		Full    []string `json:"full_access_collection_ids"`
	}
	has := func(ids []string, id string) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}
	readMe := func() meBody {
		t.Helper()
		rr := f.doAuth(tok.Token, "GET", mePath, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("/me: %d %s", rr.Code, rr.Body.String())
		}
		var m meBody
		parseJSON(t, rr, &m)
		return m
	}

	// --- Unlisted ---
	if err := st.SetMemberCollectionAccess(f.ws.ID, member.ID, "specific", []string{alpha.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	if code, body := get(itemPath); code != http.StatusNotFound {
		t.Errorf("unlisted: item GET = %d, want 404; body=%s", code, body)
	}
	if code, body := get(searchPath); code != http.StatusOK || strings.Contains(body, sysItem.ID) {
		t.Errorf("unlisted: search returned the system item (code %d): %s", code, body)
	}
	if m := readMe(); has(m.Visible, sysColl.ID) || has(m.Full, sysColl.ID) {
		t.Errorf("unlisted: /me still lists the system collection: visible=%v full=%v", m.Visible, m.Full)
	}

	// --- Listed (control) ---
	if err := st.SetMemberCollectionAccess(f.ws.ID, member.ID, "specific", []string{alpha.ID, sysColl.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess (listed): %v", err)
	}
	if code, body := get(itemPath); code != http.StatusOK {
		t.Errorf("listed: item GET = %d, want 200; body=%s", code, body)
	}
	if code, body := get(searchPath); code != http.StatusOK || !strings.Contains(body, sysItem.ID) {
		t.Errorf("listed: search did not return the system item (code %d): %s", code, body)
	}
	if m := readMe(); !has(m.Visible, sysColl.ID) || !has(m.Full, sysColl.ID) {
		t.Errorf("listed: /me does not list the system collection: visible=%v full=%v", m.Visible, m.Full)
	}
}
