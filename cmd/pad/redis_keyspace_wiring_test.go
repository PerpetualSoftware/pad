package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestAllRedisKeyspacesShareOneNamespace is a drift guard, in the same
// spirit as internal/collections' invocation-framing test: it reads the
// wiring source and enforces an invariant the type system cannot
// (codex round 4).
//
// internal/redisns centralizes key CONSTRUCTION, but nothing stops a
// future contributor from handing one bus a different Keys value than
// another — every package would compile, every unit test would pass, and
// the deployment would quietly run with its event bus in one keyspace and
// its presence registry in another. That is a worse state than the flat
// names the namespace replaced, because it looks configured.
//
// internal/watchevents' DEPLOYMENT SCOPING note states the rule as
// "every keyspace at once, from shared config". This is that rule's
// enforcement step rather than another sentence asserting it.
//
// THE TRADE, since a source-parsing test is an unusual thing to ship
// (codex round 8 flagged it): it protects a future EDIT rather than
// runtime behaviour, and it breaks on a rename or a reformat of the
// constructor calls. Kept because the alternative on offer — a single
// construction API returning all three — cannot exist without collapsing
// three packages' constructors into one, and because this guard's
// failure mode is loud and its message says exactly what to do. A test
// that needs a one-line update after a deliberate rename is a better
// deal than an invariant with no enforcement at all, which is what the
// package comment alone amounts to.
func TestAllRedisKeyspacesShareOneNamespace(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("cmd_server.go")
	if err != nil {
		t.Fatalf("read cmd_server.go: %v", err)
	}
	text := string(src)

	// Every namespaced constructor call, with its arguments.
	call := regexp.MustCompile(`New(?:RedisBus|RedisSessionPresence)WithKeys\(([^)]*)\)`)
	matches := call.FindAllStringSubmatch(text, -1)

	// PREMISE: the guard is looking at something. Without this, a rename
	// of the constructors would make this test pass by finding nothing —
	// a guard that cannot fire, reporting success.
	const wantCalls = 3
	if len(matches) != wantCalls {
		t.Fatalf("found %d namespaced constructor calls in cmd_server.go, want %d — "+
			"either a keyspace was added without this guard being updated, or the "+
			"constructors were renamed and this guard is now inert", len(matches), wantCalls)
	}

	const sharedVar = "redisKeys"
	for _, m := range matches {
		args := m[1]
		if !argsContainIdentifier(args, sharedVar) {
			t.Errorf("constructor call %q does not pass %s — every Redis keyspace must take "+
				"the SAME namespace value, or the installation runs split across two keyspaces "+
				"while looking correctly configured", strings.TrimSpace(m[0]), sharedVar)
		}
	}

	// And that variable must come from the validated parser, not be
	// assembled locally.
	if !strings.Contains(text, sharedVar+", err := redisns.Parse(") {
		t.Errorf("%s is not produced by redisns.Parse in cmd_server.go — the namespace must be "+
			"validated once, centrally, or an invalid value reaches key names instead of failing startup", sharedVar)
	}
}

// argsContainIdentifier reports whether name appears in args as a whole
// identifier, so a substring like redisKeysOther does not satisfy the
// check.
func argsContainIdentifier(args, name string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	return re.MatchString(args)
}
