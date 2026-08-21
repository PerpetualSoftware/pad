package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BUG-2699 — the enforcement step for an invariant this package's own doc
// comments assert.
//
// publishWatchNotification's doc comment says the push handler is the ONLY
// direct Bus.Publish caller in this package, because it is the only site
// whose notification has no durable backing to fall back on. A sentence in
// a comment protects whoever reads that comment before adding a producer;
// it does nothing about the producer added by someone who didn't. So the
// split is checked here instead of asserted there.
//
// This is a SOURCE scan rather than a behavioural test on purpose: the
// thing being protected is a call-site population, and a population claim
// is checked by enumerating it. A new producer that calls
// s.watchEvents.Publish directly fails this test by name and has to make
// a deliberate choice — use the best-effort helper, or extend
// allowedDirectPublishFiles with a reason.
var allowedDirectPublishFiles = map[string]string{
	// The one caller that acts on the result: a push has no inbox, no
	// store row, nothing to read back, so a refused publish loses the
	// instruction outright and the caller has to be told.
	"handlers_push.go": "push has no durable backing; it maps the error onto the response",
	// The helper itself — where the best-effort discard is ruled, once,
	// for every other producer.
	"handlers_watch_notify.go": "publishWatchNotification: the single best-effort discard",
}

func TestWatchEventsPublishSitesAreRuled(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	found := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			code := strings.TrimSpace(line)
			// Skip comments — several files DISCUSS this call in prose
			// (including the doc comment this test enforces), and counting
			// those as call sites is the classic way a source scan turns
			// into a liar. See the dead-component sweep lesson: a basename
			// grep counts commentary as liveness.
			if strings.HasPrefix(code, "//") {
				continue
			}
			if strings.Contains(code, "s.watchEvents.Publish(") {
				found[name]++
			}
		}
	}

	// POSITIVE CONTROL. Without it, a scanner that matched nothing —
	// wrong directory, renamed method, a typo in the needle — would report
	// a clean bill of health for a package it never actually read.
	if found["handlers_push.go"] == 0 {
		t.Fatal("scanner found no direct Publish call in handlers_push.go — the scan itself is broken, not the invariant")
	}

	for file, count := range found {
		if _, ok := allowedDirectPublishFiles[file]; !ok {
			t.Errorf("%s calls s.watchEvents.Publish directly (%d site(s)).\n"+
				"Producers layered on a committed store write must use s.publishWatchNotification, "+
				"which rules the best-effort discard in one place (BUG-2699).\n"+
				"If this site genuinely needs to act on the result, add it to allowedDirectPublishFiles with a reason.",
				file, count)
		}
	}
	for file, reason := range allowedDirectPublishFiles {
		if found[file] == 0 {
			t.Errorf("%s is listed as an allowed direct Publish caller (%q) but has none — "+
				"stale allow-list entry, remove it", file, reason)
		}
	}
}
