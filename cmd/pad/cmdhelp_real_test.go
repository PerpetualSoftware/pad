package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// emitRealPadJSON builds the real pad cobra tree and emits its JSON
// representation with a static (no-resolver, no live-workspace)
// configuration. Tests that exercise schema validation and example
// drift detection against the actual command surface use this.
//
// `Resolver: nil` is deliberate: deterministic emission, no flakiness
// from a live or absent workspace, no need to mock the cli client.
func emitRealPadJSON(t *testing.T) []byte {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	if err := cmdhelp.EmitJSON(root, root, cmdhelp.Options{
		Binary:   "pad",
		Version:  "test",
		Homepage: "https://getpad.dev",
		MaxDepth: -1,
	}, &buf); err != nil {
		t.Fatalf("EmitJSON on real pad tree: %v", err)
	}
	return buf.Bytes()
}

func TestRealPadTree_ValidatesAgainstSchema(t *testing.T) {
	// THE BIG ASSERTION: the real, full pad CLI emits cmdhelp v0.1
	// JSON that satisfies every contract in schema/cmdhelp.schema.json.
	//
	// Future regressions caught by this test:
	//   - new flag with a type outside the closed vocabulary
	//   - exit_code key that isn't numeric
	//   - flag name with leading dashes (violates propertyNames)
	//   - cmdhelp_version pattern broken
	//   - any required field accidentally omitted
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	schema, err := cmdhelp.FindAndCompileSchema(cwd)
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	jsonBytes := emitRealPadJSON(t)

	var doc interface{}
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		t.Fatalf("unmarshal pad's cmdhelp JSON: %v", err)
	}
	if err := schema.Validate(doc); err != nil {
		// Don't dump the full output (it's huge) — the validator's
		// error already names the offending JSON pointer.
		t.Errorf("pad's cmdhelp JSON does not validate against schema/cmdhelp.schema.json:\n%v", err)
	}
}

func TestRealPadTree_ExampleDriftValidator(t *testing.T) {
	// THE DRIFT-PREVENTION CONTRACT (spec §6 / §11 Q5): every example
	// emitted in cmdhelp output MUST resolve against the live cobra
	// command tree. A typo'd flag, a renamed/deleted command, or a
	// stale example block in cobra Long that references a defunct
	// surface — all caught here.
	//
	// If this test fails after a code change:
	//   1. The implementation changed (renamed flag, removed cmd) but
	//      the example block in Long still references the old name —
	//      fix the example.
	//   2. Or the implementation is correct and the example was always
	//      wrong — fix the example.
	//
	// Adding a new typo'd example to any cobra Long block in cmd/pad
	// MUST break this test. That's the spec contract in action.
	root := newRootCmd()
	jsonBytes := emitRealPadJSON(t)
	var doc cmdhelp.Document
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	findings := cmdhelp.ValidateExamples(&doc, root)
	if len(findings) > 0 {
		t.Errorf("example drift detected — %d finding(s):", len(findings))
		for _, f := range findings {
			t.Errorf("  %s", f)
		}
	}
}

func TestRealPadTree_BoolArityRule(t *testing.T) {
	// Spec §5.3: bool flags must be presence-only. None of pad's
	// existing examples should reference a bool flag in valued form.
	jsonBytes := emitRealPadJSON(t)
	var doc cmdhelp.Document
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	findings := cmdhelp.ValidateBoolArity(&doc)
	if len(findings) > 0 {
		t.Errorf("bool arity violations — %d finding(s):", len(findings))
		for _, f := range findings {
			t.Errorf("  %s", f)
		}
	}
}

func TestCapabilities_FallbackEquivalent(t *testing.T) {
	// Spec §8 says either form must produce the same line. The fallback
	// form (--cmdhelp-capabilities) is implemented in main() before
	// cobra parsing; the primary form (help --capabilities) goes
	// through helpCmd. They MUST advertise the same capability bit.
	var fb bytes.Buffer
	handled := handleCmdhelpCapabilitiesFallback([]string{"--cmdhelp-capabilities"}, &fb)
	if !handled {
		t.Fatalf("handleCmdhelpCapabilitiesFallback should have handled --cmdhelp-capabilities")
	}

	// helpCmd's --capabilities path: build a synthetic root just to
	// route the flag through helpCmd's RunE. This intentionally mirrors
	// the structure of help_cmdhelp_test.go's runHelp helper to share
	// the same wiring across tests.
	primary := captureHelpCmdCapabilities(t)

	if string(fb.Bytes()) != primary {
		t.Errorf("capability bit forms differ:\nfallback: %q\nprimary:  %q", fb.String(), primary)
	}
}

func TestCapabilities_FallbackIsSideEffectFree(t *testing.T) {
	// Side-effect-free contract (spec §8): no logging, no network,
	// no auth challenge, no flag-parse errors on unrelated args.
	// Verify by passing args alongside --cmdhelp-capabilities that
	// would normally trigger errors (unknown subcommand, bad flags).
	var fb bytes.Buffer
	handled := handleCmdhelpCapabilitiesFallback(
		[]string{"completely", "bogus", "--garbage", "--cmdhelp-capabilities", "--more=junk"},
		&fb,
	)
	if !handled {
		t.Fatalf("expected fallback to handle --cmdhelp-capabilities even alongside garbage args")
	}
	want := cmdhelp.CapabilityLine(padCmdhelpFormats) + "\n"
	if fb.String() != want {
		t.Errorf("output mismatch.\n got: %q\nwant: %q", fb.String(), want)
	}
}

func TestCapabilities_FallbackNotTriggeredWithoutFlag(t *testing.T) {
	var fb bytes.Buffer
	handled := handleCmdhelpCapabilitiesFallback([]string{"item", "list"}, &fb)
	if handled {
		t.Errorf("fallback should NOT trigger on plain args; got handled=true")
	}
	if fb.Len() != 0 {
		t.Errorf("fallback should not write output without the flag; got: %q", fb.String())
	}
}

// captureHelpCmdCapabilities runs helpCmd via a synthetic root and
// returns its --capabilities output as a string. Mirrors the
// runHelp pattern in help_cmdhelp_test.go.
func captureHelpCmdCapabilities(t *testing.T) string {
	t.Helper()
	root := buildTestRoot()
	root.SetArgs([]string{"help", "--capabilities"})
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatalf("help --capabilities: unexpected error: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	// Strip any unexpected leading/trailing whitespace artifacts before
	// the comparison so the capability bit (which uses Fprintln) is the
	// only meaningful differentiator.
	return strings.TrimRight(out, "\n") + "\n"
}
