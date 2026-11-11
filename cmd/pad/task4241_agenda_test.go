package main

import (
	"strings"
	"testing"
	"time"
)

func TestAgendaStaleTitleStaysOnItsLine(t *testing.T) {
	h, _ := recordingStub(func(c recordedCall) (int, string) {
		return 200, `{"tz":"UTC","stale":true,"entries":[{"ref":"TASK-7","title":"Fix\nauth\t  now","at":"2031-03-10T09:00:00Z","roles":{},"missed_count":3}]}`
	})
	out, _, err := tzRun(t, tzCLIWithLocal(t, h, time.UTC), "item", "remind", "agenda", "--stale")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Fix auth now") || len(strings.Split(strings.TrimSpace(out), "\n")) != 2 {
		t.Fatalf("title spilled across lines: %q", out)
	}
}
