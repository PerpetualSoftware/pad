package redisns

import "testing"

// TestDefaultPreservesHistoricalNames is the compatibility promise made
// executable: an upgrade with no namespace configured must address the
// SAME keys as before, or every existing deployment silently loses its
// replay buffers, counters and presence entries.
//
// The expected strings are written out literally rather than derived from
// Name, so a change to Name's construction cannot make this test agree
// with itself.
func TestDefaultPreservesHistoricalNames(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"events:":           "pad:events:",
		"event_seq":         "pad:event_seq",
		"watchevents":       "pad:watchevents",
		"watchevents_seq":   "pad:watchevents_seq",
		"watchevents_epoch": "pad:watchevents_epoch",
		"watchevents:pub:":  "pad:watchevents:pub:",
		"session:u1:s1":     "pad:session:u1:s1",
		"sessions:u1":       "pad:sessions:u1",
	}
	for suffix, want := range cases {
		if got := Default.Name(suffix); got != want {
			t.Errorf("Default.Name(%q) = %q, want %q", suffix, got, want)
		}
	}

	// The zero value must behave identically — a struct field added later
	// with a non-zero meaning would break this.
	var zero Keys
	if got, want := zero.Name("event_seq"), "pad:event_seq"; got != want {
		t.Errorf("zero Keys.Name = %q, want %q", got, want)
	}
}

func TestNamespacedNames(t *testing.T) {
	t.Parallel()

	k, err := Parse("staging")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := k.Name("event_seq"), "pad:staging:event_seq"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got, want := k.Name("events:"), "pad:staging:events:"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got := k.Namespace(); got != "staging" {
		t.Errorf("Namespace = %q, want %q", got, "staging")
	}

	// A namespaced name must not collide with the default keyspace, which
	// is the entire point of the change.
	if k.Name("event_seq") == Default.Name("event_seq") {
		t.Fatal("a namespaced key collides with the default keyspace")
	}
}

func TestParseRejectsNamesThatCouldForgeAKeyPath(t *testing.T) {
	t.Parallel()

	// A colon is Pad's own separator, so a namespace containing one spans
	// segments and makes the keyspace ambiguous to read back.
	for _, bad := range []string{"a:events", "Staging", "with space", "emoji-🐦", "tab\there", "sub/path", "quote'"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted an invalid namespace", bad)
		}
	}

	// Whitespace-only is an ERROR, not a synonym for unset: a broken
	// template substitution produces " ", and trimming it to Default
	// would silently share the default keyspace with the very
	// installation the namespace was set to separate from.
	for _, blank := range []string{" ", "   ", "\t", "\n", " \t "} {
		if _, err := Parse(blank); err == nil {
			t.Errorf("Parse(%q) accepted a whitespace-only namespace — it would silently fall back to the shared default keyspace", blank)
		}
	}

	for _, good := range []string{"", "staging", "prod-2", "eu_west", "a", "0"} {
		if _, err := Parse(good); err != nil {
			t.Errorf("Parse(%q) rejected a valid namespace: %v", good, err)
		}
	}

	// The REAL collision the character set cannot catch: a namespace
	// equal to one of Pad's own first segments nests this installation
	// inside the default one's keyspace. "events" is the sharp case —
	// pad:events:* is the default installation's activity channel space,
	// so namespace "events" would put every key of this installation
	// inside it.
	for _, reserved := range []string{"events", "event_seq", "watchevents", "watchevents_seq", "watchevents_epoch", "session", "sessions"} {
		if _, err := Parse(reserved); err == nil {
			t.Errorf("Parse(%q) accepted a namespace equal to one of Pad's own key segments", reserved)
		}
	}

	// The control leg: names that merely CONTAIN a reserved word are fine.
	// Without it a build that rejected anything containing "session" would
	// pass the loop above while refusing perfectly good namespaces.
	for _, ok := range []string{"events-eu", "my-events", "sessions2", "prod-session"} {
		if _, err := Parse(ok); err != nil {
			t.Errorf("Parse(%q) rejected a namespace that only contains a reserved word: %v", ok, err)
		}
	}

	// Only a genuinely UNSET value yields Default.
	k, err := Parse("")
	if err != nil {
		t.Fatalf("Parse(empty): %v", err)
	}
	if k.Namespace() != "" {
		t.Fatalf("empty namespace = %q, want empty", k.Namespace())
	}
}
