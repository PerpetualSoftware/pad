package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/spf13/cobra"
)

// TASK-3132 (carved from IDEA-3131): `pad item update`'s success line said
// nothing about the body — "Updated REF" was identical for a one-character
// edit, a full replace and a write that went to an open editor — and the only
// signal for the last case was a stderr line that `| tail -1` and `2>/dev/null`
// both discard. The outcome now rides on the stdout success line.

// outcomeServer answers GET with an item whose stored body is `stored`, and
// PATCH with the item echoing the sent content, carrying the applier-path
// warning when pendingFlush is set.
func outcomeServer(t *testing.T, stored string, pendingFlush bool) {
	t.Helper()
	outcomeServerRef(t, stored, pendingFlush, true)
}

// withRef selects which success-line branch runs: a REF ("Updated DOC-3 …"),
// the common case, or the slug fallback. Both carry the outcome; a test that
// only exercised one left the other's suffix unpinned (a mutant dropping it
// from the ref branch survived the first version of this file).
func outcomeServerRef(t *testing.T, stored string, pendingFlush, withRef bool) {
	t.Helper()
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		it := models.Item{Slug: "doc-3", CollectionSlug: "docs", Title: "Design", Content: stored}
		if withRef {
			n := 3
			it.CollectionPrefix, it.ItemNumber = "DOC", &n
		}
		if r.Method == http.MethodPatch {
			var in struct {
				Content *string `json:"content"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &in)
			if in.Content != nil {
				it.Content = *in.Content
			}
			if pendingFlush {
				it.Warnings = &models.ItemWriteWarnings{ContentOutcome: models.ContentOutcomeAppliedPendingFlush}
			}
		}
		_ = json.NewEncoder(w).Encode(it)
	}))
	formatFlag = "table"
}

func runUpdate(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := updateCmd()
	cmd.SetArgs(args)
	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	return out, execErr
}

func outFirstLine(s string) string {
	return strings.SplitN(s, "\n", 2)[0]
}

func TestItemUpdateOutcome_Replaced(t *testing.T) {
	outcomeServer(t, strings.Repeat("a", 8765), false)
	withStdin(t, strings.Repeat("b", 16958))
	out, err := runUpdate(t, "DOC-3", "--stdin")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	line := outFirstLine(out)
	if !strings.Contains(line, "— body replaced, 8,765 → 16,958 bytes") {
		t.Fatalf("the success line must carry the replace and both sizes; got %q", line)
	}
	if !strings.HasPrefix(line, `Updated DOC-3 "Design" — body replaced`) {
		t.Fatalf("the outcome must be on the SAME line as Updated REF (survives | tail -1 for a one-line output); got %q", line)
	}
}

func TestItemUpdateOutcome_SlugBranchCarriesItToo(t *testing.T) {
	outcomeServerRef(t, "old", false, false)
	out, err := runUpdate(t, "doc-3", "--content", "newer")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if line := outFirstLine(out); !strings.Contains(line, `(doc-3) — body replaced, 3 → 5 bytes`) {
		t.Fatalf("the slug-fallback success line must carry the outcome too; got %q", line)
	}
}

func TestItemUpdateOutcome_PendingFlush(t *testing.T) {
	outcomeServer(t, strings.Repeat("a", 8765), true)
	out, err := runUpdate(t, "DOC-3", "--content", strings.Repeat("b", 16958))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	line := outFirstLine(out)
	for _, want := range []string{"sent to the open editor (16,958 bytes)", "stored copy was 8,765 bytes", "may return that OLD body", "not guaranteed"} {
		if !strings.Contains(line, want) {
			t.Errorf("the applier-path line must say %q; got %q", want, line)
		}
	}
	if strings.Contains(line, "body replaced") {
		t.Errorf("an applier-path write did not replace the stored body and must not say so; got %q", line)
	}
}

func TestItemUpdateOutcome_Cleared(t *testing.T) {
	outcomeServer(t, "four", false)
	out, err := runUpdate(t, "DOC-3", "--clear-content")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if line := outFirstLine(out); !strings.Contains(line, "— body cleared, 4 → 0 bytes") {
		t.Fatalf("a clear must say so on the success line; got %q", line)
	}
}

// CONTROL: an update that does not touch the body must not claim anything
// about it — otherwise the suffix is noise and a reader learns to skip it.
func TestItemUpdateOutcome_NoBodyWriteNoSuffix(t *testing.T) {
	outcomeServer(t, "unchanged body", false)
	out, err := runUpdate(t, "DOC-3", "--title", "Renamed")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if strings.Contains(out, "body ") {
		t.Fatalf("a field-only update must not describe the body; got %q", out)
	}
}

// Contract item 4: JSON stays byte-compatible apart from additive fields —
// the outcome is a TABLE-format line, never injected into the JSON.
func TestItemUpdateOutcome_JSONUnchanged(t *testing.T) {
	outcomeServer(t, "old", false)
	formatFlag = "json"
	out, err := runUpdate(t, "DOC-3", "--content", "new")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var obj map[string]any
	if jerr := json.Unmarshal([]byte(out), &obj); jerr != nil {
		t.Fatalf("--format json stdout must be exactly one JSON object; got %q (%v)", out, jerr)
	}
	if strings.Contains(out, "body replaced") {
		t.Fatalf("the outcome line leaked into JSON output: %q", out)
	}
}

// Sizes are BYTES: "é" is 2 bytes, 1 rune. A rune count would print 3 → 3.
func TestItemUpdateOutcome_SizesAreBytes(t *testing.T) {
	outcomeServer(t, "héllo", false)
	out, err := runUpdate(t, "DOC-3", "--content", "日本")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if line := outFirstLine(out); !strings.Contains(line, "— body replaced, 6 → 6 bytes") {
		t.Fatalf("sizes must be bytes (héllo=6, 日本=6); got %q", line)
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 4: "4", 999: "999", 1000: "1,000", 16958: "16,958", 1234567: "1,234,567"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q; want %q", n, got, want)
		}
	}
}

// --- pad item show: the stale-body notice (contract item 2, lead ruling) ---

func staleShowServer(t *testing.T, body string) {
	t.Helper()
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "backlinks") {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		_ = json.NewEncoder(w).Encode(models.Item{
			Slug: "doc-3", CollectionSlug: "docs", Title: "Design",
			Content: body, ContentState: models.ContentOutcomeAppliedPendingFlush,
		})
	}))
}

func runShow(t *testing.T) string {
	t.Helper()
	cmd := showCmd()
	cmd.SetArgs([]string{"DOC-3"})
	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("show: %v", execErr)
	}
	return out
}

func TestItemShowStale_TableFirstLineOnStdout(t *testing.T) {
	staleShowServer(t, "old body")
	formatFlag = "table"
	out := runShow(t)
	if line := outFirstLine(out); !strings.HasPrefix(line, "⚠ stale body") {
		t.Fatalf("table output must open with the stale-body line on stdout; first line %q", line)
	}
}

// The case the stale line is placed BEFORE the empty-body check for: the stored
// body is empty while the live document holds the real text.
func TestItemShowStale_EmptyStoredBodyStillFlagged(t *testing.T) {
	staleShowServer(t, "")
	formatFlag = "table"
	if line := outFirstLine(runShow(t)); !strings.HasPrefix(line, "⚠ stale body") {
		t.Fatalf("an empty stale body must still be flagged; first line %q", line)
	}
}

// JSON and --agent stay machine output: the table notice must not precede them.
func TestItemShowStale_JSONAndAgentUntouched(t *testing.T) {
	for name, setup := range map[string]func(cmd *cobra.Command){
		"json":  func(*cobra.Command) { formatFlag = "json" },
		"agent": func(cmd *cobra.Command) { formatFlag = "table"; _ = cmd.Flags().Set("agent", "true") },
	} {
		t.Run(name, func(t *testing.T) {
			staleShowServer(t, "old body")
			cmd := showCmd()
			setup(cmd)
			cmd.SetArgs([]string{"DOC-3"})
			var execErr error
			out := captureStdout(t, func() { execErr = cmd.Execute() })
			if execErr != nil {
				t.Fatalf("show: %v", execErr)
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(out), &obj); err != nil {
				t.Fatalf("stdout must be exactly one JSON object; got %q (%v)", out, err)
			}
		})
	}
}

// The ruled asymmetry: markdown stdout stays the body VERBATIM, because it is
// written back with `update --stdin` and a notice there would become content.
func TestItemShowStale_MarkdownStdoutIsBodyOnly(t *testing.T) {
	staleShowServer(t, "old body\n")
	formatFlag = "markdown"
	if out := runShow(t); out != "old body\n" {
		t.Fatalf("markdown stdout must be the body verbatim even when stale; got %q", out)
	}
}

// CONTROL: a body that is not stale gets no notice, so the table line above is
// the marker's doing.
func TestItemShowNotStale_NoLine(t *testing.T) {
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "backlinks") {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		_ = json.NewEncoder(w).Encode(models.Item{Slug: "doc-3", CollectionSlug: "docs", Title: "Design", Content: "fresh"})
	}))
	formatFlag = "table"
	if out := runShow(t); strings.Contains(out, "stale body") {
		t.Fatalf("a fresh body must not be marked stale; got %q", out)
	}
}
