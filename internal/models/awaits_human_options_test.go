package models

import (
	"reflect"
	"strings"
	"testing"
)

// TASK-4643: a field's awaits_human_options must name its own non-terminal
// options, compared trimmed and case-insensitively like abandoned_options.
func TestValidateAwaitsHumanOptions(t *testing.T) {
	t.Parallel()
	status := func(awaits ...string) CollectionSchema {
		return CollectionSchema{Fields: []FieldDef{{
			Key: "status", Type: "select",
			Options:            []string{"open", "needs-review", "Blocked", "done"},
			TerminalOptions:    []string{"done"},
			AwaitsHumanOptions: awaits,
		}}}
	}
	for name, tc := range map[string]struct {
		schema  CollectionSchema
		wantErr string
	}{
		"none declared":          {schema: status()},
		"valid subset":           {schema: status("needs-review", "Blocked")},
		"trimmed and folded":     {schema: status(" Needs-Review ", "blocked")},
		"not an option":          {schema: status("waiting"), wantErr: `field "status": awaits_human_options value "waiting" is not one of its options`},
		"a terminal option":      {schema: status("needs-review", "Done"), wantErr: `field "status": awaits_human_options value "Done" is one of its terminal_options`},
		"on a non-done field ok": {schema: CollectionSchema{Fields: []FieldDef{{Key: "review", Type: "select", Options: []string{"asked", "given"}, AwaitsHumanOptions: []string{"asked"}}}}},
		"on a non-done field, not an option": {
			schema:  CollectionSchema{Fields: []FieldDef{{Key: "review", Type: "select", Options: []string{"asked"}, AwaitsHumanOptions: []string{"given"}}}},
			wantErr: `field "review": awaits_human_options value "given" is not one of its options`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateAwaitsHumanOptions(tc.schema)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// The done field's declared waiting values, minus anything the done field
// treats as terminal (including the default terminal fallback when the field
// declares no terminal_options).
func TestAwaitsHumanValuesForDoneField(t *testing.T) {
	t.Parallel()
	declared := CollectionSchema{Fields: []FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "needs-review", "done"}, TerminalOptions: []string{"done"}, AwaitsHumanOptions: []string{"needs-review"}},
		{Key: "phase", Type: "select", Options: []string{"draft", "asked", "shipped"}, TerminalOptions: []string{"shipped"}, AwaitsHumanOptions: []string{"asked"}},
	}}
	for name, tc := range map[string]struct {
		schema   CollectionSchema
		settings CollectionSettings
		wantKey  string
		want     []string
	}{
		"status is the done field": {schema: declared, wantKey: "status", want: []string{"needs-review"}},
		"board_group_by moves it":  {schema: declared, settings: CollectionSettings{BoardGroupBy: "phase"}, wantKey: "phase", want: []string{"asked"}},
		"none declared":            {schema: CollectionSchema{Fields: []FieldDef{{Key: "status", Type: "select", Options: []string{"open", "done"}}}}, wantKey: "status"},
		"default terminals subtracted": {
			// No terminal_options: the default list applies, and "resolved" is in it.
			schema:  CollectionSchema{Fields: []FieldDef{{Key: "status", Type: "select", Options: []string{"open", "asked", "resolved"}, AwaitsHumanOptions: []string{"asked", "Resolved"}}}},
			wantKey: "status", want: []string{"asked"},
		},
		"multi_select is never the done field": {
			schema:  CollectionSchema{Fields: []FieldDef{{Key: "status", Type: "multi_select", Options: []string{"asked"}, AwaitsHumanOptions: []string{"asked"}}}},
			wantKey: "status",
		},
	} {
		t.Run(name, func(t *testing.T) {
			key, got := AwaitsHumanValuesForDoneField(tc.schema, tc.settings)
			if key != tc.wantKey {
				t.Errorf("key = %q, want %q", key, tc.wantKey)
			}
			if len(got) != 0 || len(tc.want) != 0 {
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("values = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
