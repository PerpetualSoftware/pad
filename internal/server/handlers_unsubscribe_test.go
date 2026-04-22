package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUnsubscribePage_EscapesUserInput confirms the rendered HTML
// doesn't contain raw attacker-controlled markup even when a malicious
// "email" string is fed to the data struct. Before TASK-657 we used
// fmt.Sprintf to inject the email directly into the page.
func TestUnsubscribePage_EscapesUserInput(t *testing.T) {
	malicious := `"><script>alert('xss')</script>`
	rr := httptest.NewRecorder()
	renderUnsubPage(rr, 200, unsubData{
		Title:            "Unsubscribed",
		Message:          "You've been unsubscribed.",
		ShowEmailMessage: true,
		Email:            malicious,
	})

	body := rr.Body.String()
	if strings.Contains(body, "<script>") {
		t.Fatalf("unescaped <script> in response:\n%s", body)
	}
	// Confirm the escaped form IS present — i.e. the content was rendered,
	// just neutered.
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("expected escaped script marker in response:\n%s", body)
	}
}

// TestUnsubscribePage_SetsStrictCSP verifies the defense-in-depth
// headers are applied on every render path.
func TestUnsubscribePage_SetsStrictCSP(t *testing.T) {
	rr := httptest.NewRecorder()
	renderUnsubPage(rr, 200, unsubData{Title: "x", Message: "x"})

	csp := rr.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("CSP header missing on unsubscribe response")
	}
	for _, want := range []string{
		"default-src 'none'",
		"script-src 'none'",
		"script-src-attr 'none'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing directive %q; got: %s", want, csp)
		}
	}
	if rr.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
}
