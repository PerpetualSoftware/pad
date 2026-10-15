package server

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3826: a user deleted between the admin handler's read and its write
// answers 404. A promotion used to answer 409 last_active_admin (a false
// message), a demotion 500, and a disable 200 for a disable of nothing.
func TestBUG3826_VanishedUserAnswers404(t *testing.T) {
	for _, door := range []struct {
		name, method, path string
		body               any
	}{
		{"promote", "PATCH", "/api/v1/admin/users/%s", map[string]any{"role": "admin"}},
		{"demote", "PATCH", "/api/v1/admin/users/%s", map[string]any{"role": "member"}},
		{"disable", "POST", "/api/v1/admin/users/%s/disable", nil},
	} {
		t.Run(door.name, func(t *testing.T) {
			srv := testServer(t)
			token := bootstrapFirstUser(t, srv, "admin-3826@test.com", "Admin")
			u, err := srv.store.CreateUser(models.UserCreate{Email: "gone-3826@test.com", Name: "Gone", Password: "pw-test-12345"})
			if err != nil {
				t.Fatal(err)
			}
			deleted := false
			adminUserAfterReadHook = func(id string) {
				adminUserAfterReadHook = nil
				if err := srv.store.DeleteAccountAtomic(id); err != nil {
					t.Errorf("delete %s mid-request: %v", id, err)
				}
				deleted = true
			}
			t.Cleanup(func() { adminUserAfterReadHook = nil })

			rr := doRequestWithCookie(srv, door.method, fmt.Sprintf(door.path, u.ID), door.body, token)
			if !deleted {
				t.Fatalf("the hook never ran: %d %s", rr.Code, rr.Body.String())
			}
			if rr.Code != http.StatusNotFound || errorCode(t, rr) != "not_found" {
				t.Errorf("%s of a user deleted mid-request: %d %s, want 404 not_found", door.name, rr.Code, rr.Body.String())
			}
		})
	}
}
