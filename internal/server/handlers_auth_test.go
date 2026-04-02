package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xarmian/pad/internal/models"
)

func bootstrapFirstUser(t *testing.T, srv *Server, email, name string) string {
	t.Helper()

	rr := doLoopbackRequest(srv, "POST", "/api/v1/auth/bootstrap", map[string]string{
		"email":    email,
		"name":     name,
		"password": "password123",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("bootstrap: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	parseJSON(t, rr, &resp)
	token, _ := resp["token"].(string)
	if token == "" {
		t.Fatal("expected bootstrap to return a session token")
	}
	return token
}

func TestAuthBootstrapFlow(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: expected 200, got %d", rr.Code)
	}

	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["setup_required"] != true {
		t.Error("expected setup_required=true when no users exist")
	}
	if session["setup_method"] != "local_cli" {
		t.Errorf("expected setup_method=local_cli, got %v", session["setup_method"])
	}
	if _, ok := session["needs_setup"]; ok {
		t.Error("did not expect deprecated needs_setup field")
	}

	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr = doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, token)
	parseJSON(t, rr, &session)
	if session["authenticated"] != true {
		t.Error("expected authenticated=true after bootstrap")
	}
	authUser, ok := session["user"].(map[string]interface{})
	if !ok || authUser == nil {
		t.Error("expected authenticated session user payload")
	} else {
		if authUser["email"] != "admin@test.com" {
			t.Errorf("expected email admin@test.com, got %v", authUser["email"])
		}
		if authUser["role"] != "admin" {
			t.Errorf("expected admin role after bootstrap, got %v", authUser["role"])
		}
	}
	if session["setup_required"] != false {
		t.Errorf("expected setup_required=false after bootstrap, got %v", session["setup_required"])
	}
}

func TestAuthBootstrapRequiresLoopback(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "POST", "/api/v1/auth/bootstrap", map[string]string{
		"email":    "admin@test.com",
		"name":     "Admin",
		"password": "password123",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("remote bootstrap: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAuthLoginFlow(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "user@test.com", "Test User")

	rr := doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "user@test.com",
		"password": "password123",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("login: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var loginResp map[string]interface{}
	parseJSON(t, rr, &loginResp)
	user := loginResp["user"].(map[string]interface{})
	if user["name"] != "Test User" {
		t.Errorf("expected name 'Test User', got %v", user["name"])
	}

	rr = doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "user@test.com",
		"password": "wrongpassword",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: expected 401, got %d", rr.Code)
	}

	rr = doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "nobody@test.com",
		"password": "anything",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("non-existent email: expected 401, got %d", rr.Code)
	}
}

func TestAuthLoginRequiresSetupWhenNoUsers(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "nobody@test.com",
		"password": "password123",
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("login without users: expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestFreshInstanceRegistrationIsForbidden(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "admin@test.com",
		"name":     "Admin",
		"password": "password123",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("fresh registration: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestInvitationRegistrationFlow(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Test"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	admin, err := srv.store.GetUserByEmail("admin@test.com")
	if err != nil || admin == nil {
		t.Fatalf("load admin user: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "owner"); err != nil {
		t.Fatalf("add admin workspace membership: %v", err)
	}
	inv, err := srv.store.CreateInvitation(ws.ID, "invitee@test.com", "viewer", admin.ID)
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":           "invitee@test.com",
		"name":            "Invitee",
		"password":        "password123",
		"invitation_code": inv.Code,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("invitation registration: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	parseJSON(t, rr, &resp)
	user := resp["user"].(map[string]interface{})
	if user["role"] != "member" {
		t.Errorf("expected invitation signup to create member role, got %v", user["role"])
	}

	invitee, err := srv.store.GetUserByEmail("invitee@test.com")
	if err != nil || invitee == nil {
		t.Fatalf("load invitee: %v", err)
	}
	member, err := srv.store.GetWorkspaceMember(ws.ID, invitee.ID)
	if err != nil {
		t.Fatalf("get workspace member: %v", err)
	}
	if member == nil || member.Role != "viewer" {
		t.Fatalf("expected invitee to be added to workspace as viewer, got %#v", member)
	}

	rr = doRequestWithCookie(srv, "POST", "/api/v1/auth/logout", nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("logout admin: expected 200, got %d", rr.Code)
	}
}

func TestAuthRegistrationValidation(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"name":     "Test",
		"password": "password123",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing email: expected 400, got %d", rr.Code)
	}

	rr = doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "not-an-email",
		"name":     "Test",
		"password": "password123",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("invalid email: expected 400, got %d", rr.Code)
	}

	rr = doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "test@test.com",
		"name":     "Test",
		"password": "short",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("short password: expected 400, got %d", rr.Code)
	}

	rr = doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "test@test.com",
		"password": "password123",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing name: expected 400, got %d", rr.Code)
	}
}

func TestAuthLogout(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "user@test.com", "Test")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/logout", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("logout: expected 200, got %d", rr.Code)
	}

	rr = doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, token)
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["authenticated"] != false {
		t.Error("expected authenticated=false after logout")
	}
}

func TestAuthRequiredWhenUsersExist(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "GET", "/api/v1/workspaces", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("no users: expected 200, got %d", rr.Code)
	}

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr = doRequest(srv, "GET", "/api/v1/workspaces", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("with users: expected 401, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("auth/session should be exempt: expected 200, got %d", rr.Code)
	}

	rr = doRequest(srv, "GET", "/api/v1/health", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("health should be exempt: expected 200, got %d", rr.Code)
	}
}

func TestAuthMeEndpoint(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "me@test.com", "Me Test")

	rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("me: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var user map[string]interface{}
	parseJSON(t, rr, &user)
	if user["name"] != "Me Test" {
		t.Errorf("expected name 'Me Test', got %v", user["name"])
	}
	if user["email"] != "me@test.com" {
		t.Errorf("expected email 'me@test.com', got %v", user["email"])
	}
}

func TestDuplicateRegistration(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "dup@test.com",
		"name":     "First",
		"password": "password123",
	}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("admin register: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "dup@test.com",
		"name":     "Second",
		"password": "password456",
	}, adminToken)
	if rr.Code == http.StatusCreated {
		t.Error("duplicate registration should not succeed")
	}
}

// doRequestWithCookie is like doRequest but adds a session cookie.
func doRequestWithCookie(srv *Server, method, path string, body interface{}, token string) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{
		Name:  "pad_session",
		Value: token,
	})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}
