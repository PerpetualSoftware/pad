package email

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestSender(ts *httptest.Server) *Sender {
	s := NewSender("test-api-key", "noreply@test.com", "TestApp", "https://example.com")
	s.client = ts.Client()
	s.endpoint = ts.URL
	return s
}

func TestSend_Success(t *testing.T) {
	var received mailerooPayload
	var authHeader string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		json.NewEncoder(w).Encode(mailerooResponse{Success: true, Message: "sent"})
	}))
	defer ts.Close()

	s := newTestSender(ts)

	err := s.Send(context.Background(), "user@example.com", "User", "Hello", "<p>Hello</p>", "Hello")
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if authHeader != "Bearer test-api-key" {
		t.Errorf("expected Authorization header 'Bearer test-api-key', got %q", authHeader)
	}
	if received.From.Address != "noreply@test.com" {
		t.Errorf("expected from address 'noreply@test.com', got %q", received.From.Address)
	}
	if received.From.DisplayName != "TestApp" {
		t.Errorf("expected from name 'TestApp', got %q", received.From.DisplayName)
	}
	if len(received.To) != 1 || received.To[0].Address != "user@example.com" {
		t.Errorf("unexpected to: %+v", received.To)
	}
	if received.To[0].DisplayName != "User" {
		t.Errorf("expected to name 'User', got %q", received.To[0].DisplayName)
	}
	if received.Subject != "Hello" {
		t.Errorf("expected subject 'Hello', got %q", received.Subject)
	}
}

func TestSendAs_CustomFromName(t *testing.T) {
	var received mailerooPayload

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(mailerooResponse{Success: true, Message: "sent"})
	}))
	defer ts.Close()

	s := newTestSender(ts)

	err := s.SendAs(context.Background(), "Dave via Pad", "user@example.com", "", "Test", "<p>test</p>", "test")
	if err != nil {
		t.Fatalf("SendAs failed: %v", err)
	}

	// From address should stay the same (verified domain)
	if received.From.Address != "noreply@test.com" {
		t.Errorf("expected from address 'noreply@test.com', got %q", received.From.Address)
	}
	// But the display name should be custom
	if received.From.DisplayName != "Dave via Pad" {
		t.Errorf("expected from name 'Dave via Pad', got %q", received.From.DisplayName)
	}
}

func TestSend_APIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(mailerooResponse{Success: false, Message: "invalid sender"})
	}))
	defer ts.Close()

	s := newTestSender(ts)

	err := s.Send(context.Background(), "user@example.com", "", "Test", "<p>test</p>", "test")
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

func TestSend_SuccessFalse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mailerooResponse{Success: false, Message: "domain not verified"})
	}))
	defer ts.Close()

	s := newTestSender(ts)

	err := s.Send(context.Background(), "user@example.com", "", "Test", "<p>test</p>", "test")
	if err == nil {
		t.Fatal("expected error for success=false response")
	}
}

func TestConfigure_UpdatesSettings(t *testing.T) {
	var received mailerooPayload

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(mailerooResponse{Success: true, Message: "sent"})
	}))
	defer ts.Close()

	s := newTestSender(ts)

	// Reconfigure at runtime
	s.Configure("new-key", "new@test.com", "NewApp", "https://new.example.com")

	err := s.Send(context.Background(), "user@example.com", "", "Test", "<p>test</p>", "test")
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if received.From.Address != "new@test.com" {
		t.Errorf("expected from 'new@test.com', got %q", received.From.Address)
	}
	if received.From.DisplayName != "NewApp" {
		t.Errorf("expected from name 'NewApp', got %q", received.From.DisplayName)
	}
}

func TestSendInvitation_ContextualFromName(t *testing.T) {
	var received mailerooPayload

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(mailerooResponse{Success: true, Message: "sent"})
	}))
	defer ts.Close()

	s := newTestSender(ts)

	err := s.SendInvitation(context.Background(), "invitee@example.com", "Alice", "My Project", "https://example.com/join/abc123")
	if err != nil {
		t.Fatalf("SendInvitation failed: %v", err)
	}

	if received.Subject != "Alice invited you to My Project on Pad" {
		t.Errorf("unexpected subject: %q", received.Subject)
	}
	// Invitation emails use contextual from name
	if received.From.DisplayName != "Alice via Pad" {
		t.Errorf("expected from name 'Alice via Pad', got %q", received.From.DisplayName)
	}
}

func TestSendWelcome(t *testing.T) {
	var received mailerooPayload

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(mailerooResponse{Success: true, Message: "sent"})
	}))
	defer ts.Close()

	s := newTestSender(ts)

	err := s.SendWelcome(context.Background(), "alice@example.com", "Alice")
	if err != nil {
		t.Fatalf("SendWelcome failed: %v", err)
	}

	if received.Subject != "Welcome to Pad" {
		t.Errorf("unexpected subject: %q", received.Subject)
	}
	if received.To[0].DisplayName != "Alice" {
		t.Errorf("expected to name 'Alice', got %q", received.To[0].DisplayName)
	}
}

func TestSendTest(t *testing.T) {
	var received mailerooPayload

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(mailerooResponse{Success: true, Message: "sent"})
	}))
	defer ts.Close()

	s := newTestSender(ts)

	err := s.SendTest(context.Background(), "admin@example.com")
	if err != nil {
		t.Fatalf("SendTest failed: %v", err)
	}

	if received.Subject != "Pad — Test Email" {
		t.Errorf("unexpected subject: %q", received.Subject)
	}
}
