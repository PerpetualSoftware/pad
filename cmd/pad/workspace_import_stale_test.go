package main

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/server"
)

// BUG-3032, codex round 1 P2: the server reports the stale-body count as a
// response header, and the CLI printed only the NUL-repair count — so the signal
// existed on the wire and nowhere a human could see it.
//
// The asymmetry with repairedNULCount is deliberate and is what these cases pin:
// that helper answers a question the operator ASKED by passing --repair-nul, so a
// missing header there needs an "unknown (...)" explanation. This one is
// unsolicited, so absent / unparseable / zero all mean "say nothing" — printing
// "unknown" on every import against an older server would invent an open
// question out of a server that simply has nothing to report.
// staleHeader builds a header through Set so the key is canonicalized the way
// the server's own writes are.
func staleHeader(v string) http.Header {
	h := http.Header{}
	h.Set(server.StaleBodyImportHeader, v)
	return h
}

func TestStaleBodyImportCountSpeaksOnlyWhenThereIsSomethingToSay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
		want   string
	}{
		{"nil headers", nil, ""},
		{"no header at all (older server)", http.Header{}, ""},
		{"explicit zero", staleHeader("0"), ""},
		{"empty value", staleHeader(""), ""},
		{"a real count", staleHeader("2"), "2"},
		{"one", staleHeader("1"), "1"},
		// A response header is middlebox- and attacker-influenced input, not a
		// trusted field. The earlier version returned any non-"0" string, so each
		// of these printed as a count while the doc comment claimed they did not
		// (codex round 2 P2).
		{"not a number", staleHeader("abc"), ""},
		{"negative", staleHeader("-1"), ""},
		{"NaN", staleHeader("NaN"), ""},
		{"float", staleHeader("2.5"), ""},
		{"a count with padding", staleHeader(" 7 "), ""},
		{"injected trailer", staleHeader("2; drop"), ""},
		// Parsed and RE-RENDERED, never echoed, so a value that parses but is
		// spelled oddly reaches the terminal in one canonical form.
		{"leading zeros are normalised", staleHeader("007"), "7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleBodyImportCount(tc.header); got != tc.want {
				t.Errorf("staleBodyImportCount = %q, want %q", got, tc.want)
			}
		})
	}
}

// The two helpers must not be confused for each other: they read DIFFERENT
// headers, and a copy-paste that pointed one at the other's header would be
// invisible in the output of an import that triggered neither.
func TestStaleBodyAndNULCountReadDifferentHeaders(t *testing.T) {
	// Set, never a literal map: http.Header keys are stored in canonical MIME
	// form and Get canonicalizes its lookup, so a literal `http.Header{...}`
	// entry for "X-Pad-Repaired-NUL-Values" is never found by a Get that looks
	// for "X-Pad-Repaired-Nul-Values". Production is unaffected — the server
	// writes through Set — but a test built on literals silently measures
	// nothing, which is how this assertion first failed.
	h := http.Header{}
	h.Set(server.NULRepairHeader, "5")
	h.Set(server.StaleBodyImportHeader, "2")
	if got := staleBodyImportCount(h); got != "2" {
		t.Errorf("staleBodyImportCount = %q, want %q — it is reading the NUL header", got, "2")
	}
	if got := repairedNULCount(h); got != "5" {
		t.Errorf("repairedNULCount = %q, want %q — it is reading the stale-body header", got, "5")
	}
	if server.NULRepairHeader == server.StaleBodyImportHeader {
		t.Fatal("the two headers have the same name; one import advisory would overwrite the other")
	}
}
