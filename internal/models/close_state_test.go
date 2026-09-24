package models

import "testing"

// BUG-2367 item 4: only the STATE is compared (lead ruling, day 78).
func TestMigrateCloseStateChange(t *testing.T) {
	tasks := CollectionSchema{Fields: []FieldDef{{Key: "status", Type: "select",
		Options:          []string{"open", "in-progress", "done", "cancelled"},
		TerminalOptions:  []string{"done", "cancelled"},
		AbandonedOptions: []string{"cancelled"}}}}
	ideas := CollectionSchema{Fields: []FieldDef{{Key: "status", Label: "Status", Type: "select",
		Options:          []string{"new", "planned", "implemented", "rejected"},
		TerminalOptions:  []string{"implemented", "rejected"},
		AbandonedOptions: []string{"rejected"}, Default: "new"}}}
	docs := CollectionSchema{Fields: []FieldDef{{Key: "category", Type: "text"}}}
	none := CollectionSettings{}

	cases := []struct {
		name     string
		src      map[string]any
		srcS     CollectionSchema
		dst      map[string]any
		dstS     CollectionSchema
		supplied string
		want     *[2]CloseState
	}{
		{"done task defaulted to open idea", map[string]any{"status": "done"}, tasks, map[string]any{"status": "new"}, ideas, "", &[2]CloseState{CloseStateDone, CloseStateOpen}},
		{"done and implemented are both delivered", map[string]any{"status": "done"}, tasks, map[string]any{"status": "implemented"}, ideas, "", nil},
		{"cancelled to implemented flips abandoned to done", map[string]any{"status": "cancelled"}, tasks, map[string]any{"status": "implemented"}, ideas, "", &[2]CloseState{CloseStateAbandoned, CloseStateDone}},
		{"cancelled to rejected stays abandoned", map[string]any{"status": "cancelled"}, tasks, map[string]any{"status": "rejected"}, ideas, "", nil},
		{"open to open", map[string]any{"status": "open"}, tasks, map[string]any{"status": "new"}, ideas, "", nil},
		{"supplied value is the override", map[string]any{"status": "done"}, tasks, map[string]any{"status": "new"}, ideas, "status", nil},
		{"destination with no done field has no state", map[string]any{"status": "done"}, tasks, map[string]any{}, docs, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MigrateCloseStateChange(tc.src, tc.srcS, none, tc.dst, tc.dstS, none,
				func(k string) bool { return k == tc.supplied })
			if tc.want == nil {
				if got != nil {
					t.Fatalf("want no change, got %+v", got)
				}
				return
			}
			if got == nil || got.From != tc.want[0] || got.To != tc.want[1] {
				t.Fatalf("want %v→%v, got %+v", tc.want[0], tc.want[1], got)
			}
		})
	}

	ch := MigrateCloseStateChange(map[string]any{"status": "done"}, tasks, none, map[string]any{"status": "new"}, ideas, none, nil)
	want := `this would change the item from done (status "done") to open (status "new"); set status explicitly to one of: new, planned, implemented, rejected`
	if ch.Message() != want {
		t.Fatalf("message:\n got %s\nwant %s", ch.Message(), want)
	}
}
