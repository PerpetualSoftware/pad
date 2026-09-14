package main

import "testing"

// BUG-3037 (codex round 3) — `--expected-seq 0` must reach the wire.
//
// 0 is the flag's zero value AND a value a user can type, so a presence check
// written as `if expectedSeq != 0` silently DROPPED the flag and sent an
// UNGUARDED write: the one outcome a caller who explicitly asked for a guard
// must never get. The server refuses a seq below 1 with a 400 — a clear answer
// — but only if the CLI actually sends it.
func TestItemUpdate_ExpectedSeqFlagIsSentByPresence(t *testing.T) {
	t.Run("a normal token is sent", func(t *testing.T) {
		body := captureUpdateBody(t, "TASK-9", "--status", "done", "--expected-seq", "42")
		if got, ok := body["expected_seq"]; !ok || got != float64(42) {
			t.Fatalf("expected_seq = %v (present=%v), want 42", got, ok)
		}
	})

	t.Run("an explicit zero is sent, not dropped", func(t *testing.T) {
		body := captureUpdateBody(t, "TASK-9", "--status", "done", "--expected-seq", "0")
		got, ok := body["expected_seq"]
		if !ok {
			t.Fatalf("--expected-seq 0 was dropped: the write went out UNGUARDED (%v)", body)
		}
		if got != float64(0) {
			t.Fatalf("expected_seq = %v, want 0 — the value must reach the server verbatim so IT answers", got)
		}
	})

	t.Run("no flag means no token", func(t *testing.T) {
		body := captureUpdateBody(t, "TASK-9", "--status", "done")
		if _, ok := body["expected_seq"]; ok {
			t.Fatalf("expected_seq was invented for a caller who sent no flag: %v", body)
		}
	})
}
