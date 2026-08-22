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

	// ONE LEVEL OF INDIRECTION IS ALLOWED AND CHECKED. The event bus is
	// built by newObservedEventBus so its observer wiring is testable
	// (BUG-2731), which means the constructor above receives that helper's
	// PARAMETER rather than the outer variable. Naming the parameter
	// redisKeys keeps the check above meaningful only if every CALL to the
	// helper also passes the shared value — otherwise the identifier check
	// is satisfied by a shadowed name and proves nothing.
	//
	// The `func ` exclusion and the EXACT count both matter (codex round 8):
	// the naive pattern also matches the helper's own DECLARATION, so a
	// version accepting "at least 2" would be satisfied by the declaration
	// plus a single call site — passing while one deployment shape went
	// unchecked.
	helperCall := regexp.MustCompile(`(?:^|[^c])(?:\bfunc\s+)?newObservedEventBus\(([^)]*)\)`)
	var helperCalls [][]string
	for _, m := range helperCall.FindAllStringSubmatch(text, -1) {
		if strings.Contains(m[0], "func ") {
			continue // the declaration, not a call
		}
		helperCalls = append(helperCalls, m)
	}
	const wantHelperCalls = 2 // the Redis shape and the in-process shape
	if len(helperCalls) != wantHelperCalls {
		t.Fatalf("found %d newObservedEventBus CALL sites in cmd_server.go, want %d (the Redis and in-process shapes) — "+
			"either a shape was dropped, a third was added without this guard being updated, or the helper "+
			"was renamed and this half of the guard is now inert",
			len(helperCalls), wantHelperCalls)
	}
	for _, m := range helperCalls {
		if !argsContainIdentifier(m[1], sharedVar) {
			t.Errorf("helper call %q does not pass %s — the namespace must reach the bus constructor "+
				"from the shared value, not from a locally assembled one", strings.TrimSpace(m[0]), sharedVar)
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
