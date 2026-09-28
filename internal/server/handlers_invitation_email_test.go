package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInvitationEmailMatches(t *testing.T) {
	for _, tc := range []struct {
		name, email, invited string
		want                 bool
	}{
		{"long_s", "ſam@x.com", "sam@x.com", false},
		{"case_and_whitespace", " \tSam@X.com\n", "sam@x.com", true},
		{"different_address", "sam@K.com", "sam@x.com", false},
		{"kelvin_lowercase", "Kam@x.com", "kam@x.com", false}, // the ASCII-only fold leaves the Kelvin sign as itself
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := invitationEmailMatches(tc.email, tc.invited); got != tc.want {
				t.Errorf("invitationEmailMatches(%q, %q) = %t, want %t", tc.email, tc.invited, got, tc.want)
			}
		})
	}
}

// BUG-3282: both entry points must compare the lowercase address bytes,
// without EqualFold's additional equivalences (notably long s and ASCII s).
func TestInvitationEmailBinding(t *testing.T) {
	for _, door := range []string{"accept", "register"} {
		t.Run(door, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				invited string
				caller  string
				matches bool
			}{
				{"long_s", "sam@x.com", "ſam@x.com", false},
				{"ascii_case", "sam@x.com", "Sam@X.com", true},
				{"whitespace", "sam@x.com", " \tSAM@X.COM \n", true},
				{"kelvin_different_address", "sam@x.com", "SAM@K.COM", false},
				// The accepting account's stored email is already lowercased
				// by the user store (the Kelvin sign becomes an ASCII k), so on
				// the accept door this is the ASCII address kam@x.com and
				// matches. Registration rejects non-ASCII addresses outright.
				{"kelvin_lowercase", "kam@x.com", "Kam@X.com", true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					e := newAcceptLimitEnv(t)
					inv := e.invite(t, tc.invited)
					before := e.members(t)
					var rr *httptest.ResponseRecorder
					wantStatus := http.StatusOK
					matches := tc.matches
					wantError := "invitation_email_mismatch"
					if door == "accept" {
						rr = e.acceptAs(t, tc.caller, inv)
					} else {
						rr = e.register(tc.caller, inv)
						wantStatus = http.StatusCreated
					}
					if !matches {
						wantStatus = http.StatusForbidden
					}
					// Registration already rejects non-ASCII addresses before
					// checking the invitation. This change preserves that policy.
					if door == "register" && strings.ContainsAny(tc.caller, "ſK") {
						matches = false
						wantStatus = http.StatusBadRequest
						wantError = "validation_error"
					}
					if rr.Code != wantStatus {
						t.Fatalf("status = %d, want %d: %s", rr.Code, wantStatus, rr.Body.String())
					}
					wantMembers := before
					if matches {
						wantMembers++
					} else {
						if got := errorCode(t, rr); got != wantError {
							t.Errorf("error code = %q, want %q", got, wantError)
						}
						if door == "register" && e.accountExists(t, tc.caller) {
							t.Error("refused registration created an account")
						}
					}
					if got := e.members(t); got != wantMembers {
						t.Errorf("members = %d, want %d", got, wantMembers)
					}
					if pending := e.stillPending(t, inv); pending == matches {
						t.Errorf("invitation pending = %t, want %t", pending, !matches)
					}
				})
			}
		})
	}
}
