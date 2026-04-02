package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthRegistrationFlow(t *testing.T) {
	srv := testServer(t)

	// Session check before any users — should indicate explicit setup state
	rr := doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("session check: expected 200, got %d", rr.Code)
	}
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["setup_required"] != true {
		t.Error("expected setup_required=true when no users exist")
	}
	if session["authenticated"] != false {
		t.Error("expected authenticated=false when no users exist")
	}
	if session["setup_method"] != "open_register" {
		t.Errorf("expected setup_method=open_register, got %v", session["setup_method"])
	}
	if session["auth_method"] != "password" {
		t.Errorf("expected auth_method=password, got %v", session["auth_method"])
	}
	if _, ok := session["needs_setup"]; ok {
		t.Error("did not expect deprecated needs_setup field")
	}

	// Register first user — should become admin
	rr = doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "admin@test.com",
		"name":     "Admin",
		"password": "password123",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var regResp map[string]interface{}
	parseJSON(t, rr, &regResp)
	user := regResp["user"].(map[string]interface{})
	if user["role"] != "admin" {
		t.Errorf("first user should be admin, got %v", user["role"])
	}
	if user["email"] != "admin@test.com" {
		t.Errorf("expected email admin@test.com, got %v", user["email"])
	}
	token := regResp["token"].(string)
	if token == "" {
		t.Error("expected non-empty token")
	}

	// Session check after registration — should be authenticated (cookie set)
	// Use the token from registration response via cookie
	rr = doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, token)
	parseJSON(t, rr, &session)
	if session["authenticated"] != true {
		t.Error("expected authenticated=true after registration")
	}
	authUser, ok := session["user"].(map[string]interface{})
	if !ok || authUser == nil {
		t.Error("expected user object in authenticated session response")
	} else if authUser["email"] != "admin@test.com" {
		t.Errorf("expected email admin@test.com in session user, got %v", authUser["email"])
	}
	if session["setup_required"] != false {
		t.Errorf("expected setup_required=false after registration, got %v", session["setup_required"])
	}
	if session["auth_method"] != "password" {
		t.Errorf("expected auth_method=password after registration, got %v", session["auth_method"])
	}
}

func TestAuthLoginFlow(t *testing.T) {
	srv := testServer(t)

	// Register a user first
	doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "user@test.com",
		"name":     "Test User",
		"password": "password123",
	})

	// Login with correct credentials
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

	// Login with wrong password
	rr = doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "user@test.com",
		"password": "wrongpassword",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: expected 401, got %d", rr.Code)
	}

	// Login with non-existent email
	rr = doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "nobody@test.com",
		"password": "anything",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("non-existent email: expected 401, got %d", rr.Code)
	}
}

func TestAuthLoginReturnsExplicitSetupStateWhenNoUsers(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "nobody@test.com",
		"password": "password123",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("login without users: expected 200, got %d", rr.Code)
	}

	var resp map[string]interface{}
	parseJSON(t, rr, &resp)
	if resp["setup_required"] != true {
		t.Errorf("expected setup_required=true, got %v", resp["setup_required"])
	}
	if resp["setup_method"] != "open_register" {
		t.Errorf("expected setup_method=open_register, got %v", resp["setup_method"])
	}
	if resp["auth_method"] != "password" {
		t.Errorf("expected auth_method=password, got %v", resp["auth_method"])
	}
	if _, ok := resp["needs_setup"]; ok {
		t.Error("did not expect deprecated needs_setup field")
	}
}

func TestAuthRegistrationValidation(t *testing.T) {
	srv := testServer(t)

	// Missing email
	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"name":     "Test",
		"password": "password123",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing email: expected 400, got %d", rr.Code)
	}

	// Invalid email
	rr = doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "not-an-email",
		"name":     "Test",
		"password": "password123",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("invalid email: expected 400, got %d", rr.Code)
	}

	// Short password
	rr = doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "test@test.com",
		"name":     "Test",
		"password": "short",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("short password: expected 400, got %d", rr.Code)
	}

	// Missing name
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

	// Register and get token
	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "user@test.com",
		"name":     "Test",
		"password": "password123",
	})
	var regResp map[string]interface{}
	parseJSON(t, rr, &regResp)
	token := regResp["token"].(string)

	// Logout
	rr = doRequestWithCookie(srv, "POST", "/api/v1/auth/logout", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("logout: expected 200, got %d", rr.Code)
	}

	// Session should no longer be valid
	rr = doRequestWithCookie(srv, "GET", "/api/v1/auth/session", nil, token)
	var session map[string]interface{}
	parseJSON(t, rr, &session)
	if session["authenticated"] != false {
		t.Error("expected authenticated=false after logout")
	}
}

func TestAuthRequiredWhenUsersExist(t *testing.T) {
	srv := testServer(t)

	// Before registration — API should work without auth
	rr := doRequest(srv, "GET", "/api/v1/workspaces", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("no users: expected 200, got %d", rr.Code)
	}

	// Register a user
	doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "admin@test.com",
		"name":     "Admin",
		"password": "password123",
	})

	// After registration — unauthenticated API requests should get 401
	rr = doRequest(srv, "GET", "/api/v1/workspaces", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("with users: expected 401, got %d: %s", rr.Code, rr.Body.String())
	}

	// Auth endpoints should still be accessible
	rr = doRequest(srv, "GET", "/api/v1/auth/session", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("auth/session should be exempt: expected 200, got %d", rr.Code)
	}

	// Health should be accessible
	rr = doRequest(srv, "GET", "/api/v1/health", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("health should be exempt: expected 200, got %d", rr.Code)
	}
}

func TestAuthMeEndpoint(t *testing.T) {
	srv := testServer(t)

	// Register
	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "me@test.com",
		"name":     "Me Test",
		"password": "password123",
	})
	var regResp map[string]interface{}
	parseJSON(t, rr, &regResp)
	token := regResp["token"].(string)

	// Get current user
	rr = doRequestWithCookie(srv, "GET", "/api/v1/auth/me", nil, token)
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

	// Register first user
	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "dup@test.com",
		"name":     "First",
		"password": "password123",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("first register: expected 201, got %d", rr.Code)
	}

	// Try to register same email again (unauthenticated — should be forbidden since users exist)
	rr = doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "dup@test.com",
		"name":     "Second",
		"password": "password456",
	})
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
	req.AddCookie(&http.Cookie{
		Name:  "pad_session",
		Value: token,
	})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}
