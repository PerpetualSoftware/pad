package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// ── fixtures ─────────────────────────────────────────────────────────────

// fullPreflight is a preview where EVERY bucket and EVERY warning is
// non-trivial. The rendering tests assert against it so a dropped line is a
// failure rather than an omission nobody notices.
func fullPreflight() *cli.ItemCopyPreflight {
	return &cli.ItemCopyPreflight{
		Source: cli.ItemCopyPreflightSource{
			WorkspaceSlug: "docapp", CollectionSlug: "ideas",
			Ref: "IDEA-12", Slug: "cross-workspace-copy", Title: "Cross-workspace copy",
		},
		Destination: cli.ItemCopyPreflightDestination{
			WorkspaceSlug: "pad-web", WorkspaceName: "Pad Web",
			CollectionSlug: "tasks", CollectionName: "Tasks",
		},
		ArchiveSource: true,
		Valid:         false,
		Fields: cli.ItemCopyPreflightFields{
			Carried: []cli.ItemCopyPreflightCarried{
				{Key: "status", Label: "Status", Type: "select", Value: "todo", From: "default"},
				{Key: "points", Label: "Points", Type: "number", Value: 3.0, From: "migrated"},
				{Key: "notes", Type: "text", Value: "", From: "override"},
			},
			Dropped: []cli.ItemCopyPreflightDropped{
				{Key: "impact", Label: "Impact", Kind: "field", Reason: "no_target_field"},
				{Key: "assignee", Label: "Assignee", Kind: "assignment", Reason: "assignee_not_a_member"},
				{Key: "agent_role", Label: "Agent role", Kind: "assignment", Reason: "agent_role_not_portable"},
			},
			NeedsValue: []cli.ItemCopyPreflightNeedsValue{
				{Key: "priority", Label: "Priority", Type: "select", Options: []string{"low", "high"}, Required: true, Reason: "missing_required"},
				{Key: "size", Label: "Size", Type: "select", Required: false, Reason: "invalid_value", Message: `"xl" is not one of s, m, l`},
			},
		},
		Warnings: cli.ItemCopyPreflightWarnings{
			ChildCount:           2,
			ChildrenOrphaned:     true,
			DroppedParent:        true,
			OutgoingLinks:        map[string]int{"related": 1, "blocks": 2},
			IncomingLinks:        map[string]int{"blocked-by": 1},
			DroppedAssignee:      true,
			DroppedAgentRole:     true,
			AttachmentCount:      3,
			AttachmentBytes:      1258291,
			UnresolvableRefCount: 1,
		},
	}
}

// emptyPreflight is the everything-is-zero case. Its whole job is to prove
// "nothing to report" renders as an explicit zero rather than as silence.
func emptyPreflight() *cli.ItemCopyPreflight {
	return &cli.ItemCopyPreflight{
		Source: cli.ItemCopyPreflightSource{
			WorkspaceSlug: "docapp", CollectionSlug: "ideas", Ref: "IDEA-12", Slug: "x", Title: "X",
		},
		Destination: cli.ItemCopyPreflightDestination{
			WorkspaceSlug: "pad-web", WorkspaceName: "Pad Web", CollectionSlug: "tasks", CollectionName: "Tasks",
		},
		Valid: true,
		Fields: cli.ItemCopyPreflightFields{
			Carried:    []cli.ItemCopyPreflightCarried{},
			Dropped:    []cli.ItemCopyPreflightDropped{},
			NeedsValue: []cli.ItemCopyPreflightNeedsValue{},
		},
		Warnings: cli.ItemCopyPreflightWarnings{
			OutgoingLinks: map[string]int{},
			IncomingLinks: map[string]int{},
		},
	}
}

// recordingDeps captures what the command sent where, and fails the test if
// the mutating call happens when it must not.
type recordingDeps struct {
	t              *testing.T
	preflightCalls []cli.ItemCopyRequest
	copyCalls      []cli.ItemCopyRequest
	schemaCalls    [][2]string

	preflight    *cli.ItemCopyPreflight
	preflightRaw json.RawMessage
	preflightErr error
	// preflightFn, when set, computes the preview FROM the request — the
	// way the real endpoint does. Takes precedence over preflight.
	preflightFn func(cli.ItemCopyRequest) *cli.ItemCopyPreflight

	result    *cli.ItemCopyResult
	resultRaw json.RawMessage
	copyErr   error

	schema models.CollectionSchema
	// forbidCopy makes any mutating call a test failure.
	forbidCopy bool
}

func (d *recordingDeps) deps() itemCopyDeps {
	return itemCopyDeps{
		Schema: func(ws, coll string) models.CollectionSchema {
			d.schemaCalls = append(d.schemaCalls, [2]string{ws, coll})
			return d.schema
		},
		Preflight: func(req cli.ItemCopyRequest) (*cli.ItemCopyPreflight, json.RawMessage, error) {
			d.preflightCalls = append(d.preflightCalls, req)
			pre := d.preflight
			if d.preflightFn != nil {
				pre = d.preflightFn(req)
			}
			raw := d.preflightRaw
			if raw == nil && pre != nil {
				b, _ := json.Marshal(pre)
				raw = b
			}
			return pre, raw, d.preflightErr
		},
		Copy: func(req cli.ItemCopyRequest) (*cli.ItemCopyResult, json.RawMessage, error) {
			if d.forbidCopy {
				d.t.Errorf("MUTATING COPY SENT — this path must send no mutating request. req=%+v", req)
			}
			d.copyCalls = append(d.copyCalls, req)
			return d.result, d.resultRaw, d.copyErr
		},
	}
}

func baseOpts() itemCopyOptions {
	return itemCopyOptions{
		Ref: "IDEA-12", TargetWorkspace: "pad-web", TargetCollection: "tasks", Format: "table",
	}
}

// ── dry-run rendering: every bucket, every warning ───────────────────────

func TestRunItemCopy_DryRunRendersEveryBucketAndWarning(t *testing.T) {
	d := &recordingDeps{t: t, preflight: fullPreflight(), forbidCopy: true}
	opts := baseOpts()
	opts.DryRun = true
	opts.ArchiveSource = true

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
		t.Fatalf("runItemCopy: %v", err)
	}
	got := out.String()

	want := []string{
		// header
		"Copy preview — dry run. Nothing was changed.",
		"docapp/ideas  IDEA-12  \"Cross-workspace copy\"",
		"pad-web/tasks  (Pad Web / Tasks)",
		"move — copy, then archive the source",

		// the three contract bucket names, with counts
		"carried (3)",
		"dropped (3)",
		"needs_value (2)",

		// carried rows, including the provenance of each value
		`status               (Status, select) = "todo"  [from default]`,
		"points               (Points, number) = 3  [from migrated]",
		`notes                (text) = ""  [from override]`,

		// dropped rows: kind AND reason, for schema fields and the DR-8 pair
		"impact               (Impact) (field) — the destination collection has no such field",
		"assignee             (Assignee) (assignment) — the assignee is not a member of the destination workspace",
		"agent_role           (Agent role) (assignment) — agent roles are workspace-local and never carry",

		// needs_value rows: required-ness, reason, options and the
		// validator's own message
		"priority             (Priority, select) required — required, with no value to carry and no default",
		`options: "low", "high"`,
		"size                 (Size, select) optional — the carried value is not valid in the destination",
		`"xl" is not one of s, m, l`,

		// DR-15's full warning set — all nine lines
		"child items (not copied)         2",
		"children orphaned by the move    yes",
		"parent dropped                   yes",
		"outgoing links dropped           3 (blocks 2, related 1)",
		"incoming links dropped           1 (blocked-by 1)",
		"assignee dropped                 yes",
		"agent role dropped               yes",
		"attachments cloned               3 (1.2 MiB, 1258291 bytes)",
		"unresolvable attachment refs     1",

		"2 fields still need a value",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("dry-run output missing %q\n--- got ---\n%s", w, got)
		}
	}
	if errOut.Len() != 0 {
		t.Errorf("dry run should write nothing to stderr; got %q", errOut.String())
	}
	if len(d.preflightCalls) != 1 {
		t.Fatalf("expected exactly one preflight call; got %d", len(d.preflightCalls))
	}
	if !d.preflightCalls[0].ArchiveSource {
		t.Error("--archive-source must reach the preflight so it can weight children_orphaned")
	}
}

// The zero case is the one a careless renderer gets wrong: an omitted line
// makes "no attachments" look identical to "this CLI does not report
// attachments".
func TestRunItemCopy_DryRunRendersZeroAndEmptyExplicitly(t *testing.T) {
	d := &recordingDeps{t: t, preflight: emptyPreflight(), forbidCopy: true}
	opts := baseOpts()
	opts.DryRun = true

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
		t.Fatalf("runItemCopy: %v", err)
	}
	got := out.String()

	want := []string{
		"copy — the source is left untouched",
		"carried (0)",
		"dropped (0)",
		"needs_value (0)",
		"child items (not copied)         0",
		"children orphaned by the move    no",
		"parent dropped                   no",
		"outgoing links dropped           0",
		"incoming links dropped           0",
		"assignee dropped                 no",
		"agent role dropped               no",
		"attachments cloned               0",
		"unresolvable attachment refs     0",
		"The field mapping is complete. Re-run without --dry-run to perform the copy.",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q\n--- got ---\n%s", w, got)
		}
	}
	// Each empty bucket says so out loud.
	if n := strings.Count(got, "(none)"); n != 3 {
		t.Errorf("expected each of the three empty buckets to render (none); got %d\n%s", n, got)
	}
}

// A nil link map (a server that omitted the key entirely) must still render
// a zero, never a blank.
func TestItemCopyLinkSummary(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]int
		want string
	}{
		{"nil", nil, "0"},
		{"empty", map[string]int{}, "0"},
		{"one", map[string]int{"blocks": 2}, "2 (blocks 2)"},
		{"sorted", map[string]int{"related": 1, "blocks": 2, "a": 3}, "6 (a 3, blocks 2, related 1)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := itemCopyLinkSummary(tc.in); got != tc.want {
				t.Errorf("itemCopyLinkSummary(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Codex round 2. Schema keys, labels and option values are user-authored
// and travel through the server verbatim, so the renderer must not let them
// forge structure: an option containing ", " has to stay ONE option (the
// user is about to retype it into --field), and a newline anywhere must not
// be able to invent a table row.
func TestRunItemCopy_HostileSchemaStringsCannotForgeStructure(t *testing.T) {
	pre := emptyPreflight()
	pre.Valid = false
	pre.Fields.NeedsValue = []cli.ItemCopyPreflightNeedsValue{
		{Key: "state", Label: "State", Type: "select", Required: true, Reason: "missing_required",
			Options: []string{"ready, waiting", "done"}},
		{Key: "forged\n  fake_key             (Text) required — invented", Required: true, Reason: "missing_required"},
	}
	d := &recordingDeps{t: t, preflight: pre, forbidCopy: true}
	opts := baseOpts()
	opts.DryRun = true

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
		t.Fatalf("runItemCopy: %v", err)
	}
	got := out.String()

	// Two options, one of which contains a comma — they must be
	// distinguishable from three plain options.
	if !strings.Contains(got, `options: "ready, waiting", "done"`) {
		t.Errorf("a comma inside an option must not read as a separator:\n%s", got)
	}
	// The forged key must be escaped onto a single line — no output line
	// may BEGIN with the invented row.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "fake_key") {
			t.Errorf("a newline in a schema key forged a table row: %q\n%s", line, got)
		}
	}
	if !strings.Contains(got, `"forged\n  fake_key`) {
		t.Errorf("expected the control character to be escaped:\n%s", got)
	}
	// needs_value still says 2, not 3.
	if !strings.Contains(got, "needs_value (2)") {
		t.Errorf("bucket count should be the server's:\n%s", got)
	}
}

// Codex round 6. A needs_value entry with an empty key cannot be supplied
// with --field (the parser rejects `--field =value`), so the refusal must
// not print a command that cannot work.
func TestRunItemCopy_RefusalHandlesAnEmptyFieldKey(t *testing.T) {
	pre := emptyPreflight()
	pre.Valid = false
	pre.Fields.NeedsValue = []cli.ItemCopyPreflightNeedsValue{
		{Key: "", Label: "Nameless", Required: true, Reason: "missing_required"},
		{Key: "priority", Required: true, Reason: "missing_required"},
	}
	d := &recordingDeps{t: t, preflight: pre, forbidCopy: true}

	var out, errOut bytes.Buffer
	if err := runItemCopy(baseOpts(), d.deps(), &out, &errOut); err == nil {
		t.Fatal("expected a refusal")
	}
	stderr := errOut.String()
	if strings.Contains(stderr, "--field =<value>") {
		t.Errorf("must not print a --field the parser would reject:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--field priority=<value>") {
		t.Errorf("the resolvable field must still be offered:\n%s", stderr)
	}
	if !strings.Contains(stderr, "empty key") {
		t.Errorf("the unresolvable entry must be called out:\n%s", stderr)
	}
}

// When EVERY entry is unnamed there is nothing to add, so the "Add:" line
// must not be printed at all.
func TestRunItemCopy_RefusalWithOnlyEmptyKeys(t *testing.T) {
	pre := emptyPreflight()
	pre.Valid = false
	pre.Fields.NeedsValue = []cli.ItemCopyPreflightNeedsValue{{Key: "  ", Required: true, Reason: "missing_required"}}
	d := &recordingDeps{t: t, preflight: pre, forbidCopy: true}

	var out, errOut bytes.Buffer
	if err := runItemCopy(baseOpts(), d.deps(), &out, &errOut); err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(errOut.String(), "Add:") {
		t.Errorf("nothing can be added; the Add line must be omitted:\n%s", errOut.String())
	}
}

func TestItemCopyLineAndList(t *testing.T) {
	// The common case is passed through untouched — quoting everything
	// would make ordinary output worse for no gain.
	if got := itemCopyLine("priority"); got != "priority" {
		t.Errorf("itemCopyLine(%q) = %q", "priority", got)
	}
	if got := itemCopyLine("Due date"); got != "Due date" {
		t.Errorf("itemCopyLine kept a space wrongly: %q", got)
	}
	// Control characters force the quoted form.
	for _, s := range []string{"a\nb", "a\tb", "a\rb", "a\x1b[2Jb", "a\x7fb"} {
		if got := itemCopyLine(s); !strings.HasPrefix(got, `"`) {
			t.Errorf("itemCopyLine(%q) = %q, want it quoted", s, got)
		}
	}
	// Lists always quote, because the separator is the ambiguity.
	if got := itemCopyList([]string{"a, b", "c"}); got != `"a, b", "c"` {
		t.Errorf("itemCopyList = %q", got)
	}
	if got := itemCopyList(nil); got != "" {
		t.Errorf("itemCopyList(nil) = %q", got)
	}
}

// A reason code this CLI has never heard of must reach the user verbatim
// rather than being flattened into a generic phrase or dropped.
func TestItemCopyReason_UnknownCodeSurfacesVerbatim(t *testing.T) {
	if got := itemCopyReason(itemCopyDroppedReasons, "some_future_reason"); got != "some_future_reason" {
		t.Errorf("unknown reason rendered as %q", got)
	}
	if got := itemCopyReason(itemCopyNeedsValueReasons, ""); got != "(no reason given)" {
		t.Errorf("empty reason rendered as %q", got)
	}
	if got := itemCopyReason(itemCopyDroppedReasons, "incompatible_type"); got == "incompatible_type" {
		t.Error("known reason should be translated to a sentence")
	}
}

// --archive-source on a dry run reports the consequence and performs none
// of it.
func TestRunItemCopy_DryRunArchiveSourceReportsWithoutPerforming(t *testing.T) {
	pre := fullPreflight()
	pre.Fields.NeedsValue = nil // otherwise the refusal message dominates
	d := &recordingDeps{t: t, preflight: pre, forbidCopy: true}
	opts := baseOpts()
	opts.DryRun = true
	opts.ArchiveSource = true

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
		t.Fatalf("runItemCopy: %v", err)
	}
	got := out.String()
	for _, w := range []string{
		"move — copy, then archive the source",
		"children orphaned by the move    yes",
		"Re-run without --dry-run to perform the move.",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q\n--- got ---\n%s", w, got)
		}
	}
	if len(d.copyCalls) != 0 {
		t.Fatalf("a dry run must perform no copy; got %d", len(d.copyCalls))
	}
}

// ── the incomplete-relationships marker (TASK-2369) ──────────────────────

// TestRunItemCopy_DryRunPartialRelationshipsAreQualified — a restricted
// caller must not read "0 / no / no / 0 / 0" and conclude the item has no
// relationships when the server has told them the counts are a floor.
func TestRunItemCopy_DryRunPartialRelationshipsAreQualified(t *testing.T) {
	// The MOVE path, where all five counters are incomplete. (The plain
	// copy differs on exactly one line — see
	// TestRunItemCopy_OrphanLineIsQualifiedOnlyOnTheMovePath.)
	pre := emptyPreflight()
	pre.ArchiveSource = true
	pre.Warnings.RelationshipsPartial = true
	d := &recordingDeps{t: t, preflight: pre, forbidCopy: true}
	opts := baseOpts()
	opts.DryRun = true
	opts.ArchiveSource = true

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
		t.Fatalf("runItemCopy: %v", err)
	}
	got := out.String()

	for _, w := range []string{
		// The five ACL-filtered counters carry the qualifier …
		"child items (not copied)         0  (visible to you only)",
		"children orphaned by the move    no  (visible to you only)",
		"parent dropped                   no  (visible to you only)",
		"outgoing links dropped           0  (visible to you only)",
		"incoming links dropped           0  (visible to you only)",
		// … and the block says, in words, what to do about it.
		"The relationship counts above are INCOMPLETE.",
		"read each qualified line as a floor, not a total.",
		"Ask someone with full access",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q\n--- got ---\n%s", w, got)
		}
	}
	// The warnings that are NOT ACL-filtered must stay unqualified —
	// blanket-qualifying the block would be its own kind of lie.
	for _, w := range []string{
		"assignee dropped                 no\n",
		"agent role dropped               no\n",
		"attachments cloned               0\n",
		"unresolvable attachment refs     0\n",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("a non-relationship warning was qualified or dropped: missing %q\n--- got ---\n%s", w, got)
		}
	}
	// It must not disclose how much is hidden — there is no count to
	// render, and inventing an adjective for one is the same mistake.
	for _, forbidden := range []string{"1 hidden", "hidden item", "hidden children (", "at least"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("output hints at the hidden volume via %q\n--- got ---\n%s", forbidden, got)
		}
	}
}

// The move path says the one extra thing that matters — that whatever is
// hidden gets orphaned unlisted — WITHOUT claiming any of it is a child.
// The marker is type-agnostic; it fires just as readily for a hidden
// parent or a hidden dependency edge with no hidden child anywhere, and a
// sentence that asserts hidden children would put a fact in the user's
// head the server never stated (Codex round 2).
func TestRunItemCopy_DryRunPartialOnTheMovePathDoesNotAssertHiddenChildren(t *testing.T) {
	pre := emptyPreflight()
	pre.ArchiveSource = true
	pre.Warnings.RelationshipsPartial = true
	d := &recordingDeps{t: t, preflight: pre, forbidCopy: true}
	opts := baseOpts()
	opts.DryRun = true
	opts.ArchiveSource = true

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
		t.Fatalf("runItemCopy: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Any of them that are children will be orphaned by this move, unlisted.") {
		t.Errorf("the move path must name the orphaning consequence\n--- got ---\n%s", got)
	}
	// The assertive forms. Each states as fact something the marker does
	// not carry.
	for _, forbidden := range []string{
		"Those hidden children", "hidden children will", "The hidden children",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the move-path line asserts hidden CHILDREN exist via %q — the marker is type-agnostic\n--- got ---\n%s",
				forbidden, got)
		}
	}

	// And the copy path (no archive) must not mention orphaning at all —
	// there is nothing to orphan.
	pre2 := emptyPreflight()
	pre2.Warnings.RelationshipsPartial = true
	d2 := &recordingDeps{t: t, preflight: pre2, forbidCopy: true}
	opts2 := baseOpts()
	opts2.DryRun = true
	var out2, errOut2 bytes.Buffer
	if err := runItemCopy(opts2, d2.deps(), &out2, &errOut2); err != nil {
		t.Fatalf("runItemCopy: %v", err)
	}
	if strings.Contains(out2.String(), "orphaned by this move") {
		t.Errorf("a plain copy must not talk about orphaning\n--- got ---\n%s", out2.String())
	}
}

// TestRunItemCopy_DryRunUnmarkedRelationshipsStayClean is the other half:
// the common case must read exactly as it did before TASK-2369, or the
// qualifier becomes noise everyone learns to ignore.
func TestRunItemCopy_DryRunUnmarkedRelationshipsStayClean(t *testing.T) {
	for _, tc := range []struct {
		name string
		pre  *cli.ItemCopyPreflight
	}{
		{"nothing to report", emptyPreflight()},
		{"a full, fully visible graph", func() *cli.ItemCopyPreflight {
			p := fullPreflight()
			p.Fields.NeedsValue = nil
			p.ArchiveSource = false
			return p
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.pre.Warnings.RelationshipsPartial {
				t.Fatal("fixture bug: this case must NOT be marked partial")
			}
			d := &recordingDeps{t: t, preflight: tc.pre, forbidCopy: true}
			opts := baseOpts()
			opts.DryRun = true

			var out, errOut bytes.Buffer
			if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
				t.Fatalf("runItemCopy: %v", err)
			}
			got := out.String()
			for _, forbidden := range []string{"visible to you only", "INCOMPLETE", "Ask someone with full access"} {
				if strings.Contains(got, forbidden) {
					t.Errorf("the unmarked common case rendered the partial caveat %q\n--- got ---\n%s", forbidden, got)
				}
			}
		})
	}
}

// `children orphaned by the move` is COMPLETE on a plain copy whatever is
// hidden — a copy archives nothing, so no child can be orphaned by it.
// Qualifying it there would invent a risk the operation does not carry,
// and would contradict the explanatory block two lines below, which
// correctly scopes orphaning to a move (Codex round 5).
func TestRunItemCopy_OrphanLineIsQualifiedOnlyOnTheMovePath(t *testing.T) {
	render := func(t *testing.T, archive bool) string {
		t.Helper()
		pre := emptyPreflight()
		pre.ArchiveSource = archive
		pre.Warnings.RelationshipsPartial = true
		d := &recordingDeps{t: t, preflight: pre, forbidCopy: true}
		opts := baseOpts()
		opts.DryRun = true
		opts.ArchiveSource = archive
		var out, errOut bytes.Buffer
		if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
			t.Fatalf("runItemCopy: %v", err)
		}
		return out.String()
	}

	copyOut := render(t, false)
	if !strings.Contains(copyOut, "children orphaned by the move    no\n") {
		t.Errorf("a plain copy must leave the orphan line unqualified\n--- got ---\n%s", copyOut)
	}
	// The other four still carry it — they are incomplete either way.
	for _, w := range []string{
		"child items (not copied)         0  (visible to you only)",
		"parent dropped                   no  (visible to you only)",
		"outgoing links dropped           0  (visible to you only)",
		"incoming links dropped           0  (visible to you only)",
	} {
		if !strings.Contains(copyOut, w) {
			t.Errorf("a plain copy dropped a qualifier it should keep: missing %q\n--- got ---\n%s", w, copyOut)
		}
	}

	moveOut := render(t, true)
	if !strings.Contains(moveOut, "children orphaned by the move    no  (visible to you only)") {
		t.Errorf("a move MUST qualify the orphan line — hidden children are stranded unlisted\n--- got ---\n%s", moveOut)
	}
}

// --dry-run is optional, and the destructive path is the one that matters.
// A restricted user who goes straight to the mutation must still be told
// the relationship picture was incomplete — on the one run where it is too
// late to reconsider (Codex round 5).
func TestRunItemCopy_MutatingPathCarriesThePartialMarker(t *testing.T) {
	run := func(t *testing.T, format string, archive bool) (string, string) {
		t.Helper()
		pre := emptyPreflight()
		pre.ArchiveSource = archive
		pre.Warnings.RelationshipsPartial = true
		d := &recordingDeps{
			t: t, preflight: pre,
			result: &cli.ItemCopyResult{
				Source: cli.ItemCopyResultSource{
					WorkspaceSlug: "docapp", CollectionSlug: "ideas", Ref: "IDEA-12", Slug: "x",
					Title: "X", Archived: archive,
				},
				Destination: cli.ItemCopyResultDestination{
					WorkspaceSlug: "pad-web", CollectionSlug: "tasks", Ref: "TASK-9", Slug: "x",
				},
				ArchiveSource: archive,
				Warnings:      cli.ItemCopyResultWarnings{DroppedFields: []string{}},
			},
			resultRaw: json.RawMessage(`{"ok":true}`),
		}
		opts := baseOpts()
		opts.Format = format
		opts.ArchiveSource = archive
		var out, errOut bytes.Buffer
		if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
			t.Fatalf("runItemCopy: %v", err)
		}
		if len(d.copyCalls) != 1 {
			t.Fatalf("expected exactly one mutating call; got %d", len(d.copyCalls))
		}
		return out.String(), errOut.String()
	}

	t.Run("table, copy", func(t *testing.T) {
		_, stderr := run(t, "table", false)
		if !strings.Contains(stderr, "some of this item's relationships are on items you do not have access to") {
			t.Errorf("the mutating path swallowed the marker\n--- stderr ---\n%s", stderr)
		}
		if !strings.Contains(stderr, "Ask someone with full access") {
			t.Errorf("the note must stay actionable\n--- stderr ---\n%s", stderr)
		}
		// Nothing was archived, so nothing was orphaned.
		if strings.Contains(stderr, "orphaned") {
			t.Errorf("a plain copy orphans nothing\n--- stderr ---\n%s", stderr)
		}
	})

	t.Run("table, move", func(t *testing.T) {
		_, stderr := run(t, "table", true)
		if !strings.Contains(stderr, "are now orphaned in the source workspace") {
			t.Errorf("a completed move must say hidden children are now orphaned\n--- stderr ---\n%s", stderr)
		}
	})

	t.Run("json keeps stdout byte-exact", func(t *testing.T) {
		stdout, stderr := run(t, "json", true)
		// PrintRawJSON re-indents but does not re-key: the note must not
		// have found its way into the document.
		var doc map[string]any
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
			t.Fatalf("stdout is not the server's JSON: %v\n%s", err, stdout)
		}
		if len(doc) != 1 || doc["ok"] != true {
			t.Errorf("--format json stdout must stay the server's response; got %q", stdout)
		}
		if strings.Contains(stdout, "access") {
			t.Errorf("the advisory note leaked into the machine-readable body:\n%s", stdout)
		}
		if !strings.Contains(stderr, "you do not have access to") {
			t.Errorf("the note belongs on stderr for json callers too\n--- stderr ---\n%s", stderr)
		}
	})

	// The committed-but-unreported branch returns nil — it is a SUCCESS —
	// so it owes the same advisory. It is also the branch where the user
	// has the least information already (Codex round 6).
	t.Run("committed but unreported", func(t *testing.T) {
		pre := emptyPreflight()
		pre.ArchiveSource = true
		pre.Warnings.RelationshipsPartial = true
		d := &recordingDeps{
			t: t, preflight: pre,
			copyErr: fmt.Errorf("read body: %w", cli.ErrCopyCommitted),
		}
		opts := baseOpts()
		opts.ArchiveSource = true
		var out, errOut bytes.Buffer
		if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
			t.Fatalf("a committed copy must exit zero: %v", err)
		}
		stderr := errOut.String()
		if !strings.Contains(stderr, "The copy SUCCEEDED, but its result could not be read") {
			t.Fatalf("fixture is not on the committed-but-unreported branch:\n%s", stderr)
		}
		if !strings.Contains(stderr, "you do not have access to") {
			t.Errorf("the committed-but-unreported success path swallowed the marker\n--- stderr ---\n%s", stderr)
		}
		if !strings.Contains(stderr, "are now orphaned in the source workspace") {
			t.Errorf("a committed MOVE must still name the orphaning\n--- stderr ---\n%s", stderr)
		}
	})

	// The outcome-unknown branch must NOT get it: nothing is known to have
	// committed, and that message has one job — do not re-run.
	t.Run("outcome unknown stays focused", func(t *testing.T) {
		pre := emptyPreflight()
		pre.Warnings.RelationshipsPartial = true
		d := &recordingDeps{
			t: t, preflight: pre,
			copyErr: fmt.Errorf("timeout: %w", cli.ErrCopyOutcomeUnknown),
		}
		var out, errOut bytes.Buffer
		if err := runItemCopy(baseOpts(), d.deps(), &out, &errOut); err == nil {
			t.Fatal("an unknown outcome must exit non-zero")
		}
		if strings.Contains(errOut.String(), "you do not have access to") {
			t.Errorf("the unknown-outcome message must stay focused on 'do not re-run'\n--- stderr ---\n%s", errOut.String())
		}
	})

	t.Run("unmarked runs say nothing", func(t *testing.T) {
		pre := emptyPreflight()
		d := &recordingDeps{
			t: t, preflight: pre,
			result: &cli.ItemCopyResult{
				Source:      cli.ItemCopyResultSource{WorkspaceSlug: "docapp", CollectionSlug: "ideas", Slug: "x"},
				Destination: cli.ItemCopyResultDestination{WorkspaceSlug: "pad-web", CollectionSlug: "tasks", Slug: "x"},
				Warnings:    cli.ItemCopyResultWarnings{DroppedFields: []string{}},
			},
		}
		var out, errOut bytes.Buffer
		if err := runItemCopy(baseOpts(), d.deps(), &out, &errOut); err != nil {
			t.Fatalf("runItemCopy: %v", err)
		}
		// Empty, not "missing one phrase": a regression that printed any
		// OTHER line of the advisory would slip past a substring check.
		if errOut.String() != "" {
			t.Errorf("the common case must print nothing on stderr at all\n--- stderr ---\n%q", errOut.String())
		}
	})
}

// ── --format json fidelity ───────────────────────────────────────────────

func TestRunItemCopy_DryRunJSONIsTheServerResponse(t *testing.T) {
	const payload = `{"valid":false,"fields":{"carried":[],"dropped":[],"needs_value":[{"key":"priority","required":true,"reason":"missing_required"}]},` +
		`"warnings":{"attachment_bytes":9007199254740993,"outgoing_links":{},"incoming_links":{}},"future_field":"kept"}`
	var pre cli.ItemCopyPreflight
	if err := json.Unmarshal([]byte(payload), &pre); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	d := &recordingDeps{t: t, preflight: &pre, preflightRaw: json.RawMessage(payload), forbidCopy: true}
	opts := baseOpts()
	opts.DryRun = true
	opts.Format = "json"

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
		t.Fatalf("runItemCopy: %v", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, out.Bytes()); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if compact.String() != payload {
		t.Errorf("--format json altered the server's response.\n got: %s\nwant: %s", compact.String(), payload)
	}
}

// The acceptance criterion asks for each bucket and each warning to be
// asserted in --format json as well as in the table, so this walks the SAME
// full fixture as TestRunItemCopy_DryRunRendersEveryBucketAndWarning and
// checks every field arrives with the right value and the right JSON name.
// The test above pins byte-fidelity; this one pins coverage.
func TestRunItemCopy_DryRunJSONCarriesEveryBucketAndWarning(t *testing.T) {
	pre := fullPreflight()
	pre.ArchiveSource = true
	raw, err := json.Marshal(pre)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	d := &recordingDeps{t: t, preflight: pre, preflightRaw: raw, forbidCopy: true}
	opts := baseOpts()
	opts.DryRun = true
	opts.ArchiveSource = true
	opts.Format = "json"

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
		t.Fatalf("runItemCopy: %v", err)
	}

	var doc struct {
		ArchiveSource bool `json:"archive_source"`
		Valid         bool `json:"valid"`
		Fields        struct {
			Carried []struct {
				Key   string `json:"key"`
				From  string `json:"from"`
				Value any    `json:"value"`
			} `json:"carried"`
			Dropped []struct {
				Key    string `json:"key"`
				Kind   string `json:"kind"`
				Reason string `json:"reason"`
			} `json:"dropped"`
			NeedsValue []struct {
				Key      string   `json:"key"`
				Required bool     `json:"required"`
				Reason   string   `json:"reason"`
				Options  []string `json:"options"`
				Message  string   `json:"message"`
			} `json:"needs_value"`
		} `json:"fields"`
		Warnings struct {
			ChildCount           *int           `json:"child_count"`
			ChildrenOrphaned     *bool          `json:"children_orphaned"`
			DroppedParent        *bool          `json:"dropped_parent"`
			OutgoingLinks        map[string]int `json:"outgoing_links"`
			IncomingLinks        map[string]int `json:"incoming_links"`
			DroppedAssignee      *bool          `json:"dropped_assignee"`
			DroppedAgentRole     *bool          `json:"dropped_agent_role"`
			AttachmentCount      *int           `json:"attachment_count"`
			AttachmentBytes      *int64         `json:"attachment_bytes"`
			UnresolvableRefCount *int           `json:"unresolvable_ref_count"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not the preflight JSON: %v\n%s", err, out.String())
	}

	// The move flag and the gate.
	if !doc.ArchiveSource {
		t.Error("archive_source did not survive to JSON")
	}
	if doc.Valid {
		t.Error("valid should be false on this fixture")
	}

	// All three contract buckets, with their contents.
	if len(doc.Fields.Carried) != 3 || doc.Fields.Carried[0].Key != "status" || doc.Fields.Carried[0].From != "default" {
		t.Errorf("carried bucket wrong: %+v", doc.Fields.Carried)
	}
	if len(doc.Fields.Dropped) != 3 {
		t.Fatalf("dropped bucket wrong: %+v", doc.Fields.Dropped)
	}
	for i, want := range []struct{ key, kind, reason string }{
		{"impact", "field", "no_target_field"},
		{"assignee", "assignment", "assignee_not_a_member"},
		{"agent_role", "assignment", "agent_role_not_portable"},
	} {
		got := doc.Fields.Dropped[i]
		if got.Key != want.key || got.Kind != want.kind || got.Reason != want.reason {
			t.Errorf("dropped[%d] = %+v, want %+v", i, got, want)
		}
	}
	if len(doc.Fields.NeedsValue) != 2 {
		t.Fatalf("needs_value bucket wrong: %+v", doc.Fields.NeedsValue)
	}
	if nv := doc.Fields.NeedsValue[0]; nv.Key != "priority" || !nv.Required || nv.Reason != "missing_required" ||
		len(nv.Options) != 2 {
		t.Errorf("needs_value[0] = %+v", nv)
	}
	if nv := doc.Fields.NeedsValue[1]; nv.Key != "size" || nv.Required || nv.Reason != "invalid_value" || nv.Message == "" {
		t.Errorf("needs_value[1] = %+v", nv)
	}

	// DR-15's full warning set — pointers so an ABSENT key fails rather
	// than silently decoding as the zero value.
	// Independent checks, not a switch: every missing warning should be
	// named in one run rather than one per fix cycle.
	warn := doc.Warnings
	if warn.ChildCount == nil || *warn.ChildCount != 2 {
		t.Errorf("child_count = %v", warn.ChildCount)
	}
	if warn.ChildrenOrphaned == nil || !*warn.ChildrenOrphaned {
		t.Errorf("children_orphaned = %v", warn.ChildrenOrphaned)
	}
	if warn.DroppedParent == nil || !*warn.DroppedParent {
		t.Errorf("dropped_parent = %v", warn.DroppedParent)
	}
	if warn.DroppedAssignee == nil || !*warn.DroppedAssignee {
		t.Errorf("dropped_assignee = %v", warn.DroppedAssignee)
	}
	if warn.DroppedAgentRole == nil || !*warn.DroppedAgentRole {
		t.Errorf("dropped_agent_role = %v", warn.DroppedAgentRole)
	}
	if warn.AttachmentCount == nil || *warn.AttachmentCount != 3 {
		t.Errorf("attachment_count = %v", warn.AttachmentCount)
	}
	if warn.AttachmentBytes == nil || *warn.AttachmentBytes != 1258291 {
		t.Errorf("attachment_bytes = %v", warn.AttachmentBytes)
	}
	if warn.UnresolvableRefCount == nil || *warn.UnresolvableRefCount != 1 {
		t.Errorf("unresolvable_ref_count = %v", warn.UnresolvableRefCount)
	}
	if warn.OutgoingLinks["blocks"] != 2 || warn.OutgoingLinks["related"] != 1 {
		t.Errorf("outgoing_links = %v", warn.OutgoingLinks)
	}
	if warn.IncomingLinks["blocked-by"] != 1 {
		t.Errorf("incoming_links = %v", warn.IncomingLinks)
	}

	// And the empty case: every warning key must still be PRESENT, so a
	// script cannot confuse "zero" with "this CLI dropped the field".
	d2 := &recordingDeps{t: t, preflight: emptyPreflight(), forbidCopy: true}
	var out2, errOut2 bytes.Buffer
	opts2 := baseOpts()
	opts2.DryRun = true
	opts2.Format = "json"
	if err := runItemCopy(opts2, d2.deps(), &out2, &errOut2); err != nil {
		t.Fatalf("runItemCopy (empty): %v", err)
	}
	var empty struct {
		Fields   map[string]json.RawMessage `json:"fields"`
		Warnings map[string]json.RawMessage `json:"warnings"`
	}
	if err := json.Unmarshal(out2.Bytes(), &empty); err != nil {
		t.Fatalf("empty preflight JSON: %v", err)
	}
	for _, k := range []string{"carried", "dropped", "needs_value"} {
		if _, ok := empty.Fields[k]; !ok {
			t.Errorf("fields.%s missing from the zero-case JSON", k)
		}
	}
	for _, k := range []string{
		"child_count", "children_orphaned", "dropped_parent", "outgoing_links", "incoming_links",
		"dropped_assignee", "dropped_agent_role", "attachment_count", "attachment_bytes", "unresolvable_ref_count",
	} {
		if _, ok := empty.Warnings[k]; !ok {
			t.Errorf("warnings.%s missing from the zero-case JSON", k)
		}
	}
}

func TestRunItemCopy_CopyJSONIsTheServerResponse(t *testing.T) {
	const payload = `{"source":{"slug":"a","archived":true,"seq":9007199254740993},"destination":{"slug":"b"},"archive_source":true,"item":null,"warnings":{"dropped_fields":[]},"future_field":1}`
	var res cli.ItemCopyResult
	if err := json.Unmarshal([]byte(payload), &res); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	pre := emptyPreflight()
	d := &recordingDeps{t: t, preflight: pre, result: &res, resultRaw: json.RawMessage(payload)}
	opts := baseOpts()
	opts.Format = "json"

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
		t.Fatalf("runItemCopy: %v", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, out.Bytes()); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if compact.String() != payload {
		t.Errorf("--format json altered the server's response.\n got: %s\nwant: %s", compact.String(), payload)
	}
}

// ── refuse to guess ──────────────────────────────────────────────────────

func TestRunItemCopy_RefusesWhenNeedsValueAndSendsNoMutatingRequest(t *testing.T) {
	d := &recordingDeps{t: t, preflight: fullPreflight(), forbidCopy: true}

	var out, errOut bytes.Buffer
	err := runItemCopy(baseOpts(), d.deps(), &out, &errOut)
	if err == nil {
		t.Fatal("expected a non-nil error so the command exits non-zero")
	}
	if !strings.Contains(err.Error(), "copy refused") {
		t.Errorf("error should say the copy was refused; got %v", err)
	}
	if len(d.copyCalls) != 0 {
		t.Fatalf("no mutating request may be sent; got %d", len(d.copyCalls))
	}

	stderr := errOut.String()
	for _, w := range []string{
		"No copy was attempted.",
		"priority",
		"size",
		`options: "low", "high"`,
		`"xl" is not one of s, m, l`,
		"--field priority=<value>",
		"--field size=<value>",
	} {
		if !strings.Contains(stderr, w) {
			t.Errorf("refusal missing %q\n--- stderr ---\n%s", w, stderr)
		}
	}
	if out.Len() != 0 {
		t.Errorf("the refusal belongs on stderr; stdout got %q", out.String())
	}
}

// On --format json the refusal still emits the endpoint's own response, so
// a script gets machine-readable needs_value rather than a bare exit code.
func TestRunItemCopy_RefusalUnderJSONEmitsThePreflight(t *testing.T) {
	pre := fullPreflight()
	raw, _ := json.Marshal(pre)
	d := &recordingDeps{t: t, preflight: pre, preflightRaw: raw, forbidCopy: true}
	opts := baseOpts()
	opts.Format = "json"

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err == nil {
		t.Fatal("expected a non-zero exit")
	}
	var decoded cli.ItemCopyPreflight
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not the preflight JSON: %v\n%s", err, out.String())
	}
	if len(decoded.Fields.NeedsValue) != 2 {
		t.Errorf("needs_value should survive to the script; got %d entries", len(decoded.Fields.NeedsValue))
	}
}

// The mirror of the refusal: the SAME command, against the SAME server
// behaviour, refuses without --field and proceeds with it.
//
// The fake preflight here is responsive rather than canned (Codex round 3):
// it reports needs_value=[priority] unless the request carries a priority
// override, exactly as the real endpoint does. A canned empty preflight
// would have made this a test of argument forwarding wearing the label of a
// test of the refuse-to-guess transition.
func TestRunItemCopy_RefusesWithoutOverrideAndProceedsWithIt(t *testing.T) {
	newDeps := func(t *testing.T, forbidCopy bool) *recordingDeps {
		d := &recordingDeps{
			t:          t,
			forbidCopy: forbidCopy,
			result: &cli.ItemCopyResult{
				Source:      cli.ItemCopyResultSource{WorkspaceSlug: "docapp", CollectionSlug: "ideas", Ref: "IDEA-12", Slug: "x", Title: "X"},
				Destination: cli.ItemCopyResultDestination{WorkspaceSlug: "pad-web", CollectionSlug: "tasks", Ref: "TASK-9", Slug: "x"},
				Warnings:    cli.ItemCopyResultWarnings{DroppedFields: []string{}},
			},
		}
		d.preflightFn = func(req cli.ItemCopyRequest) *cli.ItemCopyPreflight {
			pre := emptyPreflight()
			if _, ok := req.FieldOverrides["priority"]; !ok {
				pre.Valid = false
				pre.Fields.NeedsValue = []cli.ItemCopyPreflightNeedsValue{
					{Key: "priority", Label: "Priority", Type: "select", Required: true, Reason: "missing_required"},
				}
			}
			return pre
		}
		return d
	}

	t.Run("without --field: refuses, no mutation", func(t *testing.T) {
		d := newDeps(t, true)
		var out, errOut bytes.Buffer
		err := runItemCopy(baseOpts(), d.deps(), &out, &errOut)
		if err == nil {
			t.Fatal("expected a refusal")
		}
		if len(d.copyCalls) != 0 {
			t.Fatalf("no mutating request may be sent; got %d", len(d.copyCalls))
		}
		if !strings.Contains(errOut.String(), "--field priority=<value>") {
			t.Errorf("the refusal must name the flag to add:\n%s", errOut.String())
		}
	})

	t.Run("with --field: proceeds", func(t *testing.T) {
		d := newDeps(t, false)
		opts := baseOpts()
		opts.Fields = []string{"priority=high"}
		var out, errOut bytes.Buffer
		if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
			t.Fatalf("runItemCopy: %v", err)
		}
		if len(d.copyCalls) != 1 {
			t.Fatalf("expected exactly one mutating call; got %d", len(d.copyCalls))
		}
		if got := d.copyCalls[0].FieldOverrides["priority"]; got != "high" {
			t.Errorf("override did not reach the copy: %v", got)
		}
		if got := d.preflightCalls[0].FieldOverrides["priority"]; got != "high" {
			t.Errorf("override did not reach the preflight: %v", got)
		}
		if !strings.Contains(out.String(), "Copied IDEA-12 → pad-web TASK-9") {
			t.Errorf("unexpected output:\n%s", out.String())
		}
	})
}

// failingWriter is a stdout that stops working partway through.
type failingWriter struct {
	after int
	n     int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.n++
	if f.n > f.after {
		return 0, errors.New("no space left on device")
	}
	return len(p), nil
}

// A write failure AFTER the copy committed must not become a non-zero
// exit. A script that sees a non-zero exit will reasonably conclude the
// copy did not happen, and the obvious recovery — running it again — is
// exactly the DR-13 duplicate. The user is told on stderr instead.
func TestRunItemCopy_WriteFailureAfterCommitDoesNotFailTheCommand(t *testing.T) {
	for _, format := range []string{"table", "json"} {
		t.Run(format, func(t *testing.T) {
			raw := json.RawMessage(`{"source":{"slug":"x"},"destination":{"slug":"y"},"warnings":{"dropped_fields":[]}}`)
			d := &recordingDeps{
				t:         t,
				preflight: emptyPreflight(),
				result: &cli.ItemCopyResult{
					Source:      cli.ItemCopyResultSource{WorkspaceSlug: "docapp", CollectionSlug: "ideas", Slug: "x"},
					Destination: cli.ItemCopyResultDestination{WorkspaceSlug: "pad-web", CollectionSlug: "tasks", Slug: "y"},
					Warnings:    cli.ItemCopyResultWarnings{},
				},
				resultRaw: raw,
			}
			opts := baseOpts()
			opts.Format = format

			var errOut bytes.Buffer
			err := runItemCopy(opts, d.deps(), &failingWriter{}, &errOut)
			if err != nil {
				t.Fatalf("a committed copy must exit 0 even when its report cannot be written; got %v", err)
			}
			stderr := errOut.String()
			for _, w := range []string{"copy SUCCEEDED", "Do not re-run", "pad item list tasks --workspace pad-web"} {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr missing %q\n%s", w, stderr)
				}
			}
		})
	}
}

// The dry run has the opposite obligation: nothing happened, so failing to
// print the preview IS the failure and must exit non-zero.
func TestRunItemCopy_WriteFailureOnDryRunIsAnError(t *testing.T) {
	for _, format := range []string{"table", "json"} {
		t.Run(format, func(t *testing.T) {
			d := &recordingDeps{t: t, preflight: fullPreflight(), forbidCopy: true}
			opts := baseOpts()
			opts.DryRun = true
			opts.Format = format
			var errOut bytes.Buffer
			if err := runItemCopy(opts, d.deps(), &failingWriter{}, &errOut); err == nil {
				t.Fatal("a dry run that could not be printed must exit non-zero")
			}
		})
	}
}

// `valid: false` with an empty needs_value is a server contract violation.
// Guessing past it is the one thing this command must not do.
func TestRunItemCopy_RefusesOnInvalidWithoutNamedFields(t *testing.T) {
	pre := emptyPreflight()
	pre.Valid = false
	d := &recordingDeps{t: t, preflight: pre, forbidCopy: true}

	var out, errOut bytes.Buffer
	err := runItemCopy(baseOpts(), d.deps(), &out, &errOut)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "without naming a field") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ── DR-13 at the command layer ───────────────────────────────────────────

func TestRunItemCopy_AmbiguousFailureNeverSuggestsARetry(t *testing.T) {
	d := &recordingDeps{
		t:         t,
		preflight: emptyPreflight(),
		copyErr:   fmt.Errorf("%w: request failed: EOF", cli.ErrCopyOutcomeUnknown),
	}
	opts := baseOpts()
	opts.ArchiveSource = true

	var out, errOut bytes.Buffer
	err := runItemCopy(opts, d.deps(), &out, &errOut)
	if err == nil {
		t.Fatal("expected a non-zero exit")
	}
	if len(d.copyCalls) != 1 {
		t.Fatalf("the copy must be attempted exactly once; got %d", len(d.copyCalls))
	}
	stderr := errOut.String()
	for _, w := range []string{
		"UNKNOWN outcome",
		"may or may not have been created in workspace \"pad-web\"",
		"The source may or may not have been archived.",
		"never retries automatically",
		"DUPLICATE item",
		"pad item list tasks --workspace pad-web",
	} {
		if !strings.Contains(stderr, w) {
			t.Errorf("ambiguous-failure message missing %q\n--- stderr ---\n%s", w, stderr)
		}
	}
	// The whole point: nothing here may read as "try again".
	for _, forbidden := range []string{"retry the command", "run it again", "re-run the copy"} {
		if strings.Contains(strings.ToLower(stderr), forbidden) {
			t.Errorf("ambiguous-failure message must not suggest a retry; found %q", forbidden)
		}
	}
	if !strings.Contains(err.Error(), "check workspace") {
		t.Errorf("the returned error should point at the destination; got %v", err)
	}
}

// Codex round 4. A 2xx whose body could not be read or decoded means the
// copy COMMITTED. Returning that as a normal error would exit non-zero and
// tell a script the copy did not happen — the DR-13 duplicate, arrived at
// through the reporting layer instead of the network layer.
func TestRunItemCopy_CommittedButUnreportedExitsZero(t *testing.T) {
	for _, format := range []string{"table", "json"} {
		t.Run(format, func(t *testing.T) {
			raw := json.RawMessage(`{"source":`)
			d := &recordingDeps{
				t:         t,
				preflight: emptyPreflight(),
				copyErr:   fmt.Errorf("%w: decoding the response: unexpected EOF", cli.ErrCopyCommitted),
				resultRaw: raw,
			}
			opts := baseOpts()
			opts.Format = format

			var out, errOut bytes.Buffer
			if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
				t.Fatalf("a committed copy must exit 0; got %v", err)
			}
			if len(d.copyCalls) != 1 {
				t.Fatalf("expected exactly one copy attempt; got %d", len(d.copyCalls))
			}
			stderr := errOut.String()
			for _, w := range []string{"copy SUCCEEDED", "Do not re-run", "pad item list tasks --workspace pad-web"} {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr missing %q\n%s", w, stderr)
				}
			}
			if strings.Contains(stderr, "UNKNOWN") {
				t.Errorf("a committed copy is not an unknown outcome:\n%s", stderr)
			}
			if format == "json" && !strings.Contains(out.String(), `"source"`) {
				t.Errorf("the bytes that DID arrive should still reach stdout:\n%s", out.String())
			}
		})
	}
}

// Codex round 8. archive_source is what was ASKED for and source.archived
// is what HAPPENED; the server's contract is that they agree. When they do
// not, a MOVE did not complete, and exiting 0 would tell automation it did.
func TestRunItemCopy_PartialMoveExitsNonZeroAndSaysWhatExists(t *testing.T) {
	cases := []struct {
		name          string
		archiveSource bool
		archived      bool
		wantStderr    []string
	}{
		{
			name: "asked to archive, source survived", archiveSource: true, archived: false,
			wantStderr: []string{"PARTIAL", "do not re-run", "is NOT archived", "pad item delete IDEA-12 --workspace docapp"},
		},
		{
			name: "did not ask, source archived anyway", archiveSource: false, archived: true,
			wantStderr: []string{"PARTIAL", "was archived anyway", "pad item restore IDEA-12 --workspace docapp"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &recordingDeps{
				t:         t,
				preflight: emptyPreflight(),
				result: &cli.ItemCopyResult{
					Source: cli.ItemCopyResultSource{
						WorkspaceSlug: "docapp", CollectionSlug: "ideas", Ref: "IDEA-12", Slug: "x", Archived: tc.archived,
					},
					Destination: cli.ItemCopyResultDestination{
						WorkspaceSlug: "pad-web", CollectionSlug: "tasks", Ref: "TASK-9", Slug: "y",
					},
					ArchiveSource: tc.archiveSource,
					Warnings:      cli.ItemCopyResultWarnings{},
				},
			}
			opts := baseOpts()
			opts.ArchiveSource = tc.archiveSource

			var out, errOut bytes.Buffer
			err := runItemCopy(opts, d.deps(), &out, &errOut)
			if err == nil {
				t.Fatal("a partial outcome must exit non-zero")
			}
			for _, w := range tc.wantStderr {
				if !strings.Contains(errOut.String(), w) {
					t.Errorf("stderr missing %q\n%s", w, errOut.String())
				}
			}
			// The copy itself is still not repeatable.
			if len(d.copyCalls) != 1 {
				t.Fatalf("expected exactly one copy attempt; got %d", len(d.copyCalls))
			}
		})
	}
}

// The agreeing case — every successful response — stays exit 0.
func TestRunItemCopy_MoveThatArchivedExitsZero(t *testing.T) {
	d := &recordingDeps{
		t:         t,
		preflight: emptyPreflight(),
		result: &cli.ItemCopyResult{
			Source:        cli.ItemCopyResultSource{WorkspaceSlug: "docapp", CollectionSlug: "ideas", Ref: "IDEA-12", Slug: "x", Archived: true},
			Destination:   cli.ItemCopyResultDestination{WorkspaceSlug: "pad-web", CollectionSlug: "tasks", Ref: "TASK-9", Slug: "y"},
			ArchiveSource: true,
			Warnings:      cli.ItemCopyResultWarnings{},
		},
	}
	opts := baseOpts()
	opts.ArchiveSource = true
	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
		t.Fatalf("a complete move must exit 0; got %v", err)
	}
	if strings.Contains(errOut.String(), "PARTIAL") {
		t.Errorf("no partial warning expected:\n%s", errOut.String())
	}
	if !strings.Contains(out.String(), "Moved IDEA-12 → pad-web TASK-9") {
		t.Errorf("unexpected output:\n%s", out.String())
	}
}

// Codex round 8. The preflight receives the overrides, so a --field the
// destination REJECTED comes back in needs_value. Telling that user "no
// --field supplied one" sends them hunting for a bug in their own command
// line, and repeating the same flag as the fix is worse.
func TestRunItemCopy_RejectedOverrideIsNotReportedAsMissing(t *testing.T) {
	pre := emptyPreflight()
	pre.Valid = false
	pre.Fields.NeedsValue = []cli.ItemCopyPreflightNeedsValue{
		{Key: "size", Label: "Size", Type: "select", Required: true, Reason: "invalid_value",
			Options: []string{"s", "m"}, Message: `"xl" is not one of s, m`},
		{Key: "priority", Required: true, Reason: "missing_required"},
	}
	d := &recordingDeps{t: t, preflight: pre, forbidCopy: true}
	opts := baseOpts()
	opts.Fields = []string{"size=xl"}

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err == nil {
		t.Fatal("expected a refusal")
	}
	stderr := errOut.String()
	if strings.Contains(stderr, "no --field supplied one") {
		t.Errorf("the user DID supply --field size=xl:\n%s", stderr)
	}
	for _, w := range []string{
		`you supplied "xl" — the destination rejected it`,
		"1 supplied with --field and rejected",
		"Add: --field priority=<value>",
		"Correct: --field size=<a valid value>",
	} {
		if !strings.Contains(stderr, w) {
			t.Errorf("stderr missing %q\n%s", w, stderr)
		}
	}
	// The rejected key must NOT be offered as something to "add".
	if strings.Contains(stderr, "--field size=<value>") {
		t.Errorf("repeating the same flag is not the fix:\n%s", stderr)
	}
}

// When EVERY unresolved field was supplied and rejected, the headline says
// so outright.
func TestRunItemCopy_AllOverridesRejected(t *testing.T) {
	pre := emptyPreflight()
	pre.Valid = false
	pre.Fields.NeedsValue = []cli.ItemCopyPreflightNeedsValue{
		{Key: "size", Required: true, Reason: "invalid_value"},
	}
	d := &recordingDeps{t: t, preflight: pre, forbidCopy: true}
	opts := baseOpts()
	opts.Fields = []string{"size=xl"}

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err == nil {
		t.Fatal("expected a refusal")
	}
	stderr := errOut.String()
	if !strings.Contains(stderr, "the --field value you supplied was rejected") {
		t.Errorf("unexpected headline:\n%s", stderr)
	}
	if strings.Contains(stderr, "Add:") {
		t.Errorf("nothing to add — everything was supplied already:\n%s", stderr)
	}
}

// A refusal the server made before writing anything is passed straight
// through — no unknown-outcome scare text.
func TestRunItemCopy_DeterministicFailureIsReportedPlainly(t *testing.T) {
	apiErr := &cli.APIError{Code: "plan_limit_exceeded", Message: "workspace is at its item limit"}
	d := &recordingDeps{t: t, preflight: emptyPreflight(), copyErr: apiErr}

	var out, errOut bytes.Buffer
	err := runItemCopy(baseOpts(), d.deps(), &out, &errOut)
	if !errors.Is(err, error(apiErr)) {
		t.Fatalf("expected the API error to pass through; got %v", err)
	}
	if strings.Contains(errOut.String(), "UNKNOWN") {
		t.Errorf("a 4xx is not an unknown outcome:\n%s", errOut.String())
	}
}

// ── flag validation ──────────────────────────────────────────────────────

func TestRunItemCopy_FlagValidationSendsNothing(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*itemCopyOptions)
		wantErr string
	}{
		{"no workspace", func(o *itemCopyOptions) { o.TargetWorkspace = "" }, "--to-workspace is required"},
		{"blank workspace", func(o *itemCopyOptions) { o.TargetWorkspace = "   " }, "--to-workspace is required"},
		{"no collection", func(o *itemCopyOptions) { o.TargetCollection = "" }, "--collection is required"},
		{"field without =", func(o *itemCopyOptions) { o.Fields = []string{"priority"} }, `invalid --field "priority"`},
		{"field with empty key", func(o *itemCopyOptions) { o.Fields = []string{"=high"} }, `invalid --field "=high"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &recordingDeps{t: t, preflight: emptyPreflight(), forbidCopy: true}
			opts := baseOpts()
			tc.mutate(&opts)
			var out, errOut bytes.Buffer
			err := runItemCopy(opts, d.deps(), &out, &errOut)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if len(d.preflightCalls) != 0 {
				t.Errorf("a rejected flag must not reach the server; got %d preflight calls", len(d.preflightCalls))
			}
		})
	}
}

// `--field key=` is a legitimate empty string, not a malformed flag.
func TestRunItemCopy_EmptyFieldValueIsAllowed(t *testing.T) {
	d := &recordingDeps{t: t, preflight: emptyPreflight(), forbidCopy: true}
	opts := baseOpts()
	opts.DryRun = true
	opts.Fields = []string{"notes="}
	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
		t.Fatalf("runItemCopy: %v", err)
	}
	if got, ok := d.preflightCalls[0].FieldOverrides["notes"]; !ok || got != "" {
		t.Errorf("expected notes to be the empty string; got %v (present=%v)", got, ok)
	}
}

// --field values are typed against the DESTINATION collection's schema —
// the source's would be the wrong authority, and a number sent as a string
// fails the destination's validator.
func TestRunItemCopy_FieldsAreTypedAgainstTheDestinationSchema(t *testing.T) {
	d := &recordingDeps{
		t:          t,
		preflight:  emptyPreflight(),
		forbidCopy: true,
		schema: models.CollectionSchema{Fields: []models.FieldDef{
			{Key: "points", Type: "number"},
			{Key: "done", Type: "checkbox"},
			{Key: "title", Type: "text"},
		}},
	}
	opts := baseOpts()
	opts.DryRun = true
	opts.TargetCollection = "task" // singular; normalized on the way out
	opts.Fields = []string{"points=3", "done=true", "title=5"}

	var out, errOut bytes.Buffer
	if err := runItemCopy(opts, d.deps(), &out, &errOut); err != nil {
		t.Fatalf("runItemCopy: %v", err)
	}
	ov := d.preflightCalls[0].FieldOverrides
	if ov["points"] != 3.0 {
		t.Errorf("points = %#v, want float64(3)", ov["points"])
	}
	if ov["done"] != true {
		t.Errorf("done = %#v, want bool true", ov["done"])
	}
	if ov["title"] != "5" {
		t.Errorf("title = %#v, want the string \"5\"", ov["title"])
	}
	if len(d.schemaCalls) != 1 || d.schemaCalls[0] != [2]string{"pad-web", "tasks"} {
		t.Errorf("schema should be fetched from the destination as the normalized slug; got %v", d.schemaCalls)
	}
	if d.preflightCalls[0].TargetCollection != "tasks" {
		t.Errorf("target_collection = %q, want the normalized %q", d.preflightCalls[0].TargetCollection, "tasks")
	}
}

// A preflight failure aborts before the mutation. The dry run is the only
// thing that can tell us whether a copy is safe to send.
func TestRunItemCopy_PreflightFailureAbortsBeforeMutating(t *testing.T) {
	d := &recordingDeps{
		t:            t,
		preflightErr: &cli.APIError{Code: "collection_not_found", Message: "no such collection"},
		forbidCopy:   true,
	}
	var out, errOut bytes.Buffer
	if err := runItemCopy(baseOpts(), d.deps(), &out, &errOut); err == nil {
		t.Fatal("expected the preflight error to propagate")
	}
	if len(d.copyCalls) != 0 {
		t.Fatalf("got %d mutating calls", len(d.copyCalls))
	}
}

// ── result rendering ─────────────────────────────────────────────────────

func TestRenderItemCopyResult(t *testing.T) {
	res := &cli.ItemCopyResult{
		Source: cli.ItemCopyResultSource{
			WorkspaceSlug: "docapp", CollectionSlug: "ideas", Ref: "IDEA-12", Slug: "x", Title: "Cross-workspace copy",
			Archived: true, Seq: 412,
		},
		Destination: cli.ItemCopyResultDestination{
			WorkspaceSlug: "pad-web", WorkspaceName: "Pad Web", CollectionSlug: "tasks",
			Ref: "TASK-9", Slug: "cross-workspace-copy", Seq: 88,
		},
		ArchiveSource: true,
		Warnings: cli.ItemCopyResultWarnings{
			DroppedFields:        []string{"impact", "effort"},
			DroppedAssignee:      true,
			AttachmentCount:      3,
			AttachmentBytes:      1258291,
			UnresolvableRefCount: 1,
		},
	}
	var buf bytes.Buffer
	renderItemCopyResult(&buf, res)
	got := buf.String()
	for _, w := range []string{
		"Moved IDEA-12 → pad-web TASK-9",
		"archived    yes",
		`fields dropped                   "impact", "effort"`,
		"assignee dropped                 yes",
		"agent role dropped               no",
		"attachments cloned               3 (1.2 MiB, 1258291 bytes)",
		"unresolvable attachment refs     1",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("result output missing %q\n--- got ---\n%s", w, got)
		}
	}
}

// A plain copy says "Copied", and an empty dropped_fields list says so
// rather than rendering a blank.
func TestRenderItemCopyResult_PlainCopyAndEmptyDropList(t *testing.T) {
	res := &cli.ItemCopyResult{
		Source:      cli.ItemCopyResultSource{WorkspaceSlug: "docapp", CollectionSlug: "ideas", Slug: "x", Title: "X"},
		Destination: cli.ItemCopyResultDestination{WorkspaceSlug: "pad-web", CollectionSlug: "tasks", Slug: "y"},
		Warnings:    cli.ItemCopyResultWarnings{},
	}
	var buf bytes.Buffer
	renderItemCopyResult(&buf, res)
	got := buf.String()
	if !strings.Contains(got, "Copied x → pad-web y") {
		t.Errorf("a plain copy should say Copied and fall back to the slug:\n%s", got)
	}
	if !strings.Contains(got, "fields dropped                   (none)") {
		t.Errorf("an empty dropped list must say (none):\n%s", got)
	}
	if !strings.Contains(got, "archived    no") {
		t.Errorf("archived must be reported even when false:\n%s", got)
	}
}

// archive_source and source.archived agree on every successful response.
// If they ever do not, say so rather than silently believing one.
func TestRenderItemCopyResult_FlagsDisagreement(t *testing.T) {
	res := &cli.ItemCopyResult{
		Source:        cli.ItemCopyResultSource{Slug: "x", Archived: false},
		Destination:   cli.ItemCopyResultDestination{Slug: "y"},
		ArchiveSource: true,
		Warnings:      cli.ItemCopyResultWarnings{},
	}
	var buf bytes.Buffer
	renderItemCopyResult(&buf, res)
	if !strings.Contains(buf.String(), "WARNING") {
		t.Errorf("a disagreement between archive_source and source.archived must be surfaced:\n%s", buf.String())
	}
}

// ── wiring ───────────────────────────────────────────────────────────────

func TestItemCopyCmd_IsRegisteredWithEveryFlag(t *testing.T) {
	group := itemCmd()
	var found bool
	for _, sub := range group.Commands() {
		if sub.Name() != "copy" {
			continue
		}
		found = true
		for _, flag := range []string{"to-workspace", "collection", "dry-run", "archive-source", "field"} {
			if sub.Flags().Lookup(flag) == nil {
				t.Errorf("pad item copy is missing the --%s flag", flag)
			}
		}
		if sub.Short == "" || sub.Long == "" {
			t.Error("pad item copy needs both a Short and a Long description")
		}
		// The no-retry contract has to be discoverable from --help; it is
		// the single most surprising thing about this command.
		if !strings.Contains(sub.Long, "NEVER retried") {
			t.Error("--help must document that the copy is never retried automatically")
		}
	}
	if !found {
		t.Fatal("pad item copy is not registered in the item group")
	}
}
