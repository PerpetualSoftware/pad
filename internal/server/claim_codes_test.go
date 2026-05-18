package server

import (
	"strings"
	"testing"
	"time"
)

// Tests for the stateless HMAC claim-code mechanics (PLAN-1519 /
// TASK-1521 / IDEA-1517 §4).
//
// What's covered:
//
//   - DeriveClaimCode is deterministic for the same (secret, user,
//     workspace, time bucket) — same call twice produces the same code.
//   - Codes are six decimal digits, zero-padded.
//   - Different (user) or (workspace) inputs produce different codes
//     even with the same secret + time (no collisions by length-prefix
//     boundary games — would catch a regression where we concatenate
//     without length prefix).
//   - VerifyClaimCode accepts a code from the current bucket.
//   - VerifyClaimCode accepts a code from the PREVIOUS bucket (5–10 min
//     sliding lifetime).
//   - VerifyClaimCode REJECTS a code older than two buckets.
//   - VerifyClaimCode rejects wrong code / wrong user / wrong workspace.
//   - VerifyClaimCode rejects empty / wrong-length codes without
//     hitting the HMAC (cheap fast path).
//   - Short secret (< 16 bytes) makes VerifyClaimCode fail closed even
//     against a derive that used the same short secret — the verifier
//     enforces the minimum independently of the derive side, so a
//     misconfigured deployment can't accept its own forgeable codes.

func TestDeriveClaimCode_Deterministic(t *testing.T) {
	secret := []byte("a-very-deterministic-test-secret-32")
	at := time.Date(2026, 5, 18, 4, 0, 0, 0, time.UTC)
	a := DeriveClaimCode(secret, "user-1", "ws-1", at)
	b := DeriveClaimCode(secret, "user-1", "ws-1", at)
	if a != b {
		t.Errorf("non-deterministic derive: %q vs %q", a, b)
	}
	if len(a) != 6 {
		t.Errorf("code should be 6 digits, got %q (len=%d)", a, len(a))
	}
	for _, c := range a {
		if c < '0' || c > '9' {
			t.Errorf("code should be all decimal digits, got %q", a)
		}
	}
}

func TestDeriveClaimCode_ZeroPadded(t *testing.T) {
	// Brute-force search for an input whose derive happens to be small —
	// we want to verify zero-padding works. With 10^6 possible codes,
	// roughly 1-in-10 derives lands under 100000; trying 200 different
	// workspace IDs is overwhelmingly likely to surface one.
	secret := []byte("padding-search-secret-bytes-here-32")
	at := time.Date(2026, 5, 18, 4, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		ws := "ws-pad-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		code := DeriveClaimCode(secret, "user", ws, at)
		if strings.HasPrefix(code, "0") {
			if len(code) != 6 {
				t.Errorf("zero-padded code should still be 6 digits, got %q", code)
			}
			return // found one, assertion satisfied
		}
	}
	t.Log("no leading-zero code found in 200 derivations — extremely unlikely; rerun with more iterations if hit")
}

func TestDeriveClaimCode_NoCollisionOnConcatBoundary(t *testing.T) {
	// Without length-prefixing, ("abc", "def") and ("abcdef", "") would
	// HMAC the same byte sequence and produce identical codes. The
	// length-prefix in writeLenPrefixed prevents this. Regression
	// guard — would fail if someone "simplifies" the derive to a bare
	// concatenation.
	secret := []byte("collision-test-secret-bytes-here-32")
	at := time.Date(2026, 5, 18, 4, 0, 0, 0, time.UTC)
	a := DeriveClaimCode(secret, "abc", "def", at)
	b := DeriveClaimCode(secret, "abcdef", "", at)
	if a == b {
		t.Errorf("length-prefix regression: (abc,def) and (abcdef,'') derived to same code %q", a)
	}
}

func TestVerifyClaimCode_CurrentBucketAccepted(t *testing.T) {
	secret := []byte("verify-current-secret-bytes-here-32")
	at := time.Date(2026, 5, 18, 4, 0, 0, 0, time.UTC)
	code := DeriveClaimCode(secret, "u", "w", at)
	if !VerifyClaimCode(secret, "u", "w", code, at) {
		t.Errorf("current-bucket code %q should verify", code)
	}
}

func TestVerifyClaimCode_PreviousBucketAccepted(t *testing.T) {
	secret := []byte("verify-previous-secret-bytes-here-32")
	at := time.Date(2026, 5, 18, 4, 0, 0, 0, time.UTC)
	code := DeriveClaimCode(secret, "u", "w", at)
	// 4 minutes later — same bucket. Still verifies.
	if !VerifyClaimCode(secret, "u", "w", code, at.Add(4*time.Minute)) {
		t.Errorf("code should still verify 4 minutes later (same bucket)")
	}
	// 6 minutes later — next bucket; the original code is now in the
	// "previous bucket" position. Still verifies.
	if !VerifyClaimCode(secret, "u", "w", code, at.Add(6*time.Minute)) {
		t.Errorf("code should still verify 6 minutes later (previous bucket window)")
	}
}

func TestVerifyClaimCode_AgedOutRejected(t *testing.T) {
	secret := []byte("verify-aged-secret-bytes-here-3232")
	at := time.Date(2026, 5, 18, 4, 0, 0, 0, time.UTC)
	code := DeriveClaimCode(secret, "u", "w", at)
	// 11 minutes later — more than two buckets gone, even with sliding
	// alignment. Must NOT verify.
	if VerifyClaimCode(secret, "u", "w", code, at.Add(11*time.Minute)) {
		t.Errorf("aged-out code (>10 min) should not verify")
	}
}

func TestVerifyClaimCode_WrongInputsRejected(t *testing.T) {
	secret := []byte("verify-wrong-secret-bytes-here-3232")
	at := time.Date(2026, 5, 18, 4, 0, 0, 0, time.UTC)
	code := DeriveClaimCode(secret, "right-user", "right-ws", at)

	cases := []struct {
		name    string
		userID  string
		wsID    string
		code    string
		wantOK  bool
		comment string
	}{
		{"right code right user right ws", "right-user", "right-ws", code, true, "baseline"},
		{"wrong user same ws", "wrong-user", "right-ws", code, false, "user-bound"},
		{"right user wrong ws", "right-user", "wrong-ws", code, false, "ws-bound"},
		{"wrong code", "right-user", "right-ws", "000000", false, "wrong digits — almost certainly"},
		{"empty code fast-rejects", "right-user", "right-ws", "", false, "guard before HMAC"},
		{"5-digit code rejected", "right-user", "right-ws", "12345", false, "wrong length, guard before HMAC"},
		{"7-digit code rejected", "right-user", "right-ws", "1234567", false, "wrong length, guard before HMAC"},
		{"empty user", "", "right-ws", code, false, "guard"},
		{"empty workspace", "right-user", "", code, false, "guard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := VerifyClaimCode(secret, tc.userID, tc.wsID, tc.code, at)
			if got != tc.wantOK {
				t.Errorf("%s: want %v got %v (%s)", tc.name, tc.wantOK, got, tc.comment)
			}
		})
	}
}

func TestVerifyClaimCode_ShortSecretFailsClosed(t *testing.T) {
	// Derive with a too-short secret — the derive path doesn't reject
	// (it's a pure function; the SetClaimSecret setter is where the
	// guard nominally lives). Verify with the same secret must still
	// return false because VerifyClaimCode enforces the 16-byte
	// minimum independently. This guards against a regression where
	// VerifyClaimCode might trust whatever it's given.
	tooShort := []byte("short")
	at := time.Date(2026, 5, 18, 4, 0, 0, 0, time.UTC)
	code := DeriveClaimCode(tooShort, "u", "w", at)
	if VerifyClaimCode(tooShort, "u", "w", code, at) {
		t.Errorf("verifier must reject when secret is shorter than 16 bytes, even if derive matches")
	}
}
