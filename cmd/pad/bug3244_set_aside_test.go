package main

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3244 ruling 1 at the CLI: every stale-body notice that names a remedy
// branches on the value. For set-aside edits it names `pad item set-aside`
// and never says a tab will catch the body up. Each case renders BOTH values.

func setAsideItem(state string) *models.Item {
	return &models.Item{Slug: "fix-oauth", Ref: "TASK-5", ContentState: state}
}

func TestSetAsideStaleNoticesNameTheRightRemedy(t *testing.T) {
	type door struct {
		name string
		run  func(*models.Item) string
	}
	doors := []door{
		{"item show table line", func(it *models.Item) string { return captureStdout(t, func() { printStaleBodyLine(it) }) }},
		{"item show markdown warning", func(it *models.Item) string { return captureStderr(t, func() { warnContentStale(it) }) }},
		{"item edit refusal", func(it *models.Item) string { return staleEditRefusal(it).Error() }},
		{"item edit --force warning", func(it *models.Item) string { return captureStderr(t, func() { warnStaleEditSeed(it) }) }},
		{"playbook body warning", func(it *models.Item) string {
			return captureStderr(t, func() { warnPlaybookBodyStale(it.ContentState) })
		}},
	}
	for _, d := range doors {
		t.Run(d.name, func(t *testing.T) {
			setAside := d.run(setAsideItem(models.ContentStateSetAside))
			pending := d.run(setAsideItem(models.ContentStatePendingFlush))
			if setAside == "" {
				t.Fatal("set-aside item printed nothing")
			}
			if !strings.Contains(setAside, "earlier editor version") && !strings.Contains(setAside, "set aside") {
				t.Errorf("set-aside notice does not name the upgrade: %q", setAside)
			}
			for _, promise := range []string{"flushes", "catches up", "browser tab so", "Open the item in a browser"} {
				if strings.Contains(setAside, promise) {
					t.Errorf("set-aside notice promises a tab remedy (%q): %q", promise, setAside)
				}
			}
			if pending == "" || pending == setAside {
				t.Errorf("pending notice missing or identical to the set-aside one: %q", pending)
			}
		})
	}
}
