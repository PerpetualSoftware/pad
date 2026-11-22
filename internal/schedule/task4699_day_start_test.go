package schedule

import (
	"encoding/json"
	"strings"
	"testing"
)

// TASK-4699: --day-start / day_start rides the body as an instruction; the
// server rewrites the bare start. Only the window combination is refused here.
func TestTask4699_CommitmentBodyCarriesDayStart(t *testing.T) {
	body, err := CommitmentBody(CommitmentFlags{Start: "2026-11-09", DayStart: true}, nil, "")
	if err != nil || body["day_start"] != true || body["dtstart"] != "2026-11-09" {
		t.Fatalf("create: body=%v err=%v", body, err)
	}
	body, err = CommitmentBody(CommitmentFlags{Start: "2026-11-09"}, nil, "")
	if _, has := body["day_start"]; err != nil || has {
		t.Fatalf("without day_start: body=%v err=%v", body, err)
	}

	// Edit: only when named; day_start alone is a change the server judges.
	given := func(names ...string) func(string) bool {
		return func(n string) bool {
			for _, m := range names {
				if n == m {
					return true
				}
			}
			return false
		}
	}
	body, err = CommitmentBody(CommitmentFlags{Start: "2026-11-10", DayStart: true}, given("start", "day_start"), "")
	if raw, _ := json.Marshal(body); err != nil || string(raw) != `{"day_start":true,"dtstart":"2026-11-10"}` {
		t.Fatalf("edit: %s err=%v", raw, err)
	}
	body, err = CommitmentBody(CommitmentFlags{DayStart: true}, given("day_start"), "")
	if raw, _ := json.Marshal(body); err != nil || string(raw) != `{"day_start":true}` {
		t.Fatalf("edit day_start alone: %s err=%v", raw, err)
	}
	if _, err := CommitmentBody(CommitmentFlags{DayStart: true}, given(), ""); err == nil {
		t.Fatal("edit with day_start not named sent a body")
	}

	for name, c := range map[string]struct {
		f       CommitmentFlags
		changed func(string) bool
	}{
		"create": {CommitmentFlags{Start: "2026-11-09", Window: "3d", DayStart: true}, nil},
		"edit":   {CommitmentFlags{Start: "2026-11-09", Window: "3d", DayStart: true}, given("start", "window", "day_start")},
	} {
		if _, err := CommitmentBody(c.f, c.changed, ""); err == nil || !strings.Contains(err.Error(), "--day-start") || !strings.Contains(err.Error(), "--window") {
			t.Errorf("%s: day_start with window: err=%v", name, err)
		}
	}
}
