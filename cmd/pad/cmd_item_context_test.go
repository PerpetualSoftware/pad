package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// PLAN-3598: the markdown render of `pad item context`. The body is decoded
// the way the command decodes the server's response, so a renamed JSON key
// fails here rather than rendering an empty section.
func TestRenderItemContextMarkdown(t *testing.T) {
	const body = `{
	  "intent": "implement", "triggers": ["always", "on-implement"], "role": "implementer",
	  "budget": 40000, "used": 1234,
	  "item": {"ref": "TASK-5", "title": "Do it", "seq": 7, "content": "task body\n"},
	  "parents": [
	    {"ref": "PLAN-2", "title": "The plan", "seq": 3, "status": "active", "content": "line one\nline two",
	     "acceptance_criteria": {"criteria": [{"id": "AC-1", "text": "names its parent"}], "placeholders": ["AC-2"]}},
	    {"ref": "IDEA-1", "title": "The idea", "seq": 1, "status": "", "content": "not shown"}
	  ],
	  "conventions": [{"ref": "CONVE-4", "title": "Test first", "seq": 2, "trigger": "on-implement", "priority": "must", "role": "implementer", "content": "write the test\n"}],
	  "playbooks": [{"ref": "PLAYB-9", "title": "Ship", "invocation_slug": "ship"}, {"ref": "PLAYB-10", "title": "Triggered", "invocation_slug": ""}],
	  "deps": {"blocks": [{"ref": "TASK-8", "title": "Later", "link_type": "blocks", "status": "open"}],
	           "blocked_by": [{"ref": "DOC-3", "title": "Spec", "link_type": "blocks", "status": "draft"}]},
	  "decisions": [{"question_key": "scope", "current": true}, {"question_key": "risk", "current": false}],
	  "links": [{"ref": "TASK-9", "title": "Sibling", "link_type": "related", "status": ""}],
	  "omitted": [{"part": "convention", "ref": "CONVE-7", "size": 900}, {"part": "links", "ref": "", "size": 50}],
	  "conflicts": [{"kind": "open_blocker", "detail": "TASK-5 is blocked by DOC-3, which is still open"}]
	}`
	var v itemContextView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	renderItemContextMarkdown(&buf, &v)
	out := buf.String()

	for _, want := range []string{
		"# TASK-5: Do it (seq 7)\n\n",
		"Intent: implement (triggers: always, on-implement); role: implementer; 1234 of 40000 characters\n\n",
		"task body\n\n## Parents\n\n",
		"- PLAN-2 The plan (active, seq 3)\n  - AC-1: names its parent\n  - (1 placeholder criteria not counted)\n",
		"  > line one\n  > line two\n",
		"- IDEA-1 The idea (-, seq 1)\n",
		"### CONVE-4 Test first (on-implement, must, role implementer)\n\nwrite the test\n\n",
		"- PLAYB-9 Ship (/ship)\n- PLAYB-10 Triggered\n",
		"- blocked by DOC-3 Spec (draft)\n- blocks TASK-8 Later (open)\n",
		"- scope (current)\n- risk (stale)\n",
		"- related TASK-9 Sibling (-)\n",
		"## Conflicts\n\n- open_blocker: TASK-5 is blocked by DOC-3, which is still open\n",
		"- convention CONVE-7 (900 characters)\n- links - (50 characters)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render is missing %q\n--- got ---\n%s", want, out)
		}
	}
	// Only the nearest parent's body is shown.
	if strings.Contains(out, "not shown") {
		t.Errorf("a non-nearest parent's body was rendered:\n%s", out)
	}

	// An empty brief renders no section headers.
	var empty itemContextView
	empty.Item.Ref, empty.Item.Title = "TASK-1", "Bare"
	buf.Reset()
	renderItemContextMarkdown(&buf, &empty)
	if strings.Contains(buf.String(), "## ") {
		t.Errorf("an empty brief rendered a section:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Intent: - (triggers: )") {
		t.Errorf("an empty intent should render as a dash:\n%s", buf.String())
	}
}
