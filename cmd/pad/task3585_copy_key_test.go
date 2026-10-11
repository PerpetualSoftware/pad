package main

import (
	"bytes"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// TASK-3585: the copy command keys the mutating call (fresh per invocation,
// or --op-key) and never the read-only preflight.
func TestTASK3585_CopyCommandKeysTheMutatingCallOnly(t *testing.T) {
	keyOf := func(opKey string) (pre, cp string) {
		d := &recordingDeps{t: t, preflight: emptyPreflight(), result: &cli.ItemCopyResult{}}
		opts := baseOpts()
		opts.OpKey = opKey
		var out, errOut bytes.Buffer
		if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
			t.Fatalf("runItemCopy: %v", err)
		}
		if len(d.preflightCalls) != 1 || len(d.copyCalls) != 1 {
			t.Fatalf("calls: preflight %d, copy %d", len(d.preflightCalls), len(d.copyCalls))
		}
		return d.preflightCalls[0].OpKey, d.copyCalls[0].OpKey
	}
	pre, k1 := keyOf("")
	_, k2 := keyOf("")
	_, mine := keyOf("mine")
	if pre != "" {
		t.Fatalf("the preflight carried a key: %q", pre)
	}
	if k1 == "" || k1 == k2 || mine != "mine" {
		t.Fatalf("keys = %q %q %q", k1, k2, mine)
	}
}
