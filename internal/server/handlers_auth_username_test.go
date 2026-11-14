package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestProfileUnchangedReservedUsername(t *testing.T) {
	srv := testServer(t)
	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "legacy@test.com", Name: "Legacy", Username: "tutorials",
		Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := srv.store.CreateSession(user.ID, "go-test", "192.0.2.1", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{"tutorials", " Tutorials "} {
		t.Run(username, func(t *testing.T) {
			rr := doRequestWithCookie(srv, http.MethodPatch, "/api/v1/auth/me", map[string]string{
				"name": "Updated name", "username": username,
			}, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("profile update: expected 200, got %d: %s", rr.Code, rr.Body.String())
			}
			var resp struct{ Name, Username string }
			parseJSON(t, rr, &resp)
			if resp.Name != "Updated name" || resp.Username != "tutorials" {
				t.Fatalf("unexpected profile: %+v", resp)
			}
			saved, err := srv.store.GetUser(user.ID)
			if err != nil || saved == nil || saved.Name != "Updated name" || saved.Username != "tutorials" {
				t.Fatalf("profile was not saved: %+v, %v", saved, err)
			}
		})
	}
}

func TestProfileChangedReservedUsername(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "mara@test.com", "Mara")
	rr := doRequestWithCookie(srv, http.MethodPatch, "/api/v1/auth/me", map[string]string{
		"username": "tutorials",
	}, token)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("profile update: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct{ Code, Message string }
	}
	parseJSON(t, rr, &resp)
	if resp.Error.Code != "validation_error" || resp.Error.Message != `username "tutorials" is reserved` {
		t.Fatalf("unexpected error: %+v", resp)
	}
}

func TestRegisterGeneratedUsernameNotReserved(t *testing.T) {
	srv, _ := newCloudEmailServer(t)
	rr := doRequest(srv, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email": "newbie@pad.test", "name": "Needs", "password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct{ User struct{ Username string } }
	parseJSON(t, rr, &resp)
	if resp.User.Username == "" || IsReservedUsername(resp.User.Username) {
		t.Fatalf("register returned reserved or empty username %q", resp.User.Username)
	}
}
